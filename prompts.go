package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	maxRecentPrompts = 20
	maxRecentDirs    = 20
)

// Template is a named initial instruction saved from the launch dialog.
type Template struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

type promptData struct {
	Templates []Template `json:"templates"`
	Recent    []string   `json:"recent"`      // most recent first
	Dirs      []string   `json:"recent_dirs"` // launch directories, most recent first
}

// promptStore keeps instruction templates and recently used initial
// instructions so they can be reused when launching claude.
type promptStore struct {
	path string
	mu   sync.Mutex
	data promptData
}

func newPromptStore(path string) *promptStore {
	ps := &promptStore{path: path}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &ps.data)
	}
	return ps
}

func (ps *promptStore) snapshot() promptData {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	d := promptData{
		Templates: append([]Template{}, ps.data.Templates...),
		Recent:    append([]string{}, ps.data.Recent...),
		Dirs:      append([]string{}, ps.data.Dirs...),
	}
	return d
}

// save writes the data atomically. Caller holds mu.
func (ps *promptStore) save() error {
	b, err := json.MarshalIndent(ps.data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ps.path), 0o700); err != nil {
		return err
	}
	tmp := ps.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ps.path)
}

// pushFront moves text to the front of list, dropping duplicates and
// keeping at most max entries.
func pushFront(list []string, text string, max int) []string {
	out := []string{text}
	for _, r := range list {
		if r != text && len(out) < max {
			out = append(out, r)
		}
	}
	return out
}

func without(list []string, text string) []string {
	out := list[:0]
	for _, r := range list {
		if r != text {
			out = append(out, r)
		}
	}
	return out
}

func (ps *promptStore) addRecent(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.data.Recent = pushFront(ps.data.Recent, text, maxRecentPrompts)
	return ps.save()
}

func (ps *promptStore) removeRecent(text string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.data.Recent = without(ps.data.Recent, text)
	return ps.save()
}

// addDir records a directory claude was launched in.
func (ps *promptStore) addDir(dir string) error {
	if dir == "" {
		return nil
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.data.Dirs = pushFront(ps.data.Dirs, dir, maxRecentDirs)
	return ps.save()
}

func (ps *promptStore) removeDir(dir string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.data.Dirs = without(ps.data.Dirs, dir)
	return ps.save()
}

// putTemplate adds a template or replaces the one with the same name.
func (ps *promptStore) putTemplate(t Template) error {
	t.Name = strings.TrimSpace(t.Name)
	t.Text = strings.TrimSpace(t.Text)
	if t.Name == "" || t.Text == "" {
		return errors.New("template name and text are required")
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for i, old := range ps.data.Templates {
		if old.Name == t.Name {
			ps.data.Templates[i] = t
			return ps.save()
		}
	}
	ps.data.Templates = append(ps.data.Templates, t)
	return ps.save()
}

func (ps *promptStore) deleteTemplate(name string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ts := ps.data.Templates[:0]
	for _, t := range ps.data.Templates {
		if t.Name != name {
			ts = append(ts, t)
		}
	}
	ps.data.Templates = ts
	return ps.save()
}

func (ps *promptStore) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/prompts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, ps.snapshot())
	})
	mux.HandleFunc("PUT /api/prompts/templates", func(w http.ResponseWriter, r *http.Request) {
		var t Template
		if err := readJSON(r, &t); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		if err := ps.putTemplate(t); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, ps.snapshot())
	})
	mux.HandleFunc("POST /api/prompts/templates/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := readJSON(r, &req); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		if err := ps.deleteTemplate(req.Name); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, ps.snapshot())
	})
	mux.HandleFunc("POST /api/prompts/recent/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text string `json:"text"`
		}
		if err := readJSON(r, &req); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		if err := ps.removeRecent(req.Text); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, ps.snapshot())
	})
	mux.HandleFunc("POST /api/prompts/dirs/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Dir string `json:"dir"`
		}
		if err := readJSON(r, &req); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		if err := ps.removeDir(req.Dir); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, ps.snapshot())
	})
}
