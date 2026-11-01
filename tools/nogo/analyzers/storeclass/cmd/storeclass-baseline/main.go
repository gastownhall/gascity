// Command storeclass-baseline regenerates the storeclass lint's baseline
// (tools/nogo/analyzers/storeclass/baseline.txt), its ceiling (ceiling.txt)
// and allowed.txt from the tree. It type-checks every production package of
// the gascity module with the analyzer's Default configuration, unfiltered
// by the baseline, and counts the findings per file, enclosing function and
// rule: the debt baseline.txt pins, and, in allowed.txt, the findings an
// allowlist entry for their file exempts from nogo. It does so once untagged
// and once per custom build tag a production file names (integration,
// productmetrics_testhook, ...), because nogo analyzes a package under every
// configuration that compiles it, and keeps each key's count from the
// variant that compiles its file.
//
// The baseline may only shrink. Without a flag it prints the census and
// whether the files on disk match it. -write rewrites the three files, and
// refuses, writing nothing, when any package's total for a rule would rise
// above ceiling.txt, when a package/rule pair would appear that ceiling.txt
// lacks, or when a Sealed package would get a row. -init writes the first
// ceiling and refuses once one exists. Every mode fails on an allowlist entry
// for a package the module does not have: the allowlist names each package,
// and a stale entry would exempt whatever package lands at its path later.
//
// -growth-since <ref> (CI passes the pull request's merge-base) also fails
// on any new or raised baseline, allowed.txt or ceiling row against ref, and
// on any change to the lint's protected paths or to the nogo wiring that
// runs it (MODULE.bazel's rules_go override and go_sdk.nogo registration,
// config.json's _base and storeclass scopes), unless -allow-raise: the
// storeclass/baseline-raise label (see the storeclass package doc for the
// raise procedure). A ref that predates the gate (it has no baseline) is
// judged by -base-tip instead, the base branch's tip: a change cut before
// the gate landed does not get to introduce it. Only when the tip has no
// baseline either does the change introduce the gate and pass.
//
// Run it from tools/nogo:
//
//	go run ./analyzers/storeclass/cmd/storeclass-baseline -write
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/build/constraint"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/storeclass"
)

func main() {
	repo := flag.String("repo", "", "the gascity checkout (default: the git toplevel)")
	write := flag.Bool("write", false, "rewrite baseline.txt, ceiling.txt and allowed.txt; refuse any baseline growth")
	initial := flag.Bool("init", false, "write the first ceiling.txt; refuse when one has rows")
	since := flag.String("growth-since", "", "also fail on any baseline, allowed.txt or ceiling raise against this git ref (the PR's merge-base)")
	tip := flag.String("base-tip", "", "with -growth-since: the base branch's tip, judged against when the -growth-since ref has no baseline")
	allowRaise := flag.Bool("allow-raise", false, "with -growth-since: report raises but pass (the PR carries the storeclass/baseline-raise label)")
	gateOnly := flag.Bool("gate-only", false, "with -growth-since: skip the census; only compare against the ref")
	flag.Parse()
	if !*gateOnly {
		if err := run(*repo, *write, *initial, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "storeclass-baseline:", err)
			os.Exit(1)
		}
	}
	if *since == "" {
		return
	}
	if err := gate(*repo, *since, *tip, *allowRaise, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "storeclass-baseline:", err)
		os.Exit(1)
	}
}

