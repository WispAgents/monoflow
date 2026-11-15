package verify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/antigravity/monoflow/pkg/graph"
)

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func crateNode(name, dir string) *graph.PackageNode {
	return &graph.PackageNode{
		Name: name, Path: dir, Language: graph.LangRust,
		TestCmd: cargoDefaultCmd("test", name), LintCmd: cargoDefaultCmd("clippy", name), TypecheckCmd: cargoDefaultCmd("check", name),
	}
}

func TestPlanBatchesGroupsCargoWorkspaceCrates(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "rust")
	writeFile(t, filepath.Join(ws, "Cargo.toml"), "[workspace]\nmembers = [\"crates/*\"]\n")
	for _, c := range []string{"a", "b", "custom"} {
		writeFile(t, filepath.Join(ws, "crates", c, "Cargo.toml"), "[package]\nname = \""+c+"\"\n")
	}
	writeFile(t, filepath.Join(root, "solo", "Cargo.toml"), "[package]\nname = \"solo\"\n") // no workspace
	g := graph.New(root)
	g.AddPackage(crateNode("a", "rust/crates/a"))
	g.AddPackage(crateNode("b", "rust/crates/b"))
	custom := crateNode("custom", "rust/crates/custom")
	custom.TestCmd = "cargo nextest run -p custom" // configured: runs as configured
	g.AddPackage(custom)
	g.AddPackage(crateNode("solo", "solo"))
	g.AddPackage(crateNode("missing", "rust/crates/missing")) // not in this worktree

	r := &Runner{RootDir: root}
	names := []string{"a", "solo", "b", "custom", "missing"}
	jobs := r.planBatches(g, names, VerifyOptions{})
	if len(jobs) != 1 {
		t.Fatalf("want one cargo job, got %d", len(jobs))
	}
	job := jobs[0]
	var kinds []string
	for _, st := range job.stages {
		var members []string
		for _, m := range st.members {
			members = append(members, m.pkg.Name)
		}
		sort.Strings(members)
		kinds = append(kinds, st.kind.name+":"+strings.Join(members, ","))
		if st.group != ws {
			t.Errorf("group %q, want the workspace root", st.group)
		}
	}
	want := "cargo check:a,b,custom|cargo clippy:a,b,custom|cargo test:a,b"
	if got := strings.Join(kinds, "|"); got != want {
		t.Errorf("stages = %s, want %s", got, want)
	}
	if got := job.idx; len(got) != 3 || got[0] != 0 || got[1] != 2 || got[2] != 3 {
		t.Errorf("job packages = %v, want [0 2 3]", got)
	}

	// A single package, or MONOFLOW_NO_BATCH, batches nothing.
	if jobs := r.planBatches(g, []string{"a"}, VerifyOptions{}); len(jobs) != 0 {
		t.Errorf("single package batched: %d jobs", len(jobs))
	}
	t.Setenv("MONOFLOW_NO_BATCH", "1")
	if jobs := r.planBatches(g, names, VerifyOptions{}); len(jobs) != 0 {
		t.Errorf("MONOFLOW_NO_BATCH ignored")
	}
}

func TestCargoWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "rust", "Cargo.toml"), "[workspace]\nmembers = [\"*\"]\n")
	writeFile(t, filepath.Join(root, "rust", "a", "Cargo.toml"), "[package]\nname = \"a\"\n")
	writeFile(t, filepath.Join(root, "rust", "b", "Cargo.toml"), "[package]\nname = \"b\"\n")
	writeFile(t, filepath.Join(root, "rust", "b", ".cargo", "config.toml"), "[build]\n")
	writeFile(t, filepath.Join(root, "own", "Cargo.toml"), "[package]\nname = \"own\"\n\n[workspace]\n")
	if got := cargoWorkspaceRoot(root, filepath.Join(root, "rust", "a")); got != filepath.Join(root, "rust") {
		t.Errorf("a: %q", got)
	}
	if got := cargoWorkspaceRoot(root, filepath.Join(root, "rust", "b")); got != "" {
		t.Errorf("b has its own cargo config, must not batch: %q", got)
	}
	if got := cargoWorkspaceRoot(root, filepath.Join(root, "own")); got != filepath.Join(root, "own") {
		t.Errorf("own: %q", got)
	}
	if got := cargoWorkspaceRoot(root, filepath.Join(root, "nowhere")); got != "" {
		t.Errorf("no workspace: %q", got)
	}
}

