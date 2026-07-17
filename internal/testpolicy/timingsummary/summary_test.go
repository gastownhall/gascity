package timingsummary

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunAggregatesHistoricalSchemaV1Timings(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	first := timingArtifact("101", defaultRunner("runner-a"), []timingUnit{
		{UnitID: "cmd/gc:TestAlpha", Kind: "test", Package: "github.com/gastownhall/gascity/cmd/gc", Test: "TestAlpha", Outcome: "pass", DurationSeconds: 1},
		{UnitID: "cmd/gc:TestBeta", Kind: "test", Package: "github.com/gastownhall/gascity/cmd/gc", Test: "TestBeta", Outcome: "pass", DurationSeconds: 4},
		{UnitID: "cmd/gc:TestAlpha/slow", Kind: "test", Package: "github.com/gastownhall/gascity/cmd/gc", Test: "TestAlpha", Subtest: "slow", Outcome: "pass", DurationSeconds: 999},
		{UnitID: "cmd/gc", Kind: "package", Package: "github.com/gastownhall/gascity/cmd/gc", Outcome: "pass", DurationSeconds: 1000},
	})
	writeArtifact(t, root, "first/timing.json", first)
	writeArtifact(t, root, "duplicate/timing.json", first)
	writeArtifact(t, root, "second.json", timingArtifact("102", defaultRunner("runner-b"), []timingUnit{
		{UnitID: "cmd/gc:TestAlpha", Kind: "test", Package: "github.com/gastownhall/gascity/cmd/gc", Test: "TestAlpha", Outcome: "pass", DurationSeconds: 2},
		{UnitID: "cmd/gc:TestBeta", Kind: "test", Package: "github.com/gastownhall/gascity/cmd/gc", Test: "TestBeta", Outcome: "fail", DurationSeconds: 5},
		{UnitID: "cmd/gc:TestOnlyFails", Kind: "test", Package: "github.com/gastownhall/gascity/cmd/gc", Test: "TestOnlyFails", Outcome: "fail", DurationSeconds: 6},
	}))
	writeArtifact(t, root, "third.json", timingArtifact("103", defaultRunner("runner-c"), []timingUnit{
		{UnitID: "cmd/gc:TestAlpha", Kind: "test", Package: "github.com/gastownhall/gascity/cmd/gc", Test: "TestAlpha", Outcome: "pass", DurationSeconds: 100},
		{UnitID: "cmd/gc:TestBeta", Kind: "test", Package: "github.com/gastownhall/gascity/cmd/gc", Test: "TestBeta", Outcome: "skip", DurationSeconds: 0},
		{UnitID: "cmd/gc:TestOnlyFails", Kind: "test", Package: "github.com/gastownhall/gascity/cmd/gc", Test: "TestOnlyFails", Outcome: "skip", DurationSeconds: 0},
	}))

	stdout, stderr, exitCode := runSummary(root)
	if exitCode != 0 {
		t.Fatalf("Run exit = %d, stderr:\n%s", exitCode, stderr)
	}
	for _, want := range []string{
		"3 unique schema-v1 artifacts; 1 duplicate download ignored",
		"Top-level outcomes: 4 pass, 2 fail, 2 skip.",
		"| `cmd/gc:TestAlpha` | 3 | 0 | 0 | 2.000s | 100.000s | 100.000s |",
		"| `cmd/gc:TestBeta` | 1 | 1 | 1 | 4.000s | 4.000s | 4.000s |",
		"| `cmd/gc:TestAlpha` | 3 | 0 | 0 | 2156.222222 | 100.000s |",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("summary does not contain %q:\n%s", want, stdout)
		}
	}
	for _, excluded := range []string{"TestAlpha/slow", "| `cmd/gc` |", "| `cmd/gc:TestOnlyFails` |"} {
		if strings.Contains(stdout, excluded) {
			t.Fatalf("summary includes non-runnable %q:\n%s", excluded, stdout)
		}
	}
}

