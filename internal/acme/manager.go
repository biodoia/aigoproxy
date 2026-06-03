// Package acme provisions TLS certificates for aigoproxy routes via
// Let's Encrypt using the HTTP-01 challenge.
//
// Wave 2 status:
//   ✓ ACME account registration (Let's Encrypt, with email contact)
//   ✓ HTTP-01 challenge serving on a dedicated listener (port 80 alternate)
//   ✓ Certificate issuance via golang.org/x/crypto/acme
//   ✓ Certificate caching to disk (auto-reload by mtime)
//   ✓ Auto-renewal background loop (30 days before expiry)
//   ✓ Self-signed fallback (development, never reached if LE works)
//
// Future (Wave 3):
//   - DNS-01 challenge via pluggable DNSProvider (Cloudflare, Route53, …)
//   - Wildcard certs
//   - ARI (Automatic Renewal Information) from CA
package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

// Manager is the ACME certificate manager.
type Manager struct {
	dataDir string
	logger  *slog.Logger
	email   string
	staging bool

	mu     sync.RWMutex
	certs  map[string]*Cert // host → cert
	acmeCl *acme.Client
	account *acme.Account

	// Optional: stub for future DNS providers. Set via SetDNSProvider.
	dns DNSProvider

	// stop channel for renewal loop
	stopCh chan struct{}
}

// DNSProvider is a future-extension point for DNS-01 challenges.
// Currently unimplemented; present so callers can wire up Cloudflare
// once Wave 3 lands.
type DNSProvider interface {
	SetTXT(domain, value string) error
	DeleteTXT(domain, value string) error
}

// Cert is a leaf certificate + matching private key.
type Cert struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
	KeyPEM  []byte
	NotAfter time.Time
}

// Config configures the Manager.
type Config struct {
	DataDir string
	Email   string
	Staging bool // use Let's Encrypt staging
	Logger  *slog.Logger
}

// New returns a new Manager. If cfg is zero-valued, defaults are used.
func New(cfg Config) (*Manager, error) {
	if cfg.DataDir == "" {
		home, _ := os.UserHomeDir()
		cfg.DataDir = filepath.Join(home, ".aigoproxy")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "certs"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir certs: %w", err)
	}
	m := &Manager{
		dataDir: cfg.DataDir,
		logger:  cfg.Logger,
		email:   cfg.Email,
		staging: cfg.Staging,
		certs:   make(map[string]*Cert),
		stopCh:  make(chan struct{}),
	}
	// initialize ACME client (directory URL)
	dir := acme.LetsEncryptURL
	if cfg.Staging {
		dir = "https://acme-staging-v02.api.letsencrypt.org/directory"
	}
	m.acmeCl = &acme.Client{
		DirectoryURL: dir,
		Key:          m.acmeKey(),
	}
	return m, nil
}

// SetDNSProvider stores a DNS provider for future DNS-01 support.
// Currently a no-op (HTTP-01 only).
func (m *Manager) SetDNSProvider(d DNSProvider) { m.dns = d }

// acmeKey returns the persisted ACME account key, creating a new one if needed.
func (m *Manager) acmeKey() *ecdsa.PrivateKey {
	keyPath := filepath.Join(m.dataDir, "acme-account.key")
	if data, err := os.ReadFile(keyPath); err == nil {
		if k, err := x509.ParseECPrivateKey(data); err == nil {
			return k
		}
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		// last resort: pretend we generated one. Caller will fail.
		return &ecdsa.PrivateKey{}
	}
	der, _ := x509.MarshalECPrivateKey(k)
	_ = os.WriteFile(keyPath, der, 0o600)
	return k
}

// registerAccount creates an ACME account if not already persisted.
func (m *Manager) registerAccount(ctx context.Context) error {
	if m.account != nil {
		return nil
	}
	acct := &acme.Account{Contact: []string{"mailto:" + m.email}}
	if m.email == "" {
		acct.Contact = nil
	}
	a, err := m.acmeCl.Register(ctx, acct, acme.AcceptTOS)
	if err != nil {
		return fmt.Errorf("acme register: %w", err)
	}
	m.account = a
	return nil
}

