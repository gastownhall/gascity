package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// alwaysReachable / neverReachable are injected commit-reachability oracles so
// the work-record validation is testable without a real git repo.
func alwaysReachable(string, string) bool { return true }
func neverReachable(string, string) bool  { return false }

// alwaysOnRemote / neverOnRemote are injected durability oracles: they report
// whether a commit is contained in any remote-tracking ref. Reachability on a
// branch and presence on a remote are independent — a commit on a local-only
// branch is reachable but not durable — so they are separate oracles.
func alwaysOnRemote(string) bool { return true }
func neverOnRemote(string) bool  { return false }

// noRemoteContains / remoteContains are injected containment resolvers: they
// report which remote-tracking branches contain a commit. The stale claim-stamp
// rule (ADR-0009 Defect C) uses them to distinguish a delivered commit whose
// stamped gc.work_branch is merely stale from genuinely undelivered work.
func noRemoteContains(string) []string { return nil }

func remoteContains(branches ...string) func(string) []string {
	return func(string) []string { return branches }
}

func TestValidateWorkRecordOnClose(t *testing.T) {
	tests := []struct {
		name       string
		meta       map[string]string
		reachable  func(string, string) bool
		onRemote   func(string) bool
		containing func(string) []string
		wantViol   string // substring expected in the (single) violation; "" ⇒ no violations
		wantAdv    string // substring expected in the (single) advisory; "" ⇒ no advisories
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
			// az-6n75: the data-loss case. The polecat committed and the commit is
			// reachable on the branch it recorded, but the branch was never pushed,
			// so the work exists only in a worktree that any prune can destroy.
			// Reachability alone cannot see this — the branch resolves locally.
			name: "shipped with commit reachable locally but NOT on any remote is rejected",
			meta: map[string]string{
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
				beadmeta.WorkCommitMetadataKey:  "abc123",
				beadmeta.WorkBranchMetadataKey:  "gc-gastown.rictus-5bca5afe897d",
			},
			reachable: alwaysReachable,
			onRemote:  neverOnRemote,
			wantViol:  "not present on any remote",
		},
		{
			name: "shipped with commit NOT reachable on branch is rejected",
			meta: map[string]string{
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
				beadmeta.WorkCommitMetadataKey:  "abc123",
				beadmeta.WorkBranchMetadataKey:  "bd-x",
			},
			reachable: neverReachable,
			wantViol:  "not reachable",
		},
		{
			// ADR-0009 Defect C: gc.work_branch is stamped at claim time, before
			// the work exists, so an honest delivered close can carry a stale
			// branch. Delivered work (on a remote-tracking ref) must not be
			// blocked for a stale stamp — it gets a precise advisory naming the
			// branch the work actually landed on, so the record can be corrected.
			name: "shipped delivered on another remote branch passes with a stale-stamp advisory",
			meta: map[string]string{
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
				beadmeta.WorkCommitMetadataKey:  "abc123",
				beadmeta.WorkBranchMetadataKey:  "gc-gastown.nux-1a2b3c4d5e6f",
			},
			reachable:  neverReachable,
			onRemote:   alwaysOnRemote,
			containing: remoteContains("fix/wr-defect-c"),
			wantViol:   "",
			wantAdv:    "fix/wr-defect-c",
		},
		{
			// The advisory path must not open the az-6n75 hole: an unreachable
			// commit that is on NO remote ref is undelivered work and still
			// violates, stale stamp or not.
			name: "shipped unreachable and on no remote ref stays rejected",
			meta: map[string]string{
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
				beadmeta.WorkCommitMetadataKey:  "abc123",
				beadmeta.WorkBranchMetadataKey:  "gc-gastown.nux-1a2b3c4d5e6f",
			},
			reachable:  neverReachable,
			onRemote:   neverOnRemote,
			containing: noRemoteContains,
			wantViol:   "not reachable",
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
			onRemote := tc.onRemote
			if onRemote == nil {
				// Default to durable so pre-existing cases exercise only the rule
				// they were written for.
				onRemote = alwaysOnRemote
			}
			containing := tc.containing
			if containing == nil {
				// Default to no containing branches so pre-existing cases keep
				// their original violation semantics.
				containing = noRemoteContains
			}
			bead := beads.Bead{ID: "wr-1", Type: "task", Metadata: tc.meta}
			got, advisories := validateWorkRecordOnClose(bead, reachable, onRemote, containing)
			if tc.wantAdv == "" {
				if len(advisories) != 0 {
					t.Fatalf("expected no advisories, got %v", advisories)
				}
			} else {
				if len(advisories) == 0 {
					t.Fatalf("expected an advisory containing %q, got none", tc.wantAdv)
				}
				if joined := strings.Join(advisories, " | "); !strings.Contains(joined, tc.wantAdv) {
					t.Fatalf("advisory %q does not contain %q", joined, tc.wantAdv)
				}
			}
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

// newGateRepo creates a repo with one configured remote (remoteName) backed by
// a bare origin directory, a base commit on main pushed to that remote, and the
// remote's HEAD symref set (as a real clone would have it). It is the shared
// setup for every real-repo gate test; keeping it in one place keeps the gate
// regression scenarios from silently diverging.
func newGateRepo(t *testing.T, remoteName string) (repoDir string) {
	t.Helper()
	bareDir := t.TempDir()
	runGit(t, bareDir, "init", "--bare", "--initial-branch=main")

	repoDir = t.TempDir()
	runGit(t, repoDir, "init", "--initial-branch=main")
	runGit(t, repoDir, "config", "user.name", "Gas City Test")
	runGit(t, repoDir, "config", "user.email", "gc-test@test.local")
	runGit(t, repoDir, "remote", "add", remoteName, bareDir)
	if err := os.WriteFile(filepath.Join(repoDir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	runGit(t, repoDir, "add", "base.txt")
	runGit(t, repoDir, "commit", "-m", "test: base")
	runGit(t, repoDir, "push", remoteName, "main")
	// A real clone carries refs/remotes/<name>/HEAD; tests must too, because
	// %(refname:short) renders it as the bare remote name — a shape the
	// containment filter has to reject (review finding: it sorts first and
	// would otherwise become the advisory's suggested correction).
	runGit(t, repoDir, "fetch", remoteName)
	runGit(t, repoDir, "remote", "set-head", remoteName, "main")
	return repoDir
}

// gateCommit writes path with content and commits it in repoDir, returning the
// commit SHA.
func gateCommit(t *testing.T, repoDir, path, content, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repoDir, path), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	runGit(t, repoDir, "add", path)
	runGit(t, repoDir, "commit", "-m", message)
	return strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))
}

// gateRefContains reports whether ref contains commit, asking git the same
// question the production oracles ask: `rev-list --max-count=1 <commit> --not
// <ref>` prints nothing when ref already contains commit.
//
// It routes through runGit so a git error — a mistyped ref, a bad repo — fails
// the test instead of reading as "not contained". These call sites are
// preconditions, and a precondition that a typo can satisfy vacuously is worse
// than no precondition at all.
func gateRefContains(t *testing.T, repoDir, ref, commit string) bool {
	t.Helper()
	return strings.TrimSpace(runGit(t, repoDir, "rev-list", "--max-count=1", commit, "--not", ref)) == ""
}

// gateStoreWith returns a fresh in-memory store holding one gated task bead
// whose work dir is repoDir.
func gateStoreWith(id, repoDir string) beads.Store {
	return beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     id,
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey: repoDir,
		},
	}}, nil)
}

