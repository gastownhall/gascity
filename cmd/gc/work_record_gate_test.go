package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// alwaysReachable / neverReachable are injected commit-publication oracles so
// the work-record validation is testable without a real git repo.
func alwaysReachable(string) bool { return true }
func neverReachable(string) bool  { return false }

func TestValidateWorkRecordOnClose(t *testing.T) {
	tests := []struct {
		name      string
		meta      map[string]string
		reachable func(string) bool
		wantViol  string // substring expected in the (single) violation; "" ⇒ no violations
	}{
		{
			name:     "no-op close passes",
			meta:     map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeNoOp},
			wantViol: "",
		},
		{
			name:     "blocked close passes",
			meta:     map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked},
			wantViol: "",
		},
		{
			name: "shipped with reachable commit passes",
			meta: map[string]string{
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
				beadmeta.WorkCommitMetadataKey:  "abc123",
				beadmeta.WorkBranchMetadataKey:  "bd-x",
			},
			reachable: alwaysReachable,
			wantViol:  "",
		},
		{
			name: "shipped with unpublished commit is rejected",
			meta: map[string]string{
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
				beadmeta.WorkCommitMetadataKey:  "abc123",
				beadmeta.WorkBranchMetadataKey:  "bd-x",
			},
			reachable: neverReachable,
			wantViol:  "not published",
		},
		{
			name: "shipped without commit is rejected",
			meta: map[string]string{
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
				beadmeta.WorkBranchMetadataKey:  "bd-x",
			},
			reachable: alwaysReachable,
			wantViol:  beadmeta.WorkCommitMetadataKey,
		},
		{
			name: "shipped without branch is rejected",
			meta: map[string]string{
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
				beadmeta.WorkCommitMetadataKey:  "abc123",
			},
			reachable: alwaysReachable,
			wantViol:  beadmeta.WorkBranchMetadataKey,
		},
		{
			name:     "missing outcome is rejected",
			meta:     map[string]string{},
			wantViol: "missing " + beadmeta.WorkOutcomeMetadataKey,
		},
		{
			name:     "unknown outcome is rejected",
			meta:     map[string]string{beadmeta.WorkOutcomeMetadataKey: "done"},
			wantViol: "invalid",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reachable := tc.reachable
			if reachable == nil {
				reachable = neverReachable
			}
			bead := beads.Bead{ID: "wr-1", Type: "task", Metadata: tc.meta}
			got := validateWorkRecordOnClose(bead, reachable)
			if tc.wantViol == "" {
				if len(got) != 0 {
					t.Fatalf("expected no violations, got %v", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("expected a violation containing %q, got none", tc.wantViol)
			}
			joined := strings.Join(got, " | ")
			if !strings.Contains(joined, tc.wantViol) {
				t.Fatalf("violation %q does not contain %q", joined, tc.wantViol)
			}
		})
	}
}

