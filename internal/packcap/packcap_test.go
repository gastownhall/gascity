package packcap

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// writeFile writes content to dir/rel, creating parent directories.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// basePack writes a minimal pack with one of everything the manifest reads
// and returns its directory.
func basePack(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "pack.toml", "[pack]\nname = \"fixture\"\nversion = \"0.1.0\"\n")
	writeFile(t, dir, "commands/status/run.sh", "#!/bin/sh\necho ok\n")
	writeFile(t, dir, "agents/worker/prompt.template.md",
		"# Worker\n\nYou pick up work from the queue and finish it.\n"+
			"Never claim work through gc bd ready; claim it with gc hook --claim.\n")
	writeFile(t, dir, "template-fragments/report.template.md", "Write a short report when you finish.\n")
	writeFile(t, dir, "formulas/do-work.toml",
		"formula = \"do-work\"\n\n[[steps]]\nid = \"work\"\n\n[steps.check]\nrun = \"true\"\n\n[vars]\noutput = \"gc.output_json\"\n")
	return dir
}

// copyPack copies the pack at src into a new temporary directory.
func copyPack(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func replaceIn(t *testing.T, dir, rel, old, new string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), old) {
		t.Fatalf("%s does not contain %q", rel, old)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, dir, rel, text string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, filepath.FromSlash(rel)), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func mustCompute(t *testing.T, dir string) Manifest {
	t.Helper()
	m, err := Compute(dir)
	if err != nil {
		t.Fatalf("Compute(%s): %v", dir, err)
	}
	return m
}

