package verify

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/antigravity/monoflow/pkg/cache"
	"github.com/antigravity/monoflow/pkg/graph"
)

// Batched checks. Starting a check once per package pays its startup again each time: cargo's
// lock, startup and test-binary link (wisp, 149 crates: 23 of 33 CI minutes), tsc and vitest
// (wisp, 46 npm packages: 128.6 s per package, 38.5 s batched). Packages whose check is the
// discovered default command of a kind that can check several packages in one process
// (cargo check/clippy/test -p, root-scoped vitest, tsc -p) run it once for all of them; the
// output is split back per package, so results, retries and the cache stay per package.
//
// Conservative by design: only default commands are batched (a configured command runs as
// configured), only packages without a cached pass, only groups of two or more, and a package
// whose result the output cannot attribute for certain runs on its own as before.

// batchOutcome is one package's result of a check run for several packages at once.
type batchOutcome struct {
	out   []byte
	err   error             // non-nil: the check failed for this package
	diags []DiagnosticError // the package's diagnostics (nil: parse out)
}

var errBatchFailed = errors.New("check failed (batched run)")

// batchMember is one package's command in a batch.
type batchMember struct {
	pkg *graph.PackageNode
	cmd string
	abs string // package directory
	idx int    // index in VerifyPackages' names
}

// batchKind is a check that can run for several packages in one process.
type batchKind struct {
	name string
	// group returns the batch group of pkg's command c, "" when c is not batchable. Members of
	// one group run in one process.
	group func(r *Runner, pkg *graph.PackageNode, c string, opt VerifyOptions) string
	// job returns the job a group belongs to: a job runs its groups one after the other, in
	// kind order, as VerifyTarget runs one package's checks.
	job func(group string) string
	// run checks the members and returns the outcome of every package it can attribute; the
	// others are checked on their own afterwards.
	run func(r *Runner, group string, members []batchMember, opt VerifyOptions) map[string]batchOutcome
}

// batchKinds in check order (typecheck, lint, test).
var batchKinds = []batchKind{
	cargoBatchKind("check"),
	tscBatchKind,
	cargoBatchKind("clippy"),
	cargoBatchKind("test"),
	vitestBatchKind,
}

type batchStage struct {
	kind    *batchKind
	group   string
	members []batchMember
}

// batchJob is a set of packages checked together: its stages run in order, then each package's
// VerifyTarget picks up its batched results.
type batchJob struct {
	idx    []int
	stages []batchStage
}

func (r *Runner) putBatched(name, c string, o batchOutcome) {
	r.batchMu.Lock()
	defer r.batchMu.Unlock()
	if r.batched == nil {
		r.batched = map[string]map[string]batchOutcome{}
	}
	if r.batched[name] == nil {
		r.batched[name] = map[string]batchOutcome{}
	}
	r.batched[name][c] = o
}

func (r *Runner) takeBatched(name, c string) (batchOutcome, bool) {
	r.batchMu.Lock()
	defer r.batchMu.Unlock()
	o, ok := r.batched[name][c]
	if ok {
		delete(r.batched[name], c)
	}
	return o, ok
}

// dropBatched forgets a package's batched results (a re-run of the package runs its checks).
func (r *Runner) dropBatched(name string) {
	r.batchMu.Lock()
	defer r.batchMu.Unlock()
	delete(r.batched, name)
}

