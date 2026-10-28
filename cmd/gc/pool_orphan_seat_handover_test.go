package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// seatHandoverCity is a pool-capable "worker" template backing the configured
// named session "reviewer", whose runtime name is "reviewer".
func seatHandoverCity() *config.City {
	return &config.City{
		Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{
			Name:     "reviewer",
			Template: "worker",
			Mode:     "on_demand",
		}},
		ResolvedWorkspaceName: "test-city",
	}
}

// seatRowMeta is the metadata of a "reviewer" seat row of template "worker",
// with overrides applied (an empty value deletes the key).
func seatRowMeta(state string, overrides map[string]string) map[string]string {
	meta := map[string]string{
		"template":                   "worker",
		"session_name":               "reviewer",
		"state":                      state,
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "reviewer",
	}
	for k, v := range overrides {
		if v == "" {
			delete(meta, k)
			continue
		}
		meta[k] = v
	}
	return meta
}

func createSeatSessionRow(t *testing.T, store beads.Store, title string, meta map[string]string) beads.Bead {
	t.Helper()
	row, err := store.Create(beads.Bead{
		Title:    title,
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("create session row %s: %v", title, err)
	}
	return row
}

// createClosedSeatPredecessor creates the predecessor row and closes it, as a
// drained seat's row is closed.
func createClosedSeatPredecessor(t *testing.T, store beads.Store, overrides map[string]string) beads.Bead {
	t.Helper()
	pred := createSeatSessionRow(t, store, "predecessor", seatRowMeta("asleep", overrides))
	if err := store.Close(pred.ID); err != nil {
		t.Fatalf("close predecessor %s: %v", pred.ID, err)
	}
	return pred
}

const seatHandoverClaimedAt = "2026-09-14T19:51:07Z"

// createInProgressWorkClaimedBy creates in_progress "worker"-routed work held
// under holder's bead ID, with the claim projection its hook stamped.
func createInProgressWorkClaimedBy(t *testing.T, store beads.Store, holder beads.Bead) beads.Bead {
	t.Helper()
	work, err := store.Create(beads.Bead{
		Title:    "long bake",
		Type:     "task",
		Assignee: holder.ID,
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:    "worker",
			beadmeta.SessionIDMetadataKey:   holder.ID,
			beadmeta.SessionNameMetadataKey: holder.Metadata["session_name"],
			beadmeta.ClaimedAtMetadataKey:   seatHandoverClaimedAt,
		},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set work in_progress: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work: %v", err)
	}
	return work
}

// sweepSeatWork runs the dead-assignee sweep over work with openRows as the
// open-session snapshot, on a single-store city.
func sweepSeatWork(store beads.Store, cfg *config.City, openRows []beads.Bead, work []beads.Bead) []releasedPoolAssignment {
	return releaseOrphanedPoolAssignments(
		store,
		beads.SessionStore{Store: store},
		cfg,
		"",
		sessionInfosFromBeads(openRows),
		work,
		nil, nil, nil, nil, nil, nil,
	)
}

func getSeatBead(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b
}

// TestReleaseOrphanedPoolAssignments_HandsClosedSeatRowClaimToSuccessor
// (AC-1): work held under the bead ID of a closed named seat row is handed to
// the seat's one live successor row instead of being reopened, so no
// bead.dead_assignee_reopened is recorded for it (gastownhall/gascity#6362).
func TestReleaseOrphanedPoolAssignments_HandsClosedSeatRowClaimToSuccessor(t *testing.T) {
	store := beads.NewMemStore()
	cfg := seatHandoverCity()
	pred := createClosedSeatPredecessor(t, store, nil)
	work := createInProgressWorkClaimedBy(t, store, pred)
	successor := createSeatSessionRow(t, store, "successor", seatRowMeta("awake", map[string]string{"currently_processing_bead_id": work.ID}))

	snapshot := []beads.Bead{work}
	released := sweepSeatWork(store, cfg, []beads.Bead{successor}, snapshot)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none: the seat's live successor %s took over the claim of closed row %s", released, successor.ID, pred.ID)
	}
	rec := events.NewFake()
	emitDeadAssigneeReopenedEvents(rec, snapshot, released, time.Now())
	if len(rec.Events) != 0 {
		t.Fatalf("recorded %d bead.dead_assignee_reopened events, want none", len(rec.Events))
	}
	got := getSeatBead(t, store, work.ID)
	if got.Assignee != successor.ID {
		t.Fatalf("assignee = %q, want successor %q", got.Assignee, successor.ID)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
}

// TestReleaseOrphanedPoolAssignments_SeatHandOverWritesOnlyAssignee (AC-2):
// the hand-over moves the claim and nothing else. Status, the claim
// projection (gc.session_id, gc.session_name, gc.claimed_at), routing and all
// other metadata stay as they were until the successor's own hook rebinds
// them. Revision, ClaimFence and UpdatedAt are the store's bookkeeping of the
// write itself.
func TestReleaseOrphanedPoolAssignments_SeatHandOverWritesOnlyAssignee(t *testing.T) {
	store := beads.NewMemStore()
	cfg := seatHandoverCity()
	pred := createClosedSeatPredecessor(t, store, nil)
	work := createInProgressWorkClaimedBy(t, store, pred)
	if err := store.SetMetadata(work.ID, "gc.session_affinity", "require"); err != nil {
		t.Fatalf("set affinity: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Labels: []string{"pool:worker"}, Description: stringPtr("bake for a while")}); err != nil {
		t.Fatalf("set labels: %v", err)
	}
	work = getSeatBead(t, store, work.ID)
	successor := createSeatSessionRow(t, store, "successor", seatRowMeta("awake", nil))

	if released := sweepSeatWork(store, cfg, []beads.Bead{successor}, []beads.Bead{work}); len(released) != 0 {
		t.Fatalf("released = %v, want none", released)
	}
	got := getSeatBead(t, store, work.ID)
	if got.Assignee != successor.ID {
		t.Fatalf("assignee = %q, want successor %q", got.Assignee, successor.ID)
	}
	got.Assignee = work.Assignee
	got.Revision, got.ClaimFence, got.UpdatedAt = work.Revision, work.ClaimFence, work.UpdatedAt
	if !reflect.DeepEqual(got, work) {
		t.Fatalf("hand-over changed more than the assignee:\n got  %+v\n want %+v", got, work)
	}
}

// TestReleaseOrphanedPoolAssignments_ReleasesClaimOfSeatlessClosedRow (AC-3):
// a closed row with no seat identity has no successor, even when a sibling of
// its template is processing the bead: siblings never inherit its claims
// (ga-r22k2y), so the sweep releases it as at base.
func TestReleaseOrphanedPoolAssignments_ReleasesClaimOfSeatlessClosedRow(t *testing.T) {
	poolRow := map[string]string{
		namedSessionMetadataKey:      "",
		namedSessionIdentityMetadata: "",
		poolManagedMetadataKey:       "true",
	}
	for _, tc := range []struct {
		name      string
		pred      map[string]string
		successor map[string]string
	}{{
		name:      "unaliased pool row",
		pred:      mergeSeatMeta(poolRow, map[string]string{"session_name": "worker-gc-pred"}),
		successor: mergeSeatMeta(poolRow, map[string]string{"session_name": "worker-gc-sibling"}),
	}, {
		name:      "transient pool-slot alias",
		pred:      mergeSeatMeta(poolRow, map[string]string{"session_name": "worker-1", "alias": "worker-1", "pool_slot": "1"}),
		successor: mergeSeatMeta(poolRow, map[string]string{"session_name": "worker-1", "alias": "worker-1", "pool_slot": "1"}),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			cfg := seatHandoverCity()
			pred := createClosedSeatPredecessor(t, store, tc.pred)
			work := createInProgressWorkClaimedBy(t, store, pred)
			sibling := createSeatSessionRow(t, store, "sibling", seatRowMeta("awake", mergeSeatMeta(tc.successor, map[string]string{"currently_processing_bead_id": work.ID})))

			released := sweepSeatWork(store, cfg, []beads.Bead{sibling}, []beads.Bead{work})
			assertSeatWorkReleased(t, store, work, released)
		})
	}
}

func assertSeatWorkReleased(t *testing.T, store beads.Store, work beads.Bead, released []releasedPoolAssignment) {
	t.Helper()
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %+v, want exactly %s", released, work.ID)
	}
	got := getSeatBead(t, store, work.ID)
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("work = status %q assignee %q, want open/unassigned", got.Status, got.Assignee)
	}
}

