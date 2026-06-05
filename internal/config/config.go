// Package config defines the on-disk YAML config for aigoproxy.
//
// Example ~/.aigoproxy/state.yaml:
//
//   base_domain: biodoia.ts.net
//   http_addr: ":80"
//   https_addr: ":443"
//   data_dir: ~/.aigoproxy
//   acme:
//     enabled: false          # Wave 2; uses Tailscale certs for now
//     email: sergio@example.com
//     dns_provider: cloudflare
//     dns_config:
//       CLOUDFLARE_API_TOKEN: cf_***
//   routes:
//     - host: grafana.biodoia.ts.net
//       upstream: http://192.168.1.50:3000
//       health: /api/health
//       auth: none
//     - host: jellyfin.biodoia.ts.net
//       upstream: http://192.168.1.51:8096
//       health: /web/index.html
//       auth: tailscale
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the root YAML config.
type Config struct {
	BaseDomain string         `yaml:"base_domain"`
	HTTPAddr   string         `yaml:"http_addr"`
	HTTPSAddr  string         `yaml:"https_addr"`
	DataDir    string         `yaml:"data_dir"`
	ACME       ACMEConfig     `yaml:"acme"`
	Routes     []Route        `yaml:"routes"`
	Tailscale  TailscaleConfig `yaml:"tailscale"`
}

// ACMEConfig configures Let's Encrypt certificate provisioning.
type ACMEConfig struct {
	Enabled     bool              `yaml:"enabled"`
	Email       string            `yaml:"email"`
	Staging     bool              `yaml:"staging"`
	DNSProvider string            `yaml:"dns_provider"`
	DNSConfig   map[string]string `yaml:"dns_config"`
}

// TailscaleConfig controls Tailscale integration.
type TailscaleConfig struct {
	// UseFunnel tells aigoproxy to register a Funnel listener for
	// every public route, so the URL is reachable from the public
	// internet. Requires `tailscale` CLI on PATH.
	UseFunnel bool `yaml:"use_funnel"`
	// FunnelOnly makes aigoproxy skip binding :80/:443 and only
	// serve via the local Tailscale listener (Serve mode).
	FunnelOnly bool `yaml:"funnel_only"`
}

// Route is a virtual host → upstream mapping.
type Route struct {
	Host     string `yaml:"host"`
	Upstream string `yaml:"upstream"`
	// Health is an optional path to probe upstream. Empty = no health check.
	Health string `yaml:"health"`
	// Auth controls who can reach this route.
	// "none" — anyone
	// "tailscale" — must be on the tailnet (Tailscale headers check)
	// "funnel" — public via Tailscale Funnel only
	Auth string `yaml:"auth"`
	// StripPrefix removes a path prefix from incoming requests before
	// forwarding to upstream. Useful for "/grafana/" → "/" upstream.
	StripPrefix string `yaml:"strip_prefix"`
	// PathPrefix makes this route match incoming requests whose URL path
	// starts with this prefix, even when the Host header doesn't match.
	// This is the recommended pattern for services behind a Tailscale
	// Funnel listener (one :443, many services distinguished by path).
	PathPrefix string `yaml:"path_prefix"`
	// Enabled can be set to false to take a route offline temporarily
	// without deleting it.
	Enabled bool `yaml:"enabled"`
}

// Load reads the YAML config at path. If the file doesn't exist, returns
// a default config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Default(path), nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	c := &Config{}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Save writes the config to path, creating parent dirs if needed.
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// Validate ensures the config makes sense.
func (c *Config) Validate() error {
	if c.HTTPAddr == "" && c.HTTPSAddr == "" && !c.Tailscale.FunnelOnly {
		return errors.New("at least one of http_addr, https_addr, or tailscale.funnel_only must be set")
	}
	seen := make(map[string]bool)
	for i, r := range c.Routes {
		if r.Host == "" {
			return fmt.Errorf("routes[%d]: host is required", i)
		}
		if r.Upstream == "" {
			return fmt.Errorf("routes[%d] (%s): upstream is required", i, r.Host)
		}
		if seen[r.Host] {
			return fmt.Errorf("routes[%d]: duplicate host %q", i, r.Host)
		}
		seen[r.Host] = true
		if r.Auth == "" {
			r.Auth = "none"
		}
		switch r.Auth {
		case "none", "tailscale", "funnel":
			// ok
		default:
			return fmt.Errorf("routes[%d] (%s): invalid auth %q", i, r.Host, r.Auth)
		}
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.HTTPAddr == "" {
		c.HTTPAddr = ":80"
	}
	if c.HTTPSAddr == "" {
		c.HTTPSAddr = ":443"
	}
	if c.DataDir == "" {
		home, _ := os.UserHomeDir()
		c.DataDir = filepath.Join(home, ".aigoproxy")
	}
	if c.BaseDomain == "" {
		c.BaseDomain = "biodoia.ts.net"
	}
	for i := range c.Routes {
		if c.Routes[i].Auth == "" {
			c.Routes[i].Auth = "none"
		}
		c.Routes[i].Enabled = true
	}
}

// Default returns a config preloaded with reasonable defaults.
func Default(path string) *Config {
	c := &Config{
		HTTPAddr:  ":80",
		HTTPSAddr: ":443",
		BaseDomain: "biodoia.ts.net",
		Routes:     []Route{},
	}
	c.applyDefaults()
	return c
}