// TestDiffVerdicts covers the verdict cases of the original
// contrib/pack-capability/pack_diff_test.sh and one case per property and
// direction. An edit inside an existing file that only a computable
// addition explains is UNCLASSIFIED: the rest of the file moved too.
func TestDiffVerdicts(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(t *testing.T, dir string)
		want     Verdict
		wantExit int
		// wantFinding, when set, must appear among the findings.
		wantFinding *Finding
	}{
		{
			name:     "identical copy",
			mutate:   func(*testing.T, string) {},
			want:     None,
			wantExit: 0,
		},
		{
			name: "formula added",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "formulas/review.toml", "formula = \"review\"\n")
			},
			want:        Additive,
			wantExit:    0,
			wantFinding: &Finding{Property: Provides, Change: Added, Verdict: Additive, Values: []string{"formula:review"}},
		},
		{
			name: "command removed",
			mutate: func(t *testing.T, dir string) {
				if err := os.RemoveAll(filepath.Join(dir, "commands", "status")); err != nil {
					t.Fatal(err)
				}
			},
			want:        Breaking,
			wantExit:    2,
			wantFinding: &Finding{Property: Provides, Change: Removed, Verdict: Breaking, Values: []string{"command:status"}},
		},
		{
			name: "mandated command changed",
			mutate: func(t *testing.T, dir string) {
				replaceIn(t, dir, "agents/worker/prompt.template.md", "gc hook --claim", "gc work claim")
			},
			want:        Breaking,
			wantExit:    2,
			wantFinding: &Finding{Property: Mandates, Change: Removed, Verdict: Breaking, Values: []string{"gc hook --claim"}},
		},
		{
			name: "norm inverted",
			mutate: func(t *testing.T, dir string) {
				replaceIn(t, dir, "agents/worker/prompt.template.md", "Never claim", "Always claim")
			},
			want:     Breaking,
			wantExit: 2,
			wantFinding: &Finding{Property: Norms, Change: Added, Verdict: Breaking, Values: []string{
				"agents/worker/prompt.template.md: Always claim work through gc bd ready; claim it with gc hook --claim.",
			}},
		},
		{
			name: "demand added",
			mutate: func(t *testing.T, dir string) {
				appendTo(t, dir, "formulas/do-work.toml", "\n[meta]\nscope = \"gc.scope_rig\"\n")
			},
			want:        Breaking,
			wantExit:    2,
			wantFinding: &Finding{Property: Demands, Change: Added, Verdict: Breaking, Values: []string{"gc.scope_rig"}},
		},
		{
			name: "demand dropped",
			mutate: func(t *testing.T, dir string) {
				replaceIn(t, dir, "formulas/do-work.toml", "gc.output_json", "plain")
			},
			// The finding is additive, but the edited formula file moved
			// in ways nothing computable accounts for.
			want:        Unclassified,
			wantExit:    1,
			wantFinding: &Finding{Property: Demands, Change: Removed, Verdict: Additive, Values: []string{"gc.output_json"}},
		},
		{
			name: "formula construct removed",
			mutate: func(t *testing.T, dir string) {
				replaceIn(t, dir, "formulas/do-work.toml", "[steps.check]\nrun = \"true\"\n", "")
			},
			want:        Breaking,
			wantExit:    2,
			wantFinding: &Finding{Property: Uses, Change: Removed, Verdict: Breaking, Values: []string{"steps.check"}},
		},
		{
			name: "formula construct added",
			mutate: func(t *testing.T, dir string) {
				replaceIn(t, dir, "formulas/do-work.toml", "id = \"work\"\n", "id = \"work\"\nretry = { max_attempts = 3 }\n")
			},
			// The finding is additive, but the edited formula file moved
			// in ways nothing computable accounts for.
			want:        Unclassified,
			wantExit:    1,
			wantFinding: &Finding{Property: Uses, Change: Added, Verdict: Additive, Values: []string{"steps.retry"}},
		},
		{
			name: "prose reworded",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "template-fragments/report.template.md", "Write a short report, with links, when you finish.\n")
			},
			want:     Unclassified,
			wantExit: 1,
		},
		{
			name: "prose reworded alongside an addition",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "template-fragments/report.template.md", "Write a short report, with links, when you finish.\n")
				writeFile(t, dir, "formulas/review.toml", "formula = \"review\"\n")
			},
			// UNCLASSIFIED outranks ADDITIVE: the addition does not
			// account for the reworded fragment.
			want:     Unclassified,
			wantExit: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := basePack(t)
			b := copyPack(t, a)
			tc.mutate(t, b)

			r := Diff(mustCompute(t, a), mustCompute(t, b))
			if r.Verdict != tc.want {
				t.Fatalf("verdict = %s, want %s; result %+v", r.Verdict, tc.want, r)
			}
			if got := r.Verdict.ExitCode(); got != tc.wantExit {
				t.Fatalf("exit code = %d, want %d", got, tc.wantExit)
			}
			if tc.wantFinding != nil && !containsFinding(r.Findings, *tc.wantFinding) {
				t.Fatalf("findings %+v do not include %+v", r.Findings, *tc.wantFinding)
			}
			if tc.want == Unclassified && len(r.MovedFiles) == 0 {
				t.Fatalf("UNCLASSIFIED without moved files")
			}
			if tc.want == None && (len(r.Findings) != 0 || len(r.MovedFiles) != 0) {
				t.Fatalf("NONE with differences: %+v", r)
			}
		})
	}
}

func containsFinding(fs []Finding, want Finding) bool {
	for _, f := range fs {
		if reflect.DeepEqual(f, want) {
			return true
		}
	}
	return false
}

func TestDiffUnclassifiedListsMovedFiles(t *testing.T) {
	a := basePack(t)
	b := copyPack(t, a)
	writeFile(t, b, "template-fragments/report.template.md", "Write a short report, with links, when you finish.\n")
	writeFile(t, b, "docs/a file with spaces.md", "Notes.\n")

	r := Diff(mustCompute(t, a), mustCompute(t, b))
	want := []string{"docs/a file with spaces.md", "template-fragments/report.template.md"}
	if !reflect.DeepEqual(r.MovedFiles, want) {
		t.Fatalf("MovedFiles = %q, want %q", r.MovedFiles, want)
	}
	if len(r.Findings) != 0 {
		t.Fatalf("findings = %+v, want none", r.Findings)
	}
}

func TestComputeIsPathIndependentAndDeterministic(t *testing.T) {
	a := basePack(t)
	b := copyPack(t, a)

	var first, second, again bytes.Buffer
	if err := mustCompute(t, a).WriteTSV(&first); err != nil {
		t.Fatal(err)
	}
	if err := mustCompute(t, b).WriteTSV(&second); err != nil {
		t.Fatal(err)
	}
	if err := mustCompute(t, a).WriteTSV(&again); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatalf("manifest depends on location:\n%s\nvs\n%s", first.String(), second.String())
	}
	if first.String() != again.String() {
		t.Fatalf("manifest is not deterministic")
	}
}