func mergeSeatMeta(base, overrides map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// TestReleaseOrphanedPoolAssignments_ReleasesClaimWithoutUniqueSeatSuccessor
// (AC-4): the hand-over needs exactly one open, non-archived row of the
// predecessor's template on its seat that reaches the work's store. Anything
// else releases the bead as at base; there is no tie-break.
func TestReleaseOrphanedPoolAssignments_ReleasesClaimWithoutUniqueSeatSuccessor(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []map[string]string
	}{
		{name: "no row on the seat"},
		{name: "two rows on the seat", rows: []map[string]string{seatRowMeta("awake", nil), seatRowMeta("asleep", nil)}},
		{name: "only an archived row", rows: []map[string]string{seatRowMeta("archived", nil)}},
		{name: "only a row of another template", rows: []map[string]string{seatRowMeta("awake", map[string]string{"template": "other"})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			cfg := seatHandoverCity()
			cfg.Agents = append(cfg.Agents, config.Agent{Name: "other", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)})
			pred := createClosedSeatPredecessor(t, store, nil)
			work := createInProgressWorkClaimedBy(t, store, pred)
			var open []beads.Bead
			for i, meta := range tc.rows {
				open = append(open, createSeatSessionRow(t, store, fmt.Sprintf("row-%d", i), meta))
			}

			released := sweepSeatWork(store, cfg, open, []beads.Bead{work})
			assertSeatWorkReleased(t, store, work, released)
		})
	}

	// The successor row is rig-scoped to repo-a. Work held in repo-b's store
	// is out of its reach, so it is released; the control case in repo-a's
	// store is handed over.
	for _, tc := range []struct {
		name     string
		workRig  string
		released bool
	}{
		{name: "only a row whose stores exclude the work's", workRig: "repo-b", released: true},
		{name: "control: the row reaches the work's store", workRig: "repo-a", released: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			cityStore := beads.NewMemStore()
			rigStores := map[string]beads.Store{"repo-a": seatRigMemStore("ra"), "repo-b": seatRigMemStore("rb")}
			workStore := rigStores[tc.workRig]
			cfg := &config.City{
				Agents: []config.Agent{{Name: "worker", Dir: "repo-a", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
				NamedSessions: []config.NamedSession{{
					Name:     "reviewer",
					Template: "worker",
					Dir:      "repo-a",
					Mode:     "on_demand",
				}},
				Rigs: []config.Rig{
					{Name: "repo-a", Path: filepath.Join(cityPath, "repo-a")},
					{Name: "repo-b", Path: filepath.Join(cityPath, "repo-b")},
				},
				ResolvedWorkspaceName: "test-city",
			}
			rigRow := map[string]string{
				"template":                   "repo-a/worker",
				"session_name":               "repo-a--reviewer",
				namedSessionIdentityMetadata: "repo-a/reviewer",
			}
			pred := createClosedSeatPredecessor(t, cityStore, rigRow)
			work := createInProgressWorkClaimedBy(t, workStore, pred)
			if err := workStore.SetMetadata(work.ID, beadmeta.RoutedToMetadataKey, "repo-a/worker"); err != nil {
				t.Fatalf("route work: %v", err)
			}
			work = getSeatBead(t, workStore, work.ID)
			successor := createSeatSessionRow(t, cityStore, "successor", seatRowMeta("awake", rigRow))

			released := releaseOrphanedPoolAssignments(
				cityStore,
				beads.SessionStore{Store: cityStore},
				cfg,
				cityPath,
				sessionInfosFromBeads([]beads.Bead{successor}),
				[]beads.Bead{work},
				[]beads.Store{workStore},
				[]string{tc.workRig},
				rigStores,
				nil, nil, nil,
			)
			if tc.released {
				assertSeatWorkReleased(t, workStore, work, released)
				return
			}
			if len(released) != 0 {
				t.Fatalf("released = %v, want none", released)
			}
			assertSeatWorkKept(t, workStore, work.ID, successor.ID)
		})
	}
}

