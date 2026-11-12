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
	"PermissionRequest", "Notification", "Stop", "SubagentStop", "PreCompact", "SessionEnd",
}

// runHook は stdin の hook JSON に claude プロセスの PID / TTY を付与してサーバへ送る。
// stdout には何も出さない（PreToolUse などで判定結果と解釈されないように）。
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
	// tmux 内で動いていれば、Web から入力を送れるようにペイン情報を付与する
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

// findClaudePID は親プロセスを遡って claude 本体の PID を探す（Linux の /proc 前提）。
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
	// comm に空白や括弧が含まれうるので最後の ')' 以降をパースする
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
