package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var hookEvents = []string{
	"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse",
	"PermissionRequest", "Notification", "Stop", "SubagentStart", "SubagentStop", "PreCompact", "SessionEnd",
}

// runHook adds the PID / TTY of the claude process to the hook JSON read from
// stdin and sends it to the server. It never writes to stdout, so the output is
// not interpreted as a decision (e.g. for PreToolUse).
func runHook(addr string) {
	defer func() { recover() }()

	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	if err != nil || len(raw) == 0 {
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	if pid := findClaudePID(); pid > 0 {
		payload["_pid"] = pid
		payload["_tty"] = ttyOf(pid)
	}
	// When running inside tmux, attach the pane info so the dashboard can send input.
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		payload["_tmux_pane"] = pane
		if t := os.Getenv("TMUX"); t != "" {
			payload["_tmux_socket"], _, _ = strings.Cut(t, ",")
		}
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/hook", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tonberry-Token", readToken())
	client := &http.Client{Timeout: 700 * time.Millisecond}
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	}
}

// findClaudePID walks up the parent processes to find the claude process (uses Linux /proc).
func findClaudePID() int {
	pid := os.Getppid()
	for i := 0; i < 12 && pid > 1; i++ {
		comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if strings.TrimSpace(string(comm)) == "claude" {
			return pid
		}
		if cmd, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
			args := strings.Split(string(cmd), "\x00")
			for _, a := range args[:min(2, len(args))] {
				b := filepath.Base(a)
				if b == "claude" || strings.Contains(a, "@anthropic-ai/claude-code") {
					return pid
				}
			}
		}
		pid = parentOf(pid)
	}
	return 0
}

func parentOf(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// comm may contain spaces or parentheses, so parse after the last ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0
	}
	p, _ := strconv.Atoi(f[1])
	return p
}

func ttyOf(pid int) string {
	for _, fd := range []string{"0", "1", "2"} {
		if l, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd)); err == nil && (strings.HasPrefix(l, "/dev/pts/") || strings.HasPrefix(l, "/dev/tty")) {
			return strings.TrimPrefix(l, "/dev/")
		}
	}
	return ""
}

func printHooksConfig() {
	exe, err := os.Executable()
	if err != nil {
		exe = "tonberry-king"
	}
	entry := []map[string]any{{
		"hooks": []map[string]any{{"type": "command", "command": exe + " hook", "timeout": 5}},
	}}
	hooks := map[string]any{}
	for _, e := range hookEvents {
		hooks[e] = entry
	}
	out, _ := json.MarshalIndent(map[string]any{"hooks": hooks}, "", "  ")
	fmt.Println(string(out))
}