func run(repo string, write, initial bool, out io.Writer) error {
	repo, err := checkout(repo)
	if err != nil {
		return err
	}
	dir := filepath.Join(repo, "tools", "nogo", "analyzers", "storeclass")
	tl, pkgs, err := census(repo)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "%s\nallowlisted files: %d findings in %d rows (allowed.txt)\n", summary(tl.rows), countFindings(tl.allowed), len(tl.allowed)); err != nil {
		return err
	}
	if stale := staleAllows(storeclass.Default.Allow, pkgs); len(stale) > 0 {
		return fmt.Errorf("the allowlist (config.go) names packages the module does not have; drop them:\n  %s", strings.Join(stale, "\n  "))
	}

	ceilingPath := filepath.Join(dir, "ceiling.txt")
	ceilingText, err := os.ReadFile(ceilingPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ceiling, err := storeclass.ParseCeiling(string(ceilingText))
	if err != nil {
		return err
	}
	if initial && len(ceiling) > 0 {
		return errors.New("-init: ceiling.txt already has rows; the ceiling only ever lowers")
	}
	refusals := growth(tl.rows, ceiling, storeclass.Default.Sealed, initial)
	files := []censusFile{
		{filepath.Join(dir, "baseline.txt"), storeclass.FormatBaseline(tl.rows)},
		{ceilingPath, storeclass.FormatCeiling(storeclass.Totals(tl.rows))},
		{filepath.Join(dir, "allowed.txt"), storeclass.FormatAllowed(tl.allowed)},
	}
	if !write && !initial {
		return check(out, refusals, matchesDisk(files))
	}
	if len(refusals) > 0 {
		return fmt.Errorf("refusing to grow the baseline; nothing written:\n  %s", strings.Join(refusals, "\n  "))
	}
	for _, f := range files {
		if err := os.WriteFile(f.path, []byte(f.text), 0o644); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(out, "wrote %s, %s and %s\n", files[0].path, files[1].path, files[2].path)
	return err
}

// censusFile is one file the generator writes, and its text from the census.
type censusFile struct{ path, text string }

// matchesDisk reports whether each file on disk holds exactly its text.
func matchesDisk(files []censusFile) bool {
	for _, f := range files {
		onDisk, err := os.ReadFile(f.path)
		if err != nil || !bytes.Equal(onDisk, []byte(f.text)) {
			return false
		}
	}
	return true
}

// check reports, writing nothing, the growth the census would refuse and
// whether the files on disk match it, which decides the result.
func check(out io.Writer, refusals []string, matches bool) error {
	var b strings.Builder
	for _, r := range refusals {
		fmt.Fprintln(&b, "GROWTH:", r)
	}
	if matches {
		b.WriteString("baseline.txt, ceiling.txt and allowed.txt match the tree\n")
	}
	if _, err := io.WriteString(out, b.String()); err != nil {
		return err
	}
	if matches {
		return nil
	}
	return errors.New("baseline.txt, ceiling.txt or allowed.txt does not match the tree: rerun with -write")
}

// checkout is repo, or the git toplevel of the working directory when repo
// is "".
func checkout(repo string) (string, error) {
	if repo != "" {
		return repo, nil
	}
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("finding the checkout: %w", err)
	}
	return strings.TrimSpace(string(top)), nil
}

// tally is a census's findings per key: rows, the debt baseline.txt pins,
// and allowed, the findings an Allow entry for their file exempts from nogo,
// which allowed.txt pins so that the growth gate still judges them.
type tally struct {
	rows, allowed map[storeclass.Key]int
}

func newTally() tally {
	return tally{rows: map[storeclass.Key]int{}, allowed: map[storeclass.Key]int{}}
}

// add counts one collected finding where it belongs.
func (t tally) add(f storeclass.Finding) {
	if f.Allowed {
		t.allowed[f.Key]++
		return
	}
	t.rows[f.Key]++
}

// census type-checks the module's production packages once per build-tag
// variant and tallies the findings per key. nogo analyzes a package under
// every configuration that compiles it (a gotags test or binary compiles its
// dependencies with the tag too), so the tally holds the union: each key's
// count from whichever variant compiles its file. pkgs is every module
// package a variant loaded, module-relative.
func census(repo string) (tally, map[string]bool, error) {
	tags, err := repoBuildTags(repo)
	if err != nil {
		return tally{}, nil, err
	}
	merged := newTally()
	pkgs := map[string]bool{}
	for _, tag := range append([]string{""}, tags...) {
		variant, loaded, err := censusWith(repo, tag)
		if err != nil {
			return tally{}, nil, err
		}
		mergeVariant(merged.rows, variant.rows)
		mergeVariant(merged.allowed, variant.allowed)
		for _, p := range loaded {
			pkgs[p] = true
		}
	}
	return merged, pkgs, nil
}

// staleAllows is the package of each allowlist entry that names no package
// in pkgs, sorted and once each.
func staleAllows(allow []storeclass.Allow, pkgs map[string]bool) []string {
	var stale []string
	for _, a := range allow {
		if !pkgs[a.Package] {
			stale = append(stale, a.Package)
		}
	}
	sort.Strings(stale)
	return slices.Compact(stale)
}

