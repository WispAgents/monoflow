package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// commandFlags lists the flags each command accepts in this build. A flag a command does not
// know used to be accepted and ignored: an older build silently ignored `verify --base`, and
// computed the affected set against the wrong base with no warning (#40). Commands missing
// from this table (hook, internal commands) are not checked. --json and --help are global.
//
// A name ending in "=" takes its value inline (--base=<ref>); one ending in " " takes the next
// argument as its value (-m "<msg>"); others are switches. Keep the table in sync with the
// handlers: TestCommandFlagsCoverHandlers checks every flag literal in this package is listed.
var commandFlags = map[string][]string{
	"verify": {"--affected", "--all", "--all-checks", "--base=", "--force", "--lint", "--no-cache", "--no-fix",
		"--package=", "--target=", "--proof=", "--strict", "--test", "--typecheck", "--update-snapshots", "-u",
		"--verbose", "--max-packages="},
	"checkpoint": {"--force"},
	"graph":      {},
	// find is not checked: its search text may start with "-" (monoflow find "--force").
	// commit and push also take what the git shim (.agents/bin/git) passes on from a raw
	// git commit / git push (-a, -q, --no-verify, -u ...); they ignore it.
	"commit": {"-m ", "-am ", "-a", "--all", "-q", "--quiet", "-v", "--verbose", "--no-verify", "-n",
		"--no-outer", "--allow-root"},
	"push": {"--no-watch", "--skip-verify", "--force", "--direct", "--remote=", "--branch=",
		"-u", "--set-upstream", "-q", "--quiet", "-v", "--verbose", "--no-verify", "--force-with-lease",
		"--force-with-lease=", "-f", "--allow-root"},
	"sync":  {"--rebase", "--merge", "--stacked-on=", "--autostash", "--remote=", "--branch="},
	"ship":  {"-m ", "-m=", "--message=", "--title=", "--body=", "--body-file=", "--verbose", "--wait", "--deliver"},
	"start": {"--packages=", "--packages ", "--package=", "--package ", "--deps=", "--include-deps=", "--isolated-deps", "--base=", "--from-local"},
	"worktree": {"--packages=", "--packages ", "--package=", "--package ", "--deps=", "--include-deps=", "--isolated-deps",
		"--base=", "--from-local", "--force", "--remote=", "--branch=", "--to=", "--from="},
	"subset": {"--packages=", "--packages ", "--package=", "--package ", "--deps=", "--include-deps=", "--force",
		"--remote=", "--branch="},
	"clone":  {"--packages=", "--packages ", "--include-deps=", "--branch=", "--deps="},
	"branch": {"-c", "-b", "--create"},
	"pr": {"--title=", "--body=", "--body-file=", "--base=", "--head=", "--remote=", "--draft", "--stack", "--no-push",
		"--verbose", "--state=", "--limit=", "--approve", "--reject", "--request-changes", "--comment=",
		"--against-master", "--worktree", "--worktree=", "--deps=", "--strategy=", "--queue", "--delete-branch"},
	"events": {"--all", "--job=", "--remote=", "--commit=", "--branch=", "--timeout=", "--verbose"},
	"outer": {"--commit=", "--branch=", "--remote=", "--remote-branch=", "--push", "--auto-push", "--revert",
		"--auto-revert", "--async", "--no-e2e", "--e2e", "--timeout=", "--verbose", "--job="},
	"ci": {"--commit=", "--branch=", "--async", "--head=", "--workflow=", "--limit=", "--check=", "--logs",
		"--remote=", "--short", "--verbose"},
	"improve": {"--title=", "--area=", "--kind=", "--severity=", "--detail=", "--evidence=", "--same-as=",
		"--dry-run", "--all", "--since=", "--note=", "--exact"},
	"version": {"--cascade", "--base=", "--version=", "--to=", "--allow-root"},
	"init":    {"--setup-gh-actions", "--with=", "--minimal"},
	"uninit":  {},
	"cache":   {"--port=", "--dir=", "--token="},
	"deliver": {"--target=", "--package=", "--no-cache", "--force"},
}

func init() {
	commandFlags["pull"] = commandFlags["sync"]
	commandFlags["switch"] = commandFlags["branch"]
	commandFlags["retro"] = commandFlags["improve"]
}

// unknownFlags returns the flags in args (the arguments after the command) that cmd does not
// accept. Arguments after "--" and the values of value-taking flags are not flags.
func unknownFlags(cmd string, args []string) (bad []string, checked bool) {
	known, ok := commandFlags[cmd]
	if !ok {
		return nil, false
	}
	switches, inline, separate := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, f := range known {
		switch {
		case strings.HasSuffix(f, "="):
			inline[strings.TrimSuffix(f, "=")] = true
		case strings.HasSuffix(f, " "):
			separate[strings.TrimSuffix(f, " ")] = true
		default:
			switches[f] = true
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" || isNumber(a) {
			continue
		}
		name, _, hasValue := strings.Cut(a, "=")
		switch {
		case hasValue && inline[name]:
		case !hasValue && switches[name]:
		case !hasValue && separate[name]:
			i++ // its value
		default:
			bad = append(bad, a)
		}
	}
	return bad, true
}

func isNumber(s string) bool {
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

// rejectUnknownFlags exits 2 when the command got a flag it does not support (#40).
func rejectUnknownFlags(cmd string, args []string) {
	bad, _ := unknownFlags(cmd, args)
	if len(bad) == 0 {
		return
	}
	known := append([]string(nil), commandFlags[cmd]...)
	for i, f := range known {
		known[i] = strings.TrimSpace(f)
		if strings.HasSuffix(known[i], "=") {
			known[i] += "<v>"
		}
	}
	sort.Strings(known)
	msg := fmt.Sprintf("monoflow %s: unknown flag %s (not supported by this build)", cmd, strings.Join(bad, ", "))
	if len(known) > 0 {
		msg += "; accepted: " + strings.Join(known, " ")
	} else {
		msg += "; it takes no flags"
	}
	msg += ". See 'monoflow help " + cmd + "'."
	if jsonMode() {
		printOut(map[string]any{"status": "error", "code": "unknown_flag", "error": msg, "flags": bad})
	} else {
		fmt.Fprintln(os.Stderr, msg)
	}
	exit(2)
}
