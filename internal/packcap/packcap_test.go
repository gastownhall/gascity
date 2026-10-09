package packcap

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
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

// TestDiffVerdicts ports every verdict case of the original
// contrib/pack-capability/pack_diff_test.sh and adds one case per
// property and direction.
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
			want:        Additive,
			wantExit:    0,
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
			want:        Additive,
			wantExit:    0,
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
			want:     Additive,
			wantExit: 0,
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
	want := []string{"requires.formula_compiler", "steps.drain", "steps.gate", "steps.on_complete"}
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
	want := []string{"gc hook --claim --json", "gc runtime drain-ack when told", "gc agent claim and gc hook --claim on one line"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractMandates = %q, want %q", got, want)
	}
	if got := ExtractMandates(""); got != nil {
		t.Fatalf("ExtractMandates(\"\") = %q, want nil", got)
	}
}
