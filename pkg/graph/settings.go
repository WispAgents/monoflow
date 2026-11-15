package graph

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Settings are the repo-level switches of .agents/monoflow.config.json besides the packages.
// They are read on their own (no discovery), so hot paths such as hooks can afford them.
type Settings struct {
	// SharedWorktree declares that several agents edit this checkout at once. Commands that
	// rewrite files they did not write refuse there (checkpoint revert --force, ship moving the
	// tree into a worktree, outer --revert), and background speculation is off unless Speculate
	// is set. MONOFLOW_SHARED_WORKTREE=1 sets it too.
	SharedWorktree bool `json:"shared_worktree,omitempty"`
	// Speculate turns background verify on (true) or off (false). Unset: on, except in a shared
	// worktree or when more than one agent session used this checkout in the last 15 minutes.
	Speculate *bool `json:"speculate,omitempty"`
	// GlobalInputs are files (glob patterns, ** matches any number of directories) that affect
	// every package: the root toolchain, lockfiles, CI workflows. They add to the built-in list.
	GlobalInputs []string `json:"global_inputs,omitempty"`
	// IgnoreInputs are files that affect nothing, on purpose (docs, design files).
	IgnoreInputs []string `json:"ignore_inputs,omitempty"`
	// Unmapped is what a changed file that no package owns (and that no list above names)
	// affects: "nothing" (the default; it is reported as unmapped) or "all".
	Unmapped string `json:"unmapped,omitempty"`
}

// LoadSettings reads the settings of the repository at root.
func LoadSettings(root string) Settings {
	var s Settings
	if data, err := os.ReadFile(filepath.Join(root, ".agents", "monoflow.config.json")); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	if v := os.Getenv("MONOFLOW_SHARED_WORKTREE"); v == "1" || v == "true" {
		s.SharedWorktree = true
	}
	return s
}

// MatchInput reports whether the repo-relative file matches one of the glob patterns.
// "dir/**" matches everything under dir, "**/x" x in any directory, a wildcard pattern without a
// slash ("*.md") the base name in any directory; anything else the whole path ("Cargo.toml" is
// the root one only: crates/a/Cargo.toml belongs to its crate).
func MatchInput(patterns []string, file string) bool {
	file = strings.TrimPrefix(filepath.ToSlash(file), "./")
	for _, p := range patterns {
		p = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(p)), "./")
		if p == "" {
			continue
		}
		if globMatch(p, file) {
			return true
		}
		if !strings.Contains(p, "/") && strings.ContainsAny(p, "*?[") {
			if ok, _ := path.Match(p, path.Base(file)); ok {
				return true
			}
		}
	}
	return false
}

// globMatch matches slash-separated segments; a "**" segment matches zero or more segments.
func globMatch(pattern, name string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegs(p, n []string) bool {
	for len(p) > 0 {
		if p[0] == "**" {
			if len(p) == 1 {
				return true
			}
			for i := 0; i <= len(n); i++ {
				if matchSegs(p[1:], n[i:]) {
					return true
				}
			}
			return false
		}
		if len(n) == 0 {
			return false
		}
		if ok, _ := path.Match(p[0], n[0]); !ok {
			return false
		}
		p, n = p[1:], n[1:]
	}
	return len(n) == 0
}
