package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// splitStrictBaselineHeader is the baseline's column header.
const splitStrictBaselineHeader = "# file\tfunc\top\tview\tclass\tcaller\tmax_lines\tmax_caller_lines\ttests\tscope\tverdict\tnote\n"

// splitStrictPkg lays out a package's gate inputs in a temp dir: the
// baseline rows, an empty known-failures list and an empty flaky list.
func splitStrictPkg(t *testing.T, baselineRows ...string) (root, pkg string) {
	t.Helper()
	root = t.TempDir()
	pkg = "pkg"
	files := map[string]string{
		"split_strict_baseline.tsv":        splitStrictBaselineHeader + strings.Join(baselineRows, ""),
		"split_strict_known_failures.list": "# header\n",
		"split_strict_known_flaky.list":    "# header\n",
	}
	for name, body := range files {
		path := filepath.Join(root, pkg, "testdata", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, pkg
}

// splitStrictShard writes one shard's test.log of a run, carrying the
// observed table rows the gate prints.
func splitStrictShard(t *testing.T, root, run, shard string, rows ...string) {
	t.Helper()
	path := filepath.Join(root, run, shard, "test.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	log := "=== RUN   TestA\n--- PASS: TestA (0.00s)\n" +
		"SPLIT-STRICT TABLE BEGIN split-strict-observed.tsv\n" +
		"# file\tfunc\top\tview\tclass\tcaller\tlines\tcaller_lines\ttests\tevents\ttest_names\tsample_via\n" +
		strings.Join(rows, "") + "SPLIT-STRICT TABLE END\n"
	if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runSplitStrictBaseline(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	cmd := testCommand("python3", append([]string{filepath.Join(repoRoot(t), "scripts", "split-strict-baseline")}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestSplitStrictBaselineCheckCapsLinesAcrossShards: the gate caps a
// function's distinct lines per shard only, so two shards each reaching one
// new line both pass. --check over every shard's log caps the union.
func TestSplitStrictBaselineCheckCapsLinesAcrossShards(t *testing.T) {
	root, pkg := splitStrictPkg(t, "cmd/gc/a.go\tgc.a\tget\twork\tgraph\tgc.caller\t1\t1\t2\t*\tmisroute\tnote\n")
	row := func(line, callerLine string) string {
		return "cmd/gc/a.go\tgc.a\tget\twork\tgraph\tgc.caller\t" + line + "\t" + callerLine + "\t1\t1\tTestA\t\n"
	}
	splitStrictShard(t, root, "run", "shard_1_of_2", row("10", "20"))
	splitStrictShard(t, root, "run", "shard_2_of_2", row("10", "20"))
	if out, err := runSplitStrictBaseline(t, root, "--check", pkg, "run"); err != nil {
		t.Fatalf("--check over shards at the capped lines failed: %v\n%s", err, out)
	}
	splitStrictShard(t, root, "run", "shard_2_of_2", row("11", "21"))
	out, err := runSplitStrictBaseline(t, root, "--check", pkg, "run")
	if err == nil {
		t.Fatalf("--check passed a function reached at 2 lines across shards against a cap of 1:\n%s", out)
	}
	for _, want := range []string{"LINES cmd/gc/a.go gc.a: 2 distinct line(s) (10,11) across shards, cap 1", "CALLER LINES gc.caller: 2 distinct line(s) (20,21) across shards, cap 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("--check output lacks %q:\n%s", want, out)
		}
	}
	// --check-caps (the PR lane) reports the caps alone: an unreached row is
	// the nightly's business.
	root2, pkg2 := splitStrictPkg(t,
		"cmd/gc/a.go\tgc.a\tget\twork\tgraph\tgc.caller\t1\t1\t2\t*\tmisroute\tnote\n",
		"cmd/gc/b.go\tgc.b\tget\twork\tgraph\t-\t1\t0\t1\t*\tmisroute\tunreached\n")
	splitStrictShard(t, root2, "run", "shard_1_of_2", row("10", "20"))
	if out, err := runSplitStrictBaseline(t, root2, "--check-caps", pkg2, "run"); err != nil {
		t.Fatalf("--check-caps failed on an unreached row: %v\n%s", err, out)
	}
	splitStrictShard(t, root2, "run", "shard_2_of_2", row("11", "20"))
	if out, err := runSplitStrictBaseline(t, root2, "--check-caps", pkg2, "run"); err == nil || !strings.Contains(out, "LINES cmd/gc/a.go gc.a") {
		t.Fatalf("--check-caps passed lines past the cap across shards: %v\n%s", err, out)
	}
}

// TestSplitStrictBaselineNewKeyIsAlwaysTriage: a key new on all six columns
// is triage even when exactly one old row matches it on five; the old
// verdict is only a hint in the note.
func TestSplitStrictBaselineNewKeyIsAlwaysTriage(t *testing.T) {
	root, pkg := splitStrictPkg(t, "cmd/gc/a.go\tgc.a\tget\twork\tgraph\tgc.oldCaller\t1\t1\t1\t*\tmisroute\tS3c: old path\n")
	rows := "cmd/gc/a.go\tgc.a\tget\twork\tgraph\tgc.oldCaller\t10\t20\t1\t1\tTestA\t\n" +
		"cmd/gc/a.go\tgc.a\tget\twork\tgraph\tgc.newCaller\t10\t30\t1\t1\tTestA\t\n"
	for _, run := range []string{"r1", "r2"} {
		splitStrictShard(t, root, run, "shard_1_of_1", rows)
	}
	if out, err := runSplitStrictBaseline(t, root, pkg, "r1", "r2"); err != nil {
		t.Fatalf("regeneration failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(root, pkg, "testdata", "split_strict_baseline.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	verdicts := map[string][2]string{}
	for _, line := range strings.Split(string(data), "\n") {
		cols := strings.Split(line, "\t")
		if len(cols) == 12 {
			verdicts[cols[5]] = [2]string{cols[10], cols[11]}
		}
	}
	if got := verdicts["gc.oldCaller"]; got != [2]string{"misroute", "S3c: old path"} {
		t.Errorf("the old key = %v, want its verdict and note kept", got)
	}
	got := verdicts["gc.newCaller"]
	if got[0] != "triage" {
		t.Errorf("a new caller's key = %v, want triage (a new production path is never pre-adjudicated)", got)
	}
	if !strings.Contains(got[1], "misroute: S3c: old path") {
		t.Errorf("the new key's note = %q, want the five-column match as a hint", got[1])
	}
	if !strings.HasPrefix(string(data), splitStrictBaselineHeader) {
		t.Errorf("regeneration dropped the header:\n%s", data)
	}
}

// TestSplitStrictBaselineNewKnownFailureIsUntriaged: regeneration keeps a
// known failure's reason and lists a test new to the list as UNTRIAGED with
// its first logged message, which the gate fails on until someone rules. The
// message is the one its own test logged, across parallel tests' interleaved
// output (=== CONT, === NAME).
func TestSplitStrictBaselineNewKnownFailureIsUntriaged(t *testing.T) {
	root, pkg := splitStrictPkg(t)
	known := filepath.Join(root, pkg, "testdata", "split_strict_known_failures.list")
	if err := os.WriteFile(known, []byte("# header\nTestOld\tfixture shape\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := "=== RUN   TestOld\n" +
		"    a_test.go:3: old failure\n" +
		"--- FAIL: TestOld (0.00s)\n" +
		"=== RUN   TestNew\n" +
		"=== PAUSE TestNew\n" +
		"=== RUN   TestOther\n" +
		"=== PAUSE TestOther\n" +
		"=== CONT  TestNew\n" +
		"=== CONT  TestOther\n" +
		"=== NAME  TestNew\n" +
		"    a_test.go:9: want 1, got 0\n" +
		"=== NAME  TestOther\n" +
		"    a_test.go:12: other test's log line\n" +
		"--- FAIL: TestNew (0.00s)\n" +
		"--- PASS: TestOther (0.00s)\n" +
		"SPLIT-STRICT TABLE BEGIN split-strict-observed.tsv\n" +
		"# file\tfunc\top\tview\tclass\tcaller\tlines\tcaller_lines\ttests\tevents\ttest_names\tsample_via\n" +
		"SPLIT-STRICT TABLE END\n"
	for _, run := range []string{"r1", "r2"} {
		path := filepath.Join(root, run, "shard_1_of_1", "test.log")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := runSplitStrictBaseline(t, root, pkg, "r1", "r2"); err != nil {
		t.Fatalf("regeneration failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(known)
	if err != nil {
		t.Fatal(err)
	}
	want := "# header\nTestNew\tUNTRIAGED: want 1, got 0\nTestOld\tfixture shape\n"
	if string(data) != want {
		t.Fatalf("known failures after regeneration:\n%s\nwant:\n%s", data, want)
	}
}
