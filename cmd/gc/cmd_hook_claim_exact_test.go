package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// assignment, when set, is the only fence claims write through: the
	// store offers no revision fence.
	assignment beads.AssignmentGuardedUpdater
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
	if f.assignment != nil {
		return molecule.ClaimExactWriters{Assignment: f.assignment}
	}
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

// fakeAssignmentGuardBd is a bd that knows --if-status/--if-assignee (unless
// noGuard) but not --if-revision. It serves its beads, applies a guarded update
// only while the bead's status and assignee match the guard, answers --claim
// as the runner's BEADS_ACTOR, and records every command.
type fakeAssignmentGuardBd struct {
	beads   map[string]beads.Bead
	noGuard bool
	ran     [][]string
}

func newFakeAssignmentGuardBd(rows ...beads.Bead) *fakeAssignmentGuardBd {
	f := &fakeAssignmentGuardBd{beads: map[string]beads.Bead{}}
	for _, b := range rows {
		f.beads[b.ID] = b
	}
	return f
}

func (f *fakeAssignmentGuardBd) runner(_ context.Context, env map[string]string) beads.CommandRunner {
	actor := env["BEADS_ACTOR"]
	return func(_ string, name string, args ...string) ([]byte, error) {
		f.ran = append(f.ran, append([]string{name}, args...))
		if name != "bd" || len(args) == 0 {
			return nil, fmt.Errorf("unexpected command %s %q", name, args)
		}
		switch args[0] {
		case "show":
			b, ok := f.beads[args[len(args)-1]]
			if !ok {
				return nil, fmt.Errorf("unexpected show %q", args)
			}
			return json.Marshal([]beads.Bead{b})
		case "update", "close", "assign", "delete":
			if len(args) == 2 && args[1] == "--help" {
				if f.noGuard {
					return []byte("Flags:\n      --claim\n"), nil
				}
				return []byte("Flags:\n      --if-status string\n      --if-assignee string\n"), nil
			}
			if args[0] == "update" {
				return f.update(actor, args[1:])
			}
		}
		return nil, fmt.Errorf("unexpected command %s %q", name, args)
	}
}

func (f *fakeAssignmentGuardBd) update(actor string, args []string) ([]byte, error) {
	var id string
	var claim bool
	var ifStatus, ifAssignee *string
	var sets [][2]string
	for i := 0; i < len(args); i++ {
		flag := args[i]
		switch {
		case flag == "--json":
			continue
		case flag == "--claim":
			claim = true
			continue
		case !strings.HasPrefix(flag, "--"):
			id = flag
			continue
		case f.noGuard && (flag == "--if-status" || flag == "--if-assignee"):
			return []byte("Error: unknown flag: " + flag), fakeBdExitError(1)
		case i+1 >= len(args):
			return nil, fmt.Errorf("flag %s has no value", flag)
		}
		value := args[i+1]
		i++
		switch flag {
		case "--if-status":
			ifStatus = &value
		case "--if-assignee":
			ifAssignee = &value
		case "--status", "--assignee", "--set-metadata":
			sets = append(sets, [2]string{flag, value})
		default:
			return nil, fmt.Errorf("unknown flag: %s", flag)
		}
	}
	current, ok := f.beads[id]
	if !ok {
		return nil, fmt.Errorf("unexpected update of %q", id)
	}
	next := current
	next.Metadata = map[string]string{}
	for k, v := range current.Metadata {
		next.Metadata[k] = v
	}
	switch {
	case claim:
		if current.Status != "open" || current.Assignee != "" {
			return nil, fmt.Errorf("%s is already claimed", id)
		}
		next.Status, next.Assignee = "in_progress", actor
	case ifStatus == nil || ifAssignee == nil:
		return nil, fmt.Errorf("unguarded update %q", args)
	case current.Status != *ifStatus || current.Assignee != *ifAssignee:
		return nil, fakeBdExitError(13)
	}
	for _, set := range sets {
		switch set[0] {
		case "--status":
			next.Status = set[1]
		case "--assignee":
			next.Assignee = set[1]
		case "--set-metadata":
			k, v, _ := strings.Cut(set[1], "=")
			next.Metadata[k] = v
		}
	}
	f.beads[id] = next
	return json.Marshal([]beads.Bead{next})
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
	bd := newFakeAssignmentGuardBd(beads.Bead{
		ID: "work-1", Title: "work", Status: "open", Type: "task",
		Metadata: graphStepMeta("build"),
	})
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })
	hookClaimCommandRunnerWithEnvContext = bd.runner

	opts := poolClaimOpts()
	req := hookClaimExactPlan(bd.beads["work-1"], opts.Assignee, true, false, opts.Env, time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC))
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

