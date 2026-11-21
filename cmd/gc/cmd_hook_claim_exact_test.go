package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
)

// exactClaimFixture is a work store holding graph-step beads, the observed
// rows the work query serves for them, and hook seams that claim through the
// store's revision fence.
type exactClaimFixture struct {
	t        *testing.T
	store    *beads.MemStore
	observed []beads.Bead
	rejected []string
	failed   []string
	claims   []string
	// revision, when set, is the revision fence claims write through in
	// place of the store itself.
	revision beads.ConditionalWriter
}

func newExactClaimFixture(t *testing.T) *exactClaimFixture {
	t.Helper()
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	if _, err := store.Create(beads.Bead{ID: "root-1", Title: "root", Status: "open"}); err != nil {
		t.Fatalf("create root: %v", err)
	}
	return &exactClaimFixture{t: t, store: store}
}

// graphStepMeta is the metadata of an unbound pool graph step under root-1.
func graphStepMeta(step string) map[string]string {
	return map[string]string{
		beadmeta.RoutedToMetadataKey:   "worker",
		beadmeta.RootBeadIDMetadataKey: "root-1",
		beadmeta.StepIDMetadataKey:     step,
	}
}

// add stores bead as the canonical row and observed as the work query's
// projection of it.
func (f *exactClaimFixture) add(stored, observed beads.Bead) {
	f.t.Helper()
	if _, err := f.store.Create(stored); err != nil {
		f.t.Fatalf("create %s: %v", stored.ID, err)
	}
	f.observed = append(f.observed, observed)
}

func (f *exactClaimFixture) writers() molecule.ClaimExactWriters {
	if f.revision != nil {
		return molecule.ClaimExactWriters{Revision: f.revision}
	}
	return molecule.ClaimExactWriters{Revision: f.store}
}

// recordingRevisionWriter is a revision fence over a MemStore that records
// every conditional update it is asked to make.
type recordingRevisionWriter struct {
	beads.ConditionalWriter
	updates []beads.UpdateOpts
}

func (w *recordingRevisionWriter) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	w.updates = append(w.updates, opts)
	return w.ConditionalWriter.UpdateIfMatch(id, expectedRevision, opts)
}

func (f *exactClaimFixture) ops() hookClaimOps {
	f.t.Helper()
	rows, err := json.Marshal(f.observed)
	if err != nil {
		f.t.Fatalf("marshal candidates: %v", err)
	}
	return hookClaimOps{
		Runner: func(string, string) (string, error) { return string(rows), nil },
		Claim: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, bool, error) {
			f.claims = append(f.claims, id)
			return beads.Bead{}, false, nil
		},
		ClaimExact: func(_ context.Context, _ string, _ []string, actor string, req molecule.ClaimExactRequest) (beads.Bead, hookClaimVerdict, error) {
			return hookClaimExactThroughStore(f.store, f.writers(), actor, req)
		},
		EmitClaimRejected: func(id, _, _ string) { f.rejected = append(f.rejected, id) },
		EmitClaimFailed:   func(id string, _ hookClaimOptions, _ error) { f.failed = append(f.failed, id) },
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" },
		StampWorkMeta:     noopStampWorkMeta,
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
			return f.store.Get(id)
		},
		PublishRunMap:     noopPublishRunMap,
		StampSessionClaim: noopStampSessionClaim,
		WorkflowStore:     func(context.Context, string, []string, string, string) beads.Store { return f.store },
		DrainPending:      func(string) (bool, error) { return false, nil },
		InputVerdict: func(context.Context, string, []string, beads.Bead, []string, workflowInputAbsenceProver) (workflowInputVerdict, error) {
			return workflowInputVerdict{State: workflowInputPending}, nil
		},
	}
}

func (f *exactClaimFixture) get(id string) beads.Bead {
	f.t.Helper()
	b, err := f.store.Get(id)
	if err != nil {
		f.t.Fatalf("get %s: %v", id, err)
	}
	return b
}

