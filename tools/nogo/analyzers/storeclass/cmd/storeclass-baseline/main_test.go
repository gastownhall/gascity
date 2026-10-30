package main

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/analyzertest"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/storeclass"
)

func TestGrowthRefusesAnyRise(t *testing.T) {
	ceiling := map[storeclass.CeilingKey]int{
		{Package: "cmd/gc", Rule: storeclass.RawParam}: 3,
	}
	row := func(p, fn string, r storeclass.Rule) storeclass.Key {
		return storeclass.Key{Path: p, Func: fn, Rule: r}
	}
	for name, tc := range map[string]struct {
		rows    map[storeclass.Key]int
		sealed  []string
		initial bool
		want    string
	}{
		"at the ceiling": {rows: map[storeclass.Key]int{row("cmd/gc/a.go", "f", storeclass.RawParam): 3}},
		"below it":       {rows: map[storeclass.Key]int{row("cmd/gc/a.go", "f", storeclass.RawParam): 1}},
		"above it, spread over two files": {
			rows: map[storeclass.Key]int{row("cmd/gc/a.go", "f", storeclass.RawParam): 2, row("cmd/gc/b.go", "g", storeclass.RawParam): 2},
			want: "cmd/gc raw-param: 4 findings, ceiling.txt allows 3",
		},
		"a new rule": {
			rows: map[storeclass.Key]int{row("cmd/gc/a.go", "f", storeclass.Mint): 1},
			want: "cmd/gc mint: 1 findings where ceiling.txt allows none",
		},
		"a new package": {
			rows: map[storeclass.Key]int{row("internal/x/a.go", "f", storeclass.RawParam): 1},
			want: "internal/x raw-param: 1 findings where ceiling.txt allows none",
		},
		"a sealed package": {
			rows:   map[storeclass.Key]int{row("cmd/gc/a.go", "f", storeclass.RawParam): 1},
			sealed: []string{"cmd/gc"},
			want:   "cmd/gc is sealed",
		},
		"the first ceiling takes anything unsealed": {
			rows:    map[storeclass.Key]int{row("internal/x/a.go", "f", storeclass.RawParam): 9},
			initial: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := growth(tc.rows, ceiling, tc.sealed, tc.initial)
			switch {
			case tc.want == "" && len(got) != 0:
				t.Fatalf("refused: %v", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Fatalf("refusals = %v, want one containing %q", got, tc.want)
			}
		})
	}
}

func TestBuildTagsOfAFile(t *testing.T) {
	got := buildTags("//go:build integration && (linux || darwin) && !productmetrics_testhook && go1.22 && !ignore\n\npackage p\n")
	want := []string{"integration", "productmetrics_testhook"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("buildTags = %v, want %v", got, want)
	}
	if got := buildTags("package p\n\n//go:build integration\n"); len(got) != 0 {
		t.Fatalf("a constraint after the package clause counted: %v", got)
	}
	if got := buildTags("// Copyright\n\n//go:build unix && cgo\n\npackage p\n"); len(got) != 0 {
		t.Fatalf("platform tags counted: %v", got)
	}
}

func TestMergeVariantsTakesEachKeysMax(t *testing.T) {
	k := func(p string) storeclass.Key { return storeclass.Key{Path: p, Func: "f", Rule: storeclass.RawParam} }
	merged := map[storeclass.Key]int{}
	mergeVariant(merged, map[storeclass.Key]int{k("a.go"): 2, k("b.go"): 1})
	mergeVariant(merged, map[storeclass.Key]int{k("a.go"): 2, k("c.go"): 3})
	if merged[k("a.go")] != 2 || merged[k("b.go")] != 1 || merged[k("c.go")] != 3 || len(merged) != 3 {
		t.Fatalf("merged = %v", merged)
	}
}

