package main

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestPromptStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompts.json")
	ps := newPromptStore(path)

	for i := 0; i < maxRecentPrompts+5; i++ {
		ps.addRecent(fmt.Sprintf("p%d", i))
	}
	ps.addRecent("p3") // moves to the front without duplicating
	ps.addRecent("  ") // ignored
	d := ps.snapshot()
	if len(d.Recent) != maxRecentPrompts || d.Recent[0] != "p3" || d.Recent[1] != fmt.Sprintf("p%d", maxRecentPrompts+4) {
		t.Fatalf("recent = %v", d.Recent)
	}

	if err := ps.putTemplate(Template{Name: "review", Text: "review the diff"}); err != nil {
		t.Fatal(err)
	}
	ps.putTemplate(Template{Name: "review", Text: "review it again"})
	ps.putTemplate(Template{Name: "test", Text: "run tests"})
	if err := ps.putTemplate(Template{Name: "", Text: "x"}); err == nil {
		t.Fatal("empty name accepted")
	}
	ps.removeRecent("p3")

	// Reload from disk.
	d = newPromptStore(path).snapshot()
	if len(d.Templates) != 2 || d.Templates[0].Text != "review it again" {
		t.Fatalf("templates = %v", d.Templates)
	}
	if d.Recent[0] == "p3" {
		t.Fatalf("p3 not removed: %v", d.Recent)
	}
	ps.deleteTemplate("review")
	if d = ps.snapshot(); len(d.Templates) != 1 || d.Templates[0].Name != "test" {
		t.Fatalf("templates after delete = %v", d.Templates)
	}

	ps.addDir("/a")
	ps.addDir("/b")
	ps.addDir("/a")
	ps.addDir("")
	if d = newPromptStore(path).snapshot(); len(d.Dirs) != 2 || d.Dirs[0] != "/a" || d.Dirs[1] != "/b" {
		t.Fatalf("dirs = %v", d.Dirs)
	}
	ps.removeDir("/a")
	if d = ps.snapshot(); len(d.Dirs) != 1 || d.Dirs[0] != "/b" {
		t.Fatalf("dirs after delete = %v", d.Dirs)
	}
}