// planBatches groups the batchable checks of the named packages that have to run.
func (r *Runner) planBatches(g *graph.DependencyGraph, names []string, opt VerifyOptions) []*batchJob {
	if os.Getenv("MONOFLOW_NO_BATCH") != "" || len(names) < 2 {
		return nil
	}
	type stageKey struct {
		kind  int
		group string
	}
	stages := map[stageKey][]batchMember{}
	for i, name := range names {
		pkg, ok := g.GetPackage(name)
		if !ok {
			continue
		}
		abs := filepath.Join(r.RootDir, pkg.Path)
		if _, err := os.Stat(abs); err != nil {
			continue // VerifyTarget reports it as not projected
		}
		if r.cachedPass(pkg, opt) {
			continue
		}
		cmds, unaffected := r.commandsFor(pkg, opt)
		if unaffected {
			continue
		}
		for _, c := range cmds {
			if ck := r.commandKey(pkg, c, opt); ck != "" {
				if rec, hit := r.cacheMgr.PeekVerification(ck); hit && rec.Status == "passed" {
					continue
				}
			}
			for k := range batchKinds {
				if grp := batchKinds[k].group(r, pkg, c, opt); grp != "" {
					key := stageKey{k, grp}
					stages[key] = append(stages[key], batchMember{pkg: pkg, cmd: c, abs: abs, idx: i})
					break
				}
			}
		}
	}
	var keys []stageKey
	for k, m := range stages {
		if len(m) >= 2 {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(a, b int) bool {
		if keys[a].kind != keys[b].kind {
			return keys[a].kind < keys[b].kind
		}
		return keys[a].group < keys[b].group
	})
	jobs := map[string]*batchJob{}
	var order []string
	owner := map[int]string{} // package index -> its job
	for _, k := range keys {
		kind := &batchKinds[k.kind]
		jk := kind.job(k.group)
		var members []batchMember
		for _, m := range stages[k] {
			if j, taken := owner[m.idx]; taken && j != jk {
				continue // a package is checked by one job
			}
			members = append(members, m)
		}
		if len(members) < 2 {
			continue
		}
		job := jobs[jk]
		if job == nil {
			job = &batchJob{}
			jobs[jk] = job
			order = append(order, jk)
		}
		for _, m := range members {
			if _, taken := owner[m.idx]; !taken {
				owner[m.idx] = jk
				job.idx = append(job.idx, m.idx)
			}
		}
		job.stages = append(job.stages, batchStage{kind: kind, group: k.group, members: members})
	}
	out := make([]*batchJob, 0, len(order))
	for _, jk := range order {
		sort.Ints(jobs[jk].idx)
		out = append(out, jobs[jk])
	}
	return out
}

// runBatchJob runs the job's stages, leaving each package's outcomes for its VerifyTarget.
func (r *Runner) runBatchJob(job *batchJob, opt VerifyOptions) {
	pkgs := map[string]*graph.PackageNode{}
	for _, st := range job.stages {
		for _, m := range st.members {
			pkgs[m.pkg.Name] = m.pkg
		}
	}
	var names []string
	for n := range pkgs {
		names = append(names, n)
	}
	sort.Strings(names) // one lock order for every run
	var unlocks []func()
	for _, n := range names {
		unlocks = append(unlocks, r.lockPackage(pkgs[n]))
	}
	defer func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}()
	failed := map[string]bool{}
	for _, st := range job.stages {
		var members []batchMember
		for _, m := range st.members {
			if !failed[m.pkg.Name] { // VerifyTarget stops at a package's first failing check
				members = append(members, m)
			}
		}
		r.runBatchStage(st, members, opt, failed)
	}
}

// runBatchStage runs one group's batch. When some packages failed and others could not be
// attributed (cargo stops at a crate that does not compile), it runs once more for those.
func (r *Runner) runBatchStage(st batchStage, members []batchMember, opt VerifyOptions, failed map[string]bool) {
	for attempt := 0; attempt < 2 && len(members) >= 2; attempt++ {
		outs := st.kind.run(r, st.group, members, opt)
		var rest []batchMember
		anyFailed := false
		for _, m := range members {
			o, ok := outs[m.pkg.Name]
			if !ok {
				rest = append(rest, m)
				continue
			}
			r.putBatched(m.pkg.Name, m.cmd, o)
			if o.err != nil {
				failed[m.pkg.Name], anyFailed = true, true
			} else if ck := r.commandKey(m.pkg, m.cmd, opt); ck != "" {
				_ = r.cacheMgr.PutVerification(&cache.VerificationRecord{
					Key: ck, Target: m.pkg.Name, Language: string(m.pkg.Language), Status: "passed",
					CommandRun: m.cmd, Timestamp: time.Now().UTC(),
				})
			}
		}
		if !anyFailed || len(rest) == len(members) {
			return
		}
		members = rest
	}
}

// runBatchCommand runs c in dir and returns its combined output and whether it exited 0.
func runBatchCommand(c, dir string) ([]byte, bool) {
	cmd := exec.Command("sh", "-c", c)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return out, err == nil
}

// failedBatch is a failed outcome with the package's own output.
func failedBatch(out string, diags []DiagnosticError) batchOutcome {
	return batchOutcome{out: []byte(out), err: errBatchFailed, diags: diags}
}

func passedBatch(out string) batchOutcome { return batchOutcome{out: []byte(out)} }

// noteBatch ends each failed package's output with the batched command that checked it.
func noteBatch(outs map[string]batchOutcome, c string, n int) map[string]batchOutcome {
	note := fmt.Sprintf("(monoflow: checked with %d packages in one run: %s)\n", n, c)
	for name, o := range outs {
		if o.err != nil {
			o.out = append(o.out, note...)
			outs[name] = o
		}
	}
	return outs
}
