package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// A long last_assistant_message must be kept in full, not cut at a few thousand characters.
func TestLongLastAssistantIsKept(t *testing.T) {
	s := newStore(filepath.Join(t.TempDir(), "sessions.json"))
	msg := strings.Repeat("あいうえお\n", 1000) + "END"
	s.apply(map[string]any{"session_id": "a", "hook_event_name": "SessionStart", "cwd": "/tmp/x"})
	s.apply(map[string]any{"session_id": "a", "hook_event_name": "Stop", "cwd": "/tmp/x", "last_assistant_message": msg})
	if got := s.sessions["a"].LastAssistant; got != msg {
		t.Fatalf("last response was cut: len=%d want %d, tail=%q", len([]rune(got)), len([]rune(msg)), got[max(0, len(got)-20):])
	}
}
