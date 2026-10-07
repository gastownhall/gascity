package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/workrecord"
)

// The ADR-0009 work-record close gate on the HTTP plane. The CLI plane has run
// it since the contract shipped; these rows are the other half — a bead closed
// through the API must answer to the same contract, or the API becomes the way
// closes get done.

// captureWorkRecordGateLog redirects the gate's warning line for one test so a
// row can assert both that a warning fired and that a clean close is silent.
func captureWorkRecordGateLog(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	original := workRecordGateLogf
	workRecordGateLogf = func(format string, args ...any) {
		fmt.Fprintf(&buf, format, args...)
		buf.WriteString("\n")
	}
	t.Cleanup(func() { workRecordGateLogf = original })
	return &buf
}

// seedGateBead plants a bead under an exact id with exact metadata, which is
// what lets a row model a specific work record (or the absence of one).
func seedGateBead(t *testing.T, store beads.Store, id string, meta map[string]string) string {
	t.Helper()
	mem, ok := store.(*beads.MemStore)
	if !ok {
		t.Fatalf("fixture store is %T, want *beads.MemStore so the test can pin the seeded id", store)
	}
	mem.HonorExplicitIDs = true
	created, err := mem.Create(beads.Bead{ID: id, Title: "work bead " + id, Type: "task", Metadata: meta})
	if err != nil {
		t.Fatalf("seeding %s: %v", id, err)
	}
	if created.ID != id {
		t.Fatalf("the fixture store minted %q instead of the pinned %q", created.ID, id)
	}
	return id
}

// closeSpelling is one of the two ways this surface closes a bead. Both must
// answer to the gate: gating one and not the other just moves the closes.
type closeSpelling struct {
	name  string
	close func(*Server, context.Context, string) error
}

func closeSpellings() []closeSpelling {
	return []closeSpelling{
		{
			name: "POST /bead/{id}/close",
			close: func(s *Server, ctx context.Context, id string) error {
				_, err := s.humaHandleBeadClose(ctx, &BeadCloseInput{ID: id})
				return err
			},
		},
		{
			name: "POST /bead/{id}/update status=closed",
			close: func(s *Server, ctx context.Context, id string) error {
				closed := "closed"
				_, err := s.humaHandleBeadUpdate(ctx, &BeadUpdateInput{ID: id, Body: beadUpdateBody{Status: &closed}})
				return err
			},
		},
	}
}

// assertConflict checks that err is the already-registered wrong-state 409 the
// gate refuses with — no new status on the route means no OpenAPI churn.
func assertConflict(t *testing.T, err error, wantDetail string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s conflict, got no error", wantDetail)
	}
	model := &apierr.ErrorModel{}
	ok := errors.As(err, &model)
	if !ok {
		t.Fatalf("expected *apierr.ErrorModel, got %T: %v", err, err)
	}
	if model.Code != apierr.ConflictWrongState.Code {
		t.Fatalf("error code = %q, want %q (detail: %s)", model.Code, apierr.ConflictWrongState.Code, model.Detail)
	}
	if model.Status != apierr.ConflictWrongState.Status {
		t.Fatalf("status = %d, want %d", model.Status, apierr.ConflictWrongState.Status)
	}
	if !strings.Contains(model.Detail, wantDetail) {
		t.Fatalf("conflict detail %q does not contain %q", model.Detail, wantDetail)
	}
}

