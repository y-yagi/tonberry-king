package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Keys that may be sent via tmux (arbitrary keys are not allowed).
var allowedKeys = map[string]bool{
	"Enter": true, "Escape": true, "Up": true, "Down": true, "Left": true, "Right": true,
	"Tab": true, "BTab": true, "C-c": true,
	"1": true, "2": true, "3": true, "4": true, "5": true, "6": true, "7": true, "8": true, "9": true,
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func tmuxOutput(sock string, stdin []byte, args ...string) (string, error) {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return "", errors.New("tmux not found (install it, e.g. sudo apt install tmux)")
	}
	if sock != "" {
		args = append([]string{"-S", sock}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %v %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func paneAlive(sock, pane string) bool {
	if pane == "" {
		return false
	}
	out, err := tmuxOutput(sock, nil, "display-message", "-p", "-t", pane, "#{pane_id} #{pane_dead}")
	return err == nil && strings.TrimSpace(out) == pane+" 0"
}

type controller struct {
	store     *Store
	claudeBin string
	remote    bool         // serves the remote (phone) listener: no launch, actions are tagged in the event log
	prompts   *promptStore // templates and recent initial instructions (PC only; may be nil)
}

func (c *controller) routes(mux *http.ServeMux) {
	if !c.remote {
		mux.HandleFunc("POST /api/launch", c.handleLaunch)
		if c.prompts != nil {
			c.prompts.routes(mux)
		}
	}
	mux.HandleFunc("POST /api/sessions/{id}/send", c.handleSend)
	mux.HandleFunc("POST /api/sessions/{id}/keys", c.handleKeys)
	mux.HandleFunc("POST /api/sessions/{id}/stop", c.handleStop)
	mux.HandleFunc("GET /api/sessions/{id}/screen", c.handleScreen)
	mux.HandleFunc("POST /api/sessions/{id}/resize", c.handleResize)
}

// logEvent records a dashboard action; actions from the remote listener are
// marked so they can be told apart from the PC.
func (c *controller) logEvent(id, name, detail string) {
	if c.remote {
		detail = strings.TrimSpace("[remote] " + detail)
	}
	c.store.logEvent(id, name, detail)
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func expandDir(d string) (string, error) {
	d = strings.TrimSpace(d)
	if d == "~" || strings.HasPrefix(d, "~/") {
		home, _ := os.UserHomeDir()
		d = filepath.Join(home, strings.TrimPrefix(d, "~"))
	}
	if !filepath.IsAbs(d) {
		return "", errors.New("directory must be an absolute path (or ~/...)")
	}
	d = filepath.Clean(d)
	st, err := os.Stat(d)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("directory does not exist: %s", d)
	}
	return d, nil
}

func (c *controller) handleLaunch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir    string `json:"dir"`
		Prompt string `json:"prompt"`
		Args   string `json:"args"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	dir, err := expandDir(req.Dir)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	bin, err := exec.LookPath(c.claudeBin)
	if err != nil {
		fail(w, http.StatusInternalServerError, fmt.Errorf("claude not found: %v", err))
		return
	}

	rb := make([]byte, 3)
	rand.Read(rb)
	base := unsafeName.ReplaceAllString(filepath.Base(dir), "_")
	if len(base) > 24 {
		base = base[:24]
	}
	name := "tk-" + base + "-" + hex.EncodeToString(rb)

	args := []string{"new-session", "-d", "-P", "-F", "#{pane_id}\t#{socket_path}",
		"-s", name, "-c", dir, "-x", "200", "-y", "50", bin}
	args = append(args, strings.Fields(req.Args)...)
	if p := strings.TrimSpace(req.Prompt); p != "" {
		args = append(args, "--", p)
	}
	out, err := tmuxOutput("", nil, args...)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	pane, sock, _ := strings.Cut(strings.TrimSpace(out), "\t")
	sess := c.store.addStarting(name, pane, sock, dir, req.Prompt)
	if c.prompts != nil {
		if err := c.prompts.addRecent(req.Prompt); err != nil {
			log.Printf("save recent prompt: %v", err)
		}
		if err := c.prompts.addDir(dir); err != nil {
			log.Printf("save recent directory: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": sess.ID, "tmux_session": name})
}

func (c *controller) target(w http.ResponseWriter, r *http.Request) (Session, bool) {
	sess, ok := c.store.get(r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, errors.New("session not found"))
		return sess, false
	}
	return sess, true
}

func (c *controller) requirePane(w http.ResponseWriter, sess Session) bool {
	if sess.TmuxPane == "" {
		fail(w, http.StatusConflict, errors.New("cannot send input to a session started outside tmux"))
		return false
	}
	if sess.Status == StatusEnded {
		fail(w, http.StatusConflict, errors.New("session has ended"))
		return false
	}
	return true
}

func (c *controller) handleSend(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.target(w, r)
	if !ok || !c.requirePane(w, sess) {
		return
	}
	var req struct {
		Text   string `json:"text"`
		Submit *bool  `json:"submit"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	text := strings.ReplaceAll(strings.TrimRight(req.Text, "\r\n"), "\r\n", "\n")
	if text != "" {
		// Paste with bracketed paste so newlines do not submit the prompt midway.
		rb := make([]byte, 4)
		rand.Read(rb)
		buf := "tk-" + hex.EncodeToString(rb)
		if _, err := tmuxOutput(sess.TmuxSocket, []byte(text), "load-buffer", "-b", buf, "-"); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		if _, err := tmuxOutput(sess.TmuxSocket, nil, "paste-buffer", "-d", "-p", "-r", "-b", buf, "-t", sess.TmuxPane); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
	}
	if req.Submit == nil || *req.Submit {
		time.Sleep(200 * time.Millisecond)
		if _, err := tmuxOutput(sess.TmuxSocket, nil, "send-keys", "-t", sess.TmuxPane, "Enter"); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
	}
	c.logEvent(sess.ID, "WebSend", truncate(text, 120))
	w.WriteHeader(http.StatusNoContent)
}

func (c *controller) handleKeys(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.target(w, r)
	if !ok || !c.requirePane(w, sess) {
		return
	}
	var req struct {
		Keys []string `json:"keys"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Keys) == 0 || len(req.Keys) > 10 {
		fail(w, http.StatusBadRequest, errors.New("keys must contain 1 to 10 entries"))
		return
	}
	for _, k := range req.Keys {
		if !allowedKeys[k] {
			fail(w, http.StatusBadRequest, fmt.Errorf("key not allowed: %s", k))
			return
		}
	}
	args := append([]string{"send-keys", "-t", sess.TmuxPane}, req.Keys...)
	if _, err := tmuxOutput(sess.TmuxSocket, nil, args...); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	c.logEvent(sess.ID, "WebKeys", strings.Join(req.Keys, " "))
	w.WriteHeader(http.StatusNoContent)
}

func (c *controller) handleStop(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.target(w, r)
	if !ok {
		return
	}
	var req struct {
		Mode string `json:"mode"` // interrupt | exit
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	switch req.Mode {
	case "interrupt":
		if !c.requirePane(w, sess) {
			return
		}
		if _, err := tmuxOutput(sess.TmuxSocket, nil, "send-keys", "-t", sess.TmuxPane, "Escape"); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		c.logEvent(sess.ID, "WebInterrupt", "")
	case "exit":
		switch {
		case sess.PID > 0 && isClaudeProcess(sess.PID):
			if err := syscall.Kill(sess.PID, syscall.SIGTERM); err != nil {
				fail(w, http.StatusInternalServerError, err)
				return
			}
			go func(pid int) { // Force kill if it does not exit within the grace period.
				for i := 0; i < 50; i++ {
					time.Sleep(100 * time.Millisecond)
					if !isClaudeProcess(pid) {
						return
					}
				}
				syscall.Kill(pid, syscall.SIGKILL)
			}(sess.PID)
		case sess.Status == StatusStarting && sess.TmuxPane != "":
			if _, err := tmuxOutput(sess.TmuxSocket, nil, "kill-pane", "-t", sess.TmuxPane); err != nil {
				fail(w, http.StatusInternalServerError, err)
				return
			}
		default:
			fail(w, http.StatusConflict, errors.New("no process to stop"))
			return
		}
		c.logEvent(sess.ID, "WebExit", "")
	default:
		fail(w, http.StatusBadRequest, errors.New("mode must be interrupt or exit"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxScreenHistory caps how many scrollback lines /screen returns.
const maxScreenHistory = 10000

func (c *controller) handleScreen(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.target(w, r)
	if !ok || !c.requirePane(w, sess) {
		return
	}
	// history=N also includes up to N lines of tmux scrollback above the visible area.
	args := []string{"capture-pane", "-p", "-t", sess.TmuxPane}
	if n, err := strconv.Atoi(r.URL.Query().Get("history")); err == nil && n > 0 {
		args = append(args, "-S", strconv.Itoa(-min(n, maxScreenHistory)))
	}
	out, err := tmuxOutput(sess.TmuxSocket, nil, args...)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if size, err := tmuxOutput(sess.TmuxSocket, nil, "display-message", "-p", "-t", sess.TmuxPane, "#{pane_width}x#{pane_height}"); err == nil {
		w.Header().Set("X-Pane-Size", strings.TrimSpace(size))
	}
	// Capture again with attributes to pick up the dimmed prompt suggestion.
	if styled, err := tmuxOutput(sess.TmuxSocket, nil, "capture-pane", "-p", "-e", "-t", sess.TmuxPane); err == nil {
		if sug := parseSuggestion(styled); sug != "" {
			w.Header().Set("X-Suggestion", url.QueryEscape(sug))
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, strings.TrimRight(out, "\n")+"\n")
}

// parseSuggestion extracts the prompt suggestion Claude Code shows as dim text
// in an empty input box ("❯\u00a0" followed by an SGR 2 run). It returns "" when
// the input box holds typed text or no suggestion is shown.
func parseSuggestion(styled string) string {
	const prompt = "❯\u00a0"
	lines := strings.Split(styled, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		idx := strings.LastIndex(lines[i], prompt)
		if idx < 0 {
			continue
		}
		rest := lines[i][idx+len(prompt):]
		body, ok := strings.CutPrefix(rest, "\x1b[2m")
		if !ok {
			return ""
		}
		text, _, _ := strings.Cut(body, "\x1b[")
		return strings.TrimSpace(text)
	}
	return ""
}

// handleResize changes the column count of the tmux window. With auto=true it
// restores automatic sizing to the attached clients.
func (c *controller) handleResize(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.target(w, r)
	if !ok || !c.requirePane(w, sess) {
		return
	}
	var req struct {
		Cols int  `json:"cols"`
		Auto bool `json:"auto"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	args := []string{"resize-window", "-t", sess.TmuxPane}
	switch {
	case req.Auto:
		args = append(args, "-A")
	case req.Cols >= 40 && req.Cols <= 500:
		args = append(args, "-x", strconv.Itoa(req.Cols))
	default:
		fail(w, http.StatusBadRequest, errors.New("cols must be between 40 and 500"))
		return
	}
	if _, err := tmuxOutput(sess.TmuxSocket, nil, args...); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
