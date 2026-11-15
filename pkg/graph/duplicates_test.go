package graph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #35: three packages called "wisp" (an npm workspace and two Cargo crates) are all kept.
func TestDuplicateNamesAreKept(t *testing.T) {
	g := New(t.TempDir())
	g.AddPackage(&PackageNode{Name: "wisp", Path: ".", Language: LangTypeScript})
	g.AddPackage(&PackageNode{Name: "wisp", Path: "crates/wisp", Language: LangRust})
	g.AddPackage(&PackageNode{Name: "wisp", Path: "crates/cli", Language: LangRust})
	g.AddPackage(&PackageNode{Name: "web", Path: "apps/web", Language: LangTypeScript, Dependencies: []string{"wisp"}})
	g.AddPackage(&PackageNode{Name: "core", Path: "crates/core", Language: LangRust, Dependencies: []string{"wisp"}})
	g.RebuildEdges()

	if n := len(g.GetAllPackages()); n != 5 {
		t.Fatalf("want 5 packages, got %d", n)
	}
	for _, n := range []string{"wisp@root", "wisp@crates/wisp", "wisp@crates/cli"} {
		if _, ok := g.GetPackage(n); !ok {
			t.Errorf("%s missing", n)
		}
	}
	if _, ok := g.GetPackage("wisp"); ok {
		t.Error("the bare name is ambiguous and must not pick one")
	}
	if p, ok := g.GetPackage("crates/cli"); !ok || p.Name != "wisp@crates/cli" {
		t.Errorf("by path: %v", p)
	}
	// A JS dependent links to the npm package, a Rust one to both crates.
	if up := g.GetUpstream("web"); strings.Join(up, ",") != "wisp@root" {
		t.Errorf("web upstream = %v", up)
	}
	if up := g.GetUpstream("core"); strings.Join(up, ",") != "wisp@crates/cli,wisp@crates/wisp" {
		t.Errorf("core upstream = %v", up)
	}
	aff := g.ComputeAffected([]string{"crates/wisp/src/lib.rs"})
	if strings.Join(aff.AllAffected, ",") != "core,wisp@crates/wisp" {
		t.Errorf("affected = %v", aff.AllAffected)
	}
	w := g.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], `3 packages are named "wisp"`) {
		t.Errorf("warnings = %v", w)
	}
}

// #36: global_inputs reach everything, ignore_inputs nothing, anything else unowned is unmapped.
func TestGlobalAndIgnoredInputs(t *testing.T) {
	g := New(t.TempDir())
	g.AddPackage(&PackageNode{Name: "a", Path: "pkgs/a"})
	g.AddPackage(&PackageNode{Name: "b", Path: "pkgs/b"})
	g.RebuildEdges()
	g.Settings = Settings{GlobalInputs: []string{"tools/**", "*.bzl"}, IgnoreInputs: []string{"design/**", "pkgs/b/*.txt"}}

	cases := map[string]struct {
		all, unmapped string
	}{
		"rust-toolchain.toml":   {"a,b", ""},
		"tools/gen/main.sh":     {"a,b", ""},
		"rules/defs.bzl":        {"a,b", ""},
		"design/logo.fig":       {"", ""},
		"pkgs/b/notes.txt":      {"", ""},
		"pkgs/a/x.go":           {"a", ""},
		"scripts/release.sh":    {"", "scripts/release.sh"},
		".github/workflows/x.y": {"a,b", ""},
	}
	for f, want := range cases {
		aff := g.ComputeAffected([]string{f})
		if strings.Join(aff.AllAffected, ",") != want.all || strings.Join(aff.Unmapped, ",") != want.unmapped {
			t.Errorf("%s: all=%v unmapped=%v, want %q / %q", f, aff.AllAffected, aff.Unmapped, want.all, want.unmapped)
		}
	}
	g.Settings.Unmapped = "all"
	if aff := g.ComputeAffected([]string{"scripts/release.sh"}); strings.Join(aff.AllAffected, ",") != "a,b" {
		t.Errorf("unmapped=all: %v", aff.AllAffected)
	}
}

func TestMatchInput(t *testing.T) {
	for _, c := range []struct {
		pat, file string
		want      bool
	}{
		{"docs/**", "docs/a/b.md", true}, {"docs/**", "docsx/a", false}, {"**/BUILD", "a/b/BUILD", true},
		{"**/BUILD", "BUILD", true}, {"*.md", "x/y/README.md", true}, {"Cargo.toml", "crates/a/Cargo.toml", false},
		{".github/**", ".github/workflows/ci.yml", true},
	} {
		if got := MatchInput([]string{c.pat}, c.file); got != c.want {
			t.Errorf("%s ~ %s = %v", c.pat, c.file, got)
		}
	}
}

// A saved config holds the name@path names: loading it keeps them, the warning, and the edges.
func TestDuplicateNamesSurviveConfig(t *testing.T) {
	root := t.TempDir()
	g := New(root)
	g.AddPackage(&PackageNode{Name: "wisp", Path: "crates/wisp", Language: LangRust})
	g.AddPackage(&PackageNode{Name: "wisp", Path: "crates/cli", Language: LangRust})
	g.AddPackage(&PackageNode{Name: "web", Path: "apps/web", Language: LangTypeScript, Dependencies: []string{"wisp"}})
	g.AddPackage(&PackageNode{Name: "core", Path: "crates/core", Language: LangRust, Dependencies: []string{"wisp"}})
	g.RebuildEdges()
	if up := g.GetUpstream("web"); len(up) != 0 {
		t.Errorf("an npm package never depends on a Cargo crate: %v", up)
	}
	if err := g.SaveConfig(root + "/.agents/monoflow.config.json"); err != nil {
		t.Fatal(err)
	}
	g2, err := LoadOrDiscover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(g2.Warnings()) != 1 {
		t.Errorf("warnings after load: %v", g2.Warnings())
	}
	if up := g2.GetUpstream("core"); strings.Join(up, ",") != "wisp@crates/cli,wisp@crates/wisp" {
		t.Errorf("core upstream after load = %v", up)
	}
}

// Manifest edges reach the same-named package of the consumer's ecosystem (review of #35).
func TestScanEdgesWithDuplicateNames(t *testing.T) {
	root := t.TempDir()
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(root, p), []byte(s), 0o644)
	}
	write("package.json", `{"name":"repo","private":true,"workspaces":["packages/*"]}`)
	write("packages/wisp/package.json", `{"name":"wisp","version":"1.0.0"}`)
	write("packages/app/package.json", `{"name":"app","dependencies":{"wisp":"^1.0.0"}}`)
	write("crates/wisp/Cargo.toml", "[package]\nname = \"wisp\"\nversion = \"0.1.0\"\n")
	write("crates/core/Cargo.toml", "[package]\nname = \"core\"\nversion = \"0.1.0\"\n[dependencies]\nwisp = { path = \"../wisp\" }\n")
	g := New(root)
	if err := g.AutoDiscover(); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range g.ScanEdges() {
		got[e.Consumer] = e.Dependency
	}
	if got["app"] != "wisp@packages/wisp" || got["core"] != "wisp@crates/wisp" {
		t.Errorf("edges = %v", got)
	}
	if p, _ := g.GetPackage("wisp@crates/wisp"); p.RawName() != "wisp" {
		t.Errorf("raw name = %q", p.RawName())
	}
}
