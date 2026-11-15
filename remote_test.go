package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testRemoteHost = "pc.tailnet.ts.net"

// setupHandlers builds the PC and remote handlers like serve does, with the
// state directory in a temp dir.
func setupHandlers(t *testing.T) (pc, remote http.Handler, pcToken, remoteToken string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var err error
	if pcToken, err = loadOrCreateToken(); err != nil {
		t.Fatal(err)
	}
	if remoteToken, err = loadOrCreateTokenAt(remoteTokenPath()); err != nil {
		t.Fatal(err)
	}
	store := newStore(filepath.Join(t.TempDir(), "sessions.json"))
	store.apply(map[string]any{"session_id": "s1", "hook_event_name": "SessionStart", "cwd": "/tmp/x"})
	pcOK := func(r *http.Request) bool { return tokenMatches(r, pcToken) }
	pc = guard(loopbackHostOK, func() string { return pcToken }, newMux(store, &controller{store: store, claudeBin: "claude"}, false, pcOK))
	remote = remoteHandler(testRemoteHost, newMux(store, &controller{store: store, claudeBin: "claude", remote: true}, true, remoteStillAuthorized))
	return
}

type reqOpt func(*http.Request)

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

func do(h http.Handler, method, path, host, token string, opts ...reqOpt) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r.Host = host
	if token != "" {
		r.Header.Set("X-Tonberry-Token", token)
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRemoteForbiddenRoutes(t *testing.T) {
	_, remote, _, tok := setupHandlers(t)
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/launch"},
		{"DELETE", "/api/sessions/s1"},
		{"POST", "/api/sessions/clear-ended"},
		{"POST", "/api/hook"},
		{"POST", "/api/sessions/s1/../../launch"},
		{"POST", "/api/sessions/s1/%2e%2e/%2e%2e/launch"},
	} {
		// 404/405 for the plain routes; the mux answers unclean paths with a
		// redirect, whose target is checked again on the next request.
		w := do(remote, c.method, c.path, testRemoteHost, tok)
		switch w.Code {
		case http.StatusNotFound, http.StatusMethodNotAllowed:
		case http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			loc := w.Header().Get("Location")
			if r := do(remote, c.method, loc, testRemoteHost, tok); r.Code != http.StatusNotFound && r.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s -> %s: got %d, want 404/405", c.method, c.path, loc, r.Code)
			}
		default:
			t.Errorf("%s %s: got %d, want 404/405", c.method, c.path, w.Code)
		}
	}
	// Nothing was deleted.
	if w := do(remote, "GET", "/api/sessions", testRemoteHost, tok); !strings.Contains(w.Body.String(), `"s1"`) {
		t.Errorf("session s1 should still exist: %s", w.Body.String())
	}
}

func TestRemoteAllowedRoutes(t *testing.T) {
	_, remote, _, tok := setupHandlers(t)
	for _, path := range []string{"/", "/manifest.json", "/icon-192.png", "/api/sessions", "/api/info"} {
		if w := do(remote, "GET", path, testRemoteHost, tok); w.Code != http.StatusOK {
			t.Errorf("GET %s: got %d", path, w.Code)
		}
	}
	w := do(remote, "GET", "/api/info", testRemoteHost, tok)
	if !strings.Contains(w.Body.String(), `"remote":true`) {
		t.Errorf("info: %s", w.Body.String())
	}
	// Control routes are registered (the session has no tmux pane, so 409).
	for _, a := range []string{"send", "keys", "resize"} {
		if w := do(remote, "POST", "/api/sessions/s1/"+a, testRemoteHost, tok); w.Code != http.StatusConflict {
			t.Errorf("POST %s: got %d, want 409", a, w.Code)
		}
	}
}

func TestRemoteTokens(t *testing.T) {
	pc, remote, pcTok, remoteTok := setupHandlers(t)
	if w := do(remote, "GET", "/api/sessions", testRemoteHost, pcTok); w.Code != http.StatusUnauthorized {
		t.Errorf("PC token on remote: got %d, want 401", w.Code)
	}
	if w := do(remote, "GET", "/api/sessions", testRemoteHost, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token on remote: got %d, want 401", w.Code)
	}
	if w := do(remote, "GET", "/api/sessions?token="+remoteTok, testRemoteHost, ""); w.Code != http.StatusOK {
		t.Errorf("query token on remote: got %d, want 200", w.Code)
	}
	if w := do(pc, "GET", "/api/sessions", "127.0.0.1:7777", remoteTok); w.Code != http.StatusUnauthorized {
		t.Errorf("remote token on PC: got %d, want 401", w.Code)
	}

	// Rotation takes effect without a restart.
	if _, err := rotateRemoteToken(); err != nil {
		t.Fatal(err)
	}
	if w := do(remote, "GET", "/api/sessions", testRemoteHost, remoteTok); w.Code != http.StatusUnauthorized {
		t.Errorf("old remote token after rotation: got %d, want 401", w.Code)
	}
	if w := do(remote, "GET", "/api/sessions", testRemoteHost, readTokenFile(remoteTokenPath())); w.Code != http.StatusOK {
		t.Errorf("new remote token: got %d, want 200", w.Code)
	}
}