// countFindings is the sum of rows' counts.
func countFindings(rows map[storeclass.Key]int) int {
	n := 0
	for _, c := range rows {
		n += c
	}
	return n
}

// mergeVariant folds one variant's counts into merged, keeping each key's
// largest count: a file's findings are the same in every variant that
// compiles it.
func mergeVariant(merged, rows map[storeclass.Key]int) {
	for k, n := range rows {
		merged[k] = max(merged[k], n)
	}
}

// censusWith is one build-tag variant's tally, and the module-relative path
// of every package it loaded.
func censusWith(repo, tag string) (tally, []string, error) {
	cfg := &packages.Config{
		Mode:  packages.LoadSyntax,
		Dir:   repo,
		Tests: false,
		Env:   append(os.Environ(), "GOWORK=off"),
	}
	if tag != "" {
		cfg.BuildFlags = []string{"-tags=" + tag}
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return tally{}, nil, fmt.Errorf("loading packages (tags %q): %w", tag, err)
	}
	// The untagged tree must type-check. A tag alone can leave a package
	// that needs a combination of tags uncompilable; no Bazel target builds
	// it that way, so a tagged variant skips it.
	var loadErrs, loaded []string
	var ok []*packages.Package
	for _, p := range pkgs {
		if rel, in := moduleRel(p.PkgPath); in {
			loaded = append(loaded, rel)
		}
		if len(p.Errors) == 0 {
			ok = append(ok, p)
			continue
		}
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, e.Error())
		}
	}
	if tag == "" && len(loadErrs) > 0 {
		return tally{}, nil, fmt.Errorf("type-checking the tree:\n  %s", strings.Join(loadErrs, "\n  "))
	}
	tl := newTally()
	a := storeclass.NewCollector(storeclass.Default, tl.add)
	graph, err := checker.Analyze([]*analysis.Analyzer{a}, ok, nil)
	if err != nil {
		return tally{}, nil, err
	}
	for act := range graph.All() {
		if act.Err != nil {
			return tally{}, nil, fmt.Errorf("%s: %w", act.Package.PkgPath, act.Err)
		}
	}
	return tl, loaded, nil
}

// moduleRel is pkgPath relative to the gascity module, "." for its root, and
// whether it is in the module at all.
func moduleRel(pkgPath string) (string, bool) {
	if pkgPath == strings.TrimSuffix(storeclass.Default.Module, "/") {
		return ".", true
	}
	return strings.CutPrefix(pkgPath, storeclass.Default.Module)
}

// repoBuildTags is every custom build tag a production file of the module
// names in its //go:build line, sorted.
func repoBuildTags(repo string) ([]string, error) {
	seen := map[string]bool{}
	err := filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch name := d.Name(); {
			case p == repo:
			case name == "testdata" || name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "bazel-"):
				return filepath.SkipDir
			case fileExists(filepath.Join(p, "go.mod")):
				return filepath.SkipDir // another module (tools/nogo, ...)
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, t := range buildTags(string(src)) {
			seen[t] = true
		}
		return nil
	})
	tags := make([]string, 0, len(seen))
	for t := range seen {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	return tags, err
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// platformTags are build tags the toolchain sets from the target, not ones a
// build chooses.
var platformTags = map[string]bool{
	"unix": true, "cgo": true, "gc": true, "gccgo": true, "ignore": true,
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true, "hurd": true,
	"illumos": true, "ios": true, "js": true, "linux": true, "nacl": true, "netbsd": true,
	"openbsd": true, "plan9": true, "solaris": true, "wasip1": true, "windows": true, "zos": true,
	"386": true, "amd64": true, "arm": true, "arm64": true, "loong64": true, "mips": true, "mipsle": true,
	"mips64": true, "mips64le": true, "ppc64": true, "ppc64le": true, "riscv64": true, "s390x": true, "wasm": true,
}

// buildTags is the custom tags src's //go:build constraint names: those a
// build opts into, not platform or release tags.
func buildTags(src string) []string {
	var tags []string
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			break
		}
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			continue
		}
		expr.Eval(func(tag string) bool {
			if !platformTags[tag] && !strings.HasPrefix(tag, "go1.") && !slices.Contains(tags, tag) {
				tags = append(tags, tag)
			}
			return true
		})
	}
	sort.Strings(tags)
	return tags
}