func TestRunCapsAndOrdersBothTablesDeterministically(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for run, multiplier := range []float64{1, 2} {
		units := make([]timingUnit, 0, 12)
		for n := 1; n <= 12; n++ {
			name := fmt.Sprintf("Test%02d", n)
			units = append(units, timingUnit{
				UnitID:          "internal/example:" + name,
				Kind:            "test",
				Package:         "github.com/gastownhall/gascity/internal/example",
				Test:            name,
				Outcome:         "pass",
				DurationSeconds: float64(n) * multiplier,
			})
		}
		writeArtifact(t, root, fmt.Sprintf("run-%d.json", run), timingArtifact(fmt.Sprint(200+run), defaultRunner(fmt.Sprintf("runner-%d", run)), units))
	}

	stdout, stderr, exitCode := runSummary(root)
	if exitCode != 0 {
		t.Fatalf("Run exit = %d, stderr:\n%s", exitCode, stderr)
	}
	for _, heading := range []string{"### Ten slowest top-level tests", "### Ten highest-variance top-level tests"} {
		section := summarySection(t, stdout, heading)
		if strings.Contains(section, "Test01") || strings.Contains(section, "Test02") {
			t.Fatalf("%s contains a unit outside the top ten:\n%s", heading, section)
		}
		previous := -1
		for n := 12; n >= 3; n-- {
			position := strings.Index(section, fmt.Sprintf("Test%02d", n))
			if position < 0 || position <= previous {
				t.Fatalf("%s is not deterministically descending at Test%02d:\n%s", heading, n, section)
			}
			previous = position
		}
	}
}

func TestRunSeparatesIncomparableRunnerProfiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for index, tc := range []struct {
		runner   timingRunner
		duration float64
	}{
		{runner: defaultRunner("ephemeral-a"), duration: 1},
		{runner: defaultRunner("ephemeral-b"), duration: 3},
		{runner: timingRunner{Label: "blacksmith-64vcpu", Name: "ephemeral-c", OS: "Linux", Arch: "X64", CPUCount: 64}, duration: 10},
	} {
		writeArtifact(t, root, fmt.Sprintf("profile-%d.json", index), timingArtifact(fmt.Sprint(300+index), tc.runner, []timingUnit{{
			UnitID: "internal/example:TestProfile", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestProfile", Outcome: "pass", DurationSeconds: tc.duration,
		}}))
	}

	stdout, stderr, exitCode := runSummary(root)
	if exitCode != 0 {
		t.Fatalf("Run exit = %d, stderr:\n%s", exitCode, stderr)
	}
	if got := strings.Count(stdout, "## Profile "); got != 2 {
		t.Fatalf("profile count = %d, want 2:\n%s", got, stdout)
	}
	for _, runnerName := range []string{"ephemeral-a", "ephemeral-b", "ephemeral-c"} {
		if strings.Contains(stdout, runnerName) {
			t.Fatalf("summary must exclude ephemeral runner name %q:\n%s", runnerName, stdout)
		}
	}
	for _, want := range []string{"| 2 | 0 | 0 | 1.000s | 3.000s | 3.000s |", "| 1 | 0 | 0 | 10.000s | 10.000s | 10.000s |"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("summary does not contain separated profile sample %q:\n%s", want, stdout)
		}
	}
}

