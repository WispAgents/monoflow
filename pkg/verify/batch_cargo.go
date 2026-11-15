package verify

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/antigravity/monoflow/pkg/graph"
)

// Cargo: crates of one workspace whose check, clippy or test is the discovered default
// ("cargo check -p <crate>") run as one cargo call per check (-p a -p b …, or --workspace when
// every member is affected). check and clippy report per crate through --message-format=json;
// tests are split by the "Running <binary>" / "Doc-tests <crate>" sections of each crate.

// cargoDefaultCmd is discovery's command of the given kind for crate name.
func cargoDefaultCmd(sub, name string) string {
	switch sub {
	case "check":
		return "cargo check -p " + name
	case "clippy":
		return "cargo clippy -p " + name + " -- -D warnings"
	case "test":
		return "cargo test -p " + name
	}
	return ""
}

func cargoBatchKind(sub string) batchKind {
	return batchKind{
		name: "cargo " + sub,
		group: func(r *Runner, pkg *graph.PackageNode, c string, opt VerifyOptions) string {
			if pkg.Language != graph.LangRust || c != cargoDefaultCmd(sub, pkg.Name) {
				return ""
			}
			return cargoWorkspaceRoot(r.RootDir, filepath.Join(r.RootDir, pkg.Path))
		},
		job: func(group string) string { return "cargo\x00" + group },
		run: func(r *Runner, ws string, members []batchMember, opt VerifyOptions) map[string]batchOutcome {
			return runCargoBatch(sub, ws, members)
		},
	}
}

var cargoWorkspaceSection = regexp.MustCompile(`(?m)^\s*\[workspace\]`)

// cargoWorkspaceRoot returns the directory of the workspace crateDir belongs to (the nearest
// Cargo.toml with [workspace], at most up to root), or "" when it has none or when cargo would
// see a different configuration from the crate's directory than from the workspace root (a
// .cargo/config or rust-toolchain file between them).
func cargoWorkspaceRoot(root, crateDir string) string {
	root, err1 := filepath.Abs(root)
	crateDir, err2 := filepath.Abs(crateDir)
	if err1 != nil || err2 != nil {
		return ""
	}
	var below []string
	for dir := crateDir; ; dir = filepath.Dir(dir) {
		if data, err := os.ReadFile(filepath.Join(dir, "Cargo.toml")); err == nil && cargoWorkspaceSection.Match(data) {
			for _, d := range below {
				for _, f := range []string{".cargo/config.toml", ".cargo/config", "rust-toolchain", "rust-toolchain.toml"} {
					if _, err := os.Stat(filepath.Join(d, f)); err == nil {
						return ""
					}
				}
			}
			return dir
		}
		below = append(below, dir)
		if dir == root || filepath.Dir(dir) == dir || !strings.HasPrefix(dir, root) {
			return ""
		}
	}
}

// cargoMetadata is what the batch needs from `cargo metadata --no-deps`.
type cargoMetadata struct {
	WorkspaceRoot    string   `json:"workspace_root"`
	WorkspaceMembers []string `json:"workspace_members"`
	Packages         []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		ManifestPath string `json:"manifest_path"`
		Targets      []struct {
			Name             string   `json:"name"`
			Kind             []string `json:"kind"`
			Doctest          bool     `json:"doctest"`
			RequiredFeatures []string `json:"required-features"`
		} `json:"targets"`
	} `json:"packages"`
}

// cargoCrates describes the batched crates for splitting the output.
type cargoCrates struct {
	byManifest map[string]string   // manifest path as cargo reports it -> crate
	build      map[string][]string // crate -> lib and bin targets a check builds
	doc        map[string][]string // crate -> doc-tested crate names ("Doc-tests <name>")
}

var cargoLibKinds = map[string]bool{"lib": true, "rlib": true, "dylib": true, "proc-macro": true, "cdylib": true, "staticlib": true}

// cargoSelect maps the members onto the workspace's packages. sel is the package selection
// (--workspace when it covers every workspace member) and crates the members cargo knows.
func cargoSelect(meta cargoMetadata, members []batchMember) (sel []string, crates cargoCrates) {
	crates = cargoCrates{byManifest: map[string]string{}, build: map[string][]string{}, doc: map[string][]string{}}
	want := map[string]string{} // resolved manifest -> crate
	for _, m := range members {
		want[resolved(filepath.Join(m.abs, "Cargo.toml"))] = m.pkg.Name
	}
	inWorkspace := map[string]bool{}
	for _, id := range meta.WorkspaceMembers {
		inWorkspace[id] = true
	}
	covered := 0
	for _, p := range meta.Packages {
		if !inWorkspace[p.ID] {
			continue
		}
		name, ok := want[resolved(p.ManifestPath)]
		if !ok || name != p.Name {
			continue
		}
		covered++
		crates.byManifest[p.ManifestPath] = name
		for _, t := range p.Targets {
			lib, bin := false, false
			for _, k := range t.Kind {
				lib = lib || cargoLibKinds[k]
				bin = bin || k == "bin"
			}
			if (lib || bin) && len(t.RequiredFeatures) == 0 {
				crates.build[name] = append(crates.build[name], t.Name)
			}
			if lib && t.Doctest && doctestable(t.Kind) {
				crates.doc[name] = append(crates.doc[name], strings.ReplaceAll(t.Name, "-", "_"))
			}
		}
		sel = append(sel, name)
	}
	sort.Strings(sel)
	if covered > 0 && covered == len(meta.WorkspaceMembers) {
		return []string{"--workspace"}, crates
	}
	var args []string
	for _, n := range sel {
		args = append(args, "-p", shellQuote(n))
	}
	return args, crates
}