func TestComputeBaseManifest(t *testing.T) {
	var got bytes.Buffer
	if err := mustCompute(t, basePack(t)).WriteTSV(&got); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"DEMANDS\tgc.output_json",
		"MANDATES\tgc hook --claim",
		"NORMS\tagents/worker/prompt.template.md: Never claim work through gc bd ready; claim it with gc hook --claim.",
		"OPAQUE\tagents/worker/prompt.template.md 7c86a37736d3",
		"OPAQUE\tcommands/status/run.sh b4d644d42795",
		"OPAQUE\tformulas/do-work.toml 4248a9cdc525",
		"OPAQUE\tpack.toml 4d396ae6f38e",
		"OPAQUE\ttemplate-fragments/report.template.md c90f08392ca5",
		"PROVIDES\tagent:worker",
		"PROVIDES\tcommand:status",
		"PROVIDES\tformula:do-work",
		"USES\tsteps.check",
	}, "\n") + "\n"
	if got.String() != want {
		t.Fatalf("manifest:\n%s\nwant:\n%s", got.String(), want)
	}
	for _, p := range Properties {
		if len(mustCompute(t, basePack(t)).Values(p)) == 0 {
			t.Fatalf("manifest is missing %s", p)
		}
	}
}

func TestComputeProvides(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "commands/repo/sync/run.sh", "#!/bin/sh\n")
	writeFile(t, dir, "commands/_shared/lib.sh", "#!/bin/sh\n")
	writeFile(t, dir, "roles/agents/reviewer/prompt.md", "Review.\n")
	writeFile(t, dir, "agents/coder/prompt.md", "Code.\n")
	writeFile(t, dir, "formulas/mol-a.toml", "formula = \"mol-a\"\n")
	writeFile(t, dir, "formulas/mol-b.formula.toml", "formula = \"mol-b\"\n")
	writeFile(t, dir, "formulas/notes.txt", "not a formula\n")

	got := mustCompute(t, dir).Values(Provides)
	want := []string{"agent:coder", "agent:reviewer", "command:repo sync", "formula:mol-a", "formula:mol-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PROVIDES = %q, want %q", got, want)
	}
}

func TestComputeUsesReadsTOMLStructure(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "formulas/f.toml", `formula = "f"

[requires]
formula_compiler = ">=2"

[[steps]]
id = "a"
description = """
[steps.loop]
this header is inside a string and is not a construct
"""
drain = { formula = "item" }

[[steps.children]]
id = "a1"

[steps.children.gate]
type = "human"

[[template]]
id = "t"
on_complete = { for_each = "x" }
`)
	writeFile(t, dir, "README.md", "Set formula_compiler and [steps.check] in your formula.\n")

	got := mustCompute(t, dir).Values(Uses)
	want := []string{`formulas/f.toml: requires.formula_compiler = ">=2"`, "steps.drain", "steps.gate", "steps.on_complete"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("USES = %q, want %q", got, want)
	}
}

func TestComputeNormsAreScopedToTheJudgmentSurface(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "assets/prompts/worker.md", "- You must close the bead when done.\n# Never a heading\nnever\n")
	writeFile(t, dir, "roles/agents/lead/prompt.md", "> Do not merge without review.\n")
	writeFile(t, dir, "formulas/f.toml", "formula = \"f\"\ndescription = \"You must run the tests.\"\n")
	writeFile(t, dir, "README.md", "You must read this.\n")

	got := mustCompute(t, dir).Values(Norms)
	want := []string{
		"assets/prompts/worker.md: You must close the bead when done.",
		"roles/agents/lead/prompt.md: Do not merge without review.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NORMS = %q, want %q", got, want)
	}
}

func TestComputeNormsTruncateOnARuneBoundary(t *testing.T) {
	dir := t.TempDir()
	line := "You must " + strings.Repeat("a", 149) + "—tail"
	writeFile(t, dir, "agents/w/prompt.md", line+"\n")

	got := mustCompute(t, dir).Values(Norms)
	want := "agents/w/prompt.md: " + "You must " + strings.Repeat("a", 149)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("NORMS = %q, want [%q]", got, want)
	}
}