// summary is the census: totals per rule, then per package and rule.
func summary(rows map[storeclass.Key]int) string {
	var out strings.Builder
	byRule := map[storeclass.Rule]int{}
	byPkg := map[string]map[storeclass.Rule]int{}
	total := 0
	for k, n := range rows {
		byRule[k.Rule] += n
		if byPkg[k.Package()] == nil {
			byPkg[k.Package()] = map[storeclass.Rule]int{}
		}
		byPkg[k.Package()][k.Rule] += n
		total += n
	}
	fmt.Fprintf(&out, "storeclass census: %d findings in %d rows\n", total, len(rows))
	for _, r := range storeclass.Rules {
		fmt.Fprintf(&out, "  %-17s %6d\n", r, byRule[r])
	}
	pkgs := make([]string, 0, len(byPkg))
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	fmt.Fprintf(&out, "\n%-40s", "package")
	for _, r := range storeclass.Rules {
		fmt.Fprintf(&out, " %s", r)
	}
	out.WriteString("\n")
	for _, p := range pkgs {
		fmt.Fprintf(&out, "%-40s", p)
		for _, r := range storeclass.Rules {
			fmt.Fprintf(&out, " %*d", len(r), byPkg[p][r])
		}
		out.WriteString("\n")
	}
	return out.String()
}

// growth is every reason rows may not replace the baseline: a finding in a
// sealed package, and, unless this is the first ceiling, a package/rule
// total above ceiling or absent from it.
func growth(rows map[storeclass.Key]int, ceiling map[storeclass.CeilingKey]int, sealed []string, initial bool) []string {
	var refusals []string
	for k := range rows {
		for _, s := range sealed {
			if k.Package() == s {
				refusals = append(refusals, fmt.Sprintf("%s is sealed but has a finding: %s", s, k))
			}
		}
	}
	if !initial {
		for k, n := range storeclass.Totals(rows) {
			switch pinned, ok := ceiling[k]; {
			case !ok:
				refusals = append(refusals, fmt.Sprintf("%s %s: %d findings where ceiling.txt allows none", k.Package, k.Rule, n))
			case n > pinned:
				refusals = append(refusals, fmt.Sprintf("%s %s: %d findings, ceiling.txt allows %d", k.Package, k.Rule, n, pinned))
			}
		}
	}
	sort.Strings(refusals)
	return refusals
}

const (
	baselineRel = "tools/nogo/analyzers/storeclass/baseline.txt"
	ceilingRel  = "tools/nogo/analyzers/storeclass/ceiling.txt"
	allowedRel  = "tools/nogo/analyzers/storeclass/allowed.txt"
)

// gate judges the checkout against since, the change's merge-base: a new or
// raised baseline or allowed.txt row, a new or raised ceiling row, and a
// change to the lint's protected paths or its nogo wiring each fail unless
// allowRaise. A since that predates the gate (it has no baseline) is judged
// against tip, the base branch's tip, instead: a change cut before the gate
// landed does not get to introduce it. Only when tip has no baseline either
// does the change introduce the gate and pass. A ref git cannot read fails
// the gate: it never reads as one without a baseline.
func gate(repo, since, tip string, allowRaise bool, out io.Writer) error {
	repo, err := checkout(repo)
	if err != nil {
		return err
	}
	ref, err := judgedRef(repo, since, tip)
	if err != nil {
		return err
	}
	if ref == "" {
		_, err := fmt.Fprintf(out, "neither %s nor the base branch's tip %s has a storeclass baseline: this change introduces it\n", since, tip)
		return err
	}
	if ref != since {
		if _, err := fmt.Fprintf(out, "%s predates the storeclass gate: judging the change against the base branch's tip %s\n", since, tip); err != nil {
			return err
		}
	}
	old, err := readPins(atRef(repo, ref))
	if err != nil {
		return fmt.Errorf("%s: %w", ref, err)
	}
	cur, err := readPins(inCheckout(repo))
	if err != nil {
		return err
	}
	debt := raises(old, cur)
	lint, err := lintChanges(repo, ref)
	if err != nil {
		return err
	}
	if len(debt)+len(lint) == 0 {
		_, err := fmt.Fprintf(out, "no storeclass baseline raise against %s\n", ref)
		return err
	}
	msg := fmt.Sprintf("storeclass gate against %s:\n  %s\n", ref, strings.Join(slices.Concat(debt, lint), "\n  "))
	if allowRaise {
		_, err := fmt.Fprintf(out, "%sapproved by the storeclass/baseline-raise label\n", msg)
		return err
	}
	return fmt.Errorf("%s%s the coordinator's approval: the storeclass/baseline-raise label (see the storeclass package doc)", msg, approvalNeeded(len(debt) > 0, len(lint) > 0))
}