func TestAPIBeadCloseEnforcesWorkRecord(t *testing.T) {
	tests := []struct {
		name         string
		meta         map[string]string
		enforce      bool
		wantConflict string // substring of the 409 detail; "" ⇒ the close must succeed
		wantWarn     string // substring of the logged warning; "" ⇒ no warning at all
	}{
		{
			name:         "a missing outcome refuses under enforcement",
			meta:         map[string]string{},
			enforce:      true,
			wantConflict: "missing " + beadmeta.WorkOutcomeMetadataKey,
			wantWarn:     "missing " + beadmeta.WorkOutcomeMetadataKey,
		},
		{
			name:     "a missing outcome warns and proceeds by default",
			meta:     map[string]string{},
			wantWarn: "work-record gate (warn-only)",
		},
		{
			name:         "shipped without a commit refuses under enforcement",
			meta:         map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped},
			enforce:      true,
			wantConflict: beadmeta.WorkCommitMetadataKey,
			wantWarn:     "work-record gate (enforced)",
		},
		{
			name: "shipped with an unreachable commit refuses under enforcement",
			meta: map[string]string{
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
				beadmeta.WorkCommitMetadataKey:  "0000000000000000000000000000000000000000",
				beadmeta.WorkBranchMetadataKey:  "main",
			},
			enforce:      true,
			wantConflict: "not reachable",
			wantWarn:     "not reachable",
		},
		{
			name:    "an unknown outcome refuses under enforcement",
			meta:    map[string]string{beadmeta.WorkOutcomeMetadataKey: "done"},
			enforce: true,

			wantConflict: "invalid " + beadmeta.WorkOutcomeMetadataKey,
			wantWarn:     "invalid " + beadmeta.WorkOutcomeMetadataKey,
		},
		{
			name:    "a no-op outcome closes clean",
			meta:    map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeNoOp},
			enforce: true,
		},
		{
			name:    "a control bead closes untouched",
			meta:    map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
			enforce: true,
		},
	}

	for _, spelling := range closeSpellings() {
		for _, tc := range tests {
			t.Run(spelling.name+"/"+tc.name, func(t *testing.T) {
				if tc.enforce {
					t.Setenv(workrecord.EnforceEnvVar, "1")
				} else {
					t.Setenv(workrecord.EnforceEnvVar, "")
				}
				st := newFakeState(t)
				city := beads.NewMemStore()
				st.cityBeadStore = city
				st.stores = nil
				st.cfg.Rigs = nil
				id := seedGateBead(t, city, "wr-1", tc.meta)
				logged := captureWorkRecordGateLog(t)

				s := New(st)
				err := spelling.close(s, context.Background(), id)

				if tc.wantConflict != "" {
					assertConflict(t, err, tc.wantConflict)
				} else if err != nil {
					t.Fatalf("close: %v (gate log: %s)", err, logged.String())
				}

				got, getErr := city.Get(id)
				if getErr != nil {
					t.Fatalf("Get(%s): %v", id, getErr)
				}
				wantStatus := "closed"
				if tc.wantConflict != "" {
					wantStatus = "open"
				}
				if got.Status != wantStatus {
					t.Fatalf("status = %q, want %q", got.Status, wantStatus)
				}

				out := logged.String()
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
}

// TestAPIBeadCloseHandsTheOracleTheRequestContext pins this plane's half of the
// reachability probe's cancellation path. The probe shells out to git, and the
// only thing that can stop that subprocess when the client is gone is the
// request's own context — so a handler that drops it leaves a blocking call
// with no way out, and a retrying client accumulates them. Whether a done
// context actually stops git is internal/workrecord's row, pinned there against
// a real repository; what belongs here is that the context the oracle is handed
// is the caller's rather than a fresh background one, on both close spellings.
//
// The assertion is a value carried on the request context rather than a
// cancellation, because canceling would only reproduce the "not reachable"
// answer an unreachable commit already gives — it would pass against a handler
// that discarded the context entirely.
func TestAPIBeadCloseHandsTheOracleTheRequestContext(t *testing.T) {
	type requestMarker struct{}

	for _, spelling := range closeSpellings() {
		t.Run(spelling.name, func(t *testing.T) {
			st := newFakeState(t)
			city := beads.NewMemStore()
			st.cityBeadStore = city
			st.stores = nil
			st.cfg.Rigs = nil
			id := seedGateBead(t, city, "wr-ctx-1", map[string]string{
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
				beadmeta.WorkCommitMetadataKey:  "0000000000000000000000000000000000000000",
				beadmeta.WorkBranchMetadataKey:  "main",
			})
			logged := captureWorkRecordGateLog(t)

			var seen []context.Context
			original := workRecordCommitReachable
			workRecordCommitReachable = func(ctx context.Context, _, _, _ string) bool {
				seen = append(seen, ctx)
				return true
			}
			t.Cleanup(func() { workRecordCommitReachable = original })

			request := context.WithValue(context.Background(), requestMarker{}, "the caller")
			if err := spelling.close(New(st), request, id); err != nil {
				t.Fatalf("close: %v (gate log: %s)", err, logged.String())
			}
			if len(seen) != 1 {
				t.Fatalf("the reachability oracle was asked %d times, want exactly 1", len(seen))
			}
			if got := seen[0].Value(requestMarker{}); got != "the caller" {
				t.Fatalf("the oracle was handed a context carrying %v, want the request's; the handler discarded it", got)
			}
		})
	}
}

// TestAPIBeadCloseResolvesTheOwningScopeAsTheCommitRepo pins the half of the
// gate that cannot be answered from the bead alone: "reachable on which
// repository?". The owning store names the scope, so a rig-resident bead is
// checked against the rig's checkout while a city-resident one is checked
// against the city — the answer is not "any repo the server can see".
//
// The resolution is asserted directly rather than through a close against a
// seeded repository: whether git calls a commit an ancestor is
// internal/workrecord's row, pinned there against a real repo, and running a
// second one here would only re-prove git. What is this plane's own is which
// directory that oracle is handed, and an exact path is a sharper statement of
// it than "the close succeeded".
func TestAPIBeadCloseResolvesTheOwningScopeAsTheCommitRepo(t *testing.T) {
	st := newFakeState(t)
	city := beads.NewMemStore()
	rig := beads.NewMemStore()
	rigPath := t.TempDir()
	st.cityBeadStore = city
	st.stores = map[string]beads.Store{"myrig": rig}
	st.cfg.Rigs = []config.Rig{{Name: "myrig", Path: rigPath}}
	s := New(st)

	shipped := beads.Bead{Type: "task", Metadata: beads.StringMap{
		beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
		beadmeta.WorkCommitMetadataKey:  "0000000000000000000000000000000000000000",
		beadmeta.WorkBranchMetadataKey:  "main",
	}}

	if got := s.workRecordRepoDir(rig, shipped); got != rigPath {
		t.Fatalf("rig-resident bead resolves to %q, want the rig checkout %q", got, rigPath)
	}
	if got := s.workRecordRepoDir(city, shipped); got != st.cityPath {
		t.Fatalf("city-resident bead resolves to %q, want the city directory %q", got, st.cityPath)
	}

	// A bead the relocated class binding owns answers to the city checkout as
	// well. The binding is a store rather than a checkout, but the CLI class door
	// hands its own gate cityPath for exactly this population, and a bead the two
	// doors ask different repositories about is the divergence this gate exists
	// to close.
	binding := beads.NewMemStore()
	st.graphBeadStore = binding
	if got := s.workRecordRepoDir(binding, shipped); got != st.cityPath {
		t.Fatalf("binding-owned bead resolves to %q, want the city directory %q", got, st.cityPath)
	}

	// A rig path is configured relative to the city, so the scope root — not the
	// server's working directory — is what a relative path resolves against.
	st.cfg.Rigs = []config.Rig{{Name: "myrig", Path: "rigs/myrig"}}
	wantRelative := filepath.Join(st.cityPath, "rigs/myrig")
	if got := s.workRecordRepoDir(rig, shipped); got != wantRelative {
		t.Fatalf("relative rig path resolves to %q, want %q", got, wantRelative)
	}

	// The rig that answered but names no checkout is unknown, not the city. The
	// city fallback is for a store no configured rig claims; reaching it from a
	// rig that did claim the bead would ask about a repository that is not the
	// bead's, which under enforcement is a false refusal rather than a degraded
	// clause.
	//
	// This row is constructed, not observed: buildStores skips a path-less rig, so
	// the production State registers no store for one and the branch is defensive.
	// It is pinned because State is an interface — a future implementation that
	// does register such a store must still resolve to "unknown".
	st.cfg.Rigs = []config.Rig{{Name: "myrig", Path: "  "}}
	if got := s.workRecordRepoDir(rig, shipped); got != "" {
		t.Fatalf("rig with no configured checkout resolves to %q, want %q (unknown)", got, "")
	}

	// A bead that recorded its own work directory outranks the scope root: the
	// commit was made where the work happened.
	recorded := shipped
	recorded.Metadata = beads.StringMap{beadmeta.WorkDirMetadataKey: "/work/elsewhere"}
	if got := s.workRecordRepoDir(rig, recorded); got != "/work/elsewhere" {
		t.Fatalf("bead with %s resolves to %q, want the recorded directory", beadmeta.WorkDirMetadataKey, got)
	}

	// The resolved directory is handed to the oracle rather than dropped: the rig
	// checkout is a real path but not a repository, so the clause answers "not
	// reachable" instead of degrading the way an unknown root does.
	st.cfg.Rigs = []config.Rig{{Name: "myrig", Path: rigPath}}
	rigID := seedGateBead(t, rig, "wr-rig-1", shipped.Metadata)
	t.Setenv(workrecord.EnforceEnvVar, "1")
	logged := captureWorkRecordGateLog(t)

	_, err := s.humaHandleBeadClose(context.Background(), &BeadCloseInput{ID: rigID})
	assertConflict(t, err, "not reachable")
	if out := logged.String(); strings.Contains(out, "reachability unverified") {
		t.Fatalf("gate output %q degraded the clause for a known scope root", out)
	}
}

// TestAPIBeadCloseDegradesReachabilityWhenTheScopeRootIsUnknown covers the bead
// no scope can name a checkout for: it is owned by the relocated class binding,
// which is a store rather than a checkout, in a city that has no path either —
// so the fallback that answers a binding-owned bead elsewhere has nothing to
// offer, there is no repository to ask, and the reachability clause degrades to
// a warning rather than refusing a close it cannot judge.
//
// The city path is cleared deliberately. With one configured, this population
// resolves to the city checkout — the same directory the CLI class door hands
// its gate — and the clause is evaluated, not degraded; that is the row
// TestAPIBeadCloseResolvesTheOwningScopeAsTheCommitRepo pins. What is left here
// is the genuinely unknowable case.
//
// The control is the clause that never degrades — a missing outcome still
// refuses, because "I could not check the commit" is not a reason to accept a
// bead with no record at all.
//
// The state below is constructed rather than observed: the production
// controllerState takes cityPath at construction from an already-resolved
// directory and never reassigns it, so an empty one is not a state the city
// boots into. State is an interface, so the degradation contract is pinned for
// any implementation that can produce it.
func TestAPIBeadCloseDegradesReachabilityWhenTheScopeRootIsUnknown(t *testing.T) {
	st, binding, _, _ := relocatedGraphRouteState(t)
	st.cityPath = ""
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatalf("ReservedClassPrefix(graph) returned ok=false; expected a reserved prefix")
	}
	shipped := seedGateBead(t, binding, prefix+"-1", map[string]string{
		beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
		beadmeta.WorkCommitMetadataKey:  "0000000000000000000000000000000000000000",
		beadmeta.WorkBranchMetadataKey:  "main",
	})
	recordless := seedGateBead(t, binding, prefix+"-2", map[string]string{})

	t.Setenv(workrecord.EnforceEnvVar, "1")
	logged := captureWorkRecordGateLog(t)
	s := New(st)

	if _, err := s.humaHandleBeadClose(context.Background(), &BeadCloseInput{ID: shipped}); err != nil {
		t.Fatalf("close with an unknowable scope root: %v (gate log: %s)", err, logged.String())
	}
	if out := logged.String(); !strings.Contains(out, "reachability unverified") {
		t.Fatalf("gate output %q does not report the degraded clause", out)
	}
	got, err := binding.Get(shipped)
	if err != nil {
		t.Fatalf("Get(%s): %v", shipped, err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}

	_, err = s.humaHandleBeadClose(context.Background(), &BeadCloseInput{ID: recordless})
	assertConflict(t, err, "missing "+beadmeta.WorkOutcomeMetadataKey)
}

// TestAPIBeadUpdateValidatesTheSubmittedWorkRecord covers the documented atomic
// close: one request that stamps the work record and closes. Validating only the
// stored row would refuse a request that carries a perfectly good record.
func TestAPIBeadUpdateValidatesTheSubmittedWorkRecord(t *testing.T) {
	t.Setenv(workrecord.EnforceEnvVar, "1")
	st := newFakeState(t)
	city := beads.NewMemStore()
	st.cityBeadStore = city
	st.stores = nil
	st.cfg.Rigs = nil
	id := seedGateBead(t, city, "wr-atomic-1", map[string]string{})
	logged := captureWorkRecordGateLog(t)

	s := New(st)
	closed := "closed"
	_, err := s.humaHandleBeadUpdate(context.Background(), &BeadUpdateInput{ID: id, Body: beadUpdateBody{
		Status:   &closed,
		Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeNoOp},
	}})
	if err != nil {
		t.Fatalf("atomic stamp-and-close: %v (gate log: %s)", err, logged.String())
	}
	if out := logged.String(); out != "" {
		t.Fatalf("expected no gate output for a submitted valid record, got %q", out)
	}
	got, getErr := city.Get(id)
	if getErr != nil {
		t.Fatalf("Get(%s): %v", id, getErr)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
}

// TestAPIBeadUpdateGateIgnoresNonClosingWrites is the control on the update arm:
// the gate fires on the closed-status arm only, so an ordinary edit of a bead
// with no work record is untouched.
func TestAPIBeadUpdateGateIgnoresNonClosingWrites(t *testing.T) {
	t.Setenv(workrecord.EnforceEnvVar, "1")
	st := newFakeState(t)
	city := beads.NewMemStore()
	st.cityBeadStore = city
	st.stores = nil
	st.cfg.Rigs = nil
	id := seedGateBead(t, city, "wr-edit-1", map[string]string{})
	logged := captureWorkRecordGateLog(t)

	s := New(st)
	title := "renamed, not closed"
	inProgress := "in_progress"
	for _, body := range []beadUpdateBody{{Title: &title}, {Status: &inProgress}} {
		if _, err := s.humaHandleBeadUpdate(context.Background(), &BeadUpdateInput{ID: id, Body: body}); err != nil {
			t.Fatalf("non-closing update: %v (gate log: %s)", err, logged.String())
		}
	}
	if out := logged.String(); out != "" {
		t.Fatalf("expected no gate output for a non-closing update, got %q", out)
	}
	got, getErr := city.Get(id)
	if getErr != nil {
		t.Fatalf("Get(%s): %v", id, getErr)
	}
	if got.Title != title || got.Status != inProgress {
		t.Fatalf("bead = (%q, %q), want (%q, %q)", got.Title, got.Status, title, inProgress)
	}
}

// TestAPIBeadDeleteStaysOutsideTheWorkRecordGate pins a deliberate scope
// boundary. DELETE on this surface is a soft close, but it means "this bead
// should not exist", not "this work completed": gating it would make an
// unwanted bead undeletable under enforcement, and the CLI plane draws the same
// line (bd delete is not a close spelling). Changing this is a policy decision,
// not a refactor.
func TestAPIBeadDeleteStaysOutsideTheWorkRecordGate(t *testing.T) {
	t.Setenv(workrecord.EnforceEnvVar, "1")
	st := newFakeState(t)
	city := beads.NewMemStore()
	st.cityBeadStore = city
	st.stores = nil
	st.cfg.Rigs = nil
	id := seedGateBead(t, city, "wr-delete-1", map[string]string{})
	logged := captureWorkRecordGateLog(t)

	s := New(st)
	if _, err := s.humaHandleBeadDelete(context.Background(), &BeadDeleteInput{ID: id}); err != nil {
		t.Fatalf("delete: %v (gate log: %s)", err, logged.String())
	}
	if out := logged.String(); out != "" {
		t.Fatalf("expected no gate output for a delete, got %q", out)
	}
}