// gateShippedCloseArgs is the documented atomic shipped-close invocation for
// bead id: stamp the typed work record and close in one update.
func gateShippedCloseArgs(id, commit, branch string) []string {
	return []string{
		"update", id,
		"--set-metadata", beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeShipped,
		"--set-metadata", beadmeta.WorkCommitMetadataKey + "=" + commit,
		"--set-metadata", beadmeta.WorkBranchMetadataKey + "=" + branch,
		"--status=closed",
	}
}

// TestEvaluateWorkRecordCloseGateNonOriginRemote covers the review finding that
// the remote-first resolution must not hardcode the remote name "origin": in a
// rig whose only remote is named upstream (git clone -o upstream), the #5037
// stale-local-ref fix must still work, and a stale-stamp advisory must suggest
// the bare branch name, never a remote-qualified one.
func TestEvaluateWorkRecordCloseGateNonOriginRemote(t *testing.T) {
	repoDir := newGateRepo(t, "upstream")

	// Land the work on upstream/main without moving local main (the refinery
	// topology), as in TestEvaluateWorkRecordCloseGateStaleLocalRef.
	runGit(t, repoDir, "checkout", "-b", "tmp-land")
	commit := gateCommit(t, repoDir, "landed.txt", "merged over the network\n", "feat: the work")
	runGit(t, repoDir, "push", "upstream", "tmp-land:main")
	runGit(t, repoDir, "fetch", "upstream")
	runGit(t, repoDir, "checkout", "main")
	runGit(t, repoDir, "branch", "-D", "tmp-land")

	if gateRefContains(t, repoDir, "refs/heads/main", commit) {
		t.Fatalf("precondition: commit must NOT be reachable on the stale local main")
	}
	if !gitCommitReachableOnBranch(repoDir, commit, "main") {
		t.Fatalf("remote-first resolution is inert for a remote not named origin")
	}

	var stderr strings.Builder
	if block := evaluateWorkRecordCloseGate(gateShippedCloseArgs("wr-upstream", commit, "main"), gateStoreWith("wr-upstream", repoDir), nil, repoDir, true, &stderr); block {
		t.Fatalf("delivered close blocked in a non-origin repo; stderr=%s", stderr.String())
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("clean delivered close produced gate output: %q", got)
	}

	// Stale stamp in the same repo: the advisory must suggest the unqualified
	// branch name — never "upstream/main", and never the bare remote name.
	var staleStderr strings.Builder
	if block := evaluateWorkRecordCloseGate(gateShippedCloseArgs("wr-upstream", commit, "gc-gastown.nux-1a2b3c4d5e6f"), gateStoreWith("wr-upstream", repoDir), nil, repoDir, true, &staleStderr); block {
		t.Fatalf("delivered stale-stamp close blocked; stderr=%s", staleStderr.String())
	}
	out := staleStderr.String()
	if !strings.Contains(out, beadmeta.WorkBranchMetadataKey+"=main") {
		t.Fatalf("advisory must suggest the unqualified branch: %q", out)
	}
	if strings.Contains(out, beadmeta.WorkBranchMetadataKey+"=upstream") {
		t.Fatalf("advisory suggested a remote-qualified or bare-remote value: %q", out)
	}
}