// judgedRef is the ref the gate judges the change against: since when it
// has a baseline, else tip when that has one, else "" (the change introduces
// the gate). Without a tip, a since with no baseline is an error: the gate
// could not tell a change that introduces it from one cut before it landed.
func judgedRef(repo, since, tip string) (string, error) {
	for _, ref := range []string{since, tip} {
		if ref == "" {
			continue
		}
		_, ok, err := fileAt(repo, ref, baselineRel)
		if err != nil {
			return "", err
		}
		if ok {
			return ref, nil
		}
	}
	if tip == "" {
		return "", fmt.Errorf("%s has no storeclass baseline and no -base-tip names the base branch's tip: the gate cannot tell a change that introduces it from one cut before it landed", since)
	}
	return "", nil
}

// fileAt is rel's text at ref, and whether ref has it. A ref that names no
// commit, or any other git failure, is an error, never a file ref lacks.
func fileAt(repo, ref, rel string) (string, bool, error) {
	if err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Run(); err != nil {
		return "", false, fmt.Errorf("%s names no commit: %w", ref, err)
	}
	listed, err := exec.Command("git", "-C", repo, "ls-tree", "--full-tree", "--name-only", ref, "--", rel).Output()
	if err != nil {
		return "", false, fmt.Errorf("listing %s at %s: %w", rel, ref, err)
	}
	if len(bytes.TrimSpace(listed)) == 0 {
		return "", false, nil
	}
	text, err := exec.Command("git", "-C", repo, "show", ref+":"+rel).Output()
	if err != nil {
		return "", false, fmt.Errorf("reading %s at %s: %w", rel, ref, err)
	}
	return string(text), true, nil
}

// reader reads one file of a tree by its repository-relative path.
type reader func(rel string) (string, error)

// atRef reads ref's files: "" for a file ref lacks.
func atRef(repo, ref string) reader {
	return func(rel string) (string, error) {
		text, _, err := fileAt(repo, ref, rel)
		return text, err
	}
}

// inCheckout reads the checkout's files: a file it lacks is an error.
func inCheckout(repo string) reader {
	return func(rel string) (string, error) {
		text, err := os.ReadFile(filepath.Join(repo, rel))
		return string(text), err
	}
}

// pins is what a tree pins: baseline.txt's and allowed.txt's rows, and
// ceiling.txt's.
type pins struct {
	rows, allowed map[storeclass.Key]int
	ceiling       map[storeclass.CeilingKey]int
}

// readPins parses a tree's baseline.txt, ceiling.txt and allowed.txt.
func readPins(read reader) (pins, error) {
	var text [3]string
	for i, rel := range []string{baselineRel, ceilingRel, allowedRel} {
		t, err := read(rel)
		if err != nil {
			return pins{}, err
		}
		text[i] = t
	}
	var p pins
	var err error
	if p.rows, err = storeclass.ParseBaseline(text[0]); err != nil {
		return pins{}, fmt.Errorf("%s: %w", baselineRel, err)
	}
	if p.ceiling, err = storeclass.ParseCeiling(text[1]); err != nil {
		return pins{}, fmt.Errorf("%s: %w", ceilingRel, err)
	}
	if p.allowed, err = storeclass.ParseBaseline(text[2]); err != nil {
		return pins{}, fmt.Errorf("%s: %w", allowedRel, err)
	}
	return p, nil
}