func cargoMeta(t *testing.T, root string, all []string) cargoMetadata {
	t.Helper()
	type target struct {
		Name    string   `json:"name"`
		Kind    []string `json:"kind"`
		Doctest bool     `json:"doctest"`
	}
	type pkg struct {
		ID           string   `json:"id"`
		Name         string   `json:"name"`
		ManifestPath string   `json:"manifest_path"`
		Targets      []target `json:"targets"`
	}
	var meta struct {
		WorkspaceRoot    string   `json:"workspace_root"`
		WorkspaceMembers []string `json:"workspace_members"`
		Packages         []pkg    `json:"packages"`
	}
	meta.WorkspaceRoot = root
	for _, n := range all {
		id := "path+file://" + root + "/crates/" + n + "#0.1.0"
		meta.WorkspaceMembers = append(meta.WorkspaceMembers, id)
		meta.Packages = append(meta.Packages, pkg{ID: id, Name: n, ManifestPath: filepath.Join(root, "crates", n, "Cargo.toml"),
			Targets: []target{{Name: n, Kind: []string{"lib"}, Doctest: true}}})
	}
	data, _ := json.Marshal(meta)
	var m cargoMetadata
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func cargoMembers(root string, names ...string) []batchMember {
	var ms []batchMember
	for _, n := range names {
		ms = append(ms, batchMember{pkg: crateNode(n, "crates/"+n), abs: filepath.Join(root, "crates", n)})
	}
	return ms
}

func TestCargoSelect(t *testing.T) {
	root := t.TempDir()
	meta := cargoMeta(t, root, []string{"a", "b", "c"})
	sel, crates := cargoSelect(meta, cargoMembers(root, "c", "a"))
	if got := strings.Join(sel, " "); got != "-p 'a' -p 'c'" {
		t.Errorf("selection %q", got)
	}
	if len(crates.byManifest) != 2 || crates.doc["a"][0] != "a" {
		t.Errorf("crates %+v", crates)
	}
	sel, _ = cargoSelect(meta, cargoMembers(root, "a", "b", "c"))
	if got := strings.Join(sel, " "); got != "--workspace" {
		t.Errorf("every member affected: %q, want --workspace", got)
	}
	if got := cargoBatchCommand("clippy", []string{"--workspace"}); got != "cargo clippy --workspace --keep-going --message-format=json -- -D warnings" {
		t.Errorf("clippy command %q", got)
	}
}

func cargoJSON(t *testing.T, v map[string]any) string {
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func testCrates(root string, names ...string) cargoCrates {
	c := cargoCrates{byManifest: map[string]string{}, build: map[string][]string{}, doc: map[string][]string{}}
	for _, n := range names {
		c.byManifest[filepath.Join(root, "crates", n, "Cargo.toml")] = n
		c.build[n] = []string{n}
		c.doc[n] = []string{n}
	}
	return c
}

func TestSplitCargoBuild(t *testing.T) {
	root := "/ws"
	crates := testCrates(root, "a", "b", "c", "d")
	man := func(n string) string { return filepath.Join(root, "crates", n, "Cargo.toml") }
	artifact := func(n string) string {
		return cargoJSON(t, map[string]any{"reason": "compiler-artifact", "manifest_path": man(n), "target": map[string]any{"name": n, "kind": []string{"lib"}}})
	}
	out := strings.Join([]string{
		"    Checking a v0.1.0 (/ws/crates/a)",
		cargoJSON(t, map[string]any{"reason": "compiler-message", "manifest_path": man("c"), "message": map[string]any{"level": "error",
			"rendered": "error: equal expressions as operands to `==`\n --> crates/c/src/lib.rs:1:27\n"}}),
		cargoJSON(t, map[string]any{"reason": "compiler-message", "manifest_path": man("a"), "message": map[string]any{"level": "warning",
			"rendered": "warning: unused variable: `x`\n --> crates/a/src/lib.rs:2:9\n"}}),
		"error: could not compile `c` (lib) due to 1 previous error",
		artifact("a"),
		artifact("b"),
		// d depends on c: never built, so not attributable
		cargoJSON(t, map[string]any{"reason": "build-finished", "success": false}),
	}, "\n")
	res := splitCargoBuild([]byte(out), crates)
	if o, ok := res["c"]; !ok || o.err == nil {
		t.Fatalf("c must fail: %+v", res["c"])
	} else if errs := ParseDiagnostics(string(o.out)); len(errs) == 0 || errs[0].File != "crates/c/src/lib.rs" || errs[0].Line != 1 {
		t.Errorf("c's diagnostics %+v from %q", errs, o.out)
	}
	for _, n := range []string{"a", "b"} {
		if o, ok := res[n]; !ok || o.err != nil {
			t.Errorf("%s must pass: %+v", n, o)
		}
	}
	if !strings.Contains(string(res["a"].out), "unused variable") || strings.Contains(string(res["b"].out), "unused") {
		t.Errorf("warnings belong to their crate: a=%q b=%q", res["a"].out, res["b"].out)
	}
	if _, ok := res["d"]; ok {
		t.Errorf("d was blocked by c, it must run on its own")
	}

	// No build-finished (cargo could not start, e.g. an unknown flag): nothing attributed.
	if res := splitCargoBuild([]byte("error: unexpected argument '--keep-going' found\n"), crates); len(res) != 0 {
		t.Errorf("unfinished build attributed: %+v", res)
	}
	// A successful build passes every crate.
	ok := splitCargoBuild([]byte(cargoJSON(t, map[string]any{"reason": "build-finished", "success": true})), crates)
	if len(ok) != 4 {
		t.Errorf("success must pass all: %+v", ok)
	}
}

func TestSplitCargoTest(t *testing.T) {
	root := "/ws"
	crates := testCrates(root, "a", "b", "c")
	man := func(n string) string { return filepath.Join(root, "crates", n, "Cargo.toml") }
	exe := func(n, bin, hash string) string {
		return cargoJSON(t, map[string]any{"reason": "compiler-artifact", "manifest_path": man(n), "target": map[string]any{"name": bin, "kind": []string{"lib"}},
			"profile": map[string]any{"test": true}, "executable": "/ws/target/debug/deps/" + bin + "-" + hash})
	}
	lines := []string{
		exe("a", "a", "1111"),
		exe("b", "b", "2222"),
		exe("c", "c", "3333"),
		exe("c", "it", "4444"),
		cargoJSON(t, map[string]any{"reason": "build-finished", "success": true}),
		"    Finished `test` profile [unoptimized + debuginfo] target(s) in 0.30s",
		"     Running unittests src/lib.rs (target/debug/deps/a-1111)",
		"", "running 2 tests", "test t::good ... ok", "test t::bad ... FAILED", "",
		"---- t::bad stdout ----", "thread 't::bad' panicked at crates/a/src/lib.rs:6:24:", "assertion `left == right` failed",
		"", "test result: FAILED. 1 passed; 1 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.00s", "",
		"error: test failed, to rerun pass `-p a --lib`",
		"     Running unittests src/lib.rs (target/debug/deps/b-2222)",
		"", "running 1 test", "test ok ... ok", "", "test result: ok. 1 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.00s", "",
		"     Running unittests src/lib.rs (target/debug/deps/c-3333)",
		"", "test result: ok. 0 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.00s", "",
		"     Running tests/it.rs (target/debug/deps/it-4444)",
		"", "test result: ok. 1 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.00s", "",
		"   Doc-tests a", "", "test result: ok. 1 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.20s", "",
		"   Doc-tests b", "", "test result: ok. 0 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.00s", "",
		// c's doc tests never ran (output cut short): c cannot be judged
		"error: 1 target failed:", "    `-p a --lib`",
	}
	res := splitCargoTest([]byte(strings.Join(lines, "\n")), crates)
	a, ok := res["a"]
	if !ok || a.err == nil {
		t.Fatalf("a must fail: %+v", res)
	}
	if !strings.Contains(string(a.out), "t::bad") || strings.Contains(string(a.out), "test ok ... ok") {
		t.Errorf("a's output must be a's sections only: %q", a.out)
	}
	if b, ok := res["b"]; !ok || b.err != nil {
		t.Errorf("b must pass: %+v", b)
	}
	if _, ok := res["c"]; ok {
		t.Errorf("c's doc tests did not run: it must run on its own")
	}

	// A crate that does not compile fails; the others did not run.
	compile := strings.Join([]string{
		cargoJSON(t, map[string]any{"reason": "compiler-message", "manifest_path": man("c"), "message": map[string]any{"level": "error",
			"rendered": "error[E0308]: mismatched types\n --> crates/c/tests/it.rs:1:30\n"}}),
		cargoJSON(t, map[string]any{"reason": "build-finished", "success": false}),
	}, "\n")
	res = splitCargoTest([]byte(compile), crates)
	if c, ok := res["c"]; !ok || c.err == nil || len(res) != 1 {
		t.Errorf("only c is known (failed): %+v", res)
	}
}

func TestSplitTscBuild(t *testing.T) {
	root := t.TempDir()
	mk := func(name, dir string) batchMember {
		abs := filepath.Join(root, dir)
		writeFile(t, filepath.Join(abs, "tsconfig.json"), "{}")
		return batchMember{pkg: &graph.PackageNode{Name: name, Path: dir}, abs: abs}
	}
	projects := map[string]batchMember{}
	for _, m := range []batchMember{mk("a", "packages/a"), mk("b", "packages/b"), mk("c", "packages/c"), mk("d", "packages/d")} {
		projects[resolved(filepath.Join(m.abs, "tsconfig.json"))] = m
	}
	out := strings.Join([]string{
		"17:33:05 - Projects in this build: ",
		"    * packages/a/tsconfig.json",
		"    * packages/b/tsconfig.json",
		"",
		"17:33:05 - Project 'packages/a/tsconfig.json' is out of date because output file 'packages/a/tsconfig.tsbuildinfo' does not exist",
		"17:33:05 - Building project '" + filepath.Join(root, "packages/a/tsconfig.json") + "'...",
		"packages/a/src/i.ts(1,14): error TS2322: Type 'string' is not assignable to type 'number'.",
		"packages/shared/x.ts(3,1): error TS2304: Cannot find name 'y'.",
		"17:33:05 - Project 'packages/b/tsconfig.json' is out of date because output file 'packages/b/tsconfig.tsbuildinfo' does not exist",
		"17:33:05 - Building project '" + filepath.Join(root, "packages/b/tsconfig.json") + "'...",
		"",
		"17:33:05 - Project 'packages/c/tsconfig.json' is up to date because newest input 'packages/c/i.ts' is older than output 'packages/c/tsconfig.tsbuildinfo'",
	}, "\n")
	res := splitTscBuild(out, root, projects, false)
	a, ok := res["a"]
	if !ok || a.err == nil || len(a.diags) != 2 {
		t.Fatalf("a must fail with 2 errors: %+v", a)
	}
	if a.diags[0].File != "src/i.ts" || a.diags[1].File != "../shared/x.ts" {
		t.Errorf("paths must be package-relative like tsc -p: %q %q", a.diags[0].File, a.diags[1].File)
	}
	if b, ok := res["b"]; !ok || b.err != nil {
		t.Errorf("b built without errors: %+v", b)
	}
	if c, ok := res["c"]; !ok || c.err != nil {
		t.Errorf("c is up to date: %+v", c)
	}
	if _, ok := res["d"]; ok {
		t.Errorf("d never built: it must run on its own")
	}

	// An error outside any project (a bad option) leaves the projects without errors undecided.
	global := "error TS5023: Unknown compiler option '--foo'.\n17:33:05 - Building project '" + filepath.Join(root, "packages/b/tsconfig.json") + "'...\n"
	if res := splitTscBuild(global, root, projects, false); len(res) != 0 {
		t.Errorf("global error attributed: %+v", res)
	}
	if res := splitTscBuild("", root, projects, true); len(res) != 4 {
		t.Errorf("a clean exit passes every project: %+v", res)
	}
}

func TestSplitVitest(t *testing.T) {
	root := "/repo"
	files := map[string]string{
		"/repo/packages/a/src/x.test.ts":  "failed",
		"/repo/packages/a/src/y.test.ts":  "passed",
		"/repo/packages/ab/src/z.test.ts": "passed",
		"/repo/packages/b/src/b.test.ts":  "passed",
	}
	diags := ParseDiagnostics(strings.Join([]string{
		" FAIL  packages/a/src/x.test.ts > suite > adds",
		"AssertionError: expected 1 to be 2",
		" ❯ packages/a/src/x.test.ts:12:5",
	}, "\n"))
	targets := []vitestTarget{
		{name: "a", dir: "packages/a", filters: []string{"packages/a"}, full: true},
		{name: "ab", dir: "packages/ab", filters: []string{"packages/ab/src/z.test.ts"}},
		{name: "b", dir: "packages/b", filters: []string{"packages/b"}, full: true},
		{name: "empty", dir: "packages/empty", filters: []string{"packages/empty"}, full: true},
	}
	res := splitVitest(files, diags, root, targets, false, true, false)
	a, ok := res["a"]
	if !ok || a.err == nil || len(a.diags) != 1 || a.diags[0].File != "packages/a/src/x.test.ts" {
		t.Fatalf("a must fail with its test's error: %+v", a)
	}
	for _, n := range []string{"ab", "b", "empty"} {
		if o, ok := res[n]; !ok || o.err != nil {
			t.Errorf("%s must pass: %+v", n, o)
		}
	}
	// Without --passWithNoTests a package without test files fails alone: judge it alone.
	if res := splitVitest(files, diags, root, targets, false, false, false); res["empty"].out != nil {
		t.Errorf("empty package without --passWithNoTests attributed")
	}
	// Errors outside test files: only the packages with failed files are known.
	res = splitVitest(files, diags, root, targets, false, true, true)
	if _, ok := res["b"]; ok || res["a"].err == nil {
		t.Errorf("unhandled errors: %+v", res)
	}
	// A failing exit with no failed file (coverage threshold, crash): nothing is known.
	pass := map[string]string{"/repo/packages/b/src/b.test.ts": "passed"}
	if res := splitVitest(pass, nil, root, targets, false, true, false); len(res) != 0 {
		t.Errorf("unexplained failure attributed: %+v", res)
	}
	// A failed file whose error cannot be placed: run alone.
	if res := splitVitest(files, nil, root, targets, false, true, false); res["a"].err != nil {
		t.Errorf("failure without diagnostics attributed")
	}
}

func TestVersionAtLeast(t *testing.T) {
	for v, want := range map[string]bool{"5.6.2": true, "5.9.3": true, "6.0.0-beta": true, "5.5.4": false, "4.9.5": false, "": false} {
		if got := versionAtLeast(v, 5, 6); got != want {
			t.Errorf("%q: %v", v, got)
		}
	}
}

func TestTscBatchable(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "node_modules", "typescript", "package.json"), `{"version":"5.9.3"}`)
	writeFile(t, filepath.Join(root, "a", "tsconfig.json"), `{"compilerOptions":{"strict":true}}`)
	writeFile(t, filepath.Join(root, "refs", "tsconfig.json"), `{"references":[{"path":"../a"}]}`)
	writeFile(t, filepath.Join(root, "own", "tsconfig.json"), `{}`)
	writeFile(t, filepath.Join(root, "own", "node_modules", "typescript", "package.json"), `{"version":"5.4.0"}`)
	if !tscBatchable(root, filepath.Join(root, "a")) {
		t.Errorf("a is batchable")
	}
	if tscBatchable(root, filepath.Join(root, "refs")) {
		t.Errorf("a project with references is not")
	}
	if tscBatchable(root, filepath.Join(root, "own")) {
		t.Errorf("a package with its own TypeScript is not")
	}
	writeFile(t, filepath.Join(root, "node_modules", "typescript", "package.json"), `{"version":"5.4.5"}`)
	if tscBatchable(root, filepath.Join(root, "a")) {
		t.Errorf("TypeScript < 5.6 is not")
	}
}