// TestEvaluateWorkRecordCloseGateUnpushedOnBranchSingleViolation covers the
// review finding that a commit sitting exactly on its stamped branch's local
// head — just not yet pushed — must produce only the actionable durability
// violation, not an additional false "not reachable" one: the stamped branch is
// factually correct, the only defect is the missing push.
func TestEvaluateWorkRecordCloseGateUnpushedOnBranchSingleViolation(t *testing.T) {
	repoDir := newGateRepo(t, "origin")

	const workBranch = "gc-gastown.rictus-5bca5afe897d"
	runGit(t, repoDir, "checkout", "-b", workBranch)
	gateCommit(t, repoDir, "feature.txt", "v1\n", "feat: v1")
	runGit(t, repoDir, "push", "origin", workBranch)
	runGit(t, repoDir, "fetch", "origin")
	// A second commit on the same branch, not yet pushed: origin/<workBranch>
	// exists but is one behind.
	commit := gateCommit(t, repoDir, "feature.txt", "v2\n", "feat: v2")

	var stderr strings.Builder
	if block := evaluateWorkRecordCloseGate(gateShippedCloseArgs("wr-onbranch", commit, workBranch), gateStoreWith("wr-onbranch", repoDir), nil, repoDir, true, &stderr); !block {
		t.Fatalf("close of unpushed work was allowed")
	}
	out := stderr.String()
	if !strings.Contains(out, "not present on any remote") {
		t.Fatalf("expected the durability violation, got %q", out)
	}
	if strings.Contains(out, "not reachable") {
		t.Fatalf("factually-wrong reachability violation for a commit on its stamped branch: %q", out)
	}
}