// memAssignmentGuard is bd's --if-status/--if-assignee guard over a MemStore.
// beforeWrite, when set, runs between the claim's read and its guarded write.
type memAssignmentGuard struct {
	store       *beads.MemStore
	beforeWrite func()
}

func (g *memAssignmentGuard) UpdateIfAssignment(id, expectedStatus, expectedAssignee string, opts beads.UpdateOpts) (bool, error) {
	if g.beforeWrite != nil {
		g.beforeWrite()
	}
	b, err := g.store.Get(id)
	if err != nil {
		return false, err
	}
	if b.Status != expectedStatus || b.Assignee != expectedAssignee {
		return false, nil
	}
	return true, g.store.Update(id, opts)
}

// TestHookClaimExactReleasesAClaimWhoseObservationMoved: on a store with only
// the assignment guard, the guarded write lands but the readback shows a step
// identity key that moved after the work query observed it. The hook gives
// the bead back through the assignee-CAS release, records the release with
// reason observation_moved, writes no receipt for it, and goes on to the next
// candidate; only a failed release exits 1.
func TestHookClaimExactReleasesAClaimWhoseObservationMoved(t *testing.T) {
	const self = "gc__role-mc-sess1"
	meta := func() map[string]string {
		m := graphStepMeta("build")
		m[beadmeta.ContinuationGroupMetadataKey] = "group-1"
		return m
	}
	for _, tc := range []struct {
		name       string
		key, moved string
		releaseErr error
	}{
		{name: "step_id", key: beadmeta.StepIDMetadataKey, moved: "test"},
		{name: "root_bead_id", key: beadmeta.RootBeadIDMetadataKey, moved: "root-2"},
		{name: "routed_to", key: beadmeta.RoutedToMetadataKey, moved: "other"},
		{name: "continuation_group", key: beadmeta.ContinuationGroupMetadataKey, moved: "group-2"},
		{name: "release fails", key: beadmeta.StepIDMetadataKey, moved: "test", releaseErr: errors.New("bd unreachable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExactClaimFixture(t)
			f.add(beads.Bead{ID: "work-1", Title: "moving", Status: "open", Metadata: meta()},
				beads.Bead{ID: "work-1", Status: "open", Metadata: meta()})
			f.add(beads.Bead{ID: "work-2", Title: "next", Status: "open", Metadata: graphStepMeta("build")},
				beads.Bead{ID: "work-2", Status: "open", Metadata: graphStepMeta("build")})
			moved := false
			f.assignment = &memAssignmentGuard{store: f.store, beforeWrite: func() {
				if moved {
					return
				}
				moved = true
				if err := f.store.SetMetadata("work-1", tc.key, tc.moved); err != nil {
					t.Fatalf("move %s: %v", tc.key, err)
				}
			}}
			var released []hookClaimReleaseRecord
			var releaseCalls []string
			ops := f.ops()
			ops.Release = func(_ context.Context, _ string, _ []string, id, assignee string) (bool, error) {
				releaseCalls = append(releaseCalls, id+"/"+assignee)
				if tc.releaseErr != nil {
					return false, tc.releaseErr
				}
				open, empty := "open", ""
				return true, f.store.Update(id, beads.UpdateOpts{Status: &open, Assignee: &empty})
			}
			ops.EmitClaimReleased = func(r hookClaimReleaseRecord) { released = append(released, r) }

			var stdout, stderr bytes.Buffer
			code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr)
			if len(releaseCalls) != 1 || releaseCalls[0] != "work-1/"+self {
				t.Fatalf("release calls = %q, want work-1 released as %s; stderr=%s", releaseCalls, self, stderr.String())
			}
			if strings.Contains(stdout.String(), `"bead_id":"work-1"`) {
				t.Fatalf("hook wrote a receipt for the moved claim: %s", stdout.String())
			}
			if tc.releaseErr != nil {
				if code != 1 {
					t.Fatalf("doHookClaim = %d, want 1 after a failed release; stderr=%s", code, stderr.String())
				}
				if len(released) != 0 {
					t.Fatalf("released events = %+v, want none for a failed release", released)
				}
				return
			}
			if code != 0 {
				t.Fatalf("doHookClaim = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			want := hookClaimReleaseRecord{BeadID: "work-1", Assignee: self, Reason: "observation_moved"}
			if len(released) != 1 || released[0] != want {
				t.Fatalf("released events = %+v, want [%+v]", released, want)
			}
			if got := f.get("work-1"); got.Status != "open" || got.Assignee != "" {
				t.Fatalf("work-1 = status %q assignee %q, want released", got.Status, got.Assignee)
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("decode result %q: %v", stdout.String(), err)
			}
			if result.Action != "work" || result.BeadID != "work-2" {
				t.Fatalf("result = %+v, want work on work-2", result)
			}
		})
	}
}

// TestHookClaimExactFailsClosedWithoutAnyFence: on a bd with neither
// --if-revision nor --if-status/--if-assignee, the eligible tier writes nothing
// to a graph step, names the bead and the missing capability on stderr, and
// counts it toward claims_errored; it never falls back to --claim for it. A
// non-graph candidate in the same run still claims through --claim.
func TestHookClaimExactFailsClosedWithoutAnyFence(t *testing.T) {
	const missing = "store supports neither --if-revision nor --if-status/--if-assignee"
	plain := map[string]string{beadmeta.RoutedToMetadataKey: "worker"}
	for _, tc := range []struct {
		name      string
		rows      []beads.Bead
		wantDrain bool
	}{
		{
			name: "graph step alone",
			rows: []beads.Bead{
				{ID: "work-1", Title: "step", Status: "open", Type: "task", Metadata: graphStepMeta("build")},
			},
			wantDrain: true,
		},
		{
			name: "non-graph candidate still claims",
			rows: []beads.Bead{
				{ID: "work-1", Title: "step", Status: "open", Type: "task", Metadata: graphStepMeta("build")},
				{ID: "work-2", Title: "plain", Status: "open", Type: "task", Metadata: plain},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bd := newFakeAssignmentGuardBd(tc.rows...)
			bd.noGuard = true
			originalRunner := hookClaimCommandRunnerWithEnvContext
			t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })
			hookClaimCommandRunnerWithEnvContext = bd.runner

			f := newExactClaimFixture(t)
			f.observed = tc.rows
			ops := f.ops()
			ops.Claim = hookClaimWithBdStore
			ops.ClaimExact = hookClaimExactWithBdStore
			ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
				return bd.beads[id], nil
			}
			opts := poolClaimOpts()

			var stdout, stderr bytes.Buffer
			doHookClaim("bd ready --json", t.TempDir(), opts, ops, &stdout, &stderr)

			if got := bd.beads["work-1"]; got.Status != "open" || got.Assignee != "" || got.Metadata[beadmeta.ClaimGenerationMetadataKey] != "" {
				t.Fatalf("work-1 = status %q assignee %q generation %q, want it unwritten", got.Status, got.Assignee, got.Metadata[beadmeta.ClaimGenerationMetadataKey])
			}
			if !strings.Contains(stderr.String(), "work-1") || !strings.Contains(stderr.String(), missing) {
				t.Fatalf("stderr = %q, want work-1 and %q named", stderr.String(), missing)
			}
			for _, cmd := range bd.ran {
				if strings.Contains(strings.Join(cmd, " "), "work-1 --claim") {
					t.Fatalf("--claim ran for the graph step: %q", cmd)
				}
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("decode result %q: %v; stderr=%s", stdout.String(), err, stderr.String())
			}
			if tc.wantDrain {
				if result.Action != "drain" || result.Reason != "claims_errored" {
					t.Fatalf("result = %+v, want drain/claims_errored", result)
				}
				return
			}
			if result.Action != "work" || result.BeadID != "work-2" {
				t.Fatalf("result = %+v, want work on work-2; stderr=%s", result, stderr.String())
			}
			if got := bd.beads["work-2"]; got.Status != "in_progress" || got.Assignee != opts.Assignee {
				t.Fatalf("work-2 = status %q assignee %q, want claimed through --claim", got.Status, got.Assignee)
			}
		})
	}
}