// seatRigMemStore is a MemStore minting ids under prefix, so a rig store's
// beads never share an id with the city store's.
func seatRigMemStore(prefix string) *beads.MemStore {
	m := beads.NewMemStore()
	m.IDPrefix = prefix
	return m
}

func assertSeatWorkKept(t *testing.T, store beads.Store, id, assignee string) {
	t.Helper()
	got := getSeatBead(t, store, id)
	if got.Status != "in_progress" || got.Assignee != assignee {
		t.Fatalf("work = status %q assignee %q, want in_progress/%q", got.Status, got.Assignee, assignee)
	}
}

// sessionGetErrorStore fails Get for one session row with a non-not-found
// error, as a session store that cannot be read right now does.
type sessionGetErrorStore struct {
	beads.Store
	failID string
}

func (s sessionGetErrorStore) Get(id string) (beads.Bead, error) {
	if id == s.failID {
		return beads.Bead{}, errors.New("session store unavailable")
	}
	return s.Store.Get(id)
}

// TestReleaseOrphanedPoolAssignments_SeatHandOverKeepsClaimWhenItCannotWrite
// (AC-5): when the hand-over cannot be made safely — the bead's assignee moved
// after the snapshot, the store cannot write conditionally, or the
// predecessor's row cannot be read — the pass neither reassigns the bead over
// the newer state nor releases it, and logs a line naming the bead (or, for the
// read error, the identity).
func TestReleaseOrphanedPoolAssignments_SeatHandOverKeepsClaimWhenItCannotWrite(t *testing.T) {
	t.Run("assignee changed since the snapshot", func(t *testing.T) {
		store := beads.NewMemStore()
		pred := createClosedSeatPredecessor(t, store, nil)
		snapshot := createInProgressWorkClaimedBy(t, store, pred)
		successor := createSeatSessionRow(t, store, "successor", seatRowMeta("awake", nil))
		const newer = "gc-newer-claim"
		if err := store.Update(snapshot.ID, beads.UpdateOpts{Assignee: stringPtr(newer)}); err != nil {
			t.Fatalf("land the newer claim: %v", err)
		}

		logs := sweepSeatWorkLogged(t, store, store, []beads.Bead{successor}, snapshot)
		assertSeatWorkKept(t, store, snapshot.ID, newer)
		if !strings.Contains(logs, snapshot.ID) {
			t.Fatalf("log does not name bead %s:\n%s", snapshot.ID, logs)
		}
	})

	t.Run("store cannot write conditionally", func(t *testing.T) {
		store := beads.NewMemStore()
		pred := createClosedSeatPredecessor(t, store, nil)
		work := createInProgressWorkClaimedBy(t, store, pred)
		successor := createSeatSessionRow(t, store, "successor", seatRowMeta("awake", nil))
		store.DisableConditionalWrites = true

		logs := sweepSeatWorkLogged(t, store, store, []beads.Bead{successor}, work)
		assertSeatWorkKept(t, store, work.ID, pred.ID)
		if got := linesNaming(logs, work.ID); got != 1 {
			t.Fatalf("log names bead %s on %d lines, want 1:\n%s", work.ID, got, logs)
		}
	})

	t.Run("predecessor row unreadable", func(t *testing.T) {
		store := beads.NewMemStore()
		pred := createClosedSeatPredecessor(t, store, nil)
		work := createInProgressWorkClaimedBy(t, store, pred)
		successor := createSeatSessionRow(t, store, "successor", seatRowMeta("awake", nil))

		logs := sweepSeatWorkLogged(t, store, sessionGetErrorStore{Store: store, failID: pred.ID}, []beads.Bead{successor}, work)
		assertSeatWorkKept(t, store, work.ID, pred.ID)
		if got := linesNaming(logs, pred.ID); got != 1 {
			t.Fatalf("log names identity %s on %d lines, want 1:\n%s", pred.ID, got, logs)
		}
	})
}

