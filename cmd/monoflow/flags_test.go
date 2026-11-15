package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestUnknownFlags(t *testing.T) {
	cases := []struct {
		cmd  string
		args []string
		bad  []string
	}{
		{"verify", []string{"--base=origin/main", "--affected"}, nil},
		{"verify", []string{"--bas=origin/main"}, []string{"--bas=origin/main"}},
		{"verify", []string{"--base"}, []string{"--base"}}, // --base takes an inline value
		{"verify", []string{"--frobnicate"}, []string{"--frobnicate"}},
		{"commit", []string{"-m", "--weird message", "--no-outer"}, nil}, // -m consumes its value
		{"commit", []string{"-am", "msg", "--no-verify"}, nil},           // from the git shim
		{"push", []string{"-u", "origin", "feat"}, nil},
		{"start", []string{"t1", "--package", "api"}, nil},
		{"graph", []string{"query", "pkg", "--deep"}, []string{"--deep"}},
		{"pull", []string{"--rebase"}, nil},
	}
	for _, c := range cases {
		bad, checked := unknownFlags(c.cmd, c.args)
		if !checked {
			t.Fatalf("%s not checked", c.cmd)
		}
		if strings.Join(bad, ",") != strings.Join(c.bad, ",") {
			t.Errorf("%s %v: got %v, want %v", c.cmd, c.args, bad, c.bad)
		}
	}
	for _, c := range []string{"hook", "find"} {
		if _, checked := unknownFlags(c, []string{"--anything"}); checked {
			t.Errorf("%s must not be checked", c)
		}
	}
}

// Every flag a handler compares against must be in commandFlags, or that flag would now be
// rejected before the handler sees it.
func TestCommandFlagsCoverHandlers(t *testing.T) {
	files := map[string][]string{
		"cmd_verify.go": {"verify"}, "cmd_pr.go": {"pr"}, "cmd_events.go": {"events"},
		"cmd_flow.go": {"ship", "start"}, "cmd_improve.go": {"improve"},
		"cmd_git.go":      {"commit", "push", "sync", "version"},
		"cmd_worktree.go": {"worktree", "subset", "clone", "branch", "checkpoint", "start"},
		"cmd_ci.go":       {"ci", "outer"}, "cmd_misc.go": {"init", "cache", "deliver"},
	}
	lit := regexp.MustCompile(`(?:==|HasPrefix\(\w+, )"(-[-a-zA-Z0-9]+=?)"`)
	// git's own flags, and flags of commands this table does not check (review, queue, scenario).
	ignore := map[string]bool{"--allow-root": true, "--json": true, "--help": true, "-h": true,
		"--title=": true, "--desc=": true, "--dir=": true, "-": true, "--": true}
	for file, cmds := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range lit.FindAllStringSubmatch(string(data), -1) {
			f := m[1]
			if ignore[f] {
				continue
			}
			found := false
			for _, c := range cmds {
				for _, k := range commandFlags[c] {
					if strings.TrimSpace(k) == f {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("%s compares %q, which none of %v accepts in commandFlags", file, f, cmds)
			}
		}
	}
}

func TestWithSchema(t *testing.T) {
	for in, want := range map[string]string{
		`{"a": 1}`:            "{\n  \"schema\": 1,\"a\": 1}",
		`{}`:                  "{\n  \"schema\": 1\n}",
		`[1,2]`:               `[1,2]`,
		`{"schema": 7}`:       `{"schema": 7}`,
		`{"x": {"schema":1}}`: "{\n  \"schema\": 1,\"x\": {\"schema\":1}}",
	} {
		got := string(withSchema([]byte(in)))
		if got != want {
			t.Errorf("%s -> %q, want %q", in, got, want)
		}
		if strings.HasPrefix(in, "{") {
			var m map[string]any
			if err := json.Unmarshal([]byte(got), &m); err != nil {
				t.Errorf("%s: invalid JSON %q", in, got)
			}
		}
	}
}
