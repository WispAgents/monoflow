package verify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/antigravity/monoflow/pkg/graph"
)

// npm workspaces. Typecheck: packages whose check is discovery's `<exec> tsc --noEmit -p
// tsconfig.json` are type-checked by one `tsc -b` over a solution referencing their tsconfigs
// (--verbose names the project each error belongs to). Tests: packages tested by the root's
// vitest scoped to their folder run in one vitest with every folder (or pending test file) as
// a filter, its JSON report split by folder.

// tscDefault matches discovery's per-package typecheck command.
var tscDefault = regexp.MustCompile(`^(pnpm exec|yarn|npx --no-install|bunx) tsc --noEmit -p tsconfig\.json$`)

var tsconfigReferences = regexp.MustCompile(`"references"\s*:`)

var tscBatchKind = batchKind{
	name: "tsc",
	group: func(r *Runner, pkg *graph.PackageNode, c string, opt VerifyOptions) string {
		if (pkg.Language != graph.LangTypeScript && pkg.Language != graph.LangJavaScript) || c != pkg.TypecheckCmd {
			return ""
		}
		m := tscDefault.FindStringSubmatch(c)
		if m == nil {
			return ""
		}
		abs := filepath.Join(r.RootDir, pkg.Path)
		if !tscBatchable(r.RootDir, abs) {
			return ""
		}
		return "tsc\x00" + m[1]
	},
	job: func(string) string { return "node" },
	run: runTscBatch,
}

// tscBatchable: tsc -b checks pkgDir's project as tsc -p does. It needs TypeScript >= 5.6
// (--noEmit in build mode, errors kept in .tsbuildinfo), the same TypeScript for the package as
// at the root, and a project without references (-b would build those as well).
func tscBatchable(root, pkgDir string) bool {
	rootTS := filepath.Join(root, "node_modules", "typescript")
	if !versionAtLeast(packageVersion(rootTS), 5, 6) {
		return false
	}
	if own := filepath.Join(pkgDir, "node_modules", "typescript"); fileOrDirExists(own) && resolved(own) != resolved(rootTS) {
		return false
	}
	data, err := os.ReadFile(filepath.Join(pkgDir, "tsconfig.json"))
	return err == nil && !tsconfigReferences.Match(data)
}

func fileOrDirExists(p string) bool { _, err := os.Stat(p); return err == nil }

// packageVersion reads the version of the npm package installed in dir ("" if none).
func packageVersion(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var p struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(data, &p)
	return p.Version
}

// versionAtLeast compares a semver's major.minor.
func versionAtLeast(v string, major, minor int) bool {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return false
	}
	ma, err1 := strconv.Atoi(parts[0])
	mi, err2 := strconv.Atoi(strings.TrimLeft(parts[1], "v"))
	if err1 != nil || err2 != nil {
		return false
	}
	return ma > major || (ma == major && mi >= minor)
}

func runTscBatch(r *Runner, group string, members []batchMember, opt VerifyOptions) map[string]batchOutcome {
	exec := strings.TrimPrefix(group, "tsc\x00")
	dir := filepath.Join(r.RootDir, ".agents", "verify")
	if os.MkdirAll(dir, 0o755) != nil {
		return nil
	}
	sol, err := os.CreateTemp(dir, "tsc-batch-*.json")
	if err != nil {
		return nil
	}
	defer os.Remove(sol.Name())
	type ref struct {
		Path string `json:"path"`
	}
	solution := struct {
		Files      []string `json:"files"`
		References []ref    `json:"references"`
	}{Files: []string{}}
	projects := map[string]batchMember{}
	for _, m := range members {
		rel, err := filepath.Rel(dir, filepath.Join(m.abs, "tsconfig.json"))
		if err != nil {
			return nil
		}
		solution.References = append(solution.References, ref{Path: filepath.ToSlash(rel)})
		projects[resolved(filepath.Join(m.abs, "tsconfig.json"))] = m
	}
	data, _ := json.Marshal(solution)
	_, werr := sol.Write(data)
	_ = sol.Close()
	if werr != nil {
		return nil
	}
	relSol, _ := filepath.Rel(r.RootDir, sol.Name())
	c := exec + " tsc -b " + shellQuote(filepath.ToSlash(relSol)) + " --noEmit --pretty false --verbose"

	// Build mode writes .tsbuildinfo files: leave the packages' as they were (their content is
	// part of the packages' cache keys, and a noEmit buildinfo would change the next real build).
	var dirs []string
	for _, m := range members {
		dirs = append(dirs, m.abs)
	}
	restore := snapshotFiles(dirs, ".tsbuildinfo")
	out, ok := runBatchCommand(c, r.RootDir)
	restore()

	return noteBatch(splitTscBuild(string(out), r.RootDir, projects, ok), c, len(members))
}

