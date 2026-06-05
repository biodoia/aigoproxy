// Package detector inspects a backend upstream and returns whether it
// appears to have its own authentication layer (so aigoproxy should NOT
// wrap it with another login screen).
//
// Heuristics:
//   - Backend returns 401/403 → has auth
//   - Backend redirects to /login, /auth, /signin → has auth
//   - Backend HTML contains <form> with input[type=password] → has auth
//   - Backend HTML contains "Set-Cookie" session header → has auth
//   - Otherwise: no auth detected
package detector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Result is what we learned about the upstream.
type Result struct {
	HasAuth        bool   `json:"has_auth"`
	Reason         string `json:"reason"`
	Title          string `json:"title,omitempty"`           // <title> contents, if any
	FirstHeading   string `json:"first_heading,omitempty"`   // first <h1>/<h2>
	LoginPath      string `json:"login_path,omitempty"`      // detected login path
	RecommendedAuth string `json:"recommended_auth"`          // "none" / "basic" / "tailscale" / "oidc"
}

// Default timeout for a single detection probe.
const probeTimeout = 5 * time.Second

// Inspect probes the upstream and returns a Result. Upstream is the full
// URL of the backend, e.g. "http://127.0.0.1:8983".
func Inspect(ctx context.Context, upstream string) (*Result, error) {
	dctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, "GET", upstream, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "aigoproxy-detector/1.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")
	cli := &http.Client{
		Timeout: probeTimeout,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	res := &Result{}
	// 1. Status 401/403 → has auth
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		res.HasAuth = true
		res.Reason = fmt.Sprintf("HTTP %d returned by upstream", resp.StatusCode)
		res.RecommendedAuth = "none"
		return res, nil
	}
	// 2. Redirect to login path
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := resp.Header.Get("Location")
		if isLoginPath(loc) {
			res.HasAuth = true
			res.Reason = "redirect to " + loc
			res.LoginPath = loc
			res.RecommendedAuth = "none"
			return res, nil
		}
		// Follow one redirect
		if loc != "" && strings.HasPrefix(loc, "/") {
			req2, _ := http.NewRequestWithContext(dctx, "GET", upstream+loc, nil)
			req2.Header.Set("User-Agent", "aigoproxy-detector/1.0")
			if resp2, err := cli.Do(req2); err == nil {
				defer resp2.Body.Close()
				body2, _ := io.ReadAll(io.LimitReader(resp2.Body, 64*1024))
				resp = resp2
				body = body2
			}
		}
	}
	// 3. Parse HTML body
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "application/xhtml") {
		html := string(body)
		res.Title = extractTitle(html)
		res.FirstHeading = extractFirstHeading(html)
		// password input → has auth
		if strings.Contains(html, `type="password"`) || strings.Contains(html, `type='password'`) {
			res.HasAuth = true
			res.Reason = "HTML contains password input"
			res.RecommendedAuth = "none"
			return res, nil
		}
		// form with action going to a login-like path
		if formAction := extractFormAction(html); isLoginPath(formAction) {
			res.HasAuth = true
			res.Reason = "form action: " + formAction
			res.LoginPath = formAction
			res.RecommendedAuth = "none"
			return res, nil
		}
		// Set-Cookie session header
		if hasSessionCookie(resp.Header) {
			res.HasAuth = true
			res.Reason = "session cookie set on first GET"
			res.RecommendedAuth = "none"
			return res, nil
		}
	}
	// 4. JSON API — look for a token in the response
	if strings.HasPrefix(ct, "application/json") {
		js := string(body)
		if strings.Contains(js, "token") || strings.Contains(js, "session_id") {
			res.HasAuth = true
			res.Reason = "JSON API returns token/session"
			res.RecommendedAuth = "none"
			return res, nil
		}
	}
	// No auth detected
	res.HasAuth = false
	res.Reason = "no auth markers found in response"
	res.RecommendedAuth = "tailscale" // safest default: require tailnet membership
	return res, nil
}

func isLoginPath(p string) bool {
	p = strings.ToLower(p)
	for _, marker := range []string{
		"/login", "/signin", "/sign-in", "/auth", "/authenticate",
		"/session", "/sso", "/oauth", "/saml", "/passkey",
	} {
		if strings.Contains(p, marker) {
			return true
		}
	}
	return false
}

func extractTitle(html string) string {
	i := strings.Index(strings.ToLower(html), "<title>")
	j := strings.Index(strings.ToLower(html), "</title>")
	if i < 0 || j < 0 || j <= i {
		return ""
	}
	return strings.TrimSpace(html[i+7 : j])
}

func extractFirstHeading(html string) string {
	lower := strings.ToLower(html)
	for _, tag := range []string{"<h1>", "<h1 ", "<h2>", "<h2 "} {
		i := strings.Index(lower, tag)
		if i < 0 {
			continue
		}
		end := strings.Index(lower[i:], "<")
		if end < 0 {
			continue
		}
		// bound check: we need end to be past the opening tag itself
		if end < len(tag) {
			end = len(tag)
		}
		return strings.TrimSpace(html[i+len(tag) : i+end])
	}
	return ""
}

func extractFormAction(html string) string {
	lower := strings.ToLower(html)
	i := strings.Index(lower, "<form")
	if i < 0 {
		return ""
	}
	end := strings.Index(lower[i:], ">")
	if end < 0 {
		return ""
	}
	// end is the offset of '>' within the substring lower[i:]; the tag is
	// lower[i : i+end+1]. If end < len("<form"), something is malformed.
	if end < len("<form") {
		end = len("<form")
	}
	tag := html[i : i+end+1]
	ai := strings.Index(strings.ToLower(tag), "action=")
	if ai < 0 {
		return ""
	}
	rest := tag[ai+8:]
	// skip quote
	if len(rest) == 0 {
		return ""
	}
	quote := rest[0]
	if quote != '"' && quote != '\'' {
		// unquoted, read until space
		end := strings.IndexAny(rest, " 	\n>")
		if end < 0 {
			return rest
		}
		return rest[:end]
	}
	rest = rest[1:]
	j := strings.IndexByte(rest, quote)
	if j < 0 {
		return rest
	}
	return rest[:j]
}

func hasSessionCookie(h http.Header) bool {
	for _, c := range h.Values("Set-Cookie") {
		cl := strings.ToLower(c)
		if strings.Contains(cl, "session") || strings.Contains(cl, "sid=") || strings.Contains(cl, "csrf") {
			return true
		}
	}
	return false
}
