package main

import "testing"

func TestParseSuggestion(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"suggestion", "\x1b[38;5;241m❯ \x1b[38;5;16mold prompt\x1b[39m\n\x1b[39m❯ \x1b[2mcreate the PR\x1b[0m\n\x1b[38;5;245m────", "create the PR"},
		{"empty input", "\x1b[38;5;241m❯ \x1b[39m\n────", ""},
		{"typed text", "❯ hello world\n────", ""},
		{"no prompt", "just output\n", ""},
	}
	for _, tt := range tests {
		if got := parseSuggestion(tt.in); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}