// snapshotFiles records the files with the suffix under dirs (outside node_modules and dot
// directories); the returned func restores them and removes those created since.
func snapshotFiles(dirs []string, suffix string) func() {
	list := func() []string {
		var files []string
		for _, d := range dirs {
			_ = filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if e.IsDir() && p != d && (e.Name() == "node_modules" || strings.HasPrefix(e.Name(), ".")) {
					return filepath.SkipDir
				}
				if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
					files = append(files, p)
				}
				return nil
			})
		}
		return files
	}
	before := map[string][]byte{}
	for _, f := range list() {
		if data, err := os.ReadFile(f); err == nil {
			before[f] = data
		}
	}
	return func() {
		for _, f := range list() {
			old, existed := before[f]
			if !existed {
				_ = os.Remove(f)
			} else if cur, err := os.ReadFile(f); err != nil || !bytes.Equal(cur, old) {
				_ = os.WriteFile(f, old, 0o644)
			}
		}
	}
}

var (
	// tsc --verbose status line: "17:33:05 - Building project '/abs/a/tsconfig.json'...".
	tscStatusLine = regexp.MustCompile(`^\[?\d{1,2}:\d{2}:\d{2}(?:\s?[AaPp]\.?[Mm]\.?)?\]?\s+-\s+(.*)$`)
	tscBuilding   = regexp.MustCompile(`^Building project '(.+)'\.\.\.`)
	tscUpToDate   = regexp.MustCompile(`^Project '(.+)' is up to date`)
	tscErrorText  = regexp.MustCompile(`\berror\b`)
)