// lintChanges is a refusal for each change since ref to the lint's protected
// paths or to its nogo wiring.
func lintChanges(repo, ref string) ([]string, error) {
	// --no-renames lists a renamed file under its old name too, so moving a
	// file out of a protected path changes that path; -z keeps a name git
	// would otherwise quote, and so hide from the prefix match, as it is.
	changed, err := exec.Command("git", "-C", repo, "diff", "--name-only", "--no-renames", "-z", ref, "HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("listing the files changed since %s: %w", ref, err)
	}
	lint := protectedChanges(strings.FieldsFunc(string(changed), func(r rune) bool { return r == 0 }))
	cur, err := wiringFiles(inCheckout(repo))
	if err != nil {
		return nil, err
	}
	old, err := wiringFiles(atRef(repo, ref))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref, err)
	}
	return append(lint, wiringChanges(func(f string) string { return old[f] }, func(f string) string { return cur[f] })...), nil
}

// approvalNeeded names what the label approves in a refused change: new
// raw-store debt, a change that could weaken the lint (its protected paths or
// its nogo wiring), or both.
func approvalNeeded(debt, lint bool) string {
	switch {
	case debt && lint:
		return "new raw-store debt and a change to the lint or its nogo wiring need"
	case lint:
		return "a change to the lint or its nogo wiring needs"
	}
	return "new raw-store debt needs"
}

// protectedPaths govern the lint and its gate: a change to any of them (the
// analyzer, its allowlist and sealed lists, the generator, the rules_go
// patches that keep nolint from suppressing it and hand it the baseline, the
// analyzer set and the nogo target that run it, this workflow, the code
// owners) needs the same approval as a baseline raise. baseline.txt,
// ceiling.txt and allowed.txt are judged by raises instead, and the files
// that hold nogo wiring among other things by wirings.
var protectedPaths = []string{
	"tools/nogo/analyzers/storeclass/",
	"tools/nogo/analyzers.bzl",
	"tools/nogo/BUILD.bazel",
	"third_party/patches/rules_go_nogo_golangci_nolint.patch",
	"third_party/patches/rules_go_nogo_main_repo_data.patch",
	".github/workflows/storeclass-baseline.yml",
	".github/CODEOWNERS",
}