// assertUntouched fails unless id is still open, unassigned, and carries
// neither a claim generation nor a session back-reference.
func (f *exactClaimFixture) assertUntouched(id string) {
	f.t.Helper()
	b := f.get(id)
	if b.Status != "open" || b.Assignee != "" {
		f.t.Fatalf("%s = status %q assignee %q, want open and unassigned", id, b.Status, b.Assignee)
	}
	for _, key := range []string{beadmeta.ClaimGenerationMetadataKey, beadmeta.SessionIDMetadataKey, beadmeta.SessionNameMetadataKey} {
		if v, ok := b.Metadata[key]; ok {
			f.t.Fatalf("%s carries %s=%q, want it unwritten", id, key, v)
		}
	}
}

// TestHookClaimExactFencesObservedStepID: the eligible tier observed work-1 as
// step build, but its step moved to test before the claim. The claim writes
// nothing to it, reports a rejection rather than claims_errored, and claims the
// next candidate instead.
func TestHookClaimExactFencesObservedStepID(t *testing.T) {
	f := newExactClaimFixture(t)
	f.add(
		beads.Bead{ID: "work-1", Title: "moved", Status: "open", Metadata: graphStepMeta("test")},
		beads.Bead{ID: "work-1", Status: "open", Metadata: graphStepMeta("build")},
	)
	f.add(
		beads.Bead{ID: "work-2", Title: "next", Status: "open", Metadata: graphStepMeta("build")},
		beads.Bead{ID: "work-2", Status: "open", Metadata: graphStepMeta("build")},
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), f.ops(), &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	f.assertUntouched("work-1")
	if !strings.Contains(stderr.String(), "exact claim of work-1 rejected") {
		t.Fatalf("stderr = %q, want the rejection of work-1 named", stderr.String())
	}
	if len(f.failed) != 0 || strings.Contains(stdout.String(), "claims_errored") {
		t.Fatalf("claim failures = %v stdout = %s, want no claims_errored", f.failed, stdout.String())
	}
	if len(f.claims) != 0 {
		t.Fatalf("--claim ran for graph steps %v, want none", f.claims)
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode result %q: %v", stdout.String(), err)
	}
	if result.Action != "work" || result.BeadID != "work-2" {
		t.Fatalf("result = %+v, want work on work-2", result)
	}
	if got := f.get("work-2"); got.Status != "in_progress" || got.Assignee != "gc__role-mc-sess1" {
		t.Fatalf("work-2 = status %q assignee %q, want in_progress owned by this session", got.Status, got.Assignee)
	}
}