func TestRunBreaksMetricTiesByUnitID(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for run, duration := range []float64{1, 3} {
		writeArtifact(t, root, fmt.Sprintf("tie-%d.json", run), timingArtifact(fmt.Sprint(350+run), defaultRunner(fmt.Sprintf("runner-%d", run)), []timingUnit{
			{UnitID: "internal/example:TestZulu", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestZulu", Outcome: "pass", DurationSeconds: duration},
			{UnitID: "internal/example:TestAlpha", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestAlpha", Outcome: "pass", DurationSeconds: duration},
		}))
	}

	stdout, stderr, exitCode := runSummary(root)
	if exitCode != 0 {
		t.Fatalf("Run exit = %d, stderr:\n%s", exitCode, stderr)
	}
	for _, heading := range []string{"### Ten slowest top-level tests", "### Ten highest-variance top-level tests"} {
		section := summarySection(t, stdout, heading)
		alpha := strings.Index(section, "TestAlpha")
		zulu := strings.Index(section, "TestZulu")
		if alpha < 0 || zulu < 0 || alpha > zulu {
			t.Fatalf("%s does not break equal metrics by unit ID:\n%s", heading, section)
		}
	}
}

func TestRunHandlesLargeFiniteVarianceInputsHonestly(t *testing.T) {
	t.Parallel()

	t.Run("identical large samples have zero variance", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		for run := range 2 {
			writeArtifact(t, root, fmt.Sprintf("large-%d.json", run), timingArtifact(fmt.Sprint(370+run), defaultRunner(fmt.Sprintf("runner-%d", run)), []timingUnit{{
				UnitID: "internal/example:TestLarge", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestLarge", Outcome: "pass", DurationSeconds: 1e308,
			}}))
		}

		stdout, stderr, exitCode := runSummary(root)
		if exitCode != 0 {
			t.Fatalf("Run exit = %d, stderr:\n%s", exitCode, stderr)
		}
		if strings.Contains(stdout, "+Inf") || !strings.Contains(stdout, "| 0.000000 |") {
			t.Fatalf("summary did not report finite zero variance:\n%s", stdout)
		}
	})

	t.Run("unrepresentable variance is rejected", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		for run, duration := range []float64{0, 1e308} {
			writeArtifact(t, root, fmt.Sprintf("spread-%d.json", run), timingArtifact(fmt.Sprint(380+run), defaultRunner(fmt.Sprintf("runner-%d", run)), []timingUnit{{
				UnitID: "internal/example:TestSpread", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestSpread", Outcome: "pass", DurationSeconds: duration,
			}}))
		}

		stdout, stderr, exitCode := runSummary(root)
		if exitCode != 1 || stdout != "" || !strings.Contains(stderr, "variance is not representable") {
			t.Fatalf("Run = stdout %q stderr %q exit %d", stdout, stderr, exitCode)
		}
	})

	t.Run("representable normalized variance is accepted", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		samples := []float64{0, 0, 0, 0, 0, 1e154, 1e154, 1e154, 1e154, 1e154}
		for run, duration := range samples {
			writeArtifact(t, root, fmt.Sprintf("normalized-%d.json", run), timingArtifact(fmt.Sprint(385+run), defaultRunner(fmt.Sprintf("runner-%d", run)), []timingUnit{{
				UnitID: "internal/example:TestRepresentable", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestRepresentable", Outcome: "pass", DurationSeconds: duration,
			}}))
		}

		stdout, stderr, exitCode := runSummary(root)
		if exitCode != 0 || strings.Contains(stdout, "Inf") {
			t.Fatalf("Run = stdout %q stderr %q exit %d", stdout, stderr, exitCode)
		}
		got, err := populationVariance(samples)
		if err != nil {
			t.Fatalf("populationVariance: %v", err)
		}
		const want = 2.5e307
		if relativeError := math.Abs(got-want) / want; relativeError > 1e-15 {
			t.Fatalf("populationVariance = %g, want %g (relative error %g)", got, want, relativeError)
		}

		skewed := make([]float64, 100)
		for index := 1; index < len(skewed); index++ {
			skewed[index] = 1e155
		}
		got, err = populationVariance(skewed)
		if err != nil {
			t.Fatalf("populationVariance skewed prefix: %v", err)
		}
		const skewedWant = 9.9e307
		if relativeError := math.Abs(got-skewedWant) / skewedWant; relativeError > 1e-14 {
			t.Fatalf("populationVariance skewed = %g, want %g (relative error %g)", got, skewedWant, relativeError)
		}
	})
}

func TestRunRendersUntrustedProfileMetadataAsCode(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	item := timingArtifact("390", defaultRunner("ephemeral"), []timingUnit{{
		UnitID: "internal/example:TestSafe", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestSafe", Outcome: "pass", DurationSeconds: 1,
	}})
	item.Job = "![tracker](https://example.invalid/pixel)"
	item.Variant = "**injected**"
	item.Runner.Label = "[link](https://example.invalid)"
	item.Runner.OS = "Linux|forged\nrow"
	item.Runner.Arch = "`code`</code><img src=x>"
	writeArtifact(t, root, "untrusted.json", item)

	stdout, stderr, exitCode := runSummary(root)
	if exitCode != 0 {
		t.Fatalf("Run exit = %d, stderr:\n%s", exitCode, stderr)
	}
	want := "| `![tracker](https://example.invalid/pixel)` | `**injected**` | `[link](https://example.invalid)` | `Linux&#124;forged row` | `&#96;code&#96;&lt;/code&gt;&lt;img src=x&gt;` | 32 |"
	if !strings.Contains(stdout, want) {
		t.Fatalf("profile metadata is not safely code-rendered; want row %q:\n%s", want, stdout)
	}
	for _, unsafe := range []string{"| ![tracker]", "| **injected**", "| [link]", "\nrow |", "</code><img"} {
		if strings.Contains(stdout, unsafe) {
			t.Fatalf("summary contains active Markdown/HTML fragment %q:\n%s", unsafe, stdout)
		}
	}
}

