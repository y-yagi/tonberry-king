package main

import (
	"path/filepath"
	"testing"
)

// The hook sequence below mirrors what Claude Code sends when the main agent
// launches a background subagent and ends its turn before the subagent finishes.
func TestBackgroundSubagentStatus(t *testing.T) {
	s := newStore(filepath.Join(t.TempDir(), "sessions.json"))
	const id = "sess"
	sub := map[string]any{"id": "a1", "type": "subagent", "status": "running", "description": "Run echo hi", "agent_type": "general-purpose"}
	shell := map[string]any{"id": "b1", "type": "shell", "status": "running", "description": "npm run dev"}

	steps := []struct {
		payload map[string]any
		want    string
	}{
		{map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "go"}, StatusRunning},
		{map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Agent"}, StatusRunning},
		{map[string]any{"hook_event_name": "SubagentStart", "agent_id": "a1", "agent_type": "general-purpose"}, StatusRunning},
		{map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Agent"}, StatusRunning},
		{map[string]any{"hook_event_name": "Stop", "background_tasks": []any{sub, shell}}, StatusSubagents},
		// Tool use inside the subagent must not look like the main agent running.
		{map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "agent_id": "a1"}, StatusSubagents},
		{map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "agent_id": "a1"}, StatusSubagents},
		{map[string]any{"hook_event_name": "Notification", "notification_type": "idle_prompt", "message": "Claude is waiting for your input"}, StatusSubagents},
		{map[string]any{"hook_event_name": "SubagentStop", "agent_id": "a1"}, StatusSubagents},
		// The main agent is woken up with the result and finishes its turn.
		{map[string]any{"hook_event_name": "Stop", "background_tasks": []any{shell}}, StatusInput},
	}
	for i, st := range steps {
		st.payload["session_id"] = id
		s.apply(st.payload)
		if got := s.sessions[id].Status; got != st.want {
			t.Fatalf("step %d (%s): status = %q, want %q", i, st.payload["hook_event_name"], got, st.want)
		}
	}
	if n := len(s.sessions[id].Subagents); n != 0 {
		t.Fatalf("subagents left = %d, want 0", n)
	}
}

func TestStopWithoutBackgroundTasks(t *testing.T) {
	s := newStore(filepath.Join(t.TempDir(), "sessions.json"))
	for _, ev := range []string{"UserPromptSubmit", "Stop"} {
		s.apply(map[string]any{"session_id": "x", "hook_event_name": ev})
	}
	if got := s.sessions["x"].Status; got != StatusInput {
		t.Fatalf("status = %q, want %q", got, StatusInput)
	}
}