func TestComputeDemands(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "formulas/f.toml",
		"formula = \"f\"\nmeta = { a = \"gc.kind\", b = \"gc.scope_rig\", schema = \"review.lane.v1\" }\n")

	got := mustCompute(t, dir).Values(Demands)
	want := []string{"gc.kind", "gc.scope_rig", "schema:review.lane.v1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DEMANDS = %q, want %q", got, want)
	}
}

func TestComputeSkipsGitMetadata(t *testing.T) {
	dir := basePack(t)
	writeFile(t, dir, ".git/notes.md", "You must not count this.\n")
	for _, v := range mustCompute(t, dir).Values(Opaque) {
		if strings.HasPrefix(v, ".git/") {
			t.Fatalf("OPAQUE includes git metadata: %q", v)
		}
	}
}

func TestComputeRefusesMissingDirectory(t *testing.T) {
	_, err := Compute(filepath.Join(t.TempDir(), "nope"))
	if err == nil || !strings.Contains(err.Error(), "reading pack directory") {
		t.Fatalf("err = %v, want a reading pack directory error", err)
	}
}

func TestComputeRefusesInvalidTOML(t *testing.T) {
	dir := basePack(t)
	writeFile(t, dir, "formulas/broken.toml", "formula = \n")
	_, err := Compute(dir)
	if err == nil || !strings.Contains(err.Error(), "formulas/broken.toml") {
		t.Fatalf("err = %v, want a parse error naming formulas/broken.toml", err)
	}
}

func TestExtractMandates(t *testing.T) {
	text := "Run `gc hook --claim --json` first.\n" +
		"> Then gc runtime drain-ack when told.\n" +
		"Again: gc hook --claim --json\n" +
		"gc bd ready lists work.\n" +
		"Use gc agent claim and gc hook --claim on one line.\n"
	got := ExtractMandates(text)
	want := []string{"gc hook --claim --json", "gc runtime drain-ack", "gc agent claim", "gc hook --claim"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractMandates = %q, want %q", got, want)
	}
	if got := ExtractMandates(""); got != nil {
		t.Fatalf("ExtractMandates(\"\") = %q, want nil", got)
	}
}

// Regression tests for the review of the first gc pack diff draft. Each
// reproduces a reviewer scenario that the draft reported as NONE (or
// misclassified).

// TestComputeReadsLegacyPromptTemplates covers the prompt.md.tmpl prompt
// name config.DiscoverPackAgents still accepts.
func TestComputeReadsLegacyPromptTemplates(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "agents/w/prompt.md.tmpl", "You must claim with gc hook --claim --json first.\n")
	writeFile(t, dir, "template-fragments/rules.md.tmpl", "{{ define \"rules\" }}Never merge without review.{{ end }}\n")

	m := mustCompute(t, dir)
	if got, want := m.Values(Mandates), []string{"gc hook --claim --json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("MANDATES = %q, want %q", got, want)
	}
	wantNorms := []string{
		"agents/w/prompt.md.tmpl: You must claim with gc hook --claim --json first.",
		"template-fragments/rules.md.tmpl: {{ define \"rules\" }}Never merge without review.{{ end }}",
	}
	if got := m.Values(Norms); !reflect.DeepEqual(got, wantNorms) {
		t.Fatalf("NORMS = %q, want %q", got, wantNorms)
	}
	if got := opaquePaths(m.Values(Opaque)); !reflect.DeepEqual(got, []string{"agents/w/prompt.md.tmpl", "template-fragments/rules.md.tmpl"}) {
		t.Fatalf("OPAQUE paths = %q", got)
	}

	b := copyPack(t, dir)
	writeFile(t, b, "agents/w/prompt.md.tmpl", "Claim the next item when you are ready.\n")
	if r := Diff(m, mustCompute(t, b)); r.Verdict != Breaking {
		t.Fatalf("rewriting prompt.md.tmpl: verdict = %s, want BREAKING; %+v", r.Verdict, r)
	}
}