// TestHookClaimExactFencesObservedAssignee: a graph step observed open and
// assigned to this session (ready tier), or open and unassigned (eligible
// tier), went to another worker before the claim ran. Nothing is written and
// the other worker keeps the bead. A stale lease this invocation reclaimed is
// claimed from the open, unassigned row the reclaim left.
func TestHookClaimExactFencesObservedAssignee(t *testing.T) {
	const self, other = "gc__role-mc-sess1", "other-worker"
	for _, tc := range []struct {
		name             string
		observedAssignee string
	}{
		{name: "ready tier", observedAssignee: self},
		{name: "eligible tier", observedAssignee: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExactClaimFixture(t)
			f.add(
				beads.Bead{ID: "work-1", Title: "taken", Status: "open", Assignee: other, Metadata: graphStepMeta("build")},
				beads.Bead{ID: "work-1", Status: "open", Assignee: tc.observedAssignee, Metadata: graphStepMeta("build")},
			)

			var stdout, stderr bytes.Buffer
			doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), f.ops(), &stdout, &stderr)

			if len(f.claims) != 0 {
				t.Fatalf("--claim ran for graph steps %v, want none", f.claims)
			}
			if strings.Contains(stdout.String(), `"action":"work"`) {
				t.Fatalf("handed out work: %s", stdout.String())
			}
			got := f.get("work-1")
			if got.Status != "open" || got.Assignee != other {
				t.Fatalf("work-1 = status %q assignee %q, want open and still %s's", got.Status, got.Assignee, other)
			}
			if v, ok := got.Metadata[beadmeta.ClaimGenerationMetadataKey]; ok {
				t.Fatalf("work-1 carries %s=%q, want it unwritten", beadmeta.ClaimGenerationMetadataKey, v)
			}
		})
	}

	t.Run("reclaimed lease", func(t *testing.T) {
		f := newExactClaimFixture(t)
		f.add(
			beads.Bead{ID: "work-1", Title: "stale", Status: "in_progress", Assignee: "stale-worker", Metadata: graphStepMeta("build")},
			beads.Bead{ID: "work-1", Status: "in_progress", Assignee: "stale-worker", Metadata: graphStepMeta("build")},
		)
		ops := f.ops()
		ops.ReclaimStale = func(_ context.Context, _ string, _ []string, id string) (bool, string, error) {
			open, none := "open", ""
			return true, "stale-worker", f.store.Update(id, beads.UpdateOpts{Status: &open, Assignee: &none})
		}
		ops.EmitHookClaimReclaimedStale = func(string, string, string) {}
		opts := poolClaimOpts()
		opts.AutoReclaimStaleClaims = true

		var stdout, stderr bytes.Buffer
		if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
			t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
		}
		if got := f.get("work-1"); got.Status != "in_progress" || got.Assignee != self {
			t.Fatalf("work-1 = status %q assignee %q, want in_progress owned by this session; stderr=%s", got.Status, got.Assignee, stderr.String())
		}
	})
}

// TestHookClaimExactWritesClaimAndIdentityInOneFencedWrite: a session-bound
// claim of a matching graph step lands as exactly one conditional write that
// carries the transition, the assignee, the session back-reference, the
// write-once gc.claimed_at (only when absent) and the advanced generation, and
// no later write rewrites any of those keys.
func TestHookClaimExactWritesClaimAndIdentityInOneFencedWrite(t *testing.T) {
	identityKeys := []string{
		beadmeta.SessionIDMetadataKey, beadmeta.SessionNameMetadataKey,
		beadmeta.ClaimedAtMetadataKey, beadmeta.ClaimGenerationMetadataKey,
	}
	for _, tc := range []struct {
		name           string
		extra          map[string]string
		wantGeneration string
		wantClaimedAt  bool
	}{
		{name: "first claim", wantGeneration: "1", wantClaimedAt: true},
		{
			name: "claimed before",
			extra: map[string]string{
				beadmeta.ClaimGenerationMetadataKey: "4",
				beadmeta.ClaimedAtMetadataKey:       "2026-01-01T00:00:00Z",
			},
			wantGeneration: "5",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := graphStepMeta("build")
			for k, v := range tc.extra {
				meta[k] = v
			}
			f := newExactClaimFixture(t)
			f.add(
				beads.Bead{ID: "work-1", Title: "step", Status: "open", Metadata: meta},
				beads.Bead{ID: "work-1", Status: "open", Metadata: meta},
			)
			writer := &recordingRevisionWriter{ConditionalWriter: f.store}
			f.revision = writer
			ops := f.ops()
			var stamps []map[string]string
			ops.StampWorkMeta = func(_ context.Context, _ string, _ []string, _, _ string, patch map[string]string) error {
				stamps = append(stamps, patch)
				return nil
			}

			var stdout, stderr bytes.Buffer
			if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
				t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
			}
			if len(writer.updates) != 1 {
				t.Fatalf("conditional writes = %d, want exactly 1: %+v", len(writer.updates), writer.updates)
			}
			w := writer.updates[0]
			if w.Status == nil || *w.Status != "in_progress" || w.Assignee == nil || *w.Assignee != "gc__role-mc-sess1" {
				t.Fatalf("write = %+v, want status in_progress and assignee gc__role-mc-sess1", w)
			}
			if w.Metadata[beadmeta.SessionIDMetadataKey] != "mc-sess1" || w.Metadata[beadmeta.SessionNameMetadataKey] != "gc__role-mc-sess1" {
				t.Fatalf("write metadata = %v, want the session back-reference", w.Metadata)
			}
			if got := w.Metadata[beadmeta.ClaimGenerationMetadataKey]; got != tc.wantGeneration {
				t.Fatalf("write %s = %q, want %q", beadmeta.ClaimGenerationMetadataKey, got, tc.wantGeneration)
			}
			if _, ok := w.Metadata[beadmeta.ClaimedAtMetadataKey]; ok != tc.wantClaimedAt {
				t.Fatalf("write carries %s = %v, want %v (metadata %v)", beadmeta.ClaimedAtMetadataKey, ok, tc.wantClaimedAt, w.Metadata)
			}
			for _, patch := range stamps {
				for _, key := range identityKeys {
					if v, ok := patch[key]; ok {
						t.Fatalf("later stamp rewrote %s=%q (patch %v)", key, v, patch)
					}
				}
			}
		})
	}
}

