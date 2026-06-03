// Package dnsplugin provides a pluggable DNS-01 ACME challenge mechanism.
//
// Why DNS-01: HTTP-01 only works if the host is reachable on :80 from
// the public internet. For hosts behind firewalls or with private IPs,
// DNS-01 is the only option — it sets a TXT record at
// _acme-challenge.<domain>.
//
// How to add a new provider:
//   1. Implement the Provider interface (SetTXT, DeleteTXT, possibly WaitForPropagation)
//   2. Register it with a name: registry.Register("cloudflare", NewCloudflare)
//   3. Users configure dns_provider: "cloudflare" + dns_config: { ... } in config.yaml
package dnsplugin

import (
	"errors"
	"fmt"
	"sync"
)

// Provider sets and clears DNS TXT records for ACME challenges.
type Provider interface {
	// SetTXT creates (or updates) a TXT record at
	// _acme-challenge.<domain> with the given value.
	SetTXT(domain, value string) error
	// DeleteTXT removes the TXT record.
	DeleteTXT(domain, value string) error
	// Name returns the provider's registered name (e.g. "cloudflare").
	Name() string
}

// Constructor builds a Provider from a config map.
type Constructor func(config map[string]string) (Provider, error)

var (
	regMu sync.RWMutex
	reg   = map[string]Constructor{}
)

// Register adds a provider constructor under the given name.
func Register(name string, ctor Constructor) {
	regMu.Lock()
	defer regMu.Unlock()
	reg[name] = ctor
}

// New looks up a provider by name and instantiates it.
func New(name string, config map[string]string) (Provider, error) {
	regMu.RLock()
	ctor, ok := reg[name]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("dnsprovider: unknown %q (have: %v)", name, Names())
	}
	return ctor(config)
}

// Names returns all registered provider names.
func Names() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(reg))
	for n := range reg {
		out = append(out, n)
	}
	return out
}

// Stub is a no-op provider used for tests.
type Stub struct {
	Sets    []string
	Deletes []string
}

// SetTXT records the call.
func (s *Stub) SetTXT(domain, value string) error {
	s.Sets = append(s.Sets, fmt.Sprintf("%s=%s", domain, value))
	return nil
}

// DeleteTXT records the call.
func (s *Stub) DeleteTXT(domain, value string) error {
	s.Deletes = append(s.Deletes, fmt.Sprintf("%s=%s", domain, value))
	return nil
}

// Name returns "stub".
func (s *Stub) Name() string { return "stub" }

// NewStub is a Constructor for the stub provider.
func NewStub(_ map[string]string) (Provider, error) { return &Stub{}, nil }

// ErrNotConfigured is returned when a provider is missing required config.
var ErrNotConfigured = errors.New("dnsprovider: not configured")
