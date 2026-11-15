package checkpoint

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func setupTestGitRepo(t *testing.T) string {
	tmpDir, err := os.MkdirTemp("", "monoflow_chk_test_*")
	if err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v, out: %s", args, err, string(out))
		}
	}

	run("init")
	run("config", "user.name", "Test Agent")
	run("config", "user.email", "agent@monoflow.dev")

	// Initial commit
	initFile := filepath.Join(tmpDir, "hello.txt")
	os.WriteFile(initFile, []byte("initial state\n"), 0644)
	run("add", "hello.txt")
	run("commit", "-m", "initial commit")

	return tmpDir
}

func TestCheckpointSaveAndRevert(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	defer os.RemoveAll(repoDir)

	mgr := NewManager(repoDir)
	filePath := filepath.Join(repoDir, "hello.txt")

	// 1. First edit: Valid feature state
	err := os.WriteFile(filePath, []byte("feature state 1\n"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	chk1, err := mgr.Save("good feature 1")
	if err != nil {
		t.Fatalf("failed to save checkpoint: %v", err)
	}
	if chk1.ID != "chk_001" {
		t.Errorf("expected chk_001, got %s", chk1.ID)
	}

	// 2. Second edit: Broken / experimental state
	err = os.WriteFile(filePath, []byte("broken experimental syntax\n"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// Verify file has broken content
	content, _ := os.ReadFile(filePath)
	if string(content) != "broken experimental syntax\n" {
		t.Fatalf("unexpected content before revert: %s", string(content))
	}

	// 3. Revert to chk_001
	err = mgr.Revert("chk_001")
	if err != nil {
		t.Fatalf("failed to revert checkpoint: %v", err)
	}

	// 4. Verify file is restored to feature state 1!
	revertedContent, _ := os.ReadFile(filePath)
	if string(revertedContent) != "feature state 1\n" {
		t.Fatalf("expected 'feature state 1\\n', got '%s'", string(revertedContent))
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

// #33: agent A checkpoints, agent B adds an untracked file and stages another, A reverts:
// B's files are still there, and nothing of A's tree is changed by the refused revert.
func TestRevertKeepsOtherAgentsFiles(t *testing.T) {
	repo := setupTestGitRepo(t)
	defer os.RemoveAll(repo)
	mgr := NewManager(repo)
	hello := filepath.Join(repo, "hello.txt")

	os.WriteFile(hello, []byte("A good\n"), 0644)
	if _, err := mgr.Save("A good state"); err != nil {
		t.Fatal(err)
	}
	if out := gitIn(t, repo, "diff", "--cached", "--name-only"); out != "" {
		t.Fatalf("save must not stage anything, staged: %s", out)
	}
	os.WriteFile(hello, []byte("A broken\n"), 0644)

	// Agent B
	os.WriteFile(filepath.Join(repo, "b_new.txt"), []byte("B untracked\n"), 0644)
	os.WriteFile(filepath.Join(repo, "b_staged.txt"), []byte("B staged\n"), 0644)
	gitIn(t, repo, "add", "b_staged.txt")

	_, err := mgr.RevertCheckpoint("", false)
	var foreign *ForeignChangesError
	if !errors.As(err, &foreign) {
		t.Fatalf("want ForeignChangesError, got %v", err)
	}
	if strings.Join(foreign.Untracked, ",") != "b_new.txt" || strings.Join(foreign.Staged, ",") != "b_staged.txt" {
		t.Fatalf("listed %v / %v", foreign.Untracked, foreign.Staged)
	}
	for f, want := range map[string]string{"b_new.txt": "B untracked\n", "b_staged.txt": "B staged\n", "hello.txt": "A broken\n"} {
		if got, _ := os.ReadFile(filepath.Join(repo, f)); string(got) != want {
			t.Errorf("%s = %q, want %q (a refused revert changes nothing)", f, got, want)
		}
	}
	if out := gitIn(t, repo, "diff", "--cached", "--name-only"); strings.TrimSpace(out) != "b_staged.txt" {
		t.Errorf("index changed: %q", out)
	}

	// Once B's files are out of the way (committed by B, say) A's revert goes through.
	gitIn(t, repo, "add", "b_new.txt")
	gitIn(t, repo, "commit", "-m", "B's work", "--", "b_new.txt", "b_staged.txt")
	if _, err := mgr.RevertCheckpoint("", false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(hello); string(got) != "A good\n" {
		t.Fatalf("hello.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "b_new.txt")); err != nil {
		t.Fatal("B's committed file was removed")
	}
}

// A single agent's flow still works: files edited, created and deleted after the checkpoint
// are restored; a new file the checkpoint had is brought back.
func TestRevertRestoresOwnChanges(t *testing.T) {
	repo := setupTestGitRepo(t)
	defer os.RemoveAll(repo)
	mgr := NewManager(repo)
	os.WriteFile(filepath.Join(repo, "mine.txt"), []byte("new in checkpoint\n"), 0644)
	if _, err := mgr.Save("before experiment"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("experiment\n"), 0644)
	os.Remove(filepath.Join(repo, "mine.txt"))
	if _, err := mgr.RevertCheckpoint("", false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "hello.txt")); string(got) != "initial state\n" {
		t.Errorf("hello.txt = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "mine.txt")); string(got) != "new in checkpoint\n" {
		t.Errorf("mine.txt = %q", got)
	}

	// A new file made after the checkpoint is someone's work: refused, unless --force.
	os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("x"), 0644)
	if _, err := mgr.RevertCheckpoint("", false); err == nil {
		t.Fatal("want refusal for scratch.txt")
	}
	if _, err := mgr.RevertCheckpoint("", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "scratch.txt")); !os.IsNotExist(err) {
		t.Error("--force must discard scratch.txt")
	}
}

// Review findings: sparse checkouts, staging before the save, odd file names, autocrlf warnings.
func TestRevertEdgeCases(t *testing.T) {
	t.Run("sparse", func(t *testing.T) {
		repo := setupTestGitRepo(t)
		defer os.RemoveAll(repo)
		os.MkdirAll(filepath.Join(repo, "in"), 0755)
		os.MkdirAll(filepath.Join(repo, "out"), 0755)
		os.WriteFile(filepath.Join(repo, "in", "x.txt"), []byte("1"), 0644)
		os.WriteFile(filepath.Join(repo, "out", "o.txt"), []byte("o"), 0644)
		gitIn(t, repo, "add", "-A")
		gitIn(t, repo, "commit", "-qm", "two dirs")
		gitIn(t, repo, "sparse-checkout", "set", "in")
		mgr := NewManager(repo)
		os.WriteFile(filepath.Join(repo, "in", "x.txt"), []byte("2"), 0644)
		if _, err := mgr.Save("s"); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(repo, "in", "x.txt"), []byte("3"), 0644)
		if _, err := mgr.RevertCheckpoint("", false); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(filepath.Join(repo, "in", "x.txt")); string(got) != "2" {
			t.Errorf("x.txt = %q", got)
		}
		if _, err := os.Stat(filepath.Join(repo, "out", "o.txt")); err == nil {
			t.Error("revert materialized a path outside the sparse checkout")
		}
	})
	t.Run("staged before save", func(t *testing.T) {
		repo := setupTestGitRepo(t)
		defer os.RemoveAll(repo)
		mgr := NewManager(repo)
		os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("v1\n"), 0644)
		gitIn(t, repo, "add", "hello.txt")
		os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("v2\n"), 0644)
		if _, err := mgr.Save("s"); err != nil {
			t.Fatal(err)
		}
		if _, err := mgr.RevertCheckpoint("", false); err != nil {
			t.Fatalf("staging done before the checkpoint must not block its revert: %v", err)
		}
	})
	t.Run("odd names and autocrlf", func(t *testing.T) {
		repo := setupTestGitRepo(t)
		defer os.RemoveAll(repo)
		gitIn(t, repo, "config", "core.autocrlf", "true")
		for _, f := range []string{" lead.txt", "a.txt", "b*.txt"} {
			os.WriteFile(filepath.Join(repo, f), []byte("orig\n"), 0644)
		}
		gitIn(t, repo, "add", "-A")
		gitIn(t, repo, "commit", "-qm", "odd")
		mgr := NewManager(repo)
		if _, err := mgr.Save("s"); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{" lead.txt", "a.txt", "b*.txt"} {
			os.WriteFile(filepath.Join(repo, f), []byte("changed\r\n"), 0644)
		}
		if _, err := mgr.RevertCheckpoint("", false); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{" lead.txt", "a.txt", "b*.txt"} {
			if got, _ := os.ReadFile(filepath.Join(repo, f)); strings.Contains(string(got), "changed") {
				t.Errorf("%q not reverted: %q", f, got)
			}
		}
	})
	t.Run("shared: another agent's edit to a clean file", func(t *testing.T) {
		repo := setupTestGitRepo(t)
		defer os.RemoveAll(repo)
		os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b\n"), 0644)
		gitIn(t, repo, "add", "b.txt")
		gitIn(t, repo, "commit", "-qm", "b")
		mgr := NewManager(repo)
		mgr.Shared = true
		os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("A\n"), 0644)
		if _, err := mgr.Save("s"); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("A broken\n"), 0644)
		os.WriteFile(filepath.Join(repo, "b.txt"), []byte("B's edit\n"), 0644)
		_, err := mgr.RevertCheckpoint("", false)
		var foreign *ForeignChangesError
		if !errors.As(err, &foreign) || strings.Join(foreign.Modified, ",") != "b.txt" {
			t.Fatalf("want b.txt refused, got %v", err)
		}
		if got, _ := os.ReadFile(filepath.Join(repo, "b.txt")); string(got) != "B's edit\n" {
			t.Errorf("b.txt = %q", got)
		}
	})
}