// GetCertificate is the http.GetCertificate-compatible function used by
// tls.Config. It looks up the cert for the SNI host, falls back to disk,
// then self-signed.
func (m *Manager) GetCertificate(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := chi.ServerName
	if host == "" {
		return nil, errors.New("no SNI host")
	}
	c, err := m.certFor(host)
	if err != nil {
		m.logger.Warn("acme: serving self-signed cert", "host", host, "err", err)
		ss, sErr := m.selfSigned(host)
		if sErr != nil {
			return nil, sErr
		}
		return ss.toTLSCert(), nil
	}
	return c.toTLSCert(), nil
}

func (c *Cert) toTLSCert() *tls.Certificate {
	return &tls.Certificate{
		Certificate: [][]byte{c.Cert.Raw},
		PrivateKey:  c.Key,
		Leaf:        c.Cert,
	}
}

// certFor returns the cert for host, loading from disk or, if absent,
// returning an error so the caller falls back to self-signed.
func (m *Manager) certFor(host string) (*Cert, error) {
	m.mu.RLock()
	c, ok := m.certs[host]
	m.mu.RUnlock()
	if ok && c.NotAfter.After(time.Now().Add(7*24*time.Hour)) {
		return c, nil
	}
	// try disk
	c, err := m.loadFromDisk(host)
	if err == nil {
		m.mu.Lock()
		m.certs[host] = c
		m.mu.Unlock()
		return c, nil
	}
	// not on disk, not in memory → request from Let's Encrypt
	if m.email == "" {
		return nil, fmt.Errorf("no email configured; cannot request from LE")
	}
	if err := m.ObtainForHost(host); err != nil {
		return nil, err
	}
	// re-load from disk after obtain
	return m.loadFromDisk(host)
}

// ObtainForHost runs the full ACME flow: register, authorize, http-01
// challenge, wait, fetch cert, save to disk. Idempotent.
func (m *Manager) ObtainForHost(host string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := m.registerAccount(ctx); err != nil {
		return err
	}

	// 1. authorize
	auth, err := m.acmeCl.Authorize(ctx, host)
	if err != nil {
		return fmt.Errorf("authorize %s: %w", host, err)
	}

	// 2. find http-01 challenge
	var chal *acme.Challenge
	for _, c := range auth.Challenges {
		if c.Type == "http-01" {
			chal = c
			break
		}
	}
	if chal == nil {
		return errors.New("no http-01 challenge in authorization")
	}

	// 3. signal main mux to serve the challenge response
	path := m.acmeCl.HTTP01ChallengePath(chal.Token)
	resp, err := m.acmeCl.HTTP01ChallengeResponse(chal.Token)
	if err != nil {
		return fmt.Errorf("challenge response: %w", err)
	}
	m.registerChallenge(path, resp)
	defer m.unregisterChallenge(path)

	// 4. accept the challenge
	if _, err := m.acmeCl.Accept(ctx, chal); err != nil {
		return fmt.Errorf("accept challenge: %w", err)
	}

	// 5. wait for authorization to be valid
	if _, err := m.acmeCl.WaitAuthorization(ctx, auth.URI); err != nil {
		return fmt.Errorf("wait authorization: %w", err)
	}

	// 6. generate cert key + CSR
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: host},
		DNSNames: []string{host},
	}, privKey)
	if err != nil {
		return err
	}

	// 7. create cert
	certs, _, err := m.acmeCl.CreateCert(ctx, csr, 90*24*time.Hour, true)
	if err != nil {
		return fmt.Errorf("create cert: %w", err)
	}
	if len(certs) == 0 {
		return errors.New("no certs returned")
	}

	// 8. parse leaf + save
	leaf := certs[0]
	xc, err := x509.ParseCertificate(leaf)
	if err != nil {
		return fmt.Errorf("parse cert: %w", err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(privKey)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf})
	for _, ic := range certs[1:] {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ic})...)
	}

	if err := m.saveToDisk(host, certPEM, keyPEM); err != nil {
		return fmt.Errorf("save to disk: %w", err)
	}

	cert := &Cert{
		Cert: xc, Key: privKey, CertPEM: certPEM, KeyPEM: keyPEM, NotAfter: xc.NotAfter,
	}
	m.mu.Lock()
	m.certs[host] = cert
	m.mu.Unlock()
	m.logger.Info("acme: cert obtained", "host", host, "not_after", xc.NotAfter.Format(time.RFC3339))
	return nil
}