// TestHookClaimExactFencesObservedGeneration: the work query observed
// gc.claim_generation=3, but the bead's generation advanced to 4 before the
// claim ran. The hook writes nothing and reports a rejection.
func TestHookClaimExactFencesObservedGeneration(t *testing.T) {
	observed := graphStepMeta("build")
	observed[beadmeta.ClaimGenerationMetadataKey] = "3"
	stored := graphStepMeta("build")
	stored[beadmeta.ClaimGenerationMetadataKey] = "4"
	f := newExactClaimFixture(t)
	f.add(
		beads.Bead{ID: "work-1", Title: "advanced", Status: "open", Metadata: stored},
		beads.Bead{ID: "work-1", Status: "open", Metadata: observed},
	)

	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), f.ops(), &stdout, &stderr)

	got := f.get("work-1")
	if got.Status != "open" || got.Assignee != "" || got.Metadata[beadmeta.ClaimGenerationMetadataKey] != "4" {
		t.Fatalf("work-1 = status %q assignee %q generation %q, want open, unassigned, generation 4",
			got.Status, got.Assignee, got.Metadata[beadmeta.ClaimGenerationMetadataKey])
	}
	if strings.Contains(stdout.String(), `"action":"work"`) {
		t.Fatalf("handed out work: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "exact claim of work-1 rejected") {
		t.Fatalf("stderr = %q, want the rejection of work-1 named", stderr.String())
	}
	if len(f.failed) != 0 {
		t.Fatalf("claim failures = %v, want a rejection, not claims_errored", f.failed)
	}
}

// fakeAssignmentGuardBd is a bd that knows --if-status/--if-assignee but not
// --if-revision. It serves one bead, applies a guarded update only while the
// bead's status and assignee match the guard, and records every command.
type fakeAssignmentGuardBd struct {
	bead beads.Bead
	ran  [][]string
}

func (f *fakeAssignmentGuardBd) runner(context.Context, map[string]string) beads.CommandRunner {
	return func(_ string, name string, args ...string) ([]byte, error) {
		f.ran = append(f.ran, append([]string{name}, args...))
		if name != "bd" || len(args) == 0 {
			return nil, fmt.Errorf("unexpected command %s %q", name, args)
		}
		switch args[0] {
		case "show":
			return json.Marshal([]beads.Bead{f.bead})
		case "update", "close", "assign", "delete":
			if len(args) == 2 && args[1] == "--help" {
				return []byte("Flags:\n      --if-status string\n      --if-assignee string\n"), nil
			}
			if args[0] == "update" {
				return f.update(args[1:])
			}
		}
		return nil, fmt.Errorf("unexpected command %s %q", name, args)
	}
}

