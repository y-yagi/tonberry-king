package main

// The remote listener lets a phone use the dashboard through `tailscale serve`
// (HTTPS, tailnet only). It still binds to loopback only; tailscale serve is
// the only way in. Compared with the PC listener it:
//   - accepts only its own token (remote-token), which can be rotated while running,
//   - accepts only Host = the configured ts.net name,
//   - does not register launch / delete / clear-ended / hook (see newMux),
//   - rejects cross-origin state-changing requests and sends security headers.

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
)

// hostnameRe matches a DNS name such as pc.tailnet.ts.net (no scheme, port or path).
var hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

func validateRemote(addr, remoteAddr, remoteHost string) error {
	if remoteAddr == "" || remoteHost == "" {
		return errors.New("-remote-addr and -remote-host must be set together")
	}
	if err := requireLoopbackAddr(remoteAddr); err != nil {
		return fmt.Errorf("-remote-addr: %w (expose it with `tailscale serve`, not by listening on other interfaces)", err)
	}
	if remoteAddr == addr {
		return errors.New("-remote-addr must differ from -addr")
	}
	if !hostnameRe.MatchString(remoteHost) {
		return fmt.Errorf("-remote-host must be a lowercase host name like pc.tailnet.ts.net: %q", remoteHost)
	}
	return nil
}

// remoteHostOK accepts only the configured name, on the default HTTPS port.
func remoteHostOK(want string) func(host, port string) bool {
	return func(host, port string) bool {
		return strings.EqualFold(host, want) && (port == "" || port == "443")
	}
}

// inlineScriptHashes returns CSP source expressions for the inline scripts of
// the embedded index.html, so the page works under a CSP without 'unsafe-inline'.
func inlineScriptHashes() []string {
	b, err := fs.ReadFile(webFS, "web/index.html")
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllSubmatch(b, -1) {
		sum := sha256.Sum256(m[1])
		out = append(out, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return out
}

func remoteCSP() string {
	return strings.Join([]string{
		"default-src 'self'",
		"script-src " + strings.Join(inlineScriptHashes(), " "),
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"connect-src 'self'",
		"manifest-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

func remoteToken() string { return readTokenFile(remoteTokenPath()) }

// remoteStillAuthorized lets an open event stream notice a token rotation.
func remoteStillAuthorized(r *http.Request) bool { return tokenMatches(r, remoteToken()) }

func remoteHandler(host string, mux http.Handler) http.Handler {
	origin := "https://" + strings.ToLower(host)
	csp := remoteCSP()
	inner := guard(remoteHostOK(host), remoteToken, mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" && !strings.EqualFold(o, origin) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
			if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		inner.ServeHTTP(w, r)
	})
}

func remoteDashboardURL(host, token string) string {
	return fmt.Sprintf("https://%s/#token=%s", host, token)
}

// rotateRemoteToken replaces the remote token. The running server reads it on
// every request, so the old token stops working immediately.
func rotateRemoteToken() (string, error) {
	t, err := newToken()
	if err != nil {
		return "", err
	}
	return t, writeTokenFile(remoteTokenPath(), t)
}