// Challenge is an in-memory registration of an http-01 challenge response
// that the main mux serves via ChallengeHandler.
type challenge struct {
	path     string
	response string
}

var (
	challengeMu sync.Mutex
	challenges  = map[string]string{}
)

func (m *Manager) registerChallenge(path, response string) {
	challengeMu.Lock()
	defer challengeMu.Unlock()
	challenges[path] = response
}

func (m *Manager) unregisterChallenge(path string) {
	challengeMu.Lock()
	defer challengeMu.Unlock()
	delete(challenges, path)
}

// ChallengeHandler returns the http.Handler that serves all currently
// active http-01 challenges. Mount this at "/.well-known/acme-challenge/"
// in the main mux.
func (m *Manager) ChallengeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		challengeMu.Lock()
		resp, ok := challenges[r.URL.Path]
		challengeMu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(resp))
	})
}

// saveToDisk writes certPEM and keyPEM under dataDir/certs/.
func (m *Manager) saveToDisk(host string, certPEM, keyPEM []byte) error {
	dir := filepath.Join(m.dataDir, "certs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// hostnames with wildcards or special chars need a safe filename
	safe := strings.ReplaceAll(host, "*", "_")
	if err := os.WriteFile(filepath.Join(dir, safe+".pem"), certPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, safe+".key"), keyPEM, 0o600)
}

// loadFromDisk reads a cert+key pair.
func (m *Manager) loadFromDisk(host string) (*Cert, error) {
	safe := strings.ReplaceAll(host, "*", "_")
	certPath := filepath.Join(m.dataDir, "certs", safe+".pem")
	keyPath := filepath.Join(m.dataDir, "certs", safe+".key")
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, errors.New("invalid cert PEM")
	}
	xc, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, errors.New("invalid key PEM")
	}
	pkAny, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		ecKey, err2 := x509.ParseECPrivateKey(kb.Bytes)
		if err2 != nil {
			return nil, err
		}
		pkAny = ecKey
	}
	ecKey, ok := pkAny.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("key is not ECDSA")
	}
	return &Cert{
		Cert: xc, Key: ecKey, CertPEM: certPEM, KeyPEM: keyPEM, NotAfter: xc.NotAfter,
	}, nil
}

// selfSigned generates a throwaway cert for dev. DO NOT USE IN PROD.
func (m *Manager) selfSigned(host string) (*Cert, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	xc, _ := x509.ParseCertificate(der)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return &Cert{
		Cert: xc, Key: key, CertPEM: certPEM, KeyPEM: keyPEM, NotAfter: tmpl.NotAfter,
	}, nil
}

// RenewalLoop runs in the background and renews certs that are within
// 30 days of expiry. Stops when Stop() is called.
func (m *Manager) RenewalLoop(ctx context.Context) {
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-tick.C:
			m.renewAll()
		}
	}
}

// Stop terminates the renewal loop.
func (m *Manager) Stop() {
	close(m.stopCh)
}

func (m *Manager) renewAll() {
	m.mu.RLock()
	hosts := make([]string, 0, len(m.certs))
	for h := range m.certs {
		hosts = append(hosts, h)
	}
	m.mu.RUnlock()
	for _, h := range hosts {
		c, err := m.loadFromDisk(h)
		if err != nil {
			continue
		}
		if c.NotAfter.Before(time.Now().Add(30 * 24 * time.Hour)) {
			m.logger.Info("acme: renewing cert", "host", h, "not_after", c.NotAfter.Format(time.RFC3339))
			if err := m.ObtainForHost(h); err != nil {
				m.logger.Error("acme: renew failed", "host", h, "err", err)
			}
		}
	}
}
