package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/antigravity/monoflow/pkg/graph"
	"github.com/antigravity/monoflow/pkg/runtimestate"
)

// Agent sessions seen in a checkout, from the session id in harness hook payloads. More than
// one recent session in the same checkout means agents share it (wisp runs several agents in
// one worktree), and background speculation there checks half-done edits nobody trusts (#41).

const sessionWindow = 15 * time.Minute

func sessionsDir(root string) string {
	// Keyed by the checkout: .agents/hooks may be shared with the main checkout's (worktrees
	// symlink runtime state), and other worktrees' agents do not share this one.
	return filepath.Join(root, ".agents", "hooks", "sessions", runtimestate.HashBytes([]byte(root))[:12])
}

var unsafeID = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// recordHookSession notes the session of a hook payload ({"session_id": ...}).
func recordHookSession(root string, payload []byte) {
	var p struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(payload, &p) != nil || strings.TrimSpace(p.SessionID) == "" {
		return
	}
	dir := sessionsDir(root)
	_ = os.MkdirAll(dir, 0o755)
	f := filepath.Join(dir, unsafeID.ReplaceAllString(p.SessionID, "_"))
	now := time.Now()
	if os.Chtimes(f, now, now) != nil {
		_ = os.WriteFile(f, nil, 0o644)
	}
}

// activeSessions counts the sessions that used this checkout within the window.
func activeSessions(root string) int {
	entries, err := os.ReadDir(sessionsDir(root))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if age := time.Since(info.ModTime()); age < sessionWindow {
			n++
		} else if age > 24*time.Hour {
			_ = os.Remove(filepath.Join(sessionsDir(root), e.Name())) // long gone
		}
	}
	return n
}

// speculationOff reports whether background verify is off for this checkout: by environment,
// by the "speculate" setting, or (when that is unset) because the checkout is shared.
func speculationOff(root string) bool {
	if os.Getenv("MONOFLOW_NO_SPECULATE") != "" || inCI() {
		return true // nobody edits a CI checkout: nothing to precompute
	}
	s := graph.LoadSettings(root)
	if s.Speculate != nil {
		return !*s.Speculate
	}
	return s.SharedWorktree || activeSessions(root) > 1
}

// refuseInSharedWorktree stops an action that rewrites files other agents may own, in a
// checkout declared shared (shared_worktree / MONOFLOW_SHARED_WORKTREE).
func refuseInSharedWorktree(root, action string) {
	if !graph.LoadSettings(root).SharedWorktree {
		return
	}
	printOut(map[string]any{
		"status": "refused",
		"error":  "'" + action + "' rewrites files other agents may own, and this checkout is shared (shared_worktree in .agents/monoflow.config.json or MONOFLOW_SHARED_WORKTREE)",
		"hint":   "restore only your own files, or run it in a worktree of your own (monoflow start <id>)",
	})
	exit(1)
}