func TestIsWorkRecordGatedBead(t *testing.T) {
	tests := []struct {
		name string
		bead beads.Bead
		want bool
	}{
		{name: "plain task bead is gated", bead: beads.Bead{Type: "task"}, want: true},
		{name: "empty type defaults to gated", bead: beads.Bead{}, want: true},
		{
			name: "workflow root is not gated",
			bead: beads.Bead{Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
			want: false,
		},
		{
			name: "control run step is not gated",
			bead: beads.Bead{Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindRun}},
			want: false,
		},
		{name: "convoy bead is not gated", bead: beads.Bead{Type: "convoy"}, want: false},
		{name: "message bead is not gated", bead: beads.Bead{Type: "message"}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWorkRecordGatedBead(tc.bead); got != tc.want {
				t.Fatalf("isWorkRecordGatedBead = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidWorkOutcome(t *testing.T) {
	for _, v := range []string{
		beadmeta.WorkOutcomeShipped, beadmeta.WorkOutcomeNoOp,
		beadmeta.WorkOutcomeBlocked, beadmeta.WorkOutcomeAbandoned,
	} {
		if !validWorkOutcome(v) {
			t.Errorf("validWorkOutcome(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "pass", "fail", "skipped", "done", "SHIPPED"} {
		if validWorkOutcome(v) {
			t.Errorf("validWorkOutcome(%q) = true, want false", v)
		}
	}
}

func TestWorkRecordCloseTargets(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantIDs []string
		wantOK  bool
	}{
		{"close subcommand", []string{"close", "wr-1"}, []string{"wr-1"}, true},
		{"close multiple", []string{"close", "wr-1", "wr-2"}, []string{"wr-1", "wr-2"}, true},
		{"update status=closed", []string{"update", "wr-1", "--status=closed"}, []string{"wr-1"}, true},
		{"update --status closed", []string{"update", "wr-1", "--status", "closed"}, []string{"wr-1"}, true},
		{"update -s closed", []string{"update", "wr-1", "-s", "closed"}, []string{"wr-1"}, true},
		{"last repeated status closes", []string{"update", "wr-1", "--status=open", "--status=closed"}, []string{"wr-1"}, true},
		{"last repeated status stays open", []string{"update", "wr-1", "--status=closed", "--status=open"}, nil, false},
		{"status-looking value is consumed", []string{"update", "wr-1", "--notes", "--status=open", "--status", "closed"}, []string{"wr-1"}, true},
		{"update to open is not a close", []string{"update", "wr-1", "--status=open"}, nil, false},
		{"update without status is not a close", []string{"update", "wr-1", "--notes", "x"}, nil, false},
		{"read subcommand is not a close", []string{"show", "wr-1"}, nil, false},
		{"empty args", nil, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ids, ok := workRecordCloseTargets(tc.args)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (ids=%v)", ok, tc.wantOK, ids)
			}
			if strings.Join(ids, ",") != strings.Join(tc.wantIDs, ",") {
				t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}

func TestCloseReasonCommitCandidates(t *testing.T) {
	const commit = "abc1234"
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"close reason", []string{"close", "wr-1", "--reason", "Done at " + commit}, []string{commit}},
		{"update notes", []string{"update", "wr-1", "--notes=Commit " + commit}, []string{commit}},
		{"other flag consumes reason-looking value", []string{"update", "wr-1", "--description", "--notes", commit}, nil},
		{"terminator stops flag parsing", []string{"close", "wr-1", "--", "--reason", commit}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := closeReasonCommitCandidates(tc.args, "")
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("closeReasonCommitCandidates = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEvaluateWorkRecordCloseGate exercises the full gate plumbing (store read,
// scoping, warn vs enforce fork) over an in-memory store, covering ADR-0009
// acceptance (b)/(c) at the integration level.
func TestEvaluateWorkRecordCloseGate(t *testing.T) {
	beadsList := []beads.Bead{
		{ID: "wr-shipped-nocommit", Type: "task", Status: "in_progress", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped}},
		{ID: "wr-noop", Type: "task", Status: "in_progress", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeNoOp}},
		{ID: "wr-atomic-noop", Type: "task", Status: "in_progress", Metadata: map[string]string{}},
		{ID: "wr-missing", Type: "task", Status: "in_progress", Metadata: map[string]string{}},
		{ID: "wr-control", Type: "task", Status: "in_progress", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
	}
	newStore := func() beads.Store { return beads.NewMemStoreFrom(1, beadsList, nil) }

	tests := []struct {
		name      string
		args      []string
		enforce   bool
		wantBlock bool
		wantWarn  string // substring expected on stderr; "" ⇒ no output
	}{
		{"non-close subcommand is ignored", []string{"show", "wr-shipped-nocommit"}, true, false, ""},
		{"control bead is exempt", []string{"close", "wr-control"}, true, false, ""},
		{"no-op close passes", []string{"close", "wr-noop"}, true, false, ""},
		{"shipped-no-commit warns only by default", []string{"close", "wr-shipped-nocommit"}, false, false, "work-record gate (warn-only)"},
		{"shipped-no-commit blocks when enforced", []string{"close", "wr-shipped-nocommit"}, true, true, "work-record gate (enforced)"},
		{"missing outcome blocks when enforced", []string{"close", "wr-missing"}, true, true, "missing " + beadmeta.WorkOutcomeMetadataKey},
		{"update --status=closed is gated", []string{"update", "wr-shipped-nocommit", "--status=closed"}, true, true, "close of wr-shipped-nocommit"},
		{
			"atomic update validates submitted metadata",
			[]string{"update", "wr-atomic-noop", "--set-metadata", beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeNoOp, "--status=closed"},
			true,
			false,
			"",
		},
		{
			"metadata JSON validates submitted no-op",
			[]string{"update", "wr-missing", "--metadata", `{"gc.work_outcome":"no-op"}`, "--status=closed"},
			true,
			false,
			"",
		},
		{
			"metadata equals JSON validates submitted no-op",
			[]string{"update", "wr-missing", `--metadata={"gc.work_outcome":"no-op"}`, "--status=closed"},
			true,
			false,
			"",
		},
		{
			"last repeated metadata JSON value wins",
			[]string{"update", "wr-missing", `--metadata={"gc.work_outcome":"no-op"}`, `--metadata={"unrelated":"value"}`, "--status=closed"},
			true,
			true,
			"missing " + beadmeta.WorkOutcomeMetadataKey,
		},
		{
			"last repeated metadata JSON ignores an earlier malformed value",
			[]string{"update", "wr-missing", `--metadata={not-json}`, `--metadata={"gc.work_outcome":"no-op"}`, "--status=closed"},
			true,
			false,
			"",
		},
		{
			"metadata JSON cannot hide shipped evidence requirements behind stored no-op",
			[]string{"update", "wr-noop", `--metadata={"gc.work_outcome":"shipped"}`, "--status=closed"},
			true,
			true,
			beadmeta.WorkCommitMetadataKey,
		},
		{
			"metadata JSON cannot combine with later set-metadata",
			[]string{"update", "wr-noop", `--metadata={"gc.work_outcome":"shipped"}`, "--set-metadata", beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeNoOp, "--status=closed"},
			true,
			true,
			"cannot project metadata",
		},
		{
			"metadata JSON cannot combine with earlier set-metadata",
			[]string{"update", "wr-noop", "--set-metadata", beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeNoOp, `--metadata={"gc.work_outcome":"shipped"}`, "--status=closed"},
			true,
			true,
			"cannot project metadata",
		},
		{
			"unset-metadata wins over set-metadata regardless of argv order",
			[]string{"update", "wr-missing", "--unset-metadata", beadmeta.WorkOutcomeMetadataKey, "--set-metadata", beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeNoOp, "--status=closed"},
			true,
			true,
			"missing " + beadmeta.WorkOutcomeMetadataKey,
		},
		{
			"metadata JSON cannot combine with unset-metadata",
			[]string{"update", "wr-noop", "--unset-metadata", beadmeta.WorkOutcomeMetadataKey, `--metadata={"gc.work_outcome":"no-op"}`, "--status=closed"},
			true,
			true,
			"cannot project metadata",
		},
		{
			"non-string metadata uses beads StringMap coercion",
			[]string{"update", "wr-noop", `--metadata={"gc.work_outcome":true}`, "--status=closed"},
			true,
			true,
			`invalid gc.work_outcome="true"`,
		},
		{
			"malformed metadata JSON fails closed",
			[]string{"update", "wr-noop", `--metadata={not-json}`, "--status=closed"},
			true,
			true,
			"cannot project --metadata",
		},
		{
			"metadata file input fails closed",
			[]string{"update", "wr-noop", "--metadata", "@work-record.json", "--status=closed"},
			true,
			true,
			"cannot project --metadata",
		},
		{
			"metadata-looking positional after terminator is not projected",
			[]string{"update", "wr-missing", "--status=closed", "--", "--set-metadata=" + beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeNoOp},
			true,
			true,
			"missing " + beadmeta.WorkOutcomeMetadataKey,
		},
		{
			"metadata-like flag value is not submitted metadata",
			[]string{"update", "wr-missing", "--notes", "--set-metadata=" + beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeNoOp, "--status=closed"},
			true,
			true,
			"missing " + beadmeta.WorkOutcomeMetadataKey,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr strings.Builder
			block := evaluateWorkRecordCloseGate(tc.args, newStore(), nil, t.TempDir(), tc.enforce, &stderr)
			if block != tc.wantBlock {
				t.Fatalf("block = %v, want %v; stderr=%s", block, tc.wantBlock, stderr.String())
			}
			out := stderr.String()
			if tc.wantWarn == "" {
				if out != "" {
					t.Fatalf("expected no gate output, got %q", out)
				}
				return
			}
			if !strings.Contains(out, tc.wantWarn) {
				t.Fatalf("gate output %q does not contain %q", out, tc.wantWarn)
			}
		})
	}
}

func TestEvaluateWorkRecordCloseGateAtomicShippedUpdate(t *testing.T) {
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "--initial-branch=main")
	runGit(t, repoDir, "config", "user.name", "Gas City Test")
	runGit(t, repoDir, "config", "user.email", "gc-test@test.local")
	writeFile := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	writeFile("artifact.txt", "base\n")
	runGit(t, repoDir, "add", "artifact.txt")
	runGit(t, repoDir, "commit", "-m", "test: add base artifact")
	base := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))
	runGit(t, repoDir, "update-ref", "refs/remotes/origin/main", base)

	runGit(t, repoDir, "switch", "-c", "feature")
	writeFile("artifact.txt", "integrated\n")
	runGit(t, repoDir, "add", "artifact.txt")
	runGit(t, repoDir, "commit", "-m", "test: integrate artifact")
	commit := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))

	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{
			ID:     "wr-atomic-shipped",
			Type:   "task",
			Status: "in_progress",
			Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey: repoDir,
			},
		},
		{
			ID:     "wr-reason-only",
			Type:   "task",
			Status: "in_progress",
			Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:     repoDir,
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeNoOp,
			},
		},
	}, nil)
	args := []string{
		"update", "wr-atomic-shipped",
		"--set-metadata", beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeShipped,
		"--set-metadata", beadmeta.WorkCommitMetadataKey + "=" + commit,
		"--set-metadata", beadmeta.WorkBranchMetadataKey + "=feature",
		"--status=closed",
		"--notes", "Done. Commit: " + commit + ".",
	}
	reasonArgs := []string{"close", "wr-reason-only", "--reason", "Done. Commit: " + commit + "."}

	for _, closeArgs := range [][]string{args, reasonArgs} {
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate(closeArgs, store, nil, repoDir, false, &stderr); block {
			t.Fatalf("warn-only unpublished close blocked; stderr=%s", stderr.String())
		}
		if got := stderr.String(); !strings.Contains(got, "not published") {
			t.Fatalf("unpublished close stderr = %q, want publication warning", got)
		}
	}

	runGit(t, repoDir, "switch", "main")
	writeFile("unrelated.txt", "newer main work\n")
	runGit(t, repoDir, "add", "unrelated.txt")
	runGit(t, repoDir, "commit", "-m", "test: add unrelated main work")
	runGit(t, repoDir, "cherry-pick", "--no-commit", commit)
	runGit(t, repoDir, "commit", "-m", "test: squash feature")
	squash := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))
	if squash == commit {
		t.Fatal("squash commit unexpectedly retained the feature commit SHA")
	}
	runGit(t, repoDir, "update-ref", "refs/remotes/origin/main", squash)

	for _, closeArgs := range [][]string{args, reasonArgs} {
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate(closeArgs, store, nil, repoDir, true, &stderr); block {
			t.Fatalf("published squash-equivalent close blocked; stderr=%s", stderr.String())
		}
		if got := stderr.String(); got != "" {
			t.Fatalf("published squash-equivalent close warned: %q", got)
		}
	}
}

