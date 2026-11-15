package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

const (
	StatusStarting   = "starting"
	StatusRunning    = "running"
	StatusPermission = "waiting_permission"
	StatusInput      = "waiting_input"
	StatusSubagents  = "subagents" // the main turn ended but background subagents are still working
	StatusScheduled  = "scheduled" // the main turn ended with a ScheduleWakeup (/loop) and will resume by itself
	StatusCompacting = "compacting"
	StatusEnded      = "ended"

	maxEvents    = 100
	endedTTL     = 24 * time.Hour
	livenessTick = 3 * time.Second
	authRecheck  = 2 * time.Second // how often an open event stream re-checks its token
)

type Event struct {
	Time   time.Time `json:"time"`
	Name   string    `json:"name"`
	Detail string    `json:"detail,omitempty"`
}

// Subagent is a subagent the session started that has not finished yet.
type Subagent struct {
	ID          string `json:"id"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
}

type Session struct {
	ID             string     `json:"id"`
	Cwd            string     `json:"cwd"`
	Branch         string     `json:"branch,omitempty"`
	TranscriptPath string     `json:"transcript_path,omitempty"`
	PID            int        `json:"pid,omitempty"`
	TTY            string     `json:"tty,omitempty"`
	TmuxPane       string     `json:"tmux_pane,omitempty"`
	TmuxSocket     string     `json:"tmux_socket,omitempty"`
	TmuxSession    string     `json:"tmux_session,omitempty"`
	Launched       bool       `json:"launched,omitempty"`
	Status         string     `json:"status"`
	FirstPrompt    string     `json:"first_prompt,omitempty"`
	LastPrompt     string     `json:"last_prompt,omitempty"`
	Tool           string     `json:"tool,omitempty"`
	ToolDetail     string     `json:"tool_detail,omitempty"`
	ToolRunning    bool       `json:"tool_running"`
	Message        string     `json:"message,omitempty"`
	LastAssistant  string     `json:"last_assistant,omitempty"`
	Subagents      []Subagent `json:"subagents,omitempty"`
	TurnEnded      bool       `json:"turn_ended,omitempty"` // the main agent has finished its turn
	WakeAt         *time.Time `json:"wake_at,omitempty"`    // when a ScheduleWakeup of the current turn resumes the session
	WakeReason     string     `json:"wake_reason,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	StatusSince    time.Time  `json:"status_since"`
	Events         []Event    `json:"events"`
}

type Store struct {
	mu       sync.Mutex
	sessions map[string]*Session
	path     string
	dirty    bool
	subs     map[chan []byte]struct{}
}

func newStore(path string) *Store {
	s := &Store{sessions: map[string]*Session{}, path: path, subs: map[chan []byte]struct{}{}}
	if b, err := os.ReadFile(path); err == nil {
		var list []*Session
		if json.Unmarshal(b, &list) == nil {
			for _, x := range list {
				s.sessions[x.ID] = x
			}
		}
	}
	return s
}

func stateDir() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "tonberry-king")
}

func defaultStatePath() string { return filepath.Join(stateDir(), "sessions.json") }
func tokenPath() string        { return filepath.Join(stateDir(), "token") }

// remoteTokenPath holds the token for the remote (phone) listener. It is kept
// separate so the PC token is never accepted remotely and the remote one can
// be rotated on its own.
func remoteTokenPath() string { return filepath.Join(stateDir(), "remote-token") }