// doctestable: cargo runs doc tests for libraries it can link (not cdylib/staticlib only).
func doctestable(kinds []string) bool {
	for _, k := range kinds {
		if k == "lib" || k == "rlib" || k == "dylib" || k == "proc-macro" {
			return true
		}
	}
	return false
}

// cargoBatchCommand is the one cargo call checking the selection.
func cargoBatchCommand(sub string, sel []string) string {
	s := strings.Join(sel, " ")
	switch sub {
	case "check":
		return "cargo check " + s + " --keep-going --message-format=json"
	case "clippy":
		return "cargo clippy " + s + " --keep-going --message-format=json -- -D warnings"
	default:
		return "cargo test " + s + " --no-fail-fast --message-format=json"
	}
}

func runCargoBatch(sub, ws string, members []batchMember) map[string]batchOutcome {
	cmd := exec.Command("cargo", "metadata", "--no-deps", "--format-version", "1")
	cmd.Dir = ws
	data, err := cmd.Output()
	if err != nil {
		return nil
	}
	var meta cargoMetadata
	if json.Unmarshal(data, &meta) != nil {
		return nil
	}
	sel, crates := cargoSelect(meta, members)
	if len(crates.byManifest) < 2 {
		return nil
	}
	c := cargoBatchCommand(sub, sel)
	out, _ := runBatchCommand(c, ws)
	var outs map[string]batchOutcome
	if sub == "test" {
		outs = splitCargoTest(out, crates)
	} else {
		outs = splitCargoBuild(out, crates)
	}
	return noteBatch(outs, c, len(crates.byManifest))
}

// cargoMessage is one line of cargo's --message-format=json.
type cargoMessage struct {
	Reason       string `json:"reason"`
	ManifestPath string `json:"manifest_path"`
	Target       struct {
		Name string   `json:"name"`
		Kind []string `json:"kind"`
	} `json:"target"`
	Profile struct {
		Test bool `json:"test"`
	} `json:"profile"`
	Executable *string `json:"executable"`
	Message    struct {
		Level    string `json:"level"`
		Rendered string `json:"rendered"`
	} `json:"message"`
	Success bool `json:"success"`
}

// parseCargoMessage parses a JSON message line (ok=false: another line).
func parseCargoMessage(line string) (cargoMessage, bool) {
	var m cargoMessage
	if !strings.HasPrefix(line, "{") || !strings.Contains(line, `"reason"`) || json.Unmarshal([]byte(line), &m) != nil {
		return m, false
	}
	return m, m.Reason != ""
}

func scanLines(out []byte) *bufio.Scanner {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	return sc
}

// cargoCrateMessages collects the per-crate compiler messages of a cargo JSON run.
type cargoCrateMessages struct {
	rendered map[string][]string        // crate -> rendered diagnostics
	errored  map[string]bool            // crate -> has an error-level diagnostic
	built    map[string]map[string]bool // crate -> targets with an artifact
	finished bool                       // build-finished seen
	success  bool                       // build-finished success
}

func (c *cargoCrateMessages) add(m cargoMessage, crates cargoCrates) {
	switch m.Reason {
	case "build-finished":
		c.finished, c.success = true, m.Success
	case "compiler-message":
		if name, ok := crates.byManifest[m.ManifestPath]; ok {
			c.rendered[name] = append(c.rendered[name], strings.TrimRight(m.Message.Rendered, "\n"))
			if strings.HasPrefix(m.Message.Level, "error") {
				c.errored[name] = true
			}
		}
	case "compiler-artifact":
		if name, ok := crates.byManifest[m.ManifestPath]; ok {
			if c.built[name] == nil {
				c.built[name] = map[string]bool{}
			}
			c.built[name][m.Target.Name] = true
		}
	}
}

func newCargoCrateMessages() *cargoCrateMessages {
	return &cargoCrateMessages{rendered: map[string][]string{}, errored: map[string]bool{}, built: map[string]map[string]bool{}}
}