func TestRunRejectsMalformedUnsupportedAndConflictingArtifacts(t *testing.T) {
	t.Parallel()

	valid := timingArtifact("401", defaultRunner("runner-a"), []timingUnit{{
		UnitID: "internal/example:TestValid", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestValid", Outcome: "pass", DurationSeconds: 1,
	}})
	for _, tc := range []struct {
		name       string
		artifacts  [][]byte
		wantStderr string
	}{
		{name: "malformed JSON", artifacts: [][]byte{[]byte("{not-json")}, wantStderr: "decode schema"},
		{name: "unsupported schema", artifacts: [][]byte{[]byte(`{"schema":2}`)}, wantStderr: "unsupported schema 2"},
		{name: "missing profile metadata", artifacts: [][]byte{mustJSON(t, timingArtifact("402", timingRunner{}, nil))}, wantStderr: "runner label is required"},
		{name: "invalid unit duration", artifacts: [][]byte{mustJSON(t, timingArtifact("403", defaultRunner("runner-a"), []timingUnit{{UnitID: "x:T", Kind: "test", Package: "x", Test: "T", Outcome: "pass", DurationSeconds: -1}}))}, wantStderr: "duration_seconds must be non-negative"},
		{name: "conflicting duplicate", artifacts: [][]byte{mustJSON(t, valid), mustJSON(t, withFirstDuration(valid, 2))}, wantStderr: "conflicting duplicate artifact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for index, data := range tc.artifacts {
				path := filepath.Join(root, fmt.Sprintf("artifact-%d.json", index))
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatalf("write artifact: %v", err)
				}
			}
			stdout, stderr, exitCode := runSummary(root)
			if exitCode != 1 {
				t.Fatalf("Run exit = %d, want 1; stdout=%q stderr=%q", exitCode, stdout, stderr)
			}
			if !strings.Contains(stderr, tc.wantStderr) {
				t.Fatalf("stderr does not contain %q: %s", tc.wantStderr, stderr)
			}
			if stdout != "" {
				t.Fatalf("failed Run wrote stdout: %q", stdout)
			}
		})
	}
}

func TestRunRequiresArtifactRoots(t *testing.T) {
	t.Parallel()

	stdout, stderr, exitCode := runSummary()
	if exitCode != 2 || stdout != "" || !strings.Contains(stderr, "usage:") {
		t.Fatalf("Run() = stdout %q stderr %q exit %d", stdout, stderr, exitCode)
	}
}

func TestRunRejectsInvalidFormatArguments(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "missing", args: []string{"--format"}, want: "--format requires a value"},
		{name: "unsupported", args: []string{"--format=yaml"}, want: `unsupported format "yaml"`},
		{name: "repeated", args: []string{"--format=json", "--format=markdown"}, want: "--format may be specified only once"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, exitCode := runSummary(tc.args...)
			if exitCode != 2 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("Run(%q) = stdout %q stderr %q exit %d", tc.args, stdout, stderr, exitCode)
			}
		})
	}
}

func TestRunRequiresMutationArgumentsTogether(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "update history only", args: []string{"--update-history", "history.json", "artifacts"}},
		{name: "run envelope only", args: []string{"--run-envelope", "run.json", "artifacts"}},
		{name: "retention only", args: []string{"--retain-runs", "5", "artifacts"}},
		{name: "missing retention", args: []string{"--update-history", "history.json", "--run-envelope", "run.json", "artifacts"}},
		{name: "missing run envelope", args: []string{"--update-history", "history.json", "--retain-runs", "5", "artifacts"}},
		{name: "missing update history", args: []string{"--run-envelope", "run.json", "--retain-runs", "5", "artifacts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, exitCode := runSummary(tc.args...)
			if exitCode != 2 || stdout != "" {
				t.Fatalf("Run(%q) = stdout %q stderr %q exit %d", tc.args, stdout, stderr, exitCode)
			}
			for _, want := range []string{"--update-history", "--run-envelope", "--retain-runs", "must be specified together"} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("Run(%q) stderr does not contain %q: %s", tc.args, want, stderr)
				}
			}
		})
	}
}