func readTokenFile(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

func readToken() string { return readTokenFile(tokenPath()) }

func newToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// writeTokenFile atomically writes a token with mode 0600. The temp file gets
// a fresh random name (CreateTemp uses O_EXCL and mode 0600), so a leftover or
// planted file cannot widen the permissions or redirect the write.
func writeTokenFile(path, t string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // no-op after a successful rename
	if _, err := f.WriteString(t + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// loadOrCreateTokenAt loads the token at path, creating it with mode 0600 if missing.
func loadOrCreateTokenAt(path string) (string, error) {
	if t := readTokenFile(path); t != "" {
		return t, nil
	}
	t, err := newToken()
	if err != nil {
		return "", err
	}
	return t, writeTokenFile(path, t)
}

// loadOrCreateToken loads the API token, creating it with mode 0600 if missing.
func loadOrCreateToken() (string, error) { return loadOrCreateTokenAt(tokenPath()) }

func dashboardURL(addr, token string) string {
	return fmt.Sprintf("http://%s/#token=%s", addr, token)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func summarizeToolInput(in any) string {
	m, ok := in.(map[string]any)
	if !ok {
		return ""
	}
	for _, k := range []string{"command", "file_path", "notebook_path", "pattern", "url", "query", "description", "prompt", "path"} {
		if v := str(m[k]); v != "" {
			return truncate(v, 300)
		}
	}
	for _, v := range m {
		if s := str(v); s != "" {
			return truncate(s, 300)
		}
	}
	return ""
}

// runningSubagents extracts the subagents that are still running from the
// background_tasks field of a Stop payload. Background shells are ignored:
// they may run indefinitely (e.g. dev servers) and do not mean claude is busy.
func runningSubagents(tasks []any) []Subagent {
	var out []Subagent
	for _, t := range tasks {
		m, ok := t.(map[string]any)
		if !ok || str(m["type"]) != "subagent" || str(m["status"]) != "running" || str(m["id"]) == "" {
			continue
		}
		out = append(out, Subagent{ID: str(m["id"]), Type: str(m["agent_type"]), Description: truncate(str(m["description"]), 200)})
	}
	return out
}

func (sess *Session) addSubagent(a Subagent) {
	for _, x := range sess.Subagents {
		if x.ID == a.ID {
			return
		}
	}
	sess.Subagents = append(sess.Subagents, a)
}

func (sess *Session) removeSubagent(id string) {
	if id == "" {
		return
	}
	out := sess.Subagents[:0]
	for _, x := range sess.Subagents {
		if x.ID != id {
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		out = nil
	}
	sess.Subagents = out
}

// scheduleWakeup records the wakeup a main-agent ScheduleWakeup call asks
// for. The runtime clamps the delay to [60, 3600] seconds; stop: true ends
// the loop, so no wakeup follows.
func (sess *Session) scheduleWakeup(input any, now time.Time) {
	m, _ := input.(map[string]any)
	if stop, _ := m["stop"].(bool); stop {
		sess.WakeAt, sess.WakeReason = nil, ""
		return
	}
	d, _ := m["delaySeconds"].(float64)
	d = min(max(d, 60), 3600)
	t := now.Add(time.Duration(d) * time.Second)
	sess.WakeAt, sess.WakeReason = &t, truncate(str(m["reason"]), 200)
}

func (s *Store) setStatus(sess *Session, st string, now time.Time) {
	if sess.Status != st {
		sess.Status = st
		sess.StatusSince = now
	}
}

// apply updates the session state from a single hook event.
func (s *Store) apply(p map[string]any) {
	id := str(p["session_id"])
	if id == "" {
		return
	}
	ev := str(p["hook_event_name"])
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		sess = &Session{ID: id, StartedAt: now, Status: StatusInput, StatusSince: now}
		s.sessions[id] = sess
	}
	cwdChanged := false
	if c := str(p["cwd"]); c != "" && c != sess.Cwd {
		sess.Cwd = c
		cwdChanged = true
	}
	if t := str(p["transcript_path"]); t != "" {
		sess.TranscriptPath = t
	}
	if pid, ok := p["_pid"].(float64); ok && pid > 0 {
		sess.PID = int(pid)
		// After /clear the same process continues under a new session_id: hand
		// the old session over to the new one so the dashboard keeps one card.
		cleared := ev == "SessionStart" && str(p["source"]) == "clear"
		// If the same process starts using a new session_id otherwise, mark the old one as ended.
		for oid, o := range s.sessions {
			if oid == id || o.PID != sess.PID {
				continue
			}
			if cleared {
				sess.Launched = sess.Launched || o.Launched
				if sess.TmuxSession == "" && o.TmuxPane != "" && o.TmuxPane == str(p["_tmux_pane"]) {
					sess.TmuxPane, sess.TmuxSocket, sess.TmuxSession = o.TmuxPane, o.TmuxSocket, o.TmuxSession
				}
				delete(s.sessions, oid)
				continue
			}
			if o.Status != StatusEnded {
				s.setStatus(o, StatusEnded, now)
				o.Message = "A new session was started in the same process"
				o.UpdatedAt = now
			}
		}
	}
	if t := str(p["_tty"]); t != "" {
		sess.TTY = t
	}
	if pane := str(p["_tmux_pane"]); pane != "" {
		sock := str(p["_tmux_socket"])
		if sess.TmuxPane != pane || sess.TmuxSocket != sock {
			sess.TmuxPane, sess.TmuxSocket, sess.TmuxSession = pane, sock, ""
		}
		// Hand over a placeholder session launched from the dashboard to the real session.
		for oid, o := range s.sessions {
			if o.Status == StatusStarting && o.TmuxPane == pane && (o.TmuxSocket == "" || o.TmuxSocket == sock) {
				sess.Launched = true
				sess.TmuxSession = o.TmuxSession
				if sess.FirstPrompt == "" {
					sess.FirstPrompt = o.FirstPrompt
				}
				delete(s.sessions, oid)
			}
		}
		if sess.TmuxSession == "" {
			go s.loadTmuxSessionName(id, sock, pane)
		}
	}
	sess.UpdatedAt = now

	// Events fired from inside a subagent carry agent_id. They tell us the
	// subagent is alive, but they say nothing about the main agent's turn.
	agentID := str(p["agent_id"])
	if agentID != "" && ev != "SubagentStop" {
		sess.addSubagent(Subagent{ID: agentID, Type: str(p["agent_type"])})
	}
	// busy is the status to show while work is in progress: if the main turn
	// has already ended, the activity comes from background subagents.
	busy := StatusRunning
	if agentID != "" && sess.TurnEnded && len(sess.Subagents) > 0 {
		busy = StatusSubagents
	}
	if agentID == "" && (ev == "PreToolUse" || ev == "PostToolUse") {
		// The main agent is working again after its turn ended, so a wakeup
		// from that turn has fired (or the session was woken another way).
		if sess.TurnEnded {
			sess.WakeAt, sess.WakeReason = nil, ""
		}
		sess.TurnEnded = false
		if ev == "PreToolUse" && str(p["tool_name"]) == "ScheduleWakeup" {
			sess.scheduleWakeup(p["tool_input"], now)
		}
	}

	detail := ""
	switch ev {
	case "SessionStart":
		s.setStatus(sess, StatusInput, now)
		sess.Message = ""
		sess.ToolRunning = false
		sess.Subagents, sess.TurnEnded = nil, false
		sess.WakeAt, sess.WakeReason = nil, ""
		detail = str(p["source"])
	case "UserPromptSubmit":
		prompt := str(p["prompt"])
		if sess.FirstPrompt == "" {
			sess.FirstPrompt = truncate(prompt, 200)
		}
		sess.LastPrompt = truncate(prompt, 1000)
		sess.Message = ""
		sess.Tool, sess.ToolDetail, sess.ToolRunning = "", "", false
		sess.TurnEnded = false
		sess.WakeAt, sess.WakeReason = nil, ""
		s.setStatus(sess, StatusRunning, now)
		detail = truncate(prompt, 120)
	case "PreToolUse":
		sess.Tool = str(p["tool_name"])
		sess.ToolDetail = summarizeToolInput(p["tool_input"])
		sess.ToolRunning = true
		sess.Message = ""
		s.setStatus(sess, busy, now)
		detail = sess.Tool + ": " + truncate(sess.ToolDetail, 120)
	case "PostToolUse":
		sess.ToolRunning = false
		sess.Message = ""
		s.setStatus(sess, busy, now)
		detail = str(p["tool_name"])
	case "PermissionRequest":
		tool := str(p["tool_name"])
		sess.Tool = tool
		sess.ToolDetail = summarizeToolInput(p["tool_input"])
		sess.Message = "Waiting for permission to use " + tool
		s.setStatus(sess, StatusPermission, now)
		detail = tool + ": " + truncate(sess.ToolDetail, 120)
	case "Notification":
		msg := str(p["message"])
		typ := str(p["notification_type"])
		low := strings.ToLower(msg)
		switch {
		case typ == "permission_prompt" || strings.Contains(low, "permission"):
			s.setStatus(sess, StatusPermission, now)
		case typ == "idle_prompt" || strings.Contains(low, "waiting for your input"):
			if sess.Status != StatusSubagents && sess.Status != StatusScheduled {
				s.setStatus(sess, StatusInput, now)
			}
		case typ == "elicitation_dialog":
			s.setStatus(sess, StatusPermission, now)
		}
		sess.Message = msg
		detail = msg
	case "Stop":
		sess.ToolRunning = false
		sess.Message = ""
		sess.TurnEnded = true
		if bg, ok := p["background_tasks"].([]any); ok {
			sess.Subagents = runningSubagents(bg)
		}
		switch {
		case len(sess.Subagents) > 0:
			s.setStatus(sess, StatusSubagents, now)
		case sess.WakeAt != nil:
			s.setStatus(sess, StatusScheduled, now)
		default:
			s.setStatus(sess, StatusInput, now)
		}
		if m := str(p["last_assistant_message"]); m != "" {
			sess.LastAssistant = truncate(m, 2000)
		} else if sess.TranscriptPath != "" {
			go s.loadLastAssistant(id, sess.TranscriptPath)
		}
	case "SubagentStart":
		detail = str(p["agent_type"])
	case "SubagentStop":
		// The main agent is woken up with the result and the following Stop
		// settles the status, so only the bookkeeping is updated here.
		sess.removeSubagent(agentID)
		detail = str(p["agent_type"])
	case "PreCompact":
		s.setStatus(sess, StatusCompacting, now)
		detail = str(p["trigger"])
	case "SessionEnd":
		s.setStatus(sess, StatusEnded, now)
		sess.ToolRunning = false
		sess.Message = ""
		sess.Subagents, sess.TurnEnded = nil, false
		sess.WakeAt, sess.WakeReason = nil, ""
		detail = str(p["reason"])
	}

	s.addEventLocked(sess, Event{Time: now, Name: ev, Detail: detail})

	if cwdChanged || ev == "SessionStart" || ev == "UserPromptSubmit" || ev == "Stop" {
		go s.loadBranch(id, sess.Cwd)
	}
	s.changedLocked()
}

func (s *Store) addEventLocked(sess *Session, e Event) {
	sess.Events = append(sess.Events, e)
	if len(sess.Events) > maxEvents {
		sess.Events = sess.Events[len(sess.Events)-maxEvents:]
	}
}

// addStarting registers a placeholder session for a launch that has not sent any hook yet.
func (s *Store) addStarting(name, pane, sock, dir, prompt string) *Session {
	now := time.Now()
	sess := &Session{
		ID: "launch-" + name, Cwd: dir, TmuxPane: pane, TmuxSocket: sock, TmuxSession: name,
		Launched: true, Status: StatusStarting, FirstPrompt: truncate(prompt, 200),
		StartedAt: now, UpdatedAt: now, StatusSince: now,
		Events: []Event{{Time: now, Name: "Launch", Detail: "tmux " + name}},
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Drop ended sessions from the same directory so relaunches don't pile up.
	for id, x := range s.sessions {
		if x.Status == StatusEnded && x.Cwd != "" && filepath.Clean(x.Cwd) == dir {
			delete(s.sessions, id)
		}
	}
	s.sessions[sess.ID] = sess
	s.changedLocked()
	go s.loadBranch(sess.ID, dir)
	return sess
}

func (s *Store) get(id string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.sessions[id]
	if !ok {
		return Session{}, false
	}
	return *x, true
}

func (s *Store) logEvent(id, name, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		s.addEventLocked(sess, Event{Time: time.Now(), Name: name, Detail: detail})
		s.changedLocked()
	}
}

func (s *Store) loadTmuxSessionName(id, sock, pane string) {
	out, err := tmuxOutput(sock, nil, "display-message", "-p", "-t", pane, "#{session_name}")
	if err != nil {
		return
	}
	name := strings.TrimSpace(out)
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok && sess.TmuxPane == pane && sess.TmuxSession != name {
		sess.TmuxSession = name
		s.changedLocked()
	}
}

func (s *Store) loadBranch(id, cwd string) {
	if cwd == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "branch", "--show-current").Output()
	if err != nil {
		return
	}
	b := strings.TrimSpace(string(out))
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok && sess.Branch != b {
		sess.Branch = b
		s.changedLocked()
	}
}

// loadLastAssistant extracts the last assistant text from the end of the transcript (JSONL).
func (s *Store) loadLastAssistant(id, path string) {
	time.Sleep(400 * time.Millisecond) // wait briefly for the transcript to be flushed
	text := lastAssistantText(path)
	if text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		sess.LastAssistant = truncate(text, 2000)
		s.changedLocked()
	}
}

func lastAssistantText(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const tail = 1 << 20
	if st, err := f.Stat(); err == nil && st.Size() > tail {
		f.Seek(st.Size()-tail, io.SeekStart)
	}
	var lines [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		lines = append(lines, bytes.Clone(sc.Bytes()))
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(lines[i], &rec) != nil || rec.Type != "assistant" {
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(rec.Message.Content, &parts) != nil {
			continue
		}
		var texts []string
		for _, p := range parts {
			if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
				texts = append(texts, p.Text)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}
	return ""
}

func isClaudeProcess(pid int) bool {
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return false
	}
	if strings.TrimSpace(string(comm)) == "claude" {
		return true
	}
	cmd, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return bytes.Contains(cmd, []byte("claude"))
}

// sweep marks sessions whose process is gone as ended and removes old ended sessions.
func (s *Store) sweep() {
	now := time.Now()
	_, procErr := os.Stat("/proc/self")

	// For starting placeholders, check the tmux pane is alive (run tmux outside the lock).
	type paneRef struct{ id, sock, pane string }
	var starting []paneRef
	s.mu.Lock()
	for id, x := range s.sessions {
		if x.Status == StatusStarting {
			starting = append(starting, paneRef{id, x.TmuxSocket, x.TmuxPane})
		}
	}
	s.mu.Unlock()
	dead := map[string]bool{}
	for _, r := range starting {
		if !paneAlive(r.sock, r.pane) {
			dead[r.id] = true
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for id, sess := range s.sessions {
		if dead[id] && sess.Status == StatusStarting {
			s.setStatus(sess, StatusEnded, now)
			sess.Message = "tmux session ended (launch may have failed)"
			sess.UpdatedAt = now
			changed = true
		}
		if sess.Status != StatusEnded && sess.Status != StatusStarting && sess.PID > 0 && procErr == nil && !isClaudeProcess(sess.PID) {
			s.setStatus(sess, StatusEnded, now)
			sess.ToolRunning = false
			sess.Message = "Process exit detected"
			sess.UpdatedAt = now
			changed = true
		}
		if sess.Status == StatusEnded && now.Sub(sess.UpdatedAt) > endedTTL {
			delete(s.sessions, id)
			changed = true
		}
	}
	if changed {
		s.changedLocked()
	}
}

func (s *Store) snapshotLocked() []byte {
	list := make([]*Session, 0, len(s.sessions))
	for _, x := range s.sessions {
		list = append(list, x)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].StartedAt.Before(list[j].StartedAt) })
	b, _ := json.Marshal(list)
	return b
}

func (s *Store) snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Store) changedLocked() {
	s.dirty = true
	snap := s.snapshotLocked()
	for ch := range s.subs {
		select {
		case ch <- snap:
		default: // slow client: drop the stale snapshot and send only the latest
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- snap:
			default:
			}
		}
	}
}

func (s *Store) persist() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	s.dirty = false
	snap := s.snapshotLocked()
	s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		log.Printf("persist: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, snap, 0o600); err != nil {
		log.Printf("persist: %v", err)
		return
	}
	os.Rename(tmp, s.path)
}