// splitCargoBuild attributes a `cargo check|clippy --keep-going --message-format=json` run to
// its crates: a crate with an error fails with its diagnostics; one without passes when the
// build succeeded or its targets were all built; any other (blocked by a failing dependency, a
// build script that failed) is left out, to run on its own.
func splitCargoBuild(out []byte, crates cargoCrates) map[string]batchOutcome {
	msgs := newCargoCrateMessages()
	sc := scanLines(out)
	for sc.Scan() {
		if m, ok := parseCargoMessage(sc.Text()); ok {
			msgs.add(m, crates)
		}
	}
	if !msgs.finished {
		return nil // cargo did not get to build (unknown flag, lock, metadata error)
	}
	res := map[string]batchOutcome{}
	for _, name := range crates.byManifest {
		text := strings.Join(msgs.rendered[name], "\n")
		if text != "" {
			text += "\n"
		}
		switch {
		case msgs.errored[name]:
			res[name] = failedBatch(text, nil)
		case msgs.success:
			res[name] = passedBatch(text)
		default:
			complete := len(crates.build[name]) > 0
			for _, t := range crates.build[name] {
				complete = complete && msgs.built[name][t]
			}
			if complete {
				res[name] = passedBatch(text)
			}
		}
	}
	return res
}

var (
	cargoRunningLine  = regexp.MustCompile(`^\s+Running (?:.*\((.+)\)|(\S+))\s*$`)
	cargoDocTestsLine = regexp.MustCompile(`^\s+Doc-tests (\S+)\s*$`)
	cargoSummaryLine  = regexp.MustCompile(`^error: \d+ targets? failed:`)
	cargoTestOK       = regexp.MustCompile(`(?m)^test result: ok\.`)
)

// splitCargoTest attributes a `cargo test --no-fail-fast --message-format=json` run to its
// crates: compile errors by manifest, test output by the section of each test binary (named
// by its artifact) and doc-test crate. A crate passes when all its test binaries and doc
// tests ran and reported ok; it fails on a compile error or a section that did not end ok;
// otherwise (the run stopped before it) it is left out, to run on its own.
func splitCargoTest(out []byte, crates cargoCrates) map[string]batchOutcome {
	msgs := newCargoCrateMessages()
	exeOwner := map[string]string{}      // test binary file name -> crate
	exes := map[string][]string{}        // crate -> its test binaries
	docOwner := map[string]string{}      // doc-tested crate name -> crate ("" if ambiguous)
	sections := map[string][]string{}    // crate -> its sections' text
	failedSec := map[string]bool{}       // crate -> a section did not end ok
	seen := map[string]map[string]bool{} // crate -> binaries and doc tests that ran
	for name, docs := range crates.doc {
		for _, d := range docs {
			if _, dup := docOwner[d]; dup {
				docOwner[d] = ""
			} else {
				docOwner[d] = name
			}
		}
	}
	unmapped := false
	owner, key := "", ""
	var buf []string
	flush := func() {
		if owner != "" {
			text := strings.Join(buf, "\n") + "\n"
			sections[owner] = append(sections[owner], text)
			if !cargoTestOK.MatchString(text) {
				failedSec[owner] = true
			}
			if seen[owner] == nil {
				seen[owner] = map[string]bool{}
			}
			seen[owner][key] = true
		}
		owner, key, buf = "", "", nil
	}
	start := func(o, k, line string) {
		flush()
		if o == "" {
			unmapped = true
		}
		owner, key, buf = o, k, []string{line}
	}
	sc := scanLines(out)
	for sc.Scan() {
		line := sc.Text()
		if m, ok := parseCargoMessage(line); ok {
			msgs.add(m, crates)
			if name, mine := crates.byManifest[m.ManifestPath]; mine && m.Reason == "compiler-artifact" && m.Profile.Test && m.Executable != nil && *m.Executable != "" {
				exe := filepath.Base(*m.Executable)
				exeOwner[exe] = name
				exes[name] = append(exes[name], exe)
			}
			continue
		}
		switch {
		case cargoRunningLine.MatchString(line):
			m := cargoRunningLine.FindStringSubmatch(line)
			p := m[1]
			if p == "" {
				p = m[2]
			}
			exe := filepath.Base(strings.ReplaceAll(p, `\`, "/"))
			start(exeOwner[exe], "exe:"+exe, line)
		case cargoDocTestsLine.MatchString(line):
			d := cargoDocTestsLine.FindStringSubmatch(line)[1]
			start(docOwner[d], "doc:"+d, line)
		case cargoSummaryLine.MatchString(line):
			flush()
		default:
			if owner != "" {
				buf = append(buf, line)
			}
		}
	}
	flush()
	res := map[string]batchOutcome{}
	for _, name := range crates.byManifest {
		text := strings.Join(msgs.rendered[name], "\n")
		if text != "" {
			text += "\n"
		}
		text += strings.Join(sections[name], "")
		switch {
		case msgs.errored[name]:
			res[name] = failedBatch(text, nil)
		case !msgs.finished || !msgs.success:
			// the tests did not run (another crate did not compile)
		case failedSec[name]:
			res[name] = failedBatch(text, nil)
		case unmapped:
			// a section nobody owns may be this crate's
		default:
			complete := true
			for _, e := range exes[name] {
				complete = complete && seen[name]["exe:"+e]
			}
			for _, d := range crates.doc[name] {
				complete = complete && seen[name]["doc:"+d]
			}
			if complete {
				res[name] = passedBatch(text)
			}
		}
	}
	return res
}
