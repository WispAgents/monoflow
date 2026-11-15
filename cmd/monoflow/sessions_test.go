package main

import (
	"os"
	"path/filepath"
	"testing"
)

// #41: two agent sessions in one checkout turn background speculation off by default;
// "speculate": true in the config turns it back on.
func TestSpeculationOffWhenShared(t *testing.T) {
	t.Setenv("MONOFLOW_NO_SPECULATE", "")
	t.Setenv("MONOFLOW_SHARED_WORKTREE", "")
	root := t.TempDir()
	recordHookSession(root, []byte(`{"session_id": "a"}`))
	if speculationOff(root) {
		t.Fatal("one session: speculation stays on")
	}
	recordHookSession(root, []byte(`{"session_id": "b/../x"}`))
	if n := activeSessions(root); n != 2 {
		t.Fatalf("sessions = %d", n)
	}
	if !speculationOff(root) {
		t.Fatal("two sessions: speculation off")
	}
	os.MkdirAll(filepath.Join(root, ".agents"), 0o755)
	os.WriteFile(filepath.Join(root, ".agents", "monoflow.config.json"), []byte(`{"speculate": true}`), 0o644)
	if speculationOff(root) {
		t.Fatal(`"speculate": true wins`)
	}
	os.WriteFile(filepath.Join(root, ".agents", "monoflow.config.json"), []byte(`{"shared_worktree": true, "speculate": false}`), 0o644)
	if !speculationOff(root) {
		t.Fatal(`"speculate": false`)
	}
	// Another checkout's sessions do not count.
	if activeSessions(t.TempDir()) != 0 {
		t.Fatal("sessions leak across checkouts")
	}
}
