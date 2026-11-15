package initialize

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// UninitResult lists what Uninit removed (repo-relative where inside the repo).
type UninitResult struct {
	Removed []string `json:"removed"`
	Kept    []string `json:"kept,omitempty"` // files that looked like MonoFlow's but were changed by hand
}

// Uninit removes what init installed besides the graph: the git shield hooks (only MonoFlow's
// own scripts), the git shim, the MonoFlow section of AGENTS.md / CLAUDE.md, the MonoFlow hook
// entries of .claude/settings.json, the opencode plugin, the agent rules and skill, and the
// bin/monoflow link. The config (.agents/monoflow.config.json) and the binary stay.
func Uninit(root string) (*UninitResult, error) {
	res := &UninitResult{Removed: []string{}}
	rel := func(p string) string {
		if r, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(r, "..") {
			return filepath.ToSlash(r)
		}
		return p
	}
	remove := func(p string) {
		if os.Remove(p) == nil {
			res.Removed = append(res.Removed, rel(p))
		}
	}

	if hooksDir, err := HooksDir(root); err == nil {
		for _, name := range []string{"pre-commit", "pre-push"} {
			p := filepath.Join(hooksDir, name)
			if data, err := os.ReadFile(p); err == nil && strings.Contains(string(data), shieldMarker) {
				remove(p)
			}
		}
	}
	shim := filepath.Join(root, ".agents", "bin", "git")
	if data, err := os.ReadFile(shim); err == nil && strings.Contains(string(data), "MonoFlow Git Interception Shim") {
		remove(shim)
	}
	for _, f := range []string{"AGENTS.md", "CLAUDE.md"} {
		p := filepath.Join(root, f)
		if removed, emptied := removeSection(p); removed {
			res.Removed = append(res.Removed, f+" (MonoFlow section)")
			if emptied {
				remove(p)
			}
		}
	}
	if removeClaudeHooks(filepath.Join(root, ".claude", "settings.json")) {
		res.Removed = append(res.Removed, ".claude/settings.json (MonoFlow hooks)")
	}
	plugin := filepath.Join(root, ".opencode", "plugin", "monoflow.js")
	if data, err := os.ReadFile(plugin); err == nil {
		if strings.Contains(string(data), "Installed by monoflow init") {
			remove(plugin)
		} else {
			res.Kept = append(res.Kept, rel(plugin))
		}
	}
	for p, tmpl := range map[string]string{
		filepath.Join(root, ".agents", "rules", "agentic-monorepo.md"):   AgentRuleTemplate,
		filepath.Join(root, ".agents", "skills", "monoflow", "SKILL.md"): AgentSkillTemplate,
	} {
		if data, err := os.ReadFile(p); err == nil {
			if string(data) == tmpl {
				remove(p)
			} else {
				res.Kept = append(res.Kept, rel(p)) // edited by hand
			}
		}
	}
	link := filepath.Join(root, "bin", "monoflow")
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if target, _ := os.Readlink(link); strings.Contains(target, ".agents") {
			remove(link)
		}
	}
	return res, nil
}

// removeSection drops the MonoFlow-managed section of a markdown file; emptied reports that
// nothing else was in it.
func removeSection(path string) (removed, emptied bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	s := string(data)
	i := strings.Index(s, sectionStart)
	if i < 0 {
		return false, false
	}
	j := strings.Index(s[i:], sectionEnd)
	if j < 0 {
		return false, false
	}
	end := i + j + len(sectionEnd)
	rest := strings.TrimLeft(s[:i]+s[end:], "\n")
	if strings.TrimSpace(rest) == "" {
		return true, true
	}
	return os.WriteFile(path, []byte(rest), 0644) == nil, false
}

// removeClaudeHooks removes the hook entries that run "monoflow hook" from Claude settings,
// leaving the rest of the file as it is.
func removeClaudeHooks(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	settings := map[string]any{}
	if json.Unmarshal(data, &settings) != nil {
		return false
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}
	changed := false
	for event, v := range hooks {
		list, _ := v.([]any)
		kept := []any{}
		for _, h := range list {
			if b, _ := json.Marshal(h); strings.Contains(string(b), "monoflow") && strings.Contains(string(b), " hook ") {
				changed = true
				continue
			}
			kept = append(kept, h)
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if !changed {
		return false
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}
	if len(settings) == 0 {
		return os.Remove(path) == nil
	}
	out, _ := json.MarshalIndent(settings, "", "  ")
	return os.WriteFile(path, append(out, '\n'), 0644) == nil
}
