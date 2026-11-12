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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// tmux で使える送信キー（任意キー送信は許可しない）
var allowedKeys = map[string]bool{
	"Enter": true, "Escape": true, "Up": true, "Down": true, "Left": true, "Right": true,
	"Tab": true, "BTab": true, "C-c": true, "1": true, "2": true, "3": true,
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func tmuxOutput(sock string, stdin []byte, args ...string) (string, error) {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return "", errors.New("tmux が見つかりません（sudo apt install tmux）")
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
}

func (c *controller) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/launch", c.handleLaunch)
	mux.HandleFunc("POST /api/sessions/{id}/send", c.handleSend)
	mux.HandleFunc("POST /api/sessions/{id}/keys", c.handleKeys)
	mux.HandleFunc("POST /api/sessions/{id}/stop", c.handleStop)
	mux.HandleFunc("GET /api/sessions/{id}/screen", c.handleScreen)
	mux.HandleFunc("POST /api/sessions/{id}/resize", c.handleResize)
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
		return "", errors.New("ディレクトリは絶対パス（または ~/...）で指定してください")
	}
	d = filepath.Clean(d)
	st, err := os.Stat(d)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("ディレクトリが存在しません: %s", d)
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
		fail(w, http.StatusInternalServerError, fmt.Errorf("claude が見つかりません: %v", err))
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
	writeJSON(w, http.StatusOK, map[string]string{"id": sess.ID, "tmux_session": name})
}

func (c *controller) target(w http.ResponseWriter, r *http.Request) (Session, bool) {
	sess, ok := c.store.get(r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, errors.New("セッションが見つかりません"))
		return sess, false
	}
	return sess, true
}

func (c *controller) requirePane(w http.ResponseWriter, sess Session) bool {
	if sess.TmuxPane == "" {
		fail(w, http.StatusConflict, errors.New("tmux 外で起動されたセッションには入力を送れません"))
		return false
	}
	if sess.Status == StatusEnded {
		fail(w, http.StatusConflict, errors.New("セッションは終了しています"))
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
		// bracketed paste で貼り付ける（改行で途中送信されないように）
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
	c.store.logEvent(sess.ID, "WebSend", truncate(text, 120))
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
		fail(w, http.StatusBadRequest, errors.New("keys は 1〜10 個で指定してください"))
		return
	}
	for _, k := range req.Keys {
		if !allowedKeys[k] {
			fail(w, http.StatusBadRequest, fmt.Errorf("送信できないキーです: %s", k))
			return
		}
	}
	args := append([]string{"send-keys", "-t", sess.TmuxPane}, req.Keys...)
	if _, err := tmuxOutput(sess.TmuxSocket, nil, args...); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	c.store.logEvent(sess.ID, "WebKeys", strings.Join(req.Keys, " "))
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
		c.store.logEvent(sess.ID, "WebInterrupt", "")
	case "exit":
		switch {
		case sess.PID > 0 && isClaudeProcess(sess.PID):
			if err := syscall.Kill(sess.PID, syscall.SIGTERM); err != nil {
				fail(w, http.StatusInternalServerError, err)
				return
			}
			go func(pid int) { // 一定時間で終わらなければ強制終了
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
			fail(w, http.StatusConflict, errors.New("停止できるプロセスが見つかりません"))
			return
		}
		c.store.logEvent(sess.ID, "WebExit", "")
	default:
		fail(w, http.StatusBadRequest, errors.New("mode は interrupt か exit を指定してください"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *controller) handleScreen(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.target(w, r)
	if !ok || !c.requirePane(w, sess) {
		return
	}
	out, err := tmuxOutput(sess.TmuxSocket, nil, "capture-pane", "-p", "-t", sess.TmuxPane)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if size, err := tmuxOutput(sess.TmuxSocket, nil, "display-message", "-p", "-t", sess.TmuxPane, "#{pane_width}x#{pane_height}"); err == nil {
		w.Header().Set("X-Pane-Size", strings.TrimSpace(size))
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, strings.TrimRight(out, "\n")+"\n")
}

// handleResize は tmux ウィンドウの桁数を変える。auto=true なら接続中の端末サイズに合わせる設定へ戻す。
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
		fail(w, http.StatusBadRequest, errors.New("cols は 40〜500 で指定してください"))
		return
	}
	if _, err := tmuxOutput(sess.TmuxSocket, nil, args...); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