func TestRunRejectsRepeatedMutationArguments(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "update history",
			args: []string{"--update-history", "history.json", "--update-history=other.json", "--run-envelope", "run.json", "--retain-runs", "5", "artifacts"},
			want: "--update-history may be specified only once",
		},
		{
			name: "run envelope",
			args: []string{"--update-history", "history.json", "--run-envelope", "run.json", "--run-envelope=other.json", "--retain-runs", "5", "artifacts"},
			want: "--run-envelope may be specified only once",
		},
		{
			name: "retention",
			args: []string{"--update-history", "history.json", "--run-envelope", "run.json", "--retain-runs", "5", "--retain-runs=10", "artifacts"},
			want: "--retain-runs may be specified only once",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, exitCode := runSummary(tc.args...)
			if exitCode != 2 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("Run(%q) = stdout %q stderr %q exit %d; want stderr containing %q", tc.args, stdout, stderr, exitCode, tc.want)
			}
		})
	}
}

func TestRunRejectsInvalidRetainRunsArguments(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing value",
			args: []string{"--update-history", "history.json", "--run-envelope", "run.json", "artifacts", "--retain-runs"},
			want: "--retain-runs requires a value",
		},
		{
			name: "empty value",
			args: []string{"--update-history", "history.json", "--run-envelope", "run.json", "--retain-runs=", "artifacts"},
			want: "--retain-runs requires a value",
		},
		{
			name: "zero",
			args: []string{"--update-history", "history.json", "--run-envelope", "run.json", "--retain-runs", "0", "artifacts"},
			want: "--retain-runs must be a positive integer",
		},
		{
			name: "negative",
			args: []string{"--update-history", "history.json", "--run-envelope", "run.json", "--retain-runs=-1", "artifacts"},
			want: "--retain-runs must be a positive integer",
		},
		{
			name: "fractional",
			args: []string{"--update-history", "history.json", "--run-envelope", "run.json", "--retain-runs", "1.5", "artifacts"},
			want: "--retain-runs must be a positive integer",
		},
		{
			name: "non-numeric",
			args: []string{"--update-history", "history.json", "--run-envelope", "run.json", "--retain-runs", "many", "artifacts"},
			want: "--retain-runs must be a positive integer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, exitCode := runSummary(tc.args...)
			if exitCode != 2 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("Run(%q) = stdout %q stderr %q exit %d; want stderr containing %q", tc.args, stdout, stderr, exitCode, tc.want)
			}
		})
	}
}

func TestRunRejectsRecognizedOptionAsSpaceFormValue(t *testing.T) {
	t.Parallel()

	args := []string{
		"--update-history", "--format=json",
		"--run-envelope", "run.json",
		"--retain-runs", "5",
		"missing-artifacts",
	}
	stdout, stderr, exitCode := runSummary(args...)
	if exitCode != 2 || stdout != "" || !strings.Contains(stderr, "--update-history requires a value") {
		t.Fatalf("Run(%q) = stdout %q stderr %q exit %d; want a usage error for an option used as a value", args, stdout, stderr, exitCode)
	}
}

func TestRunMutationRequiresArtifactRoot(t *testing.T) {
	t.Parallel()

	args := []string{"--update-history", "history.json", "--run-envelope", "run.json", "--retain-runs", "5"}
	stdout, stderr, exitCode := runSummary(args...)
	if exitCode != 2 || stdout != "" || !strings.Contains(stderr, "usage:") {
		t.Fatalf("Run(%q) = stdout %q stderr %q exit %d", args, stdout, stderr, exitCode)
	}
}