// protectedChanges is a refusal for each changed file under protectedPaths.
func protectedChanges(changed []string) []string {
	var out []string
	for _, f := range changed {
		if f == baselineRel || f == ceilingRel || f == allowedRel {
			continue
		}
		for _, p := range protectedPaths {
			if f == p || strings.HasSuffix(p, "/") && strings.HasPrefix(f, p) {
				out = append(out, "protected path changed: "+f)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// wiring is a part of a file the gate does not protect whole that still
// decides whether nogo runs storeclass, and how. extract is the part's text
// with comments and layout dropped, or "" when the file does not hold it.
type wiring struct {
	file, what string
	extract    func(src string) string
}

// wirings are MODULE.bazel's rules_go override, whose patches keep nolint
// from suppressing storeclass and hand it the baseline, and its go_sdk.nogo
// registration of //tools/nogo; and config.json's scope for every analyzer
// (_base) and for storeclass. The rest of those files changes freely.
var wirings = []wiring{
	{"MODULE.bazel", "the rules_go override, which applies the nogo patches", rulesGoOverride},
	{"MODULE.bazel", "the go_sdk.nogo registration", func(src string) string {
		return strings.Join(starlarkCalls(src, "go_sdk.nogo"), "\n")
	}},
	{"tools/nogo/config.json", "the _base and storeclass analyzer scopes", nogoScopes},
}

// wiringFiles is a tree's text of every file a wiring lives in.
func wiringFiles(read reader) (map[string]string, error) {
	files := map[string]string{}
	for _, w := range wirings {
		if _, ok := files[w.file]; ok {
			continue
		}
		text, err := read(w.file)
		if err != nil {
			return nil, err
		}
		files[w.file] = text
	}
	return files, nil
}

// wiringChanges is a refusal for each wiring the change's tree (cur) lacks,
// since the gate cannot judge it, or holds otherwise than the merge-base
// (old). old and cur return a file's text, "" when it is absent.
func wiringChanges(old, cur func(file string) string) []string {
	var out []string
	for _, w := range wirings {
		switch now := w.extract(cur(w.file)); {
		case now == "":
			out = append(out, fmt.Sprintf("nogo wiring not found: %s (%s)", w.what, w.file))
		case now != w.extract(old(w.file)):
			out = append(out, fmt.Sprintf("nogo wiring changed: %s (%s)", w.what, w.file))
		}
	}
	return out
}

// rulesGoOverride is MODULE.bazel's single_version_override of rules_go: its
// version and every patch it applies.
func rulesGoOverride(src string) string {
	var out []string
	for _, call := range starlarkCalls(src, "single_version_override") {
		if strings.Contains(call, `module_name = "rules_go"`) {
			out = append(out, call)
		}
	}
	return strings.Join(out, "\n")
}

// nogoScopes is config.json's _base and storeclass entries, compacted, or ""
// when it does not parse. Other analyzers' entries are left out.
func nogoScopes(src string) string {
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal([]byte(src), &cfg); err != nil {
		return ""
	}
	scopes := map[string]json.RawMessage{}
	for _, name := range []string{"_base", "storeclass"} {
		if entry, ok := cfg[name]; ok {
			scopes[name] = entry
		}
	}
	text, err := json.Marshal(scopes)
	if err != nil {
		return ""
	}
	return string(text)
}

// starlarkCalls is every call of the dotted name callee in src, each as its
// tokens from the name through the closing parenthesis, space-joined.
func starlarkCalls(src, callee string) []string {
	toks := starlarkTokens(src)
	name := starlarkTokens(callee)
	var calls []string
	for i := 0; i+len(name) < len(toks); i++ {
		open := i + len(name)
		if !slices.Equal(toks[i:open], name) || toks[open] != "(" || i > 0 && toks[i-1] == "." {
			continue
		}
		end := closingParen(toks, open)
		calls = append(calls, strings.Join(toks[i:end], " "))
		i = end - 1
	}
	return calls
}

// closingParen is the index just past the parenthesis that closes
// toks[open], or len(toks).
func closingParen(toks []string, open int) int {
	depth := 0
	for j := open; j < len(toks); j++ {
		switch toks[j] {
		case "(":
			depth++
		case ")":
			if depth--; depth == 0 {
				return j + 1
			}
		}
	}
	return len(toks)
}

// starlarkTokens splits Starlark source into tokens, dropping comments and
// layout: a string literal whole, a name or number, or one other character.
func starlarkTokens(src string) []string {
	var toks []string
	for i := 0; i < len(src); {
		c := src[i]
		end := i + 1
		switch {
		case c == '#':
			end = strings.IndexByte(src[i:], '\n')
			if end < 0 {
				return toks
			}
			i += end
			continue
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
			continue
		case c == '"' || c == '\'':
			end = stringEnd(src, i)
		case isNameByte(c):
			for end < len(src) && isNameByte(src[end]) {
				end++
			}
		}
		toks = append(toks, src[i:end])
		i = end
	}
	return toks
}

func isNameByte(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// stringEnd is the index just past the string literal that opens at i,
// triple-quoted or not; an unterminated literal runs to the end of src.
func stringEnd(src string, i int) int {
	q := src[i]
	if triple := strings.Repeat(string(q), 3); strings.HasPrefix(src[i:], triple) {
		if end := strings.Index(src[i+3:], triple); end >= 0 {
			return i + 3 + end + 3
		}
		return len(src)
	}
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case q, '\n':
			return j + 1
		}
	}
	return len(src)
}

// raises is every way the change's pins grow past the merge-base's: a new
// or raised baseline or allowed.txt row, and a new or raised ceiling row.
func raises(old, cur pins) []string {
	out := slices.Concat(rowRaises("", old.rows, cur.rows), rowRaises("allowed.txt ", old.allowed, cur.allowed))
	for k, n := range cur.ceiling {
		switch was, ok := old.ceiling[k]; {
		case !ok:
			out = append(out, fmt.Sprintf("new ceiling row %s %s (%d)", k.Package, k.Rule, n))
		case n > was:
			out = append(out, fmt.Sprintf("ceiling %s %s raised %d -> %d", k.Package, k.Rule, was, n))
		}
	}
	sort.Strings(out)
	return out
}

// rowRaises is each row of cur that oldRows lacks or pins lower; file, when
// set, names the file the rows are from ("allowed.txt ").
func rowRaises(file string, oldRows, cur map[storeclass.Key]int) []string {
	var out []string
	for k, n := range cur {
		switch was, ok := oldRows[k]; {
		case !ok:
			out = append(out, fmt.Sprintf("new %srow %s (%d)", file, k, n))
		case n > was:
			out = append(out, fmt.Sprintf("%s%s raised %d -> %d", file, k, was, n))
		}
	}
	return out
}
