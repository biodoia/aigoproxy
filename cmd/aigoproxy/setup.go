// Setup is a one-shot interactive wizard that writes a starter config.yaml
// after asking the user a few questions. Run with `aigoproxy setup`.
//
// We deliberately keep this small: no fancy UI, just a series of
// fmt.Scanf prompts. The output is a YAML file the daemon can then load.
package main

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/biodoia/aigoproxy/internal/config"
)

// runSetup runs the interactive wizard. Returns nil on success, error on failure.
func runSetup(logger *slog.Logger) error {
	reader := bufio.NewReader(os.Stdin)

	// 1. pick data dir
	home, _ := os.UserHomeDir()
	defData := filepath.Join(home, ".aigoproxy")
	fmt.Printf("Data directory [%s]: ", defData)
	dataDir, _ := readLine(reader)
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		dataDir = defData
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "certs"), 0o755); err != nil {
		return err
	}
	configPath := filepath.Join(dataDir, "config.yaml")

	// 2. base domain
	defDomain := "sapsucker-hirajoshi.ts.net"
	fmt.Printf("Tailscale base domain [%s]: ", defDomain)
	domain, _ := readLine(reader)
	domain = strings.TrimSpace(domain)
	if domain == "" {
		domain = defDomain
	}

	// 3. ACME email (optional, for Let's Encrypt)
	defEmail := os.Getenv("AIGOPROXY_ACME_EMAIL")
	fmt.Printf("ACME email for Let's Encrypt (blank = skip LE) [%s]: ", defEmail)
	email, _ := readLine(reader)
	email = strings.TrimSpace(email)
	if email == "" {
		email = defEmail
	}

	// 4. Add some starter routes?
	routes := []config.Route{}
	for {
		fmt.Print("Add a starter route? [y/N]: ")
		ans, _ := readLine(reader)
		ans = strings.ToLower(strings.TrimSpace(ans))
		if ans != "y" && ans != "yes" {
			break
		}
		fmt.Print("  Host (e.g. app.sapsucker-hirajoshi.ts.net): ")
		host, _ := readLine(reader)
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		fmt.Print("  Upstream URL (e.g. http://127.0.0.1:9001): ")
		upstream, _ := readLine(reader)
		upstream = strings.TrimSpace(upstream)
		if upstream == "" {
			continue
		}
		fmt.Print("  Auth (none/tailscale/funnel) [tailscale]: ")
		auth, _ := readLine(reader)
		auth = strings.TrimSpace(auth)
		if auth == "" {
			auth = "tailscale"
		}
		routes = append(routes, config.Route{
			Host:     host,
			Upstream: upstream,
			Auth:     auth,
			Enabled:  true,
		})
		fmt.Printf("  added %s -> %s\n", host, upstream)
	}

	// 5. write config
	cfg := &config.Config{
		HTTPAddr:   ":80",
		HTTPSAddr:  "",
		DataDir:    dataDir,
		BaseDomain: domain,
		ACME: config.ACMEConfig{
			Enabled: email != "",
			Email:   email,
		},
		Routes: routes,
	}
	// Validate so we don't write a broken file
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	if err := cfg.Save(configPath); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	fmt.Println()
	fmt.Println("Written:", configPath)
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  systemctl --user start aigoproxy    # if installed")
	fmt.Println("  ./bin/aigoproxy                    # otherwise")
	fmt.Println("  ./bin/aigoproxy -tui               # interactive TUI")
	fmt.Println()
	fmt.Println("To register a route from the CLI:")
	example := `curl -X POST http://localhost/mcp -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","method":"tools/call","params":{"name":"aigoproxy_add","arguments":{"host":"app.` + domain + `","upstream":"http://127.0.0.1:9001","auth":"tailscale"}},"id":1}'`
	fmt.Println("  " + example)
	return nil
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return line, err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
