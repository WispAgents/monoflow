package initialize

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRepo(t *testing.T) string {
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	return dir
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// #34: --minimal sets up the graph and verify only.
func TestInitMinimal(t *testing.T) {
	dir := gitRepo(t)
	res, err := InitRepoWith(dir, "", Options{With: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Installed) != 0 {
		t.Errorf("installed %v", res.Installed)
	}
	if !exists(filepath.Join(dir, ".agents", "monoflow.config.json")) {
		t.Error("config missing")
	}
	for _, p := range []string{"AGENTS.md", "CLAUDE.md", ".claude/settings.json", ".opencode", ".agents/bin/git", ".git/hooks/pre-commit", ".git/hooks/pre-push"} {
		if exists(filepath.Join(dir, p)) {
			t.Errorf("%s installed by --minimal", p)
		}
	}
}

func TestInitWithOnlyHooks(t *testing.T) {
	dir := gitRepo(t)
	with, err := ParseComponents("hooks")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitRepoWith(dir, "", Options{With: with}); err != nil {
		t.Fatal(err)
	}
	if !HasShieldHooks(dir) {
		t.Error("hooks not installed")
	}
	if exists(filepath.Join(dir, "AGENTS.md")) || exists(filepath.Join(dir, ".agents", "bin", "git")) {
		t.Error("only hooks were asked for")
	}
	if _, err := ParseComponents("hooks,bogus"); err == nil {
		t.Error("bogus component accepted")
	}
}

// uninit removes everything init installed and keeps the project's own content.
func TestUninitRoundTrip(t *testing.T) {
	dir := gitRepo(t)
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Project rules\nbe nice\n"), 0644)
	os.MkdirAll(filepath.Join(dir, ".claude"), 0755)
	os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(`{"model": "x"}`), 0644)
	if _, err := InitRepo(dir, ""); err != nil {
		t.Fatal(err)
	}
	if !HasShieldHooks(dir) {
		t.Fatal("init did not install hooks")
	}
	res, err := Uninit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if HasShieldHooks(dir) || exists(filepath.Join(dir, ".agents", "bin", "git")) || exists(filepath.Join(dir, ".opencode", "plugin", "monoflow.js")) {
		t.Errorf("left behind; removed %v", res.Removed)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md")); strings.Contains(string(data), "monoflow:start") || !strings.Contains(string(data), "be nice") {
		t.Errorf("AGENTS.md = %q", data)
	}
	if exists(filepath.Join(dir, "CLAUDE.md")) {
		t.Error("CLAUDE.md was created by init only: should be gone")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json")); strings.Contains(string(data), "hook") || !strings.Contains(string(data), `"model"`) {
		t.Errorf("settings.json = %s", data)
	}
	if !exists(filepath.Join(dir, ".agents", "monoflow.config.json")) {
		t.Error("config must stay")
	}
}
