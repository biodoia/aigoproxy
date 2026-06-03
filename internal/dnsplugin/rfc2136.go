// RFC 2136 DNS-01 provider. Uses standard dynamic-update protocol with
// optional TSIG authentication. Compatible with BIND, Knot, PowerDNS,
// Microsoft DNS, and any RFC 2136-compliant server.
//
// This is the universal fallback: any DNS server you control can use it
// without giving aigoproxy API access to a third-party service.
//
// Required config:
//   server:    IP:port of the DNS server (e.g. "192.168.1.1:53")
//   zone:      DNS zone to update (e.g. "biodoia.ts.net")
//   key_name:  TSIG key name (e.g. "aigoproxy-key")
//   key_secret: TSIG key secret (base64)
//   key_alg:   TSIG algorithm ("hmac-sha256" or "hmac-sha1")
//
// Optional:
//   ttl:       record TTL in seconds (default 60)
package dnsplugin

import (
	"fmt"
	"time"

	"github.com/miekg/dns"
)

// init registers the rfc2136 provider.
func init() {
	Register("rfc2136", NewRFC2136)
}

// RFC2136 is a TSIG-authenticated DNS-01 provider.
type RFC2136 struct {
	server  string
	zone    string
	keyName string
	keyAlg  string
	keySec  string
	ttl     uint32
}

// NewRFC2136 constructs the provider from the config map.
func NewRFC2136(cfg map[string]string) (Provider, error) {
	server := must(cfg, "server")
	zone := must(cfg, "zone")
	keyName := must(cfg, "key_name")
	keySecret := must(cfg, "key_secret")
	keyAlg := cfgOr(cfg, "key_alg", "hmac-sha256")

	// ensure zone is fully qualified
	if zone[len(zone)-1] != '.' {
		zone += "."
	}
	// ensure keyName is fully qualified
	if keyName[len(keyName)-1] != '.' {
		keyName += "."
	}

	// Validate algorithm up front
	switch keyAlg {
	case "hmac-sha256", "hmac-sha1", "hmac-sha512":
		// ok
	default:
		return nil, fmt.Errorf("dnsprovider/rfc2136: unsupported key_alg %q", keyAlg)
	}

	ttl := uint32(60)
	if v := cfg["ttl"]; v != "" {
		var t uint32
		if _, err := fmt.Sscanf(v, "%d", &t); err != nil {
			return nil, fmt.Errorf("dnsprovider/rfc2136: invalid ttl %q", v)
		}
		ttl = t
	}

	return &RFC2136{
		server:  server,
		zone:    zone,
		keyName: keyName,
		keyAlg:  keyAlg,
		keySec:  keySecret,
		ttl:     ttl,
	}, nil
}

// Name returns "rfc2136".
func (p *RFC2136) Name() string { return "rfc2136" }

// SetTXT creates a TXT record at _acme-challenge.<domain>.<zone> with value.
func (p *RFC2136) SetTXT(domain, value string) error {
	return p.update(domain, value, true)
}

// DeleteTXT removes the TXT record.
func (p *RFC2136) DeleteTXT(domain, value string) error {
	return p.update(domain, value, false)
}

func (p *RFC2136) update(domain, value string, add bool) error {
	fqdn := "_acme-challenge." + domain
	if fqdn[len(fqdn)-1] != '.' {
		fqdn += "."
	}
	c := new(dns.Client)
	// use the default TSIG provider
	m := new(dns.Msg)
	// Use a relative name to the zone
	relName := fqdn
	if dns.IsFqdn(relName) && len(relName) > len(p.zone) {
		// strip the zone suffix to get a relative name
		relName = relName[:len(relName)-len(p.zone)]
		if relName[len(relName)-1] == '.' {
			relName = relName[:len(relName)-1]
		}
	}
	m.SetUpdate(p.zone)
	if add {
		rr := &dns.TXT{
			Hdr: dns.RR_Header{
				Name:   relName + "." + p.zone,
				Rrtype: dns.TypeTXT,
				Class:  dns.ClassINET,
				Ttl:    p.ttl,
			},
			Txt: []string{value},
		}
		m.Insert([]dns.RR{rr})
	} else {
		rr := &dns.TXT{
			Hdr: dns.RR_Header{
				Name:   relName + "." + p.zone,
				Rrtype: dns.TypeTXT,
				Class:  dns.ClassNONE,
				Ttl:    p.ttl,
			},
			Txt: []string{value},
		}
		m.RemoveRRset([]dns.RR{rr})
	}
	m.SetTsig(p.keyName, p.keyAlg, 300, time.Now().Unix())
	_, _, err := c.Exchange(m, p.server)
	if err != nil {
		return fmt.Errorf("dnsprovider/rfc2136: %w", err)
	}
	return nil
}

func must(cfg map[string]string, k string) string {
	v := cfg[k]
	if v == "" {
		panic("dnsprovider/rfc2136: required config " + k)
	}
	return v
}

func cfgOr(cfg map[string]string, k, def string) string {
	if v := cfg[k]; v != "" {
		return v
	}
	return def
}