func TestRemoteHostAndOrigin(t *testing.T) {
	_, remote, _, tok := setupHandlers(t)
	for _, host := range []string{"127.0.0.1:7778", "localhost", "evil.example", testRemoteHost + ":8443", "x" + testRemoteHost} {
		if w := do(remote, "GET", "/api/sessions", host, tok); w.Code != http.StatusForbidden {
			t.Errorf("host %q: got %d, want 403", host, w.Code)
		}
	}
	if w := do(remote, "GET", "/api/sessions", testRemoteHost+":443", tok); w.Code != http.StatusOK {
		t.Errorf("host with :443: got %d, want 200", w.Code)
	}
	if w := do(remote, "POST", "/api/sessions/s1/keys", testRemoteHost, tok, withHeader("Origin", "https://evil.example")); w.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST: got %d, want 403", w.Code)
	}
	if w := do(remote, "POST", "/api/sessions/s1/keys", testRemoteHost, tok, withHeader("Sec-Fetch-Site", "cross-site")); w.Code != http.StatusForbidden {
		t.Errorf("cross-site POST: got %d, want 403", w.Code)
	}
	if w := do(remote, "POST", "/api/sessions/s1/keys", testRemoteHost, tok, withHeader("Origin", "https://"+testRemoteHost)); w.Code == http.StatusForbidden {
		t.Errorf("same-origin POST was rejected")
	}
	w := do(remote, "GET", "/", testRemoteHost, "")
	for _, h := range []string{"Content-Security-Policy", "X-Frame-Options", "Referrer-Policy"} {
		if w.Header().Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "'sha256-") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Errorf("unexpected CSP: %s", csp)
	}
}

func TestPCListenerUnchanged(t *testing.T) {
	pc, _, tok, _ := setupHandlers(t)
	if w := do(pc, "GET", "/api/sessions", testRemoteHost, tok); w.Code != http.StatusForbidden {
		t.Errorf("ts.net host on PC listener: got %d, want 403", w.Code)
	}
	if w := do(pc, "GET", "/api/info", "127.0.0.1:7777", tok); !strings.Contains(w.Body.String(), `"remote":false`) {
		t.Errorf("info: %s", w.Body.String())
	}
	if w := do(pc, "POST", "/api/sessions/clear-ended", "127.0.0.1:7777", tok); w.Code != http.StatusNoContent {
		t.Errorf("clear-ended on PC: got %d", w.Code)
	}
	if w := do(pc, "DELETE", "/api/sessions/s1", "127.0.0.1:7777", tok); w.Code != http.StatusNoContent {
		t.Errorf("delete on PC: got %d", w.Code)
	}
}

func TestValidateRemote(t *testing.T) {
	ok := [][3]string{{"127.0.0.1:7777", "127.0.0.1:7778", testRemoteHost}}
	bad := [][3]string{
		{"127.0.0.1:7777", "127.0.0.1:7778", ""},
		{"127.0.0.1:7777", "", testRemoteHost},
		{"127.0.0.1:7777", "0.0.0.0:7778", testRemoteHost},
		{"127.0.0.1:7777", "100.64.0.1:7778", testRemoteHost},
		{"127.0.0.1:7777", "127.0.0.1:7777", testRemoteHost},
		{"127.0.0.1:7777", "127.0.0.1:7778", "https://" + testRemoteHost},
		{"127.0.0.1:7777", "127.0.0.1:7778", testRemoteHost + ":443"},
		{"127.0.0.1:7777", "127.0.0.1:7778", "localhost"},
	}
	for _, c := range ok {
		if err := validateRemote(c[0], c[1], c[2]); err != nil {
			t.Errorf("%v: %v", c, err)
		}
	}
	for _, c := range bad {
		if err := validateRemote(c[0], c[1], c[2]); err == nil {
			t.Errorf("%v: want error", c)
		}
	}
}

// An event stream opened with the old token must end after rotation.
func TestRotationEndsOpenStreams(t *testing.T) {
	_, remote, _, tok := setupHandlers(t)
	srv := httptest.NewServer(remote)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/events?token="+tok, nil)
	req.Host = testRemoteHost
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("events: got %d", res.StatusCode)
	}
	buf := make([]byte, 4096)
	if _, err := res.Body.Read(buf); err != nil { // initial snapshot
		t.Fatal(err)
	}
	if _, err := rotateRemoteToken(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, res.Body); done <- err }()
	select {
	case <-done:
	case <-time.After(authRecheck + 3*time.Second):
		t.Fatal("stream with the old token is still open after rotation")
	}
}

// A leftover temp file with wide permissions must not widen the token file.
func TestTokenFileMode(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remoteTokenPath()+".tmp", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := rotateRemoteToken(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{remoteTokenPath(): 0o600, stateDir(): 0o700} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != want {
			t.Errorf("%s: mode %o, want %o", path, got, want)
		}
	}
}