// TestEvaluateWorkRecordCloseGateRemotelessStaleStamp covers the review finding
// that Defect C persisted in the local-only topology the durability oracle
// deliberately supports: with no remotes at all, a stale claim stamp must
// downgrade to an advisory naming the local branch the work landed on, not
// block the close.
func TestEvaluateWorkRecordCloseGateRemotelessStaleStamp(t *testing.T) {
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "--initial-branch=main")
	runGit(t, repoDir, "config", "user.name", "Gas City Test")
	runGit(t, repoDir, "config", "user.email", "gc-test@test.local")
	if err := os.WriteFile(filepath.Join(repoDir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	runGit(t, repoDir, "add", "base.txt")
	runGit(t, repoDir, "commit", "-m", "test: base")

	runGit(t, repoDir, "checkout", "-b", "fix/local-work")
	commit := gateCommit(t, repoDir, "feature.txt", "local-only rig\n", "feat: local work")

	var stderr strings.Builder
	if block := evaluateWorkRecordCloseGate(gateShippedCloseArgs("wr-remoteless", commit, "gc-gastown.nux-1a2b3c4d5e6f"), gateStoreWith("wr-remoteless", repoDir), nil, repoDir, true, &stderr); block {
		t.Fatalf("stale-stamp close blocked in a remoteless repo; stderr=%s", stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, "advisory") || !strings.Contains(out, "fix/local-work") {
		t.Fatalf("expected a stale-stamp advisory naming the local branch, got %q", out)
	}
}

// TestEvaluateWorkRecordCloseGateMissingStampDelivered covers the review
// finding that a delivered shipped close with no gc.work_branch at all (a
// detached-HEAD or non-repo claim omits the stamp) was still hard-blocked:
// when the containment evidence can name the landing branch, the missing stamp
// downgrades to an advisory carrying the correction.
func TestEvaluateWorkRecordCloseGateMissingStampDelivered(t *testing.T) {
	repoDir := newGateRepo(t, "origin")

	const landedBranch = "fix/wr-detached-claim"
	runGit(t, repoDir, "checkout", "-b", landedBranch)
	commit := gateCommit(t, repoDir, "feature.txt", "delivered\n", "feat: the work")
	runGit(t, repoDir, "push", "origin", landedBranch)
	runGit(t, repoDir, "fetch", "origin")

	args := []string{
		"update", "wr-nostamp",
		"--set-metadata", beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeShipped,
		"--set-metadata", beadmeta.WorkCommitMetadataKey + "=" + commit,
		"--status=closed",
	}
	var stderr strings.Builder
	if block := evaluateWorkRecordCloseGate(args, gateStoreWith("wr-nostamp", repoDir), nil, repoDir, true, &stderr); block {
		t.Fatalf("delivered close with a missing stamp blocked; stderr=%s", stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, "advisory") || !strings.Contains(out, landedBranch) {
		t.Fatalf("expected a missing-stamp advisory naming the landed branch, got %q", out)
	}

	// The paired negative: missing stamp AND undelivered commit must still
	// violate — the downgrade rides on delivery evidence, not on leniency.
	unpushed := gateCommit(t, repoDir, "feature.txt", "delivered v2\n", "feat: never pushed")
	undeliveredArgs := []string{
		"update", "wr-nostamp",
		"--set-metadata", beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeShipped,
		"--set-metadata", beadmeta.WorkCommitMetadataKey + "=" + unpushed,
		"--status=closed",
	}
	var undeliveredStderr strings.Builder
	if block := evaluateWorkRecordCloseGate(undeliveredArgs, gateStoreWith("wr-nostamp", repoDir), nil, repoDir, true, &undeliveredStderr); !block {
		t.Fatalf("undelivered close with a missing stamp was allowed")
	}
}

// TestEvaluateWorkRecordCloseGateAdvisoryPrefersDefaultBranch covers the review
// finding that the suggested correction was containing[0] — the alphabetically
// first remote branch, an arbitrary choice for an older commit contained in
// many branches. The remote's default branch must win when it contains the
// commit, and the bare remote name (origin/HEAD's short form) must never be
// suggested.
func TestEvaluateWorkRecordCloseGateAdvisoryPrefersDefaultBranch(t *testing.T) {
	repoDir := newGateRepo(t, "origin")

	// Land the work on main, then cut branches that also contain it and sort
	// ahead of "main" alphabetically.
	runGit(t, repoDir, "checkout", "main")
	commit := gateCommit(t, repoDir, "feature.txt", "landed\n", "feat: the work")
	runGit(t, repoDir, "push", "origin", "main")
	for _, b := range []string{"aa-derived", "ab-derived"} {
		runGit(t, repoDir, "branch", b)
		runGit(t, repoDir, "push", "origin", b)
	}
	runGit(t, repoDir, "fetch", "origin")

	var stderr strings.Builder
	if block := evaluateWorkRecordCloseGate(gateShippedCloseArgs("wr-prefer", commit, "gc-gastown.nux-1a2b3c4d5e6f"), gateStoreWith("wr-prefer", repoDir), nil, repoDir, true, &stderr); block {
		t.Fatalf("delivered stale-stamp close blocked; stderr=%s", stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, beadmeta.WorkBranchMetadataKey+"=main") {
		t.Fatalf("advisory must suggest the remote default branch, got %q", out)
	}
	if strings.Contains(out, beadmeta.WorkBranchMetadataKey+"=aa-derived") {
		t.Fatalf("advisory suggested the alphabetically-first branch instead of the default: %q", out)
	}
}

func TestEvaluateWorkRecordCloseGateAtomicShippedUpdate(t *testing.T) {
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "--initial-branch=main")
	runGit(t, repoDir, "config", "user.name", "Gas City Test")
	runGit(t, repoDir, "config", "user.email", "gc-test@test.local")
	artifactPath := filepath.Join(repoDir, "artifact.txt")
	if err := os.WriteFile(artifactPath, []byte("integrated\n"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	runGit(t, repoDir, "add", "artifact.txt")
	runGit(t, repoDir, "commit", "-m", "test: integrate artifact")
	commit := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))

	store := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     "wr-atomic-shipped",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey: repoDir,
		},
	}}, nil)
	args := []string{
		"update", "wr-atomic-shipped",
		"--set-metadata", beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeShipped,
		"--set-metadata", beadmeta.WorkCommitMetadataKey + "=" + commit,
		"--set-metadata", beadmeta.WorkBranchMetadataKey + "=main",
		"--status=closed",
	}
	var stderr strings.Builder
	if block := evaluateWorkRecordCloseGate(args, store, nil, repoDir, true, &stderr); block {
		t.Fatalf("valid atomic shipped close blocked; stderr=%s", stderr.String())
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("valid atomic shipped close warned: %q", got)
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

// TestEvaluateWorkRecordCloseGateUnpushedBranch is the az-6n75 regression test.
// It reproduces the kit-ccf incident end to end against a real repo: a polecat
// commits on its own branch, records a correct and internally-consistent work
// record, and closes — but never pushes. Every pre-existing rule passes. The
// commit IS reachable on the branch it names; the branch simply exists nowhere
// but this worktree, so a prune destroys the only copy.
//
// The paired assertion matters as much as the block: after the identical commit
// is pushed, the same close must be allowed. A rule that blocks unpushed work by
// blocking everything would pass a one-sided test.
func TestEvaluateWorkRecordCloseGateUnpushedBranch(t *testing.T) {
	repoDir := newGateRepo(t, "origin")

	// The polecat's own branch, mirroring gc-gastown.<name>-<hash>.
	const workBranch = "gc-gastown.rictus-5bca5afe897d"
	runGit(t, repoDir, "checkout", "-b", workBranch)
	commit := gateCommit(t, repoDir, "feature.txt", "quote stripper\n", "feat: quote stripper stage 1")

	newStore := func() beads.Store { return gateStoreWith("wr-unpushed", repoDir) }
	args := gateShippedCloseArgs("wr-unpushed", commit, workBranch)

	// Sanity: the pre-existing reachability rule cannot see this failure. If this
	// ever stops holding, the durability rule is no longer the thing under test.
	if !gitCommitReachableOnBranch(repoDir, commit, workBranch) {
		t.Fatalf("precondition: commit should be reachable on its own branch")
	}

	var stderr strings.Builder
	if block := evaluateWorkRecordCloseGate(args, newStore(), nil, repoDir, true, &stderr); !block {
		t.Fatalf("close of unpushed work was allowed; the branch exists on no remote")
	}
	if got := stderr.String(); !strings.Contains(got, "not present on any remote") {
		t.Fatalf("expected a durability violation, got %q", got)
	}

	// Same commit, same close, after publishing: must now be allowed.
	runGit(t, repoDir, "push", "origin", workBranch)
	var pushedStderr strings.Builder
	if block := evaluateWorkRecordCloseGate(args, newStore(), nil, repoDir, true, &pushedStderr); block {
		t.Fatalf("close blocked after push; stderr=%s", pushedStderr.String())
	}
	if got := pushedStderr.String(); got != "" {
		t.Fatalf("pushed close warned: %q", got)
	}
}

// TestEvaluateWorkRecordCloseGateStaleClaimStamp is the ADR-0009 Defect C
// regression test. gc.work_branch is stamped at claim time — before the work
// exists — with the branch the claiming worktree happened to be on (the
// polecat's persistent gc-<agent>-<hash> branch, cut from the default branch).
// When the work then lands on a different branch, the stamp is stale, and the
// pre-fix gate reported the delivered commit "not reachable" — under
// GC_WORK_RECORD_ENFORCE that blocks every such honest close citywide, which is
// exactly why az-fuag/az-z4p1 blocked enforcement on this defect.
//
// Contract under test: a shipped commit that IS on a remote-tracking ref must
// close (delivery is the guarantee), with a precise advisory naming the branch
// the work actually landed on so the record can be corrected — while the same
// close with an unpushed commit must still block (the az-6n75 protection).
func TestEvaluateWorkRecordCloseGateStaleClaimStamp(t *testing.T) {
	repoDir := newGateRepo(t, "origin")

	// The claim-time stamp: the polecat worktree branch, cut at base and never
	// advanced. This is what gc.work_branch carries when the close arrives.
	const claimBranch = "gc-gastown.nux-1a2b3c4d5e6f"
	runGit(t, repoDir, "branch", claimBranch)

	// The work lands on a different branch and is pushed — delivered.
	const landedBranch = "fix/wr-defect-c"
	runGit(t, repoDir, "checkout", "-b", landedBranch)
	commit := gateCommit(t, repoDir, "feature.txt", "stale stamp fix\n", "feat: the work that satisfied the bead")
	runGit(t, repoDir, "push", "origin", landedBranch)
	runGit(t, repoDir, "fetch", "origin")

	newStore := func() beads.Store { return gateStoreWith("wr-stale-stamp", repoDir) }
	args := gateShippedCloseArgs("wr-stale-stamp", commit, claimBranch)

	// Precondition: the stale stamp genuinely does not contain the commit, so
	// this test exercises the stale-stamp rule and not reachability.
	if gitCommitReachableOnBranch(repoDir, commit, claimBranch) {
		t.Fatalf("precondition: commit must not be reachable on the claim-time branch")
	}

	var stderr strings.Builder
	if block := evaluateWorkRecordCloseGate(args, newStore(), nil, repoDir, true, &stderr); block {
		t.Fatalf("delivered close blocked on a stale claim-time stamp; stderr=%s", stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, "advisory") {
		t.Fatalf("expected a stale-stamp advisory, got %q", out)
	}
	if !strings.Contains(out, landedBranch) {
		t.Fatalf("advisory %q does not name the landed branch %q", out, landedBranch)
	}
	if strings.Contains(out, "not reachable") {
		t.Fatalf("delivered close still reported unreachable: %q", out)
	}

	// Paired negative: the same stale-stamp close with an UNPUSHED commit is the
	// az-6n75 data-loss case and must still block under enforcement.
	unpushed := gateCommit(t, repoDir, "unpushed.txt", "only copy\n", "feat: never pushed")
	unpushedArgs := gateShippedCloseArgs("wr-stale-stamp", unpushed, claimBranch)
	var unpushedStderr strings.Builder
	if block := evaluateWorkRecordCloseGate(unpushedArgs, newStore(), nil, repoDir, true, &unpushedStderr); !block {
		t.Fatalf("close of unpushed work was allowed through the stale-stamp path")
	}
	if got := unpushedStderr.String(); !strings.Contains(got, "not present on any remote") {
		t.Fatalf("expected a durability violation, got %q", got)
	}
}

// TestEvaluateWorkRecordCloseGateStaleLocalRef is the gastownhall/gascity#5037
// finding-1 regression test. In any topology where merges reach the target
// branch over the network (the refinery pushes from a detached worktree),
// nothing ever advances the local ref: refs/heads/main goes permanently stale
// while refs/remotes/origin/main is the truth. The pre-fix gate resolved the
// bare branch name by gitrevisions precedence — the stale local ref — and
// reported genuinely-merged commits unreachable.
//
// Per the issue's own verification guidance: do not verify by the warning
// stopping — assert the check resolves against refs/remotes/origin/<branch> and
// passes for a commit that is on origin/main but NOT on a deliberately-stale
// local main.
func TestEvaluateWorkRecordCloseGateStaleLocalRef(t *testing.T) {
	repoDir := newGateRepo(t, "origin")

	// Land the work on origin/main without moving local main, the way the
	// refinery does: commit on a temporary branch, push it to origin's main,
	// return to the stale local main, and drop the temporary branch so the
	// commit exists locally only via the remote-tracking ref.
	runGit(t, repoDir, "checkout", "-b", "tmp-land")
	commit := gateCommit(t, repoDir, "landed.txt", "merged over the network\n", "feat: the work that satisfied the bead")
	runGit(t, repoDir, "push", "origin", "tmp-land:main")
	runGit(t, repoDir, "fetch", "origin")
	runGit(t, repoDir, "checkout", "main")
	runGit(t, repoDir, "branch", "-D", "tmp-land")

	// Preconditions from the issue's repro: the local ref is genuinely stale
	// and the remote-tracking ref genuinely contains the commit.
	if gateRefContains(t, repoDir, "refs/heads/main", commit) {
		t.Fatalf("precondition: commit must NOT be reachable on the stale local main")
	}
	if !gateRefContains(t, repoDir, "refs/remotes/origin/main", commit) {
		t.Fatalf("precondition: commit must be reachable on origin/main")
	}

	// The fixed resolver must answer via the remote-tracking ref.
	if !gitCommitReachableOnBranch(repoDir, commit, "main") {
		t.Fatalf("gitCommitReachableOnBranch resolved the stale local ref; want refs/remotes/origin/main")
	}

	store := gateStoreWith("wr-stale-local", repoDir)
	args := gateShippedCloseArgs("wr-stale-local", commit, "main")
	var stderr strings.Builder
	if block := evaluateWorkRecordCloseGate(args, store, nil, repoDir, true, &stderr); block {
		t.Fatalf("close of work merged to origin/main blocked on a stale local ref; stderr=%s", stderr.String())
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("clean delivered close produced gate output: %q", got)
	}
}

// TestEvaluateWorkRecordCloseGateSelfRemote is the gas-6tc regression test.
// The gascity repo carries a remote whose URL is the repo's own path
// (herdr-src), with fetched remote-tracking refs snapshotting local branches.
// A blanket `rev-list <sha> --not --remotes` then reads any previously-fetched
// local commit as "published" — the az-6n75 hole reopened by configuration, on
// exactly the rig where the gate guards the gc tool itself. The witness-side
// twin was fixed as gas-6wq ("exclude self-referential path remotes from the
// publication oracle"); this covers the Go side: neither the durability oracle
// nor the stale-stamp containment resolver may count a self-remote.
func TestEvaluateWorkRecordCloseGateSelfRemote(t *testing.T) {
	repoDir := newGateRepo(t, "origin")

	// The self-referential path remote, as herdr-src is configured in the wild.
	runGit(t, repoDir, "remote", "add", "self-src", repoDir)

	// Unpushed work on its own branch; then fetch the self-remote so
	// refs/remotes/self-src/* snapshot the local branches including the commit.
	const workBranch = "gc-gastown.slit-9d8c7b6a5f4e"
	runGit(t, repoDir, "checkout", "-b", workBranch)
	commit := gateCommit(t, repoDir, "feature.txt", "only local\n", "feat: never pushed off-machine")
	runGit(t, repoDir, "fetch", "self-src")

	// Precondition: the self-remote genuinely snapshots the unpushed commit —
	// the blanket --remotes query is genuinely defeated without the exclusion.
	if !gateRefContains(t, repoDir, "refs/remotes/self-src/"+workBranch, commit) {
		t.Fatalf("precondition: self-remote ref must contain the unpushed commit")
	}

	newStore := func() beads.Store { return gateStoreWith("wr-self-remote", repoDir) }
	closeArgs := func(branch string) []string {
		return gateShippedCloseArgs("wr-self-remote", commit, branch)
	}

	// With a correct branch stamp: the commit is reachable locally, its only
	// "remote" presence is the self-remote snapshot — undelivered. Must block.
	var stderr strings.Builder
	if block := evaluateWorkRecordCloseGate(closeArgs(workBranch), newStore(), nil, repoDir, true, &stderr); !block {
		t.Fatalf("close of self-remote-only work was allowed; --remotes counted the repo itself")
	}
	if got := stderr.String(); !strings.Contains(got, "not present on any remote") {
		t.Fatalf("expected a durability violation, got %q", got)
	}

	// With a stale branch stamp: the containment resolver must not surface the
	// self-remote snapshot as a landing branch — no advisory, still blocked.
	var staleStderr strings.Builder
	if block := evaluateWorkRecordCloseGate(closeArgs("gc-gastown.nux-1a2b3c4d5e6f"), newStore(), nil, repoDir, true, &staleStderr); !block {
		t.Fatalf("stale-stamp close of self-remote-only work was allowed")
	}
	if got := staleStderr.String(); strings.Contains(got, "self-src/") {
		t.Fatalf("advisory named a self-remote branch: %q", got)
	}

	// Paired positive: after a real push, the same close must be allowed — the
	// exclusion must not block genuinely delivered work.
	runGit(t, repoDir, "push", "origin", workBranch)
	var pushedStderr strings.Builder
	if block := evaluateWorkRecordCloseGate(closeArgs(workBranch), newStore(), nil, repoDir, true, &pushedStderr); block {
		t.Fatalf("close blocked after real push; stderr=%s", pushedStderr.String())
	}
	if got := pushedStderr.String(); got != "" {
		t.Fatalf("pushed close warned: %q", got)
	}
}

// TestEvaluateWorkRecordCloseGateRelativeSelfRemote covers a self-referential
// remote written as a *relative* path. Git resolves such a URL against the
// repository, so it is an ordinary configuration; resolving it against the gc
// process's own working directory instead classifies it as a real publication
// remote and its snapshot refs become delivery evidence for work that never
// left the host. TestEvaluateWorkRecordCloseGateSelfRemote cannot cover this:
// it adds its remote as an absolute t.TempDir() path.
func TestEvaluateWorkRecordCloseGateRelativeSelfRemote(t *testing.T) {
	repoDir := newGateRepo(t, "origin")

	// Relative spelling of the same self-reference: from repoDir, "../<base>"
	// is repoDir. The test process's cwd is the cmd/gc package directory, so a
	// resolution anchored there lands somewhere else entirely.
	relURL := "../" + filepath.Base(repoDir)
	runGit(t, repoDir, "remote", "add", "self-rel", relURL)

	// Precondition: the URL must stay relative. If git or a future helper ever
	// records it absolute, this test silently becomes a duplicate of the
	// absolute-path case and stops covering the branch it exists for.
	if got := strings.TrimSpace(runGit(t, repoDir, "remote", "get-url", "self-rel")); got != relURL {
		t.Fatalf("precondition: remote URL must stay relative, got %q want %q", got, relURL)
	}

	const workBranch = "gc-gastown.slit-4f3e2d1c0b9a"
	runGit(t, repoDir, "checkout", "-b", workBranch)
	commit := gateCommit(t, repoDir, "feature.txt", "only local\n", "feat: never pushed off-machine")
	runGit(t, repoDir, "fetch", "self-rel")

	// Precondition: git itself resolved the relative URL against the repository,
	// so the snapshot genuinely contains the unpushed commit.
	if !gateRefContains(t, repoDir, "refs/remotes/self-rel/"+workBranch, commit) {
		t.Fatalf("precondition: relative self-remote ref must contain the unpushed commit")
	}

	newStore := func() beads.Store { return gateStoreWith("wr-rel-self-remote", repoDir) }
	closeArgs := gateShippedCloseArgs("wr-rel-self-remote", commit, workBranch)

	var stderr strings.Builder
	if block := evaluateWorkRecordCloseGate(closeArgs, newStore(), nil, repoDir, true, &stderr); !block {
		t.Fatalf("close of relative-self-remote-only work was allowed; the snapshot counted as delivery")
	}
	if got := stderr.String(); !strings.Contains(got, "not present on any remote") {
		t.Fatalf("expected a durability violation, got %q", got)
	}

	// Paired positive: a real push must still be allowed, so the fix cannot be
	// satisfied by a filter that discards every remote.
	runGit(t, repoDir, "push", "origin", workBranch)
	var pushedStderr strings.Builder
	if block := evaluateWorkRecordCloseGate(closeArgs, newStore(), nil, repoDir, true, &pushedStderr); block {
		t.Fatalf("close blocked after real push; stderr=%s", pushedStderr.String())
	}
	if got := pushedStderr.String(); got != "" {
		t.Fatalf("pushed close warned: %q", got)
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