func TestRaisesAgainstTheMergeBase(t *testing.T) {
	k := func(p, fn string) storeclass.Key { return storeclass.Key{Path: p, Func: fn, Rule: storeclass.RawParam} }
	c := storeclass.CeilingKey{Package: "cmd/gc", Rule: storeclass.RawParam}
	boot := k("cmd/gc/storage_boot.go", "boot")
	old := pins{
		rows:    map[storeclass.Key]int{k("cmd/gc/a.go", "f"): 2, k("cmd/gc/b.go", "g"): 1},
		ceiling: map[storeclass.CeilingKey]int{c: 3},
		allowed: map[storeclass.Key]int{boot: 1},
	}
	// edited is old with edit applied to a copy.
	edited := func(edit func(p *pins)) pins {
		p := pins{rows: maps.Clone(old.rows), ceiling: maps.Clone(old.ceiling), allowed: maps.Clone(old.allowed)}
		edit(&p)
		return p
	}
	for name, tc := range map[string]struct {
		cur  pins
		want []string
	}{
		"unchanged": {cur: old},
		"a shrink": {cur: pins{
			rows:    map[storeclass.Key]int{k("cmd/gc/a.go", "f"): 1},
			ceiling: map[storeclass.CeilingKey]int{c: 1},
		}},
		"a new key": {
			cur:  edited(func(p *pins) { p.rows[k("cmd/gc/c.go", "h")] = 1 }),
			want: []string{"new row cmd/gc/c.go\th\traw-param (1)"},
		},
		"a raised count": {
			cur:  edited(func(p *pins) { p.rows[k("cmd/gc/a.go", "f")] = 3 }),
			want: []string{"cmd/gc/a.go\tf\traw-param raised 2 -> 3"},
		},
		"a raised ceiling": {
			cur:  edited(func(p *pins) { p.ceiling[c] = 4 }),
			want: []string{"ceiling cmd/gc raw-param raised 3 -> 4"},
		},
		"a new ceiling row": {
			cur:  edited(func(p *pins) { p.ceiling[storeclass.CeilingKey{Package: "internal/x", Rule: storeclass.Mint}] = 1 }),
			want: []string{"new ceiling row internal/x mint (1)"},
		},
		"a new allowed.txt row": {
			cur:  edited(func(p *pins) { p.allowed[k("cmd/gc/storage_boot.go", "open")] = 1 }),
			want: []string{"new allowed.txt row cmd/gc/storage_boot.go\topen\traw-param (1)"},
		},
		"a raised allowed.txt row": {
			cur:  edited(func(p *pins) { p.allowed[boot] = 2 }),
			want: []string{"allowed.txt cmd/gc/storage_boot.go\tboot\traw-param raised 1 -> 2"},
		},
		"a baseline row moved to allowed.txt": {
			cur: edited(func(p *pins) {
				delete(p.rows, k("cmd/gc/b.go", "g"))
				p.ceiling[c] = 2
				p.allowed[k("cmd/gc/storage_boot.go", "g")] = 1
			}),
			want: []string{"new allowed.txt row cmd/gc/storage_boot.go\tg\traw-param (1)"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := raises(old, tc.cur)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("raises = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProtectedChanges(t *testing.T) {
	got := protectedChanges([]string{
		"tools/nogo/analyzers/storeclass/baseline.txt",
		"tools/nogo/analyzers/storeclass/ceiling.txt",
		"tools/nogo/analyzers/storeclass/allowed.txt",
		"tools/nogo/analyzers/storeclass/config.go",
		"tools/nogo/analyzers/storeclass/sealed_floor.txt",
		"tools/nogo/analyzers/storeclass/storeclass.go",
		"tools/nogo/analyzers/storeclass/cmd/storeclass-baseline/main.go",
		".github/workflows/storeclass-baseline.yml",
		".github/CODEOWNERS",
		"third_party/patches/rules_go_nogo_golangci_nolint.patch",
		"third_party/patches/rules_go_nogo_main_repo_data.patch",
		"third_party/patches/rules_go_other.patch",
		"tools/nogo/analyzers.bzl",
		"tools/nogo/BUILD.bazel",
		// Judged by their nogo wiring (wiringChanges), not whole.
		"MODULE.bazel",
		"tools/nogo/config.json",
		"cmd/gc/main.go",
	})
	want := []string{
		"protected path changed: .github/CODEOWNERS",
		"protected path changed: .github/workflows/storeclass-baseline.yml",
		"protected path changed: third_party/patches/rules_go_nogo_golangci_nolint.patch",
		"protected path changed: third_party/patches/rules_go_nogo_main_repo_data.patch",
		"protected path changed: tools/nogo/BUILD.bazel",
		"protected path changed: tools/nogo/analyzers.bzl",
		"protected path changed: tools/nogo/analyzers/storeclass/cmd/storeclass-baseline/main.go",
		"protected path changed: tools/nogo/analyzers/storeclass/config.go",
		"protected path changed: tools/nogo/analyzers/storeclass/sealed_floor.txt",
		"protected path changed: tools/nogo/analyzers/storeclass/storeclass.go",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("protectedChanges = %q\nwant %q", got, want)
	}
}

// moduleBazel is MODULE.bazel's nogo wiring, as the gate reads it.
const moduleBazel = `"""The module."""

module(name = "gascity")

bazel_dep(name = "rules_go", version = "0.63.0")

# The nogo patches.
single_version_override(
    module_name = "rules_go",
    patch_strip = 1,
    patches = [
        "//third_party/patches:rules_go_nogo_golangci_nolint.patch",
        "//third_party/patches:rules_go_nogo_main_repo_data.patch",
    ],
    version = "0.63.0",
)
bazel_dep(name = "gazelle", version = "0.53.0")

go_sdk = use_extension("@rules_go//go:extensions.bzl", "go_sdk")
go_sdk.nogo(
    includes = ["//:__subpackages__"],
    nogo = "//tools/nogo",
)
`

// nolintPatchLine is moduleBazel's line that applies the nolint patch.
const nolintPatchLine = "        \"//third_party/patches:rules_go_nogo_golangci_nolint.patch\",\n"

const nogoConfig = `{
  "_base": {"exclude_files": {"^external/": "third-party module repositories"}},
  "revive": {"analyzer_flags": {"exclude": "internal/api/::var-naming"}}
}
`

func TestWiringChanges(t *testing.T) {
	edit := func(src, from, to string) string {
		t.Helper()
		if !strings.Contains(src, from) {
			t.Fatalf("the fixture lacks %q", from)
		}
		return strings.Replace(src, from, to, 1)
	}
	const (
		overrideChanged = "nogo wiring changed: the rules_go override, which applies the nogo patches (MODULE.bazel)"
		overrideMissing = "nogo wiring not found: the rules_go override, which applies the nogo patches (MODULE.bazel)"
		nogoChanged     = "nogo wiring changed: the go_sdk.nogo registration (MODULE.bazel)"
		nogoMissing     = "nogo wiring not found: the go_sdk.nogo registration (MODULE.bazel)"
		scopesChanged   = "nogo wiring changed: the _base and storeclass analyzer scopes (tools/nogo/config.json)"
		scopesMissing   = "nogo wiring not found: the _base and storeclass analyzer scopes (tools/nogo/config.json)"
	)
	for name, tc := range map[string]struct {
		module, config string
		want           []string
	}{
		"unchanged": {},
		"comments and layout": {
			module: edit(edit(moduleBazel, "patch_strip = 1,", "patch_strip = 1,  # strip a/ and b/"), `nogo = "//tools/nogo",`, "nogo =\n        \"//tools/nogo\",  # the analyzers"),
			config: edit(nogoConfig, `{"exclude_files": {`, "{\n    \"exclude_files\": {\n      "),
		},
		"another dependency bumped":   {module: edit(moduleBazel, `"gazelle", version = "0.53.0"`, `"gazelle", version = "0.54.0"`)},
		"another analyzer configured": {config: edit(nogoConfig, `"internal/api/::var-naming"`, `"internal/api/::var-naming;internal/mail/::var-naming"`)},
		"a nogo patch dropped": {
			module: edit(moduleBazel, nolintPatchLine, ""),
			want:   []string{overrideChanged},
		},
		"a nogo patch commented out": {
			module: edit(moduleBazel, `"//third_party/patches:rules_go_nogo_main_repo_data.patch",`, `# "//third_party/patches:rules_go_nogo_main_repo_data.patch",`),
			want:   []string{overrideChanged},
		},
		"another rules_go patch added": {
			module: edit(moduleBazel, "    ],\n    version", "        \"//third_party/patches:rules_go_other.patch\",\n    ],\n    version"),
			want:   []string{overrideChanged},
		},
		"the patches moved to another module": {
			module: edit(moduleBazel, `module_name = "rules_go"`, `module_name = "gazelle"`),
			want:   []string{overrideMissing},
		},
		"the registration narrowed": {
			module: edit(moduleBazel, `"//:__subpackages__"`, `"//internal/beads:__subpackages__"`),
			want:   []string{nogoChanged},
		},
		"the registration removed": {
			module: edit(moduleBazel, "go_sdk.nogo(", "go_sdk.download("),
			want:   []string{nogoMissing},
		},
		"a storeclass scope": {
			config: edit(nogoConfig, `"revive"`, `"storeclass": {"exclude_files": {"^cmd/gc/": "later"}}, "revive"`),
			want:   []string{scopesChanged},
		},
		"a _base exclusion": {
			config: edit(nogoConfig, `"third-party module repositories"`, `"third-party module repositories", "^cmd/gc/": "later"`),
			want:   []string{scopesChanged},
		},
		"an unreadable config": {config: "{", want: []string{scopesMissing}},
	} {
		t.Run(name, func(t *testing.T) {
			base := map[string]string{"MODULE.bazel": moduleBazel, "tools/nogo/config.json": nogoConfig}
			change := maps.Clone(base)
			if tc.module != "" {
				change["MODULE.bazel"] = tc.module
			}
			if tc.config != "" {
				change["tools/nogo/config.json"] = tc.config
			}
			got := wiringChanges(func(f string) string { return base[f] }, func(f string) string { return change[f] })
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("wiringChanges = %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestWiringsReadTheTree: every wiring is found in this tree, so the gate
// never compares two empty readings: a reformat the extractor misses would
// otherwise leave that wiring unguarded.
func TestWiringsReadTheTree(t *testing.T) {
	// The package directory, in the source tree or in Bazel's runfiles.
	paths := map[string]string{
		"MODULE.bazel":           "../../../../../../MODULE.bazel",
		"tools/nogo/config.json": "../../../../config.json",
	}
	read := func(file string) string {
		b, err := os.ReadFile(paths[file])
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		return string(b)
	}
	for _, w := range wirings {
		if w.extract(read(w.file)) == "" {
			t.Errorf("%s: %s not found", w.file, w.what)
		}
	}
	override := rulesGoOverride(read("MODULE.bazel"))
	for _, p := range []string{"rules_go_nogo_golangci_nolint.patch", "rules_go_nogo_main_repo_data.patch"} {
		if !strings.Contains(override, p) {
			t.Errorf("the rules_go override does not apply %s: %s", p, override)
		}
	}
	if got := wiringChanges(read, read); len(got) != 0 {
		t.Errorf("the tree against itself: %q", got)
	}
}

func TestStarlarkTokens(t *testing.T) {
	src := "\"\"\"A doc with \"quotes\" and # no comment.\"\"\"\nx = ['a#b', \"c\\\"d\"]  # a comment\nf(y)\n"
	want := []string{`"""A doc with "quotes" and # no comment."""`, "x", "=", "[", `'a#b'`, ",", `"c\"d"`, "]", "f", "(", "y", ")"}
	if got := starlarkTokens(src); !slices.Equal(got, want) {
		t.Fatalf("starlarkTokens = %q\nwant %q", got, want)
	}
	calls := starlarkCalls("a.f(1)\nf(g(2), 3)\nx.f(4)\nf (5)\n", "f")
	if want := []string{"f ( g ( 2 ) , 3 )", "f ( 5 )"}; !slices.Equal(calls, want) {
		t.Fatalf("starlarkCalls = %q, want %q", calls, want)
	}
}

// gitRepo is a new git repository for a gate test, with functions that run
// git in it and write a file into it.
func gitRepo(t *testing.T) (repo string, git func(args ...string) string, write func(rel, text string)) {
	t.Helper()
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	repo = t.TempDir()
	git = func(args ...string) string {
		t.Helper()
		base := []string{"-C", repo, "-c", "user.name=storeclass", "-c", "user.email=storeclass@example.com", "-c", "commit.gpgsign=false"}
		out, err := exec.Command("git", append(base, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write = func(rel, text string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	return repo, git, write
}

// TestGateJudgesTheChange runs the gate over a git history: a change to other
// files passes, and one that drops a nogo patch from MODULE.bazel fails
// unless the label approves it.
func TestGateJudgesTheChange(t *testing.T) {
	repo, git, write := gitRepo(t)
	write(baselineRel, "cmd/gc/a.go\tf\traw-param\t1\n")
	write(ceilingRel, "cmd/gc\traw-param\t1\n")
	write(allowedRel, "")
	write("MODULE.bazel", moduleBazel)
	write("tools/nogo/config.json", nogoConfig)
	write("cmd/gc/a.go", "package gc\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	base := git("rev-parse", "HEAD")

	write("cmd/gc/a.go", "package gc\n\n// A change elsewhere.\n")
	git("commit", "-q", "-am", "elsewhere")
	var out strings.Builder
	if err := gate(repo, base, base, false, &out); err != nil || !strings.Contains(out.String(), "no storeclass baseline raise") {
		t.Fatalf("a change elsewhere: %v\n%s", err, out.String())
	}

	write("MODULE.bazel", strings.Replace(moduleBazel, nolintPatchLine, "", 1))
	git("commit", "-q", "-am", "drop a nogo patch")
	out.Reset()
	if err := gate(repo, base, base, false, &out); err == nil || !strings.Contains(err.Error(), "nogo wiring changed: the rules_go override") ||
		!strings.Contains(err.Error(), "a change to the lint or its nogo wiring needs the coordinator's approval") || strings.Contains(err.Error(), "raw-store debt") {
		t.Fatalf("a dropped nogo patch, a lint change with no new debt: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := gate(repo, base, base, true, &out); err != nil || !strings.Contains(out.String(), "approved by the storeclass/baseline-raise label") {
		t.Fatalf("the label did not approve the dropped patch: %v\n%s", err, out.String())
	}

	// A wiring file the gate cannot read fails it, label or not.
	if err := os.Remove(filepath.Join(repo, "tools/nogo/config.json")); err != nil {
		t.Fatal(err)
	}
	if err := gate(repo, base, base, true, &out); err == nil || !strings.Contains(err.Error(), "config.json") {
		t.Fatalf("an unreadable config.json: %v", err)
	}
}

// TestGateSeesAProtectedPathGitQuotes: a file under a protected path whose
// name git quotes in a plain diff listing (a non-ASCII name) is still a
// protected change: the gate lists names unquoted.
func TestGateSeesAProtectedPathGitQuotes(t *testing.T) {
	repo, git, write := gitRepo(t)
	write(baselineRel, "")
	write(ceilingRel, "")
	write(allowedRel, "")
	write("MODULE.bazel", moduleBazel)
	write("tools/nogo/config.json", nogoConfig)
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	base := git("rev-parse", "HEAD")
	const quoted = "tools/nogo/analyzers/storeclass/\u00e9.go"
	write(quoted, "package storeclass\n")
	git("add", "-A")
	git("commit", "-q", "-m", "a protected file git quotes")
	if listed := git("-c", "core.quotePath=true", "diff", "--name-only", base, "HEAD"); !strings.HasPrefix(listed, `"`) {
		t.Fatalf("fixture: git does not quote %q, so the test proves nothing: %s", quoted, listed)
	}
	var out strings.Builder
	if err := gate(repo, base, base, false, &out); err == nil || !strings.Contains(err.Error(), "protected path changed: "+quoted) {
		t.Fatalf("a protected file git quotes: %v\n%s", err, out.String())
	}
}

// TestGateJudgesAChangeCutBeforeTheGate: a merge-base that predates the gate
// (it has no baseline) does not make the change one that introduces it while
// the base branch's tip has the gate. The gate judges the change against the
// tip, and still refuses a new row and a dropped nogo patch. Only when the
// tip has no baseline either does the change introduce the gate. A ref git
// cannot read fails the gate, label or not: it never reads as a ref without
// a baseline.
func TestGateJudgesAChangeCutBeforeTheGate(t *testing.T) {
	repo, git, write := gitRepo(t)
	write("MODULE.bazel", moduleBazel)
	write("tools/nogo/config.json", nogoConfig)
	write("cmd/gc/a.go", "package gc\n")
	git("add", "-A")
	git("commit", "-q", "-m", "before the gate")
	cut := git("rev-parse", "HEAD")
	write(baselineRel, "cmd/gc/a.go\tf\traw-param\t1\n")
	write(ceilingRel, "cmd/gc\traw-param\t1\n")
	write(allowedRel, "")
	git("add", "-A")
	git("commit", "-q", "-m", "the gate lands on the base branch")
	tip := git("rev-parse", "HEAD")

	git("checkout", "-q", "-b", "change", cut)
	write(baselineRel, "cmd/gc/a.go\tf\traw-param\t1\ncmd/gc/b.go\tg\traw-param\t1\n")
	write(ceilingRel, "cmd/gc\traw-param\t2\n")
	write(allowedRel, "")
	write("MODULE.bazel", strings.Replace(moduleBazel, nolintPatchLine, "", 1))
	git("add", "-A")
	git("commit", "-q", "-m", "cut before the gate: a new row and a dropped nogo patch")

	var out strings.Builder
	err := gate(repo, cut, tip, false, &out)
	if err == nil || !strings.Contains(err.Error(), "new row cmd/gc/b.go\tg\traw-param (1)") ||
		!strings.Contains(err.Error(), "nogo wiring changed: the rules_go override") || !strings.Contains(out.String(), cut+" predates the storeclass gate") {
		t.Fatalf("a change cut before the gate, judged against the tip: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := gate(repo, cut, cut, false, &out); err != nil || !strings.Contains(out.String(), "this change introduces it") {
		t.Fatalf("no gate on the tip either, so the change introduces it: %v\n%s", err, out.String())
	}
	if err := gate(repo, cut, "", false, &out); err == nil || !strings.Contains(err.Error(), "-base-tip") {
		t.Fatalf("no tip to tell the cases apart: %v", err)
	}
	for _, refs := range [][2]string{{"no-such-ref", tip}, {cut, "no-such-ref"}} {
		if err := gate(repo, refs[0], refs[1], true, &out); err == nil || !strings.Contains(err.Error(), "no-such-ref names no commit") {
			t.Errorf("-growth-since %s -base-tip %s: %v; want the unreadable ref refused", refs[0], refs[1], err)
		}
	}
}

// censusOf is what the generator's census tallies for a cmd/gc package of
// files over a store interface, with storage_boot.go allowlisted as Default
// allowlists it: the collector the census runs, per baseline key.
func censusOf(t *testing.T, files map[string]string) tally {
	t.Helper()
	cfg := storeclass.Config{
		Module: "example.com/m/",
		Store:  "example.com/m/internal/beads.Store",
		Allow:  []storeclass.Allow{{Package: "cmd/gc", File: "storage_boot.go", Reason: "boot routing"}},
	}
	tl := newTally()
	analyzertest.RunGraph(t, storeclass.NewCollector(cfg, tl.add), []analyzertest.Package{
		{Path: "example.com/m/internal/beads", Files: map[string]string{"beads.go": "package beads\n\ntype Store interface{ Get(id string) error }\n"}},
		{Path: "example.com/m/cmd/gc", Files: files},
	})
	return tl
}

// commitCensus writes files into the repository's cmd/gc, regenerates
// baseline.txt, ceiling.txt and allowed.txt from them as the generator does,
// and commits the lot.
func commitCensus(t *testing.T, git func(args ...string) string, write func(rel, text string), msg string, files map[string]string) tally {
	t.Helper()
	tl := censusOf(t, files)
	write(baselineRel, storeclass.FormatBaseline(tl.rows))
	write(ceilingRel, storeclass.FormatCeiling(storeclass.Totals(tl.rows)))
	write(allowedRel, storeclass.FormatAllowed(tl.allowed))
	for name, text := range files {
		write("cmd/gc/"+name, text)
	}
	git("add", "-A")
	git("commit", "-q", "-m", msg)
	return tl
}

// rawFunc is a cmd/gc file declaring fn with a raw store parameter.
func rawFunc(fn string) string {
	return "package gc\n\nimport \"example.com/m/internal/beads\"\n\nfunc " + fn + "(s beads.Store) {}\n"
}

// TestGateCatchesAGeneratedFileEscape: a generated-code header on a baselined
// file, or a rename of one to *_gen.go, takes none of its raw-store debt out
// of the regenerated baseline. The header leaves the file's row as it was; the
// rename moves the row to a new key, which the gate refuses without the
// label, as it does any rename.
func TestGateCatchesAGeneratedFileEscape(t *testing.T) {
	repo, git, write := gitRepo(t)
	const header = "// Code generated by hand; DO NOT EDIT.\n\n"
	write("MODULE.bazel", moduleBazel)
	write("tools/nogo/config.json", nogoConfig)
	before := commitCensus(t, git, write, "base", map[string]string{"a.go": rawFunc("a"), "b.go": rawFunc("b")})
	base := git("rev-parse", "HEAD")

	git("rm", "-q", "cmd/gc/b.go")
	after := commitCensus(t, git, write, "a header on a.go; b.go renamed to b_gen.go", map[string]string{"a.go": header + rawFunc("a"), "b_gen.go": rawFunc("b")})
	a := storeclass.Key{Path: "cmd/gc/a.go", Func: "a", Rule: storeclass.RawParam}
	if before.rows[a] != 1 || after.rows[a] != 1 {
		t.Fatalf("a.go's row: %d before the header, %d after it; want 1 and 1", before.rows[a], after.rows[a])
	}
	var out strings.Builder
	err := gate(repo, base, base, false, &out)
	if err == nil || !strings.Contains(err.Error(), "new row cmd/gc/b_gen.go\tb\traw-param (1)") || strings.Contains(err.Error(), "cmd/gc/a.go") ||
		!strings.Contains(err.Error(), "new raw-store debt needs the coordinator's approval") {
		t.Fatalf("without the label, want only the renamed file's new row refused: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := gate(repo, base, base, true, &out); err != nil || !strings.Contains(out.String(), "approved by the storeclass/baseline-raise label") {
		t.Fatalf("the label did not approve the rename: %v\n%s", err, out.String())
	}
}

// TestGateCatchesAMoveIntoAnAllowlistedFile: moving a baselined function
// into a file the allowlist names (storage_boot.go) retires its baseline
// row, a shrink, and lands it in allowed.txt as a new row, which the gate
// refuses without the label. Retiring an allowlisted site shrinks
// allowed.txt, which needs no label.
func TestGateCatchesAMoveIntoAnAllowlistedFile(t *testing.T) {
	repo, git, write := gitRepo(t)
	write("MODULE.bazel", moduleBazel)
	write("tools/nogo/config.json", nogoConfig)
	boot := rawFunc("boot")
	before := commitCensus(t, git, write, "base", map[string]string{"a.go": rawFunc("a"), "storage_boot.go": boot})
	base := git("rev-parse", "HEAD")

	after := commitCensus(t, git, write, "a moved into storage_boot.go", map[string]string{"a.go": "package gc\n", "storage_boot.go": boot + "\nfunc a(s beads.Store) {}\n"})
	a := storeclass.Key{Path: "cmd/gc/a.go", Func: "a", Rule: storeclass.RawParam}
	moved := storeclass.Key{Path: "cmd/gc/storage_boot.go", Func: "a", Rule: storeclass.RawParam}
	if before.rows[a] != 1 || len(after.rows) != 0 || after.allowed[moved] != 1 {
		t.Fatalf("the move should take a's row out of the baseline and into allowed.txt: rows %v before, %v after; allowed.txt %v after", before.rows, after.rows, after.allowed)
	}
	var out strings.Builder
	err := gate(repo, base, base, false, &out)
	if err == nil || !strings.Contains(err.Error(), "new allowed.txt row cmd/gc/storage_boot.go\ta\traw-param (1)") ||
		!strings.Contains(err.Error(), "new raw-store debt needs the coordinator's approval") {
		t.Fatalf("without the label, want the move refused: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := gate(repo, base, base, true, &out); err != nil || !strings.Contains(out.String(), "approved by the storeclass/baseline-raise label") {
		t.Fatalf("the label did not approve the move: %v\n%s", err, out.String())
	}

	moveBase := git("rev-parse", "HEAD")
	commitCensus(t, git, write, "boot retired", map[string]string{"a.go": "package gc\n", "storage_boot.go": rawFunc("a")})
	out.Reset()
	if err := gate(repo, moveBase, moveBase, false, &out); err != nil || !strings.Contains(out.String(), "no storeclass baseline raise") {
		t.Fatalf("a shrink of allowed.txt: %v\n%s", err, out.String())
	}
}

// TestStaleAllows: an allowlist entry for a package the census did not load
// is reported once per package, whatever the entry's file.
func TestStaleAllows(t *testing.T) {
	allow := []storeclass.Allow{
		{Package: "internal/beads"},
		{Package: "internal/beads/gone"},
		{Package: "cmd/gc", File: "storage_boot.go"},
		{Package: "cmd/old", File: "a.go"},
		{Package: "cmd/old", File: "b.go"},
	}
	got := staleAllows(allow, map[string]bool{"internal/beads": true, "cmd/gc": true, "internal/other": true})
	if want := []string{"cmd/old", "internal/beads/gone"}; !slices.Equal(got, want) {
		t.Fatalf("staleAllows = %q, want %q", got, want)
	}
}
