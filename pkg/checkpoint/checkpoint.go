package checkpoint

import (
	"encoding/json"
	"fmt"
	"github.com/antigravity/monoflow/pkg/runtimestate"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Checkpoint represents an ephemeral snapshot of the working tree.
type Checkpoint struct {
	ID        string    `json:"id"`
	TreeHash  string    `json:"tree_hash"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
	Files     []string  `json:"files"`
	// Base is HEAD when the checkpoint was saved: a path the checkpoint lacks is only deleted on
	// revert when Base had it (the checkpoint deleted it); one added later is someone's work.
	Base string `json:"base,omitempty"`
	// IndexTree is the agent's real index at save time (staged changes are judged against it).
	IndexTree string `json:"index_tree,omitempty"`
}

// Manager handles ephemeral checkpoints and isolated worktrees.
type Manager struct {
	mu      sync.Mutex
	RootDir string
	// Shared: several agents edit this checkout (shared_worktree). Revert then also refuses on
	// files that were clean at save time and changed since, instead of restoring them.
	Shared      bool
	historyFile string
}

// NewManager creates a checkpoint manager for the repository.
func NewManager(rootDir string) *Manager {
	return &Manager{
		RootDir:     rootDir,
		historyFile: filepath.Join(rootDir, ".agents", "checkpoints.json"),
	}
}

func (m *Manager) runGit(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = m.RootDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s failed: %w (output: %s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// runGitZ runs a git command whose stdout is NUL-separated paths (-z) and returns them. Only
// stdout is parsed and nothing is trimmed: stderr warnings (core.autocrlf) folded into the first
// entry, and a trimmed leading space, made revert skip or miss files.
func (m *Manager) runGitZ(args ...string) ([]string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = m.RootDir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s failed: %w (output: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return splitZ(string(out)), nil
}

func (m *Manager) runGitEnv(env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = m.RootDir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s failed: %w (output: %s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Save snapshots the working tree (tracked and new, non-ignored files) as a git tree object,
// outside git history. It uses a private copy of the index, so the agent's staged state is
// left exactly as it was: staging everything into the shared index (as Save used to) made
// another agent's new files look like part of this agent's work.
func (m *Manager) Save(message string) (*Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	idx, cleanup, err := m.privateIndex()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + idx}
	if _, err := m.runGitEnv(env, runtimestate.AddArgs()...); err != nil {
		return nil, fmt.Errorf("failed to stage changes for checkpoint: %w", err)
	}
	_, _ = m.runGitEnv(env, runtimestate.UnstageArgs(m.RootDir)...)

	treeHash, err := m.runGitEnv(env, "write-tree")
	if err != nil {
		return nil, fmt.Errorf("git write-tree failed: %w", err)
	}

	// Files differing from HEAD in the snapshot.
	changedFiles := make([]string, 0)
	if out, err := m.runGitZ("diff", "--name-only", "--no-renames", "-z", "HEAD", treeHash); err == nil {
		changedFiles = append(changedFiles, out...)
	}

	history, _ := m.loadHistory()
	id := fmt.Sprintf("chk_%03d", len(history)+1)
	base, _ := m.runGit("rev-parse", "HEAD")
	// What was staged at save time, so staging done before the checkpoint is not taken for
	// staging done after it (an unmerged index has no tree: then HEAD is the reference).
	indexTree := ""
	if realCopy, cleanupReal, err := m.privateIndex(); err == nil {
		indexTree, _ = m.runGitEnv([]string{"GIT_INDEX_FILE=" + realCopy}, "write-tree")
		cleanupReal()
	}

	chk := Checkpoint{
		ID:        id,
		TreeHash:  treeHash,
		Message:   message,
		CreatedAt: time.Now().UTC(),
		Files:     changedFiles,
		Base:      base,
		IndexTree: indexTree,
	}

	history = append(history, chk)
	if err := m.saveHistory(history); err != nil {
		return nil, err
	}

	return &chk, nil
}

// privateIndex copies the worktree's index to a temporary file (for GIT_INDEX_FILE).
func (m *Manager) privateIndex() (string, func(), error) {
	real, err := m.runGit("rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp("", "monoflow-checkpoint-index-*")
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	if data, err := os.ReadFile(real); err == nil {
		_, _ = f.Write(data)
	}
	_ = f.Close()
	if fi, err := os.Stat(name); err == nil && fi.Size() == 0 {
		_ = os.Remove(name) // no index yet: let git create a fresh one
	}
	return name, func() { _ = os.Remove(name) }, nil
}

// ForeignChangesError is returned by Revert when the working tree holds staged or new files
// the checkpoint does not know about: in a worktree several agents edit, they are most likely
// another agent's work, and a revert would delete them.
type ForeignChangesError struct {
	Checkpoint string
	Untracked  []string // new files that are not in the checkpoint
	Staged     []string // staged changes made after the checkpoint
	Modified   []string // shared worktree only: files clean at save time and changed since
}

func (e *ForeignChangesError) Error() string {
	var parts []string
	if len(e.Untracked) > 0 {
		parts = append(parts, "new files not in the checkpoint: "+strings.Join(e.Untracked, ", "))
	}
	if len(e.Staged) > 0 {
		parts = append(parts, "staged since the checkpoint: "+strings.Join(e.Staged, ", "))
	}
	if len(e.Modified) > 0 {
		parts = append(parts, "changed since the checkpoint though it did not touch them (shared worktree): "+strings.Join(e.Modified, ", "))
	}
	return fmt.Sprintf("refusing to revert to %s: %s. They may be another agent's work; nothing was changed. "+
		"Move or remove them yourself, or pass --force to discard them too", e.Checkpoint, strings.Join(parts, "; "))
}

// Paths lists every path the error names.
func (e *ForeignChangesError) Paths() []string {
	return append(append(append([]string(nil), e.Untracked...), e.Staged...), e.Modified...)
}

// Revert restores the working directory to the specified checkpoint (see RevertCheckpoint).
func (m *Manager) Revert(checkpointID string) error {
	_, err := m.RevertCheckpoint(checkpointID, false)
	return err
}

// RevertCheckpoint restores a checkpoint and returns which one it restored.
//
// Only the paths that differ between the checkpoint and the working tree are written, and the
// index is left alone. New (untracked) files the checkpoint does not contain, and changes staged
// after the checkpoint, are never touched: Revert refuses with a ForeignChangesError listing them
// (#33: `read-tree -u --reset` deleted other agents' files in a shared worktree). force restores
// the old behaviour: index and working tree reset to the checkpoint, those files discarded.
func (m *Manager) RevertCheckpoint(checkpointID string, force bool) (*Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	history, err := m.loadHistory()
	if err != nil {
		return nil, err
	}

	var target *Checkpoint
	if checkpointID == "" || checkpointID == "latest" {
		if len(history) == 0 {
			return nil, fmt.Errorf("no checkpoints available to revert")
		}
		target = &history[len(history)-1]
	} else {
		for i := range history {
			if history[i].ID == checkpointID {
				target = &history[i]
				break
			}
		}
	}

	if target == nil {
		return nil, fmt.Errorf("checkpoint '%s' not found", checkpointID)
	}

	inTree := m.treePaths(target.TreeHash)
	base := target.Base
	if base == "" {
		base = "HEAD"
	}
	inBase := m.treePaths(base)
	foreign := &ForeignChangesError{Checkpoint: target.ID}
	untracked, err := m.runGitZ("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	for _, p := range untracked {
		if !inTree[p] && !runtimestate.IsRuntimePath(p) {
			foreign.Untracked = append(foreign.Untracked, p)
		}
	}
	if force {
		if _, err := m.runGit("read-tree", "-u", "--reset", target.TreeHash); err != nil {
			return nil, fmt.Errorf("git read-tree reset failed: %w", err)
		}
		for _, p := range foreign.Untracked {
			_ = os.Remove(filepath.Join(m.RootDir, filepath.FromSlash(p)))
		}
		return target, nil
	}
	// Staged after the checkpoint: the index differs from HEAD and from what was staged at save.
	indexRef := target.IndexTree
	if indexRef == "" {
		indexRef = target.TreeHash // checkpoints saved before IndexTree was recorded
	}
	stagedVsHead, _ := m.runGitZ("diff", "--cached", "--name-only", "--no-renames", "-z", "HEAD")
	stagedVsSave, _ := m.runGitZ("diff", "--cached", "--name-only", "--no-renames", "-z", indexRef)
	sinceSave := map[string]bool{}
	for _, p := range stagedVsSave {
		sinceSave[p] = true
	}
	for _, p := range stagedVsHead {
		if sinceSave[p] && !runtimestate.IsRuntimePath(p) {
			foreign.Staged = append(foreign.Staged, p)
		}
	}

	// Paths whose working-tree content differs from the checkpoint. Entries outside a sparse
	// checkout (skip-worktree) never show here: git treats them as matching the index.
	changed, err := m.runGitZ("diff", "--name-only", "-z", "--no-renames", target.TreeHash)
	if err != nil {
		return nil, err
	}
	shared := m.Shared || sharedFromEnv()
	inFiles := map[string]bool{}
	for _, f := range target.Files {
		inFiles[f] = true
	}
	var restore, remove []string
	for _, p := range changed {
		if runtimestate.IsRuntimePath(p) {
			continue
		}
		if shared && !inFiles[p] {
			// Clean when the checkpoint was saved and changed since: in a shared worktree that
			// may be another agent's edit, which a revert must not undo.
			foreign.Modified = append(foreign.Modified, p)
			continue
		}
		if inTree[p] {
			restore = append(restore, p)
		} else if inBase[p] {
			remove = append(remove, p) // the checkpoint had deleted it
		}
		// else: added after the checkpoint (committed since, say): not the checkpoint's to delete
	}
	// The checkpoint's own new files (not in its base) that are gone from disk. Limited to them:
	// every other path of the tree is tracked, so git diff above reports it, and the tree also
	// holds the paths a sparse checkout leaves off disk on purpose.
	for p := range inTree {
		if inBase[p] || runtimestate.IsRuntimePath(p) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(m.RootDir, filepath.FromSlash(p))); os.IsNotExist(err) {
			restore = append(restore, p)
		}
	}
	if len(foreign.Paths()) > 0 {
		return nil, foreign
	}
	for _, p := range remove {
		if err := os.Remove(filepath.Join(m.RootDir, filepath.FromSlash(p))); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	restore = uniq(restore)
	if len(restore) > 0 {
		// --worktree only: the index (another agent's staged work included) is left alone.
		// Literal pathspecs from stdin: file names with spaces, globs or many files.
		cmd := exec.Command("git", "restore", "--source="+target.TreeHash, "--worktree", "--pathspec-from-file=-", "--pathspec-file-nul")
		cmd.Dir = m.RootDir
		cmd.Env = append(os.Environ(), "GIT_LITERAL_PATHSPECS=1")
		cmd.Stdin = strings.NewReader(strings.Join(restore, "\x00") + "\x00")
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("restore checkpoint files: %w (output: %s)", err, strings.TrimSpace(string(out)))
		}
	}
	return target, nil
}

func sharedFromEnv() bool {
	v := os.Getenv("MONOFLOW_SHARED_WORKTREE")
	return v == "1" || v == "true"
}

func (m *Manager) treePaths(treeish string) map[string]bool {
	paths := map[string]bool{}
	if out, err := m.runGitZ("ls-tree", "-r", "--name-only", "-z", treeish); err == nil {
		for _, p := range out {
			paths[p] = true
		}
	}
	return paths
}

func splitZ(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, p := range in {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// List returns all recorded checkpoints.
func (m *Manager) List() ([]Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadHistory()
}

func (m *Manager) loadHistory() ([]Checkpoint, error) {
	if _, err := os.Stat(m.historyFile); os.IsNotExist(err) {
		return []Checkpoint{}, nil
	}
	data, err := os.ReadFile(m.historyFile)
	if err != nil {
		return nil, err
	}
	var history []Checkpoint
	if err := json.Unmarshal(data, &history); err != nil {
		return []Checkpoint{}, nil
	}
	return history, nil
}

func (m *Manager) saveHistory(history []Checkpoint) error {
	dir := filepath.Dir(m.historyFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.historyFile, data, 0644)
}

// CreateWorktree creates an isolated worktree sandbox for concurrent agent tasks.
func (m *Manager) CreateWorktree(taskID string) (string, error) {
	worktreeDir := filepath.Join(m.RootDir, ".agents", "worktrees", taskID)
	branchName := fmt.Sprintf("monoflow-%s", taskID)

	if err := os.MkdirAll(filepath.Dir(worktreeDir), 0755); err != nil {
		return "", err
	}

	// git worktree add -b <branch> <path> HEAD
	_, err := m.runGit("worktree", "add", "-b", branchName, worktreeDir, "HEAD")
	if err != nil {
		return "", fmt.Errorf("failed to create worktree: %w", err)
	}

	return worktreeDir, nil
}

// RemoveWorktree safely cleans up an isolated worktree sandbox.
func (m *Manager) RemoveWorktree(taskID string) error {
	worktreeDir := filepath.Join(m.RootDir, ".agents", "worktrees", taskID)
	branchName := fmt.Sprintf("monoflow-%s", taskID)

	// git worktree remove <path> --force
	_, _ = m.runGit("worktree", "remove", worktreeDir, "--force")
	// git branch -D <branch>
	_, _ = m.runGit("branch", "-D", branchName)

	return nil
}
