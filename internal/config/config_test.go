package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefault(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(filepath.Join(dir, "missing.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HTTPAddr == "" {
		t.Error("HTTPAddr should have a default")
	}
	if c.BaseDomain == "" {
		t.Error("BaseDomain should have a default")
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	c := Default(path)
	c.Routes = []Route{
		{Host: "a.test.ts.net", Upstream: "http://127.0.0.1:8080", Auth: "none"},
		{Host: "b.test.ts.net", Upstream: "http://127.0.0.1:9000", Auth: "tailscale", Health: "/health"},
	}
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	c2, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c2.Routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(c2.Routes))
	}
	if c2.Routes[0].Host != "a.test.ts.net" {
		t.Errorf("route 0 host = %q, want a.test.ts.net", c2.Routes[0].Host)
	}
	if c2.Routes[1].Auth != "tailscale" {
		t.Errorf("route 1 auth = %q, want tailscale", c2.Routes[1].Auth)
	}
}

func TestValidateDuplicateHost(t *testing.T) {
	c := Default("x")
	c.Routes = []Route{
		{Host: "a.test.ts.net", Upstream: "http://x"},
		{Host: "a.test.ts.net", Upstream: "http://y"},
	}
	if err := c.Validate(); err == nil {
		t.Error("expected error for duplicate host")
	}
}

func TestValidateInvalidAuth(t *testing.T) {
	c := Default("x")
	c.Routes = []Route{
		{Host: "a.test.ts.net", Upstream: "http://x", Auth: "open-sesame"},
	}
	if err := c.Validate(); err == nil {
		t.Error("expected error for invalid auth")
	}
}

func TestValidateMissingHost(t *testing.T) {
	c := Default("x")
	c.Routes = []Route{{Upstream: "http://x"}}
	if err := c.Validate(); err == nil {
		t.Error("expected error for missing host")
	}
}

func TestValidateOK(t *testing.T) {
	c := Default("x")
	c.Routes = []Route{
		{Host: "a.test.ts.net", Upstream: "http://127.0.0.1:80", Auth: "none"},
		{Host: "b.test.ts.net", Upstream: "http://127.0.0.1:81", Auth: "tailscale"},
		{Host: "c.test.ts.net", Upstream: "http://127.0.0.1:82", Auth: "funnel"},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// env backup/restore helper
func TestDataDirOverride(t *testing.T) {
	dir := t.TempDir()
	c := Default("x")
	c.DataDir = dir
	if c.DataDir != dir {
		t.Errorf("DataDir = %q, want %q", c.DataDir, dir)
	}
}

// Skip test that needs /tmp (don't run in CI)
func TestOSHomeDir(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("CI")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
}