func (s *Store) subscribe() chan []byte {
	ch := make(chan []byte, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	ch <- s.snapshotLocked()
	s.mu.Unlock()
	return ch
}

func (s *Store) unsubscribe(ch chan []byte) {
	s.mu.Lock()
	delete(s.subs, ch)
	s.mu.Unlock()
}

func (s *Store) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	s.changedLocked()
}

func (s *Store) clearEnded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, x := range s.sessions {
		if x.Status == StatusEnded {
			delete(s.sessions, id)
		}
	}
	s.changedLocked()
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// guard requires an allowed Host header (DNS rebinding protection) and a
// token for /api/ (X-Tonberry-Token header or token query parameter). token is
// called on every request so a rotated token takes effect immediately; an
// empty token rejects every API request.
func guard(hostOK func(host, port string) bool, token func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, port, err := net.SplitHostPort(r.Host)
		if err != nil {
			host, port = r.Host, ""
		}
		if !hostOK(strings.Trim(host, "[]"), port) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if !tokenMatches(r, token()) {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requestToken returns the token sent with the request (X-Tonberry-Token
// header, or the token query parameter for EventSource).
func requestToken(r *http.Request) string {
	if t := r.Header.Get("X-Tonberry-Token"); t != "" {
		return t
	}
	return r.URL.Query().Get("token")
}

func tokenMatches(r *http.Request, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(requestToken(r)), []byte(want)) == 1
}

func loopbackHostOK(host, _ string) bool { return isLoopbackHost(host) }

type serveConfig struct {
	addr, statePath, claudeBin string
	// remoteAddr / remoteHost enable the remote (phone) listener. Both or
	// neither must be set; see remote.go.
	remoteAddr, remoteHost string
}

// newMux builds the routes. The remote mux does not register the routes the
// phone must not use (launch, delete, clear-ended, hook) at all, so they
// cannot be reached through it however the request is crafted.
//
// stillAuthorized is re-checked while an event stream is open, so a rotated
// token also cuts off streams opened with the old one.
func newMux(store *Store, ctl *controller, remote bool, stillAuthorized func(*http.Request) bool) *http.ServeMux {
	mux := http.NewServeMux()

	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServerFS(sub)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// Always revalidate so an outdated page is not kept after an update.
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	})

	mux.HandleFunc("GET /api/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"remote": remote})
	})

	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(store.snapshot())
	})

	if !remote {
		mux.HandleFunc("POST /api/hook", func(w http.ResponseWriter, r *http.Request) {
			var p map[string]any
			if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&p); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			store.apply(p)
			w.WriteHeader(http.StatusNoContent)
		})

		mux.HandleFunc("DELETE /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
			store.remove(r.PathValue("id"))
			w.WriteHeader(http.StatusNoContent)
		})

		mux.HandleFunc("POST /api/sessions/clear-ended", func(w http.ResponseWriter, r *http.Request) {
			store.clearEnded()
			w.WriteHeader(http.StatusNoContent)
		})
	}

	ctl.routes(mux)

	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		ch := store.subscribe()
		defer store.unsubscribe(ch)
		ping := time.NewTicker(20 * time.Second)
		defer ping.Stop()
		recheck := time.NewTicker(authRecheck)
		defer recheck.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case snap := <-ch:
				if !stillAuthorized(r) {
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", snap)
				flusher.Flush()
			case <-recheck.C:
				if !stillAuthorized(r) {
					return
				}
			case <-ping.C:
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			}
		}
	})
	return mux
}

func requireLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if !isLoopbackHost(host) {
		return errors.New("refusing to listen on non-loopback address: " + addr)
	}
	return nil
}

func serve(cfg serveConfig) error {
	if err := requireLoopbackAddr(cfg.addr); err != nil {
		return err
	}
	remote := cfg.remoteAddr != "" || cfg.remoteHost != ""
	if remote {
		if err := validateRemote(cfg.addr, cfg.remoteAddr, cfg.remoteHost); err != nil {
			return err
		}
		if _, err := loadOrCreateTokenAt(remoteTokenPath()); err != nil {
			return fmt.Errorf("remote token: %w", err)
		}
	}
	token, err := loadOrCreateToken()
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}

	store := newStore(cfg.statePath)
	ctl := &controller{store: store, claudeBin: cfg.claudeBin}
	pcToken := func() string { return token }
	mux := newMux(store, ctl, false, func(r *http.Request) bool { return tokenMatches(r, pcToken()) })

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		t := time.NewTicker(livenessTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				store.sweep()
				store.persist()
			}
		}
	}()

	// Request contexts derive from ctx, so open event streams end on shutdown
	// instead of holding Shutdown until its timeout.
	baseCtx := func(net.Listener) context.Context { return ctx }
	servers := []*http.Server{{Addr: cfg.addr, Handler: guard(loopbackHostOK, pcToken, mux), ReadHeaderTimeout: 5 * time.Second, BaseContext: baseCtx}}
	if remote {
		rctl := &controller{store: store, claudeBin: cfg.claudeBin, remote: true}
		rmux := newMux(store, rctl, true, remoteStillAuthorized)
		servers = append(servers, &http.Server{Addr: cfg.remoteAddr, Handler: remoteHandler(cfg.remoteHost, rmux), ReadHeaderTimeout: 5 * time.Second, BaseContext: baseCtx})
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		for _, srv := range servers {
			wg.Add(1)
			go func(srv *http.Server) { defer wg.Done(); srv.Shutdown(sctx) }(srv)
		}
		wg.Wait()
	}()

	log.Printf("tonberry-king listening on %s (state: %s)", dashboardURL(cfg.addr, token), cfg.statePath)
	if remote {
		log.Printf("remote listener on %s for https://%s (run `tonberry-king url -remote` for the phone URL)", cfg.remoteAddr, cfg.remoteHost)
	}
	errc := make(chan error, len(servers))
	for _, srv := range servers {
		go func(srv *http.Server) { errc <- srv.ListenAndServe() }(srv)
	}
	// If either listener fails, shut the whole server down.
	err = <-errc
	stop()
	for range servers[1:] {
		<-errc
	}
	store.persist()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