// sweepSeatWorkLogged runs one sweep over work with sessionStore as the
// session store and returns what it logged, failing if it released the bead.
func sweepSeatWorkLogged(t *testing.T, store, sessionStore beads.Store, openRows []beads.Bead, work beads.Bead) string {
	t.Helper()
	var buf bytes.Buffer
	restore := captureLogOutput(&buf)
	released := releaseOrphanedPoolAssignments(
		store,
		beads.SessionStore{Store: sessionStore},
		seatHandoverCity(),
		"",
		sessionInfosFromBeads(openRows),
		[]beads.Bead{work},
		nil, nil, nil, nil, nil, nil,
	)
	restore()
	if len(released) != 0 {
		t.Fatalf("released = %v, want none", released)
	}
	return buf.String()
}

// linesNaming counts the log lines that mention id.
func linesNaming(logs, id string) int {
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, id) {
			n++
		}
	}
	return n
}

// TestSeatHandOverIsAdoptedBySuccessorHook (AC-6): after the sweep hands the
// closed row's bead to its seat successor, the successor's own
// `gc hook --claim` adopts it as its existing assignment, rebinds
// gc.session_id/gc.session_name to itself, keeps the write-once gc.claimed_at,
// and records work_started for the bead.
func TestSeatHandOverIsAdoptedBySuccessorHook(t *testing.T) {
	store := beads.NewMemStore()
	pred := createClosedSeatPredecessor(t, store, nil)
	work := createInProgressWorkClaimedBy(t, store, pred)
	successor := createSeatSessionRow(t, store, "successor", seatRowMeta("awake", map[string]string{"session_name": "reviewer-next"}))

	if released := sweepSeatWork(store, seatHandoverCity(), []beads.Bead{successor}, []beads.Bead{work}); len(released) != 0 {
		t.Fatalf("released = %v, want none", released)
	}

	var started []string
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			b := getSeatBead(t, store, work.ID)
			out, err := json.Marshal([]beads.Bead{b})
			return string(out), err
		},
		Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
			t.Error("Claim must not be called: the bead is already the successor's")
			return beads.Bead{}, false, nil
		},
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" },
		StampWorkMeta: func(_ context.Context, _ string, _ []string, beadID, _ string, patch map[string]string) error {
			return store.SetMetadataBatch(beadID, patch)
		},
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
			return store.Get(beadID)
		},
		RecordWorkStarted: func(sessionID string, _ hookStartedFence, key hookStartedKey, _ string) (hookStartedResult, error) {
			started = append(started, sessionID+":"+key.Bead)
			return hookStartedRecorded, nil
		},
		ReadWorkStarted:   noStartedClaims,
		DrainPending:      func(string) (bool, error) { return false, nil },
		StoreRef:          func(string) string { return "" },
		StampSessionClaim: noopStampSessionClaim,
		PublishRunMap:     noopPublishRunMap,
	}
	env := []string{
		"GC_SESSION_ID=" + successor.ID,
		"GC_SESSION_NAME=reviewer-next",
		"GC_INSTANCE_TOKEN=tok-successor",
		"GC_RUNTIME_EPOCH=1",
	}
	opts := hookClaimOptions{
		Assignee:           successor.ID,
		IdentityCandidates: []string{successor.ID, "reviewer-next", "reviewer"},
		SessionID:          successor.ID,
		RouteTargets:       []string{"worker"},
		Env:                env,
		StartedFence:       hookStartedFenceOf(env),
		JSON:               true,
	}
	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	result := decodeTurnBoundResult(t, stdout.String())
	if result.Action != "work" || result.Reason != "existing_assignment" || result.BeadID != work.ID {
		t.Fatalf("result = %+v, want %s adopted as the successor's existing assignment; stderr=%s", result, work.ID, stderr.String())
	}
	got := getSeatBead(t, store, work.ID)
	if got.Metadata[beadmeta.SessionIDMetadataKey] != successor.ID || got.Metadata[beadmeta.SessionNameMetadataKey] != "reviewer-next" {
		t.Fatalf("claim projection = %s/%s, want the successor %s/reviewer-next",
			got.Metadata[beadmeta.SessionIDMetadataKey], got.Metadata[beadmeta.SessionNameMetadataKey], successor.ID)
	}
	if got.Metadata[beadmeta.ClaimedAtMetadataKey] != seatHandoverClaimedAt {
		t.Fatalf("gc.claimed_at = %q, want the original %q", got.Metadata[beadmeta.ClaimedAtMetadataKey], seatHandoverClaimedAt)
	}
	if want := []string{successor.ID + ":" + work.ID}; !reflect.DeepEqual(started, want) {
		t.Fatalf("work_started records = %v, want %v", started, want)
	}
}