// TestDiffCatchesNonProseChanges covers order removal, a changed
// formula_compiler requirement and a changed script: none is NONE.
func TestDiffCatchesNonProseChanges(t *testing.T) {
	base := func(t *testing.T) string {
		dir := basePack(t)
		writeFile(t, dir, "orders/gate-sweep.toml", "[order]\nformula = \"do-work\"\n")
		appendTo(t, dir, "formulas/do-work.toml", "\n[requires]\nformula_compiler = \">=2.0.0\"\n")
		// A second formula keeps the old requirement, so a pack-wide set
		// of values would hide the raise.
		writeFile(t, dir, "formulas/other.toml", "formula = \"other\"\n\n[requires]\nformula_compiler = \">=2.0.0\"\n")
		return dir
	}
	tests := []struct {
		name        string
		mutate      func(t *testing.T, dir string)
		want        Verdict
		wantFinding *Finding
		wantMoved   []string
	}{
		{
			name: "order removed",
			mutate: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, "orders", "gate-sweep.toml")); err != nil {
					t.Fatal(err)
				}
			},
			want:        Breaking,
			wantFinding: &Finding{Property: Provides, Change: Removed, Verdict: Breaking, Values: []string{"order:gate-sweep"}},
		},
		{
			name: "formula_compiler requirement raised",
			mutate: func(t *testing.T, dir string) {
				replaceIn(t, dir, "formulas/do-work.toml", ">=2.0.0", ">=9.0.0")
			},
			want:        Breaking,
			wantFinding: &Finding{Property: Uses, Change: Removed, Verdict: Breaking, Values: []string{`formulas/do-work.toml: requires.formula_compiler = ">=2.0.0"`}},
		},
		{
			name: "script body changed",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "commands/status/run.sh", "#!/bin/sh\nrm -rf \"$GC_CITY\"\n")
			},
			want:      Unclassified,
			wantMoved: []string{"commands/status/run.sh"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := base(t)
			b := copyPack(t, a)
			tc.mutate(t, b)
			r := Diff(mustCompute(t, a), mustCompute(t, b))
			if r.Verdict != tc.want {
				t.Fatalf("verdict = %s, want %s; %+v", r.Verdict, tc.want, r)
			}
			if tc.wantFinding != nil && !containsFinding(r.Findings, *tc.wantFinding) {
				t.Fatalf("findings %+v do not include %+v", r.Findings, *tc.wantFinding)
			}
			if tc.wantMoved != nil && !reflect.DeepEqual(r.MovedFiles, tc.wantMoved) {
				t.Fatalf("MovedFiles = %q, want %q", r.MovedFiles, tc.wantMoved)
			}
		})
	}
}

// TestComputeFollowsASymlinkedRoot compares a pack with a symlink to itself.
func TestComputeFollowsASymlinkedRoot(t *testing.T) {
	dir := basePack(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	a, b := mustCompute(t, dir), mustCompute(t, link)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("manifest through a symlinked root differs:\n%+v\nvs\n%+v", a.Entries, b.Entries)
	}
	if r := Diff(a, b); r.Verdict != None {
		t.Fatalf("verdict = %s, want NONE; %+v", r.Verdict, r)
	}
}

// TestComputeSymlinksInsideThePack pins the behavior for symlinked files:
// a link to a regular file inside the pack is read through; any other link
// is digested by its target string and never read.
func TestComputeSymlinksInsideThePack(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "shared/worker.md", "You must close the bead when done.\n")
	if err := os.MkdirAll(filepath.Join(dir, "agents", "w"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../shared/worker.md", filepath.Join(dir, "agents", "w", "prompt.md")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("You must never see this line.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "agents", "w", "outside.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing.md", filepath.Join(dir, "agents", "w", "dangling.md")); err != nil {
		t.Fatal(err)
	}

	m := mustCompute(t, dir)
	if got, want := m.Values(Norms), []string{"agents/w/prompt.md: You must close the bead when done."}; !reflect.DeepEqual(got, want) {
		t.Fatalf("NORMS = %q, want %q", got, want)
	}
	wantPaths := []string{"agents/w/dangling.md", "agents/w/outside.md", "agents/w/prompt.md", "shared/worker.md"}
	if got := opaquePaths(m.Values(Opaque)); !reflect.DeepEqual(got, wantPaths) {
		t.Fatalf("OPAQUE paths = %q, want %q", got, wantPaths)
	}
	if !reflect.DeepEqual(m, mustCompute(t, dir)) {
		t.Fatalf("manifest with symlinks is not deterministic")
	}
}

