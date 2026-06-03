package dnsplugin

import (
	"testing"
)

func TestNames(t *testing.T) {
	// rfc2136 registers itself via init
	Register("test-fake", NewStub)
	names := Names()
	if !contains(names, "rfc2136") {
		t.Error("rfc2136 not in registered names")
	}
	if !contains(names, "test-fake") {
		t.Error("test-fake not in registered names")
	}
}

func TestNew(t *testing.T) {
	Register("test-fake2", NewStub)
	p, err := New("test-fake2", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != "stub" {
		t.Errorf("Name = %q, want stub", p.Name())
	}
}

func TestNewUnknown(t *testing.T) {
	_, err := New("does-not-exist", nil)
	if err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestStubProvider(t *testing.T) {
	Register("stub-test", NewStub)
	p, _ := New("stub-test", nil)
	stub := p.(*Stub)
	if err := stub.SetTXT("foo.example.com", "value1"); err != nil {
		t.Fatal(err)
	}
	if err := stub.SetTXT("bar.example.com", "value2"); err != nil {
		t.Fatal(err)
	}
	if err := stub.DeleteTXT("foo.example.com", "value1"); err != nil {
		t.Fatal(err)
	}
	if len(stub.Sets) != 2 {
		t.Errorf("expected 2 sets, got %d", len(stub.Sets))
	}
	if len(stub.Deletes) != 1 {
		t.Errorf("expected 1 delete, got %d", len(stub.Deletes))
	}
}

func TestRFC2136ConfigMissing(t *testing.T) {
	// missing required config should panic
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for missing required config")
		}
	}()
	NewRFC2136(map[string]string{"zone": "example.com"})
}

func TestRFC2136ConfigOK(t *testing.T) {
	p, err := NewRFC2136(map[string]string{
		"server":     "127.0.0.1:53",
		"zone":       "example.com",
		"key_name":   "aigoproxy-key",
		"key_secret": "c2VjcmV0",
		"key_alg":    "hmac-sha256",
		"ttl":        "120",
	})
	if err != nil {
		t.Fatalf("NewRFC2136: %v", err)
	}
	if p.Name() != "rfc2136" {
		t.Errorf("Name = %q, want rfc2136", p.Name())
	}
}

func TestRFC2136BadAlg(t *testing.T) {
	_, err := NewRFC2136(map[string]string{
		"server":     "127.0.0.1:53",
		"zone":       "example.com",
		"key_name":   "k",
		"key_secret": "c2VjcmV0",
		"key_alg":    "made-up-algo",
	})
	if err == nil {
		t.Error("expected error for bad algorithm")
	}
}

func TestRFC2136BadTTL(t *testing.T) {
	_, err := NewRFC2136(map[string]string{
		"server":     "127.0.0.1:53",
		"zone":       "example.com",
		"key_name":   "k",
		"key_secret": "c2VjcmV0",
		"ttl":        "not-a-number",
	})
	if err == nil {
		t.Error("expected error for bad TTL")
	}
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}