// TestReleaseOrphanedPoolAssignments_KeepsNamedClaimOfFallbackIdentityRow
// (AC-7): a row the awake set credits with a configured named identity only
// through the configured-named fallback (configured_named_session without
// configured_named_identity, session_name the identity's runtime name) owns
// work assigned to that identity in the sweep too, even when the work's named
// route gate misses.
func TestReleaseOrphanedPoolAssignments_KeepsNamedClaimOfFallbackIdentityRow(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{SessionTemplate: "{{.City}}-{{.Name}}"},
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
			{Name: "polecat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
		},
		NamedSessions: []config.NamedSession{{
			Name:     "reviewer",
			Template: "worker",
			Mode:     "on_demand",
		}},
		ResolvedWorkspaceName: "test-city",
	}
	runtimeName := config.NamedSessionRuntimeName("test-city", cfg.Workspace, "reviewer")
	if runtimeName == "reviewer" {
		t.Fatalf("runtime name = %q, want it to differ from the identity", runtimeName)
	}
	// The row that claimed the bead under the identity has closed.
	createClosedSeatPredecessor(t, store, map[string]string{"session_name": runtimeName})
	seat := createSeatSessionRow(t, store, "seat", seatRowMeta("awake", map[string]string{
		"session_name":               runtimeName,
		namedSessionIdentityMetadata: "",
	}))
	if ids := sessionAssignmentIdentifiersForConfig(seat, cfg); !slices.Contains(ids, "reviewer") {
		t.Fatalf("awake-set identities of the seat row = %v, want them to credit reviewer", ids)
	}
	work, err := store.Create(beads.Bead{
		Title:    "polecat-routed",
		Type:     "task",
		Assignee: "reviewer",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "polecat"},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}
	work = getSeatBead(t, store, work.ID)

	if released := sweepSeatWork(store, cfg, []beads.Bead{seat}, []beads.Bead{work}); len(released) != 0 {
		t.Fatalf("released = %v, want none: open row %s answers to reviewer", released, seat.ID)
	}
	assertSeatWorkKept(t, store, work.ID, "reviewer")
}

