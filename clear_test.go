package main

import (
	"path/filepath"
	"testing"
)

// /clear sends SessionEnd for the old session_id and then SessionStart with a
// new session_id from the same process. The dashboard should keep one session.
func TestClearHandsOverSession(t *testing.T) {
	s := newStore(filepath.Join(t.TempDir(), "sessions.json"))
	base := func(id, ev string) map[string]any {
		return map[string]any{
			"session_id": id, "hook_event_name": ev, "cwd": "/tmp/x",
			"_pid": float64(4242), "_tmux_pane": "%1", "_tmux_socket": "/tmp/tmux-1000/default",
		}
	}

	s.apply(base("old", "SessionStart"))
	s.sessions["old"].Launched = true
	s.sessions["old"].TmuxSession = "work"

	end := base("old", "SessionEnd")
	end["reason"] = "clear"
	s.apply(end)
	start := base("new", "SessionStart")
	start["source"] = "clear"
	s.apply(start)

	if _, ok := s.sessions["old"]; ok {
		t.Fatalf("old session should be removed after /clear")
	}
	n, ok := s.sessions["new"]
	if !ok {
		t.Fatalf("new session missing")
	}
	if n.Status != StatusInput || !n.Launched || n.TmuxSession != "work" {
		t.Fatalf("unexpected new session: status=%s launched=%v tmux=%q", n.Status, n.Launched, n.TmuxSession)
	}
	if len(s.sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(s.sessions))
	}
}

// A new session_id in the same process without /clear still ends the old one.
func TestNewSessionInSameProcessEndsOld(t *testing.T) {
	s := newStore(filepath.Join(t.TempDir(), "sessions.json"))
	s.apply(map[string]any{"session_id": "old", "hook_event_name": "SessionStart", "_pid": float64(7)})
	s.apply(map[string]any{"session_id": "new", "hook_event_name": "SessionStart", "source": "resume", "_pid": float64(7)})
	if o := s.sessions["old"]; o == nil || o.Status != StatusEnded {
		t.Fatalf("old session should be ended")
	}
}
