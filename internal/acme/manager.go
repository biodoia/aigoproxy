// Package acme provisions TLS certificates for aigoproxy routes via
// Let's Encrypt, using the DNS-01 challenge.
//
// This package is a stub for Wave 1. The full implementation will use
// github.com/mholt/acmez/v3 (already in go.mod) to talk to the ACME
// server and a pluggable DNS provider interface to set the TXT records.
//
// Wave 1 ships:
//   - Certificate caching (load from disk, hand to net/http)
//   - Self-signed fallback for development
//   - Interface definition for DNS providers
//
// Wave 2 will add:
//   - Real ACME client (acmez/v3 integration)
//   - Cloudflare DNS provider (concrete impl)
//   - Automatic renewal background loop
package acme

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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
)

// Manager holds the certificate cache and the (future) ACME client.
type Manager struct {
	dataDir string
	logger  *slog.Logger
	mu      sync.RWMutex
	certs   map[string]*Cert // host → cert
}

// Cert is a leaf certificate + matching private key.
type Cert struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
	KeyPEM  []byte
	NotAfter time.Time
}

// DNSProvider sets TXT records for the ACME DNS-01 challenge.
type DNSProvider interface {
	// SetTXT creates a TXT record at _acme-challenge.<domain>.
	SetTXT(domain, value string) error
	// DeleteTXT removes the TXT record.
	DeleteTXT(domain, value string) error
}

// NewManager returns a Manager backed by dataDir for cert persistence.
func NewManager(dataDir string, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		dataDir: dataDir,
		logger:  logger,
		certs:   make(map[string]*Cert),
	}
}

// GetCertificate returns an http.GetCertificate-compatible function that
// looks up the cert for the SNI host and falls back to self-signed.
func (m *Manager) GetCertificate(chi *tlsClientHello) (*Cert, error) {
	if chi == nil {
		return nil, errors.New("no TLS client hello")
	}
	host := chi.ServerName
	if host == "" {
		return nil, errors.New("no SNI host")
	}
	m.mu.RLock()
	c, ok := m.certs[host]
	m.mu.RUnlock()
	if ok && c.NotAfter.After(time.Now().Add(7 * 24 * time.Hour)) {
		return c, nil
	}
	// try loading from disk
	c, err := m.loadFromDisk(host)
	if err == nil {
		m.mu.Lock()
		m.certs[host] = c
		m.mu.Unlock()
		return c, nil
	}
	// fallback: self-signed (dev mode only)
	m.logger.Warn("acme: serving self-signed cert (dev mode)", "host", host)
	return m.selfSigned(host)
}

// loadFromDisk tries to read a cert+key pair from dataDir/certs/host.pem.
func (m *Manager) loadFromDisk(host string) (*Cert, error) {
	certPath := filepath.Join(m.dataDir, "certs", host+".pem")
	keyPath := filepath.Join(m.dataDir, "certs", host+".key")
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
		// try EC private key
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

// selfSigned generates a throwaway cert for the host. DO NOT USE IN PROD.
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

// ObtainForHost is the public entry point for certificate acquisition.
// In Wave 1 it just returns the self-signed cert. In Wave 2 it will:
//   1. Spin up the ACME client (acmez/v3)
//   2. Trigger the DNS-01 challenge via the configured DNSProvider
//   3. Wait for issuance
//   4. Cache to disk and memory
func (m *Manager) ObtainForHost(host string, dns DNSProvider) (*Cert, error) {
	if dns == nil {
		// No DNS provider — fall back to self-signed. Document loudly.
		m.logger.Warn("acme: no DNS provider configured, using self-signed (do not use in production)", "host", host)
		return m.selfSigned(host)
	}
	// TODO(wave 2): real ACME flow
	return nil, fmt.Errorf("acme: real issuance not yet implemented (wave 2)")
}

// tlsClientHello is a minimal type alias so we can compile without
// pulling crypto/tls into this file. Real SNI handling uses
// crypto/tls.ClientHelloInfo.
type tlsClientHello struct {
	ServerName string
}

// Helper: get the cert for a net/http server using SNI.
// Call site wraps this with crypto/tls.Config.GetCertificate.
func (m *Manager) GetCertificateForTLS(sni string) (*Cert, error) {
	chi := &tlsClientHello{ServerName: sni}
	return m.GetCertificate(chi)
}

// Make sure errors are referenced
var _ = strings.TrimSpace

// Make sure http is referenced (future use in OCSP stapling, etc.)
var _ = http.MethodGet