func (f *fakeAssignmentGuardBd) update(args []string) ([]byte, error) {
	next := f.bead
	next.Metadata = map[string]string{}
	for k, v := range f.bead.Metadata {
		next.Metadata[k] = v
	}
	var ifStatus, ifAssignee *string
	for i := 0; i < len(args); i++ {
		flag := args[i]
		if !strings.HasPrefix(flag, "--") || flag == "--json" {
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("flag %s has no value", flag)
		}
		value := args[i+1]
		i++
		switch flag {
		case "--status":
			next.Status = value
		case "--assignee":
			next.Assignee = value
		case "--set-metadata":
			k, v, _ := strings.Cut(value, "=")
			next.Metadata[k] = v
		case "--if-status":
			ifStatus = &value
		case "--if-assignee":
			ifAssignee = &value
		default:
			return nil, fmt.Errorf("unknown flag: %s", flag)
		}
	}
	if ifStatus == nil || ifAssignee == nil {
		return nil, fmt.Errorf("unguarded update %q", args)
	}
	if f.bead.Status != *ifStatus || f.bead.Assignee != *ifAssignee {
		return nil, fakeBdExitError(13)
	}
	f.bead = next
	return json.Marshal([]beads.Bead{f.bead})
}

// fakeBdExitError is a bd process that exited with the given status.
type fakeBdExitError int

func (e fakeBdExitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// ExitCode reports the exit status, as an *exec.ExitError does.
func (e fakeBdExitError) ExitCode() int { return int(e) }

// updates returns the bd update commands that were not capability probes.
func (f *fakeAssignmentGuardBd) updates() [][]string {
	var out [][]string
	for _, cmd := range f.ran {
		if len(cmd) > 1 && cmd[1] == "update" && (len(cmd) != 3 || cmd[2] != "--help") {
			out = append(out, cmd)
		}
	}
	return out
}

// TestHookClaimExactClaimsThroughAssignmentGuardWithoutRevisionFence: on a bd
// that has no --if-revision but does guard an update on status and assignee,
// the exact claim of a graph step lands as one guarded bd update carrying the
// claim's effects, fenced on the observed status and assignee, never as a
// --claim.
func TestHookClaimExactClaimsThroughAssignmentGuardWithoutRevisionFence(t *testing.T) {
	bd := &fakeAssignmentGuardBd{bead: beads.Bead{
		ID: "work-1", Title: "work", Status: "open", Type: "task",
		Metadata: graphStepMeta("build"),
	}}
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })
	hookClaimCommandRunnerWithEnvContext = bd.runner

	opts := poolClaimOpts()
	req := hookClaimExactPlan(bd.bead, opts.Assignee, true, false, opts.Env, time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC))
	claimed, verdict, err := hookClaimExactWithBdStore(context.Background(), t.TempDir(), opts.Env, opts.Assignee, req)
	if err != nil {
		t.Fatalf("exact claim: %v; commands=%q", err, bd.ran)
	}
	if verdict != hookClaimVerdictClaimed {
		t.Fatalf("verdict = %q, want claimed; commands=%q", verdict, bd.ran)
	}
	if claimed.Assignee != opts.Assignee || claimed.Status != "in_progress" {
		t.Fatalf("claimed = %s/%s, want in_progress/%s", claimed.Status, claimed.Assignee, opts.Assignee)
	}
	updates := bd.updates()
	if len(updates) != 1 {
		t.Fatalf("bd updates = %q, want exactly one guarded update", updates)
	}
	update := strings.Join(updates[0], " ")
	for _, want := range []string{
		"--status in_progress",
		"--assignee " + opts.Assignee,
		"--set-metadata gc.claim_generation=1",
		"--set-metadata gc.session_id=mc-sess1",
		"--if-status open",
		"--if-assignee ",
	} {
		if !strings.Contains(update, want) {
			t.Errorf("guarded update %q lacks %q", update, want)
		}
	}
	for _, cmd := range bd.ran {
		if strings.Contains(strings.Join(cmd, " "), "--claim") {
			t.Fatalf("exact claim issued a --claim: %q", cmd)
		}
	}
}