func TestRunUpdatesHistoryFromEnvelopeFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	databasePath := filepath.Join(t.TempDir(), "timing-history.json")
	envelopePath := filepath.Join(t.TempDir(), "run-envelope.json")
	envelope := historyRunEnvelope("50", "sha-50", "2026-07-15T13:00:00Z")
	writeArtifact(t, root, "timing.json", historyArtifact(envelope, "shard-a", []timingUnit{{
		UnitID: "internal/example:TestCLI", Kind: "test",
		Package: "github.com/gastownhall/gascity/internal/example", Test: "TestCLI",
		Outcome: "pass", DurationSeconds: 1.25,
	}}))
	if err := os.WriteFile(envelopePath, mustJSON(t, envelope), 0o600); err != nil {
		t.Fatalf("write run envelope: %v", err)
	}

	stdout, stderr, exitCode := runSummary(
		"--update-history", databasePath,
		"--run-envelope="+envelopePath,
		"--retain-runs", "10",
		"--format=json",
		root,
	)
	if exitCode != 0 || stderr != "" {
		t.Fatalf("Run mutation = stdout %q stderr %q exit %d", stdout, stderr, exitCode)
	}
	snapshot := decodeHistorySnapshot(t, stdout)
	unit := findHistoryUnit(t, findHistoryProfile(t, snapshot, 32), "internal/example:TestCLI")
	if unit.Passes != 1 || historyFloat(t, unit.DurationSecondsP95) != 1.25 {
		t.Fatalf("mutated snapshot unit = %+v, want one 1.25s pass", unit)
	}
	if _, err := os.Stat(databasePath); err != nil {
		t.Fatalf("history database was not published: %v", err)
	}
}

type timingArtifactFixture struct {
	Schema     int          `json:"schema"`
	ShardID    string       `json:"shard_id"`
	Variant    string       `json:"variant"`
	CommitSHA  string       `json:"commit_sha"`
	Workflow   string       `json:"workflow"`
	RunID      string       `json:"run_id"`
	RunAttempt string       `json:"run_attempt"`
	Job        string       `json:"job"`
	Runner     timingRunner `json:"runner"`
	Units      []timingUnit `json:"units"`
}

type timingRunner struct {
	Label    string `json:"label"`
	Name     string `json:"name"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUCount int    `json:"cpu_count"`
}

type timingUnit struct {
	UnitID          string  `json:"unit_id"`
	Kind            string  `json:"kind"`
	Package         string  `json:"package"`
	Test            string  `json:"test"`
	Subtest         string  `json:"subtest"`
	Outcome         string  `json:"outcome"`
	DurationSeconds float64 `json:"duration_seconds"`
}

func timingArtifact(runID string, runner timingRunner, units []timingUnit) timingArtifactFixture {
	return timingArtifactFixture{
		Schema: 1, ShardID: "cmd-gc-process-1-of-12", Variant: "linux-default", CommitSHA: "deadbeef",
		Workflow: "CI", RunID: runID, RunAttempt: "1", Job: "cmd-gc-process", Runner: runner, Units: units,
	}
}

func defaultRunner(name string) timingRunner {
	return timingRunner{Label: "blacksmith-32vcpu", Name: name, OS: "Linux", Arch: "X64", CPUCount: 32}
}

func withFirstDuration(artifact timingArtifactFixture, duration float64) timingArtifactFixture {
	copyArtifact := artifact
	copyArtifact.Units = append([]timingUnit(nil), artifact.Units...)
	copyArtifact.Units[0].DurationSeconds = duration
	return copyArtifact
}

func writeArtifact(t *testing.T, root, relativePath string, artifact timingArtifactFixture) {
	t.Helper()
	path := filepath.Join(root, relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create artifact directory: %v", err)
	}
	if err := os.WriteFile(path, mustJSON(t, artifact), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return data
}

func runSummary(args ...string) (string, string, int) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := Run(args, &stdout, &stderr)
	return stdout.String(), stderr.String(), exitCode
}

func summarySection(t *testing.T, output, heading string) string {
	t.Helper()
	start := strings.Index(output, heading)
	if start < 0 {
		t.Fatalf("summary does not contain heading %q:\n%s", heading, output)
	}
	section := output[start+len(heading):]
	if next := strings.Index(section, "\n### "); next >= 0 {
		section = section[:next]
	}
	return section
}