// panicOnGetStore embeds a nil beads.Store and overrides Get to panic. It
// proves a code path never falls back to the store for a given ID — used to
// assert the close gate actually consumes preFetched beads instead of
// re-reading them: gc bd close previously paid for the same store.Get twice,
// once in the write-ID guard and once in this gate.
type panicOnGetStore struct{ beads.Store }

func (panicOnGetStore) Get(id string) (beads.Bead, error) {
	panic("store.Get called for id " + id + ": preFetched bead should have been used")
}

func TestEvaluateWorkRecordCloseGateUsesPreFetchedBead(t *testing.T) {
	preFetched := map[string]beads.Bead{
		"wr-shipped-nocommit": {ID: "wr-shipped-nocommit", Type: "task", Status: "in_progress", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped}},
	}
	var stderr strings.Builder
	block := evaluateWorkRecordCloseGate([]string{"close", "wr-shipped-nocommit"}, panicOnGetStore{}, preFetched, t.TempDir(), true, &stderr)
	if !block {
		t.Fatalf("expected block=true for shipped-without-commit, got false; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "work-record gate (enforced)") {
		t.Fatalf("expected enforced gate output, got %q", stderr.String())
	}
}

// TestRunWorkRecordCloseGateReusesPreOpenedStore proves runWorkRecordCloseGate
// never calls openStoreAtForCity when handed a preOpened store — it's the IO
// wrapper's half of the dedup (evaluateWorkRecordCloseGate proves the
// preFetched-bead half above). cityPath is deliberately bogus: opening a
// real store at it would fail, causing the gate to fail open (block=false, no
// stderr) — indistinguishable from a no-op success. Asserting a violation
// fires instead proves preOpened/preFetched were actually used, not silently
// bypassed by a failed fallback open.
func TestRunWorkRecordCloseGateReusesPreOpenedStore(t *testing.T) {
	preFetched := map[string]beads.Bead{
		"wr-shipped-nocommit": {ID: "wr-shipped-nocommit", Type: "task", Status: "in_progress", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped}},
	}
	var stderr strings.Builder
	const bogusCityPath = "/nonexistent/does-not-exist"
	t.Setenv(workRecordEnforceEnvVar, "1")
	block := runWorkRecordCloseGate([]string{"close", "wr-shipped-nocommit"}, t.TempDir(), bogusCityPath, nil, panicOnGetStore{}, preFetched, &stderr)
	if !block {
		t.Fatalf("expected block=true for shipped-without-commit, got false (fallback store open may have silently swallowed the preOpened store); stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "work-record gate (enforced)") {
		t.Fatalf("expected enforced gate output, got %q", stderr.String())
	}
}

func TestWorkRecordEnforceEnabled(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv(workRecordEnforceEnvVar, v)
		if !workRecordEnforceEnabled() {
			t.Errorf("workRecordEnforceEnabled(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "off", "nope"} {
		t.Setenv(workRecordEnforceEnvVar, v)
		if workRecordEnforceEnabled() {
			t.Errorf("workRecordEnforceEnabled(%q) = true, want false", v)
		}
	}
}