// readbackFailingReader serves the work store's rows, except that every read
// of failID after the first fails: the exact claim's own Get succeeds and its
// post-write re-read does not.
type readbackFailingReader struct {
	store  *beads.MemStore
	failID string
	reads  int
}

func (r *readbackFailingReader) Get(id string) (beads.Bead, error) {
	if id == r.failID {
		r.reads++
		if r.reads > 1 {
			return beads.Bead{}, errors.New("bd unreachable")
		}
	}
	return r.store.Get(id)
}

// TestHookClaimExactCommittedClaimWithFailedReadbackStopsTheEligibleTier: an
// exact claim whose fenced write landed but whose re-read failed is a bead this
// session now owns. The eligible tier stops with exit 1 rather than skipping it
// as an unclaimable candidate: it emits no claim failure, writes no receipt,
// and does not go on to claim the next candidate.
func TestHookClaimExactCommittedClaimWithFailedReadbackStopsTheEligibleTier(t *testing.T) {
	f := newExactClaimFixture(t)
	f.add(beads.Bead{ID: "work-1", Title: "claimed", Status: "open", Metadata: graphStepMeta("build")},
		beads.Bead{ID: "work-1", Status: "open", Metadata: graphStepMeta("build")})
	f.add(beads.Bead{ID: "work-2", Title: "next", Status: "open", Metadata: graphStepMeta("build")},
		beads.Bead{ID: "work-2", Status: "open", Metadata: graphStepMeta("build")})
	reader := &readbackFailingReader{store: f.store, failID: "work-1"}
	ops := f.ops()
	ops.ClaimExact = func(_ context.Context, _ string, _ []string, actor string, req molecule.ClaimExactRequest) (beads.Bead, hookClaimVerdict, error) {
		return hookClaimExactThroughStore(reader, f.writers(), actor, req)
	}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 1 {
		t.Fatalf("doHookClaim = %d, want 1 for a committed claim whose readback failed; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "claimed work-1 but loading canonical bead failed") {
		t.Fatalf("stderr = %q, want the committed claim of work-1 named", stderr.String())
	}
	if len(f.failed) != 0 {
		t.Fatalf("claim failures = %v, want none: the claim committed", f.failed)
	}
	if strings.Contains(stdout.String(), `"action":"work"`) {
		t.Fatalf("hook wrote a receipt after a failed readback: %s", stdout.String())
	}
	if got := f.get("work-1"); got.Status != "in_progress" || got.Assignee != "gc__role-mc-sess1" {
		t.Fatalf("work-1 = status %q assignee %q, want the committed claim kept", got.Status, got.Assignee)
	}
	f.assertUntouched("work-2")
}