// TestComputeIgnoresCRLF: line endings move OPAQUE digests and nothing else.
func TestComputeIgnoresCRLF(t *testing.T) {
	a := basePack(t)
	b := copyPack(t, a)
	for _, rel := range []string{"agents/worker/prompt.template.md", "formulas/do-work.toml"} {
		data, err := os.ReadFile(filepath.Join(b, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, b, rel, strings.ReplaceAll(string(data), "\n", "\r\n"))
	}
	r := Diff(mustCompute(t, a), mustCompute(t, b))
	if r.Verdict != Unclassified || len(r.Findings) != 0 {
		t.Fatalf("LF to CRLF: verdict = %s findings = %+v, want UNCLASSIFIED with no findings", r.Verdict, r.Findings)
	}
	if got := ExtractMandates("Run gc hook --claim\r\n"); !reflect.DeepEqual(got, []string{"gc hook --claim"}) {
		t.Fatalf("ExtractMandates with CRLF = %q", got)
	}
}

// TestExtractMandatesStopsAtProse: a mandate is gc, its subcommand words,
// and its flags and argument placeholders; never the prose around it.
func TestExtractMandatesStopsAtProse(t *testing.T) {
	tests := []struct {
		line string
		want []string
	}{
		{"Run gc hook --claim and then wait", []string{"gc hook --claim"}},
		{"Run gc hook --claim then wait", []string{"gc hook --claim"}},
		{"Then gc runtime drain-ack when told.", []string{"gc runtime drain-ack"}},
		{"Use gc agent claim and gc hook --claim on one line.", []string{"gc agent claim", "gc hook --claim"}},
		{"WORK=$(gc hook --claim --json 2>/dev/null || true)", []string{"gc hook --claim --json"}},
		{"gc runtime drain-ack || true", []string{"gc runtime drain-ack"}},
		{"gc agent claim <agent> <id>            # Put a bead on an agent's hook", []string{"gc agent claim <agent> <id>"}},
		{"gc bd update \"$WORK_BEAD_ID\" --claim", []string{"gc bd update \"$WORK_BEAD_ID\" --claim"}},
		{"gc bd update <id> --claim, then work.", []string{"gc bd update <id> --claim"}},
		{"Run `gc hook --claim --drain-ack --json` first.", []string{"gc hook --claim --drain-ack --json"}},
		{"gc hook current: session has no current claim", nil},
		{"gc dolt-cleanup apply missed ${MISSED} reclaimable bytes", nil},
		{"gc runtime drain/undrain/drain-check/drain-ack", nil},
		{"gc agent-script --script {{.ConfigDir}}/lifecycle-claim-handoff.yaml", nil},
		{"dolt_gc hook --claim is not a gc command", nil},
	}
	for _, tc := range tests {
		if got := ExtractMandates(tc.line); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ExtractMandates(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}

	a := basePack(t)
	b := copyPack(t, a)
	writeFile(t, a, "agents/worker/prompt.template.md", "Run gc hook --claim and then wait.\n")
	writeFile(t, b, "agents/worker/prompt.template.md", "Run gc hook --claim then wait.\n")
	r := Diff(mustCompute(t, a), mustCompute(t, b))
	if r.Verdict != Unclassified {
		t.Fatalf("rewording around a mandate: verdict = %s, want UNCLASSIFIED; %+v", r.Verdict, r)
	}
}

// TestComputeProvidesInlineDeclarations covers [[agent]] and [[commands]]
// declared in pack.toml.
func TestComputeProvidesInlineDeclarations(t *testing.T) {
	a := t.TempDir()
	writeFile(t, a, "pack.toml", `[pack]
name = "fixture"
version = "0.1.0"

[[agent]]
name = "inline-worker"

[[commands]]
name = "audit"
description = "Audit"
long_description = "help/audit.txt"
script = "scripts/audit.sh"
`)
	m := mustCompute(t, a)
	if got, want := m.Values(Provides), []string{"agent:inline-worker", "command:audit"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("PROVIDES = %q, want %q", got, want)
	}

	b := copyPack(t, a)
	replaceIn(t, b, "pack.toml", "[[agent]]\nname = \"inline-worker\"\n", "")
	r := Diff(m, mustCompute(t, b))
	want := Finding{Property: Provides, Change: Removed, Verdict: Breaking, Values: []string{"agent:inline-worker"}}
	if r.Verdict != Breaking || !containsFinding(r.Findings, want) {
		t.Fatalf("removing an inline agent: %+v, want BREAKING with %+v", r, want)
	}
}

// TestDiffVerdictPrecedence: BREAKING > UNCLASSIFIED > ADDITIVE > NONE. A
// moved file only an added or removed provider accounts for does not make
// the verdict UNCLASSIFIED.
func TestDiffVerdictPrecedence(t *testing.T) {
	a := basePack(t)

	added := copyPack(t, a)
	writeFile(t, added, "formulas/review.toml", "formula = \"review\"\n")
	writeFile(t, added, "agents/reviewer/prompt.md", "Review the change.\n")
	writeFile(t, added, "commands/repo/sync/run.sh", "#!/bin/sh\n")
	writeFile(t, added, "orders/nightly.order.toml", "[order]\nformula = \"review\"\n")
	if r := Diff(mustCompute(t, a), mustCompute(t, added)); r.Verdict != Additive || len(r.MovedFiles) != 0 {
		t.Fatalf("pure additions: verdict = %s moved = %q, want ADDITIVE with no moved files", r.Verdict, r.MovedFiles)
	}

	masked := copyPack(t, added)
	writeFile(t, masked, "template-fragments/report.template.md", "Write a short report, with links, when you finish.\n")
	r := Diff(mustCompute(t, a), mustCompute(t, masked))
	if r.Verdict != Unclassified || !reflect.DeepEqual(r.MovedFiles, []string{"template-fragments/report.template.md"}) {
		t.Fatalf("addition plus reworded prose: verdict = %s moved = %q, want UNCLASSIFIED naming the fragment", r.Verdict, r.MovedFiles)
	}

	broken := copyPack(t, masked)
	if err := os.RemoveAll(filepath.Join(broken, "commands", "status")); err != nil {
		t.Fatal(err)
	}
	if r := Diff(mustCompute(t, a), mustCompute(t, broken)); r.Verdict != Breaking {
		t.Fatalf("removal plus prose: verdict = %s, want BREAKING", r.Verdict)
	}
}

// TestComputeDemandKeysNeedAWordBoundary: gc.kindred is not gc.kind.
func TestComputeDemandKeysNeedAWordBoundary(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "formulas/f.toml", "formula = \"f\"\nmeta = { a = \"gc.kindred\", b = \"gc.run_targets\", c = \"gc.kind\" }\n")
	if got, want := mustCompute(t, dir).Values(Demands), []string{"gc.kind"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DEMANDS = %q, want %q", got, want)
	}
}

// TestComputeNormsMinimumLength: a 12-byte rule counts; an 11-byte one does
// not.
func TestComputeNormsMinimumLength(t *testing.T) {
	dir := t.TempDir()
	twelve, eleven := "Never do it.", "Must do it."
	if len(twelve) != normsMinLength || len(eleven) != normsMinLength-1 {
		t.Fatalf("fixture lengths %d, %d", len(twelve), len(eleven))
	}
	writeFile(t, dir, "agents/w/prompt.md", twelve+"\n"+eleven+"\n")
	if got, want := mustCompute(t, dir).Values(Norms), []string{"agents/w/prompt.md: " + twelve}; !reflect.DeepEqual(got, want) {
		t.Fatalf("NORMS = %q, want %q", got, want)
	}
}

// opaquePaths returns the sorted, distinct file paths of OPAQUE values,
// which have the form "<path> <digest>".
func opaquePaths(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		path := v
		if i := strings.LastIndexByte(v, ' '); i >= 0 {
			path = v[:i]
		}
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}