func TestSnapshotFilesRestoresBuildInfo(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "dist", "tsconfig.tsbuildinfo")
	writeFile(t, keep, "old")
	restore := snapshotFiles([]string{dir}, ".tsbuildinfo")
	writeFile(t, keep, "new")
	writeFile(t, filepath.Join(dir, "tsconfig.tsbuildinfo"), "created")
	restore()
	if data, _ := os.ReadFile(keep); string(data) != "old" {
		t.Errorf("existing buildinfo not restored: %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "tsconfig.tsbuildinfo")); err == nil {
		t.Errorf("created buildinfo not removed")
	}
}

func TestBatchedOutcomeUsedOnceByVerifyTarget(t *testing.T) {
	r := &Runner{RootDir: t.TempDir()}
	r.putBatched("a", "cargo test -p a", batchOutcome{out: []byte("x")})
	if _, ok := r.takeBatched("a", "cargo test -p a"); !ok {
		t.Fatal("outcome missing")
	}
	if _, ok := r.takeBatched("a", "cargo test -p a"); ok {
		t.Error("an outcome is used once")
	}
	r.putBatched("a", "cargo check -p a", batchOutcome{})
	r.dropBatched("a")
	if _, ok := r.takeBatched("a", "cargo check -p a"); ok {
		t.Error("dropBatched left an outcome")
	}
}

func TestVerifyTargetUsesBatchedFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "crates", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{RootDir: root}
	pkg := crateNode("a", "crates/a")
	pkg.LintCmd, pkg.TestCmd = "", ""
	diag := DiagnosticError{File: "src/i.ts", Line: 1, Code: "TS2322", Message: "bad"}
	r.putBatched("a", pkg.TypecheckCmd, failedBatch("whatever", []DiagnosticError{diag}))
	res := r.VerifyTarget(pkg, VerifyOptions{NoCache: true})
	if res.Status != "failed" || len(res.Errors) != 1 || res.Errors[0] != diag || res.CommandRun != pkg.TypecheckCmd {
		t.Fatalf("batched failure not reported as the package's: %+v", res)
	}
}
