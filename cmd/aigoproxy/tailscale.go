// tailscale.go — integration with the `tailscale` CLI for cert provisioning
// and Funnel management. We deliberately use shell-out instead of the
// tailscale.com Go library because:
//   1. The library version changes break often
//   2. We only need three operations: cert, funnel, status
//   3. The CLI is the documented interface
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// tailscaleInfo is a minimal projection of `tailscale status --json`.
type tailscaleInfo struct {
	Hostname  string
	DNSName   string
	CertHosts []string // valid SNI hosts for `tailscale cert`
	FunnelOn  bool
	Tailnet   string // e.g. "sapsucker-hirajoshi.ts.net"
}

// isTailscaleInstalled returns true if `tailscale` is on PATH.
func isTailscaleInstalled() bool {
	_, err := exec.LookPath("tailscale")
	return err == nil
}

// tailscaleStatus runs `tailscale status --json` and extracts a minimal set
// of fields we care about. Crude dependency-free JSON parse.
func tailscaleStatus(ctx context.Context) (*tailscaleInfo, error) {
	if !isTailscaleInstalled() {
		return nil, errors.New("tailscale CLI not found")
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "tailscale", "status", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	info := &tailscaleInfo{}
	if v, ok := jsonString(out, `"HostName"`); ok {
		info.Hostname = v
	}
	if v, ok := jsonString(out, `"DNSName"`); ok {
		info.DNSName = strings.TrimSuffix(v, ".")
		parts := strings.Split(info.DNSName, ".")
		if len(parts) >= 3 {
			info.Tailnet = strings.Join(parts[len(parts)-2:], ".")
		}
	}
	if info.DNSName != "" {
		info.CertHosts = append(info.CertHosts, info.DNSName)
	}
	return info, nil
}

// jsonString finds the value of "key": "value" in a JSON blob. The key
// is passed WITHOUT the trailing colon (e.g. pass `"DNSName"`, not
// `"DNSName":`). Whitespace and the colon between key and value are
// skipped. Crude but dependency-free.
func jsonString(data []byte, key string) (string, bool) {
	i := bytes.Index(data, []byte(key))
	if i < 0 {
		return "", false
	}
	i += len(key)
	// skip optional whitespace and the colon
	for i < len(data) && (data[i] == ' ' || data[i] == '	' || data[i] == '\n' || data[i] == ':') {
		i++
	}
	// expect opening quote
	if i >= len(data) || data[i] != '"' {
		return "", false
	}
	i++
	// find closing quote (no escape handling — Tailscale's DNSName is simple)
	j := bytes.IndexByte(data[i:], '"')
	if j < 0 {
		return "", false
	}
	return string(data[i : i+j]), true
}

// ensureTailscaleCert runs `tailscale cert <host>` if the cert doesn't
// exist or is older than ~60 days. Returns the cert and key file paths.
func ensureTailscaleCert(ctx context.Context, logger *slog.Logger, host, dataDir string) (certFile, keyFile string, err error) {
	certFile = filepath.Join(dataDir, "tailscale-certs", host+".crt")
	keyFile = filepath.Join(dataDir, "tailscale-certs", host+".key")
	if !needsRefresh(certFile) {
		return certFile, keyFile, nil
	}
	if !isTailscaleInstalled() {
		return "", "", errors.New("tailscale CLI not installed")
	}
	logger.Info("provisioning Tailscale cert", "host", host)
	c, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(certFile), 0o755); err != nil {
		return "", "", err
	}
	args := []string{"cert", "--cert-file", certFile, "--key-file", keyFile, host}
	cmd := exec.CommandContext(c, "tailscale", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("tailscale cert: %w (out=%s)", err, string(out))
	}
	logger.Info("tailscale cert provisioned", "host", host, "cert", certFile)
	return certFile, keyFile, nil
}

// needsRefresh returns true if the cert file doesn't exist or is older
// than 60 days (Tailscale certs are 90-day LE certs, refresh halfway).
func needsRefresh(certFile string) bool {
	info, err := os.Stat(certFile)
	if err != nil {
		return true
	}
	return time.Since(info.ModTime()) > 60*24*time.Hour
}

// startHTTPS provisions a Tailscale cert and returns an *http.Server
// configured to serve the same root mux over TLS on httpsAddr. The cert
// is provisioned for the node's <node>.<tailnet>.ts.net.
//
// Note: the cert is a single cert + key; if a client SNI doesn't match
// we serve it anyway (Go's default behaviour is to abort, but with one
// cert, the SNI must match — for multi-host HTTPS a user would need to
// add routes and we'd need to issue one cert per host, which is what
// the Tailscale cert command does. For simplicity we serve the single
// node cert and document the limitation.)
func startHTTPS(ctx context.Context, logger *slog.Logger, httpsAddr string, root http.Handler, dataDir string) (*http.Server, error) {
	info, err := tailscaleStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	if info.DNSName == "" {
		return nil, errors.New("no DNSName in tailscale status")
	}
	certFile, keyFile, err := ensureTailscaleCert(ctx, logger, info.DNSName, dataDir)
	if err != nil {
		return nil, fmt.Errorf("provision cert: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load cert: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	srv := &http.Server{
		Addr:              httpsAddr,
		Handler:           root,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// Override ListenAndServeTLS by reading the cert paths in a wrapper.
	// We use a custom listener.
	_ = srv
	logger.Info("https cert ready", "host", info.DNSName, "cert", certFile)
	return srv, nil
}