// TestReleaseOrphanedPoolAssignments_ReleasesHandedClaimWhenSuccessorCloses
// (AC-8): a bead handed to a successor that closes without adopting it is
// handed on again only if another row holds the seat; with none, the next
// sweep releases it.
func TestReleaseOrphanedPoolAssignments_ReleasesHandedClaimWhenSuccessorCloses(t *testing.T) {
	store := beads.NewMemStore()
	cfg := seatHandoverCity()
	pred := createClosedSeatPredecessor(t, store, nil)
	work := createInProgressWorkClaimedBy(t, store, pred)
	successor := createSeatSessionRow(t, store, "successor", seatRowMeta("awake", nil))

	if released := sweepSeatWork(store, cfg, []beads.Bead{successor}, []beads.Bead{work}); len(released) != 0 {
		t.Fatalf("first sweep released = %v, want the hand-over to %s", released, successor.ID)
	}
	handed := getSeatBead(t, store, work.ID)
	if handed.Assignee != successor.ID {
		t.Fatalf("assignee after first sweep = %q, want successor %q", handed.Assignee, successor.ID)
	}
	if err := store.Close(successor.ID); err != nil {
		t.Fatalf("close successor: %v", err)
	}

	released := sweepSeatWork(store, cfg, nil, []beads.Bead{handed})
	assertSeatWorkReleased(t, store, handed, released)
}