// splitTscBuild attributes a `tsc -b --verbose --pretty false` run to its projects (resolved
// tsconfig path -> member): the errors printed after "Building project X" are X's (paths made
// package-relative, as tsc -p in the package prints them). A project built without errors, or
// reported up to date, passes. Errors outside a member's section (a global option error)
// leave the members without errors undecided, to run on their own.
func splitTscBuild(out, root string, projects map[string]batchMember, exitOK bool) map[string]batchOutcome {
	sections := map[string][]string{}
	built := map[string]bool{}
	upToDate := map[string]bool{}
	cur := ""
	global := false
	abs := func(p string) string {
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		return resolved(filepath.Clean(p))
	}
	for _, line := range strings.Split(StripANSI(out), "\n") {
		if m := tscStatusLine.FindStringSubmatch(line); m != nil {
			msg := m[1]
			if b := tscBuilding.FindStringSubmatch(msg); b != nil {
				cur = ""
				if mem, ok := projects[abs(b[1])]; ok {
					cur = mem.pkg.Name
					built[cur] = true
				}
				continue
			}
			if u := tscUpToDate.FindStringSubmatch(msg); u != nil {
				if mem, ok := projects[abs(u[1])]; ok {
					upToDate[mem.pkg.Name] = true
				}
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if cur == "" {
			if tscErrorText.MatchString(line) {
				global = true
			}
			continue
		}
		sections[cur] = append(sections[cur], line)
	}
	res := map[string]batchOutcome{}
	for _, mem := range projects {
		name := mem.pkg.Name
		text := strings.Join(sections[name], "\n")
		if text != "" {
			text += "\n"
		}
		if exitOK {
			res[name] = passedBatch(text)
			continue
		}
		diags := ParseDiagnostics(text)
		for i := range diags {
			if rel, err := filepath.Rel(mem.abs, filepath.Join(root, diags[i].File)); err == nil && !filepath.IsAbs(diags[i].File) {
				diags[i].File = filepath.ToSlash(rel)
			}
		}
		switch {
		case len(diags) > 0:
			res[name] = failedBatch(text, diags)
		case global || tscErrorText.MatchString(text):
			// not attributable for certain: run on its own
		case built[name] || upToDate[name]:
			res[name] = passedBatch(text)
		}
	}
	return res
}

var vitestBatchKind = batchKind{
	name: "vitest",
	group: func(r *Runner, pkg *graph.PackageNode, c string, opt VerifyOptions) string {
		if opt.UpdateSnapshots || c != pkg.TestCmd {
			return ""
		}
		inv, ok := parseVitest(pkg, c, r.RootDir)
		if !ok || !inv.root || vitestIsolationOff(filepath.Join(r.RootDir, pkg.Path), r.RootDir) {
			return ""
		}
		return "vitest\x00" + inv.prefix + "\x00" + strings.Join(inv.args, " ")
	},
	job: func(string) string { return "node" },
	run: runVitestBatch,
}

// vitestTarget is one package of a batched vitest run.
type vitestTarget struct {
	name    string
	dir     string   // repo-relative package directory
	filters []string // what it runs: the package filter (full) or pending test files
	full    bool
}

func runVitestBatch(r *Runner, group string, members []batchMember, opt VerifyOptions) map[string]batchOutcome {
	res := map[string]batchOutcome{}
	var targets []vitestTarget
	plans := map[string]*vitestPlan{}
	var inv vitestInvocation
	var filters []string
	for _, m := range members {
		p, ok := r.planVitest(m.pkg, m.cmd, m.abs, opt)
		if !ok { // no cache: a full run, nothing recorded
			i, ok := parseVitest(m.pkg, m.cmd, r.RootDir)
			if !ok {
				continue
			}
			p = &vitestPlan{inv: i, c: m.cmd, full: true}
		} else {
			plans[m.pkg.Name] = p
		}
		if !p.full && len(p.pending) == 0 {
			res[m.pkg.Name] = passedBatch("all test files passed on these inputs\n")
			continue
		}
		inv = p.inv
		f := p.filters(m.pkg)
		targets = append(targets, vitestTarget{name: m.pkg.Name, dir: filepath.ToSlash(m.pkg.Path), filters: f, full: p.full})
		filters = append(filters, f...)
	}
	if len(targets) < 2 {
		return res // one package left to run: it runs on its own as usual
	}
	report, err := os.CreateTemp("", "monoflow-vitest-*.json")
	if err != nil {
		return res
	}
	_ = report.Close()
	defer os.Remove(report.Name())
	inv.filter = ""
	c := inv.command(filters, report.Name())
	out, exitOK := runBatchCommand(c, r.RootDir)
	data, rerr := os.ReadFile(report.Name())
	if rerr != nil {
		return res
	}
	files, perr := parseVitestReport(data)
	if perr != nil || len(files) == 0 {
		return res
	}
	for _, m := range members {
		if p := plans[m.pkg.Name]; p != nil {
			if results := vitestResultsUnder(files, m.abs); len(results) > 0 {
				r.recordVitest(m.pkg, p, results)
			}
		}
	}
	clean := StripANSI(string(out))
	passWithNoTests := false
	for _, a := range inv.args {
		passWithNoTests = passWithNoTests || a == "--passWithNoTests"
	}
	outs := splitVitest(files, ParseDiagnostics(clean), r.RootDir, targets, exitOK, passWithNoTests, vitestUnhandled.MatchString(clean))
	for n, o := range noteBatch(outs, c, len(targets)) {
		res[n] = o
	}
	return res
}

// vitestUnhandled matches vitest's report of errors outside any test (they fail the run but
// belong to no test file).
var vitestUnhandled = regexp.MustCompile(`(?i)unhandled (errors?|rejections?)|(?m)^\s*Errors\s+\d+ errors?`)

// vitestMatches reports whether a vitest filter selects the repo-relative file (vitest matches
// filters as substrings of the path, case-insensitively).
func vitestMatches(file string, filters []string) bool {
	f := strings.ToLower(filepath.ToSlash(file))
	for _, flt := range filters {
		if strings.Contains(f, strings.ToLower(strings.TrimPrefix(flt, "./"))) {
			return true
		}
	}
	return false
}

// splitVitest attributes a batched vitest run (files: the JSON report's absolute test file ->
// status; diags: the run's parsed failures) to its targets, each owning the files its filters
// select, as when it runs alone. A target fails when one of its files failed and the failures
// in them are known; passes when its files passed (or it has none and --passWithNoTests) and
// nothing else failed the run; else it is left out, to run on its own.
func splitVitest(files map[string]string, diags []DiagnosticError, root string, targets []vitestTarget, exitOK, passWithNoTests, unhandled bool) map[string]batchOutcome {
	rootR := resolved(root)
	rel := map[string]string{} // report file -> repo-relative
	anyFailed := false
	for f, st := range files {
		r, err := filepath.Rel(rootR, resolved(f))
		if err != nil || strings.HasPrefix(r, "..") {
			r = f
		}
		rel[f] = filepath.ToSlash(r)
		if st == "failed" {
			anyFailed = true
		}
	}
	res := map[string]batchOutcome{}
	for _, t := range targets {
		var mine, failed []string
		for f, st := range files {
			if vitestMatches(rel[f], t.filters) {
				mine = append(mine, rel[f])
				if st == "failed" {
					failed = append(failed, rel[f])
				}
			}
		}
		switch {
		case len(failed) > 0:
			var own []DiagnosticError
			for _, d := range diags {
				if vitestMatches(d.File, t.filters) {
					own = append(own, d)
				}
			}
			if len(own) > 0 {
				res[t.name] = failedBatch(fmt.Sprintf("failed test files: %s\n", strings.Join(failed, " ")), own)
			}
		case unhandled:
			// errors outside test files: whose they are is not known
		case len(mine) == 0:
			if passWithNoTests && (exitOK || anyFailed) {
				res[t.name] = passedBatch("no test files\n")
			}
		case exitOK || anyFailed:
			res[t.name] = passedBatch(fmt.Sprintf("%d test files passed\n", len(mine)))
		}
	}
	return res
}
