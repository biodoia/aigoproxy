package acme

import (
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	dir := t.TempDir()
	m, err := New(Config{DataDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m == nil {
		t.Fatal("nil manager")
	}
}

func TestSelfSigned(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Config{DataDir: dir})
	c, err := m.selfSigned("test.example.com")
	if err != nil {
		t.Fatalf("selfSigned: %v", err)
	}
	if c.Cert == nil {
		t.Fatal("nil cert")
	}
	if !strings.Contains(c.Cert.Subject.CommonName, "test.example.com") {
		t.Errorf("CN = %q, want test.example.com", c.Cert.Subject.CommonName)
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Config{DataDir: dir})
	host := "save.example.com"
	c, err := m.selfSigned(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.saveToDisk(host, c.CertPEM, c.KeyPEM); err != nil {
		t.Fatalf("saveToDisk: %v", err)
	}
	loaded, err := m.loadFromDisk(host)
	if err != nil {
		t.Fatalf("loadFromDisk: %v", err)
	}
	if loaded.Cert.Subject.CommonName != host {
		t.Errorf("loaded CN = %q, want %q", loaded.Cert.Subject.CommonName, host)
	}
}

func TestLoadMissing(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Config{DataDir: dir})
	_, err := m.loadFromDisk("nope.example.com")
	if err == nil {
		t.Error("expected error loading missing cert")
	}
}

func TestSafeFilename(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Config{DataDir: dir})
	host := "*.example.com"
	c, _ := m.selfSigned("example.com")
	if err := m.saveToDisk(host, c.CertPEM, c.KeyPEM); err != nil {
		t.Fatalf("save: %v", err)
	}
	expected := filepath.Join(dir, "certs", "_.example.com.pem")
	if _, err := os.ReadFile(expected); err != nil {
		t.Errorf("expected file at %s: %v", expected, err)
	}
}

func TestCertPEMFormat(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Config{DataDir: dir})
	c, _ := m.selfSigned("test.example.com")
	block, _ := pem.Decode(c.CertPEM)
	if block == nil {
		t.Fatal("CertPEM is not valid PEM")
	}
	if block.Type != "CERTIFICATE" {
		t.Errorf("PEM type = %q, want CERTIFICATE", block.Type)
	}
	xc, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if xc.Subject.CommonName != "test.example.com" {
		t.Errorf("CN = %q", xc.Subject.CommonName)
	}
}

func TestChallengeRegistration(t *testing.T) {
	// verify registerChallenge → ChallengeHandler returns the response
	dir := t.TempDir()
	m, _ := New(Config{DataDir: dir})
	path := "/.well-known/acme-challenge/abc123"
	resp := "keyauth-value"
	m.registerChallenge(path, resp)
	defer m.unregisterChallenge(path)

	// serve via ChallengeHandler
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	m.ChallengeHandler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != resp {
		t.Errorf("body = %q, want %q", rec.Body.String(), resp)
	}

	// unknown path returns 404
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/.well-known/acme-challenge/unknown", nil)
	m.ChallengeHandler().ServeHTTP(rec2, req2)
	if rec2.Code != 404 {
		t.Errorf("status for unknown = %d, want 404", rec2.Code)
	}

	// unregister
	m.unregisterChallenge(path)
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", path, nil)
	m.ChallengeHandler().ServeHTTP(rec3, req3)
	if rec3.Code != 404 {
		t.Errorf("status after unregister = %d, want 404", rec3.Code)
	}
}
