package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUserRequests(t *testing.T) {
	lines := []string{
		`{"type":"user","message":{"content":"task: build it"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"out"}]}}`,
		`{"type":"user","message":{"content":[{"type":"text","text":"two"}]}}`,
		`{"type":"user","message":{"content":"three"}}`,
		`{"type":"user","message":{"content":"four"}}`,
		`{"type":"user","message":{"content":"five"}}`,
		`not json`,
	}
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	got := userRequests(p)
	want := "[1] task: build it\n[2] three\n[3] four\n[4] five\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if userRequests(filepath.Join(t.TempDir(), "missing")) != "" {
		t.Error("missing transcript gave text")
	}
}
