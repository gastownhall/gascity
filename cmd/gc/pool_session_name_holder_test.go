package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// seedPoolSessionBead creates a pool session bead through the production create
// path and then rewrites the metadata the test cares about, so the fixture
// stays a real bead rather than a hand-rolled approximation of one.
func seedPoolSessionBead(t *testing.T, store beads.Store, identity poolSessionCreateIdentity, overrides map[string]string) sessionpkg.Info {
	t.Helper()
	info, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, nil, time.Now().UTC(), identity, "")
	if err != nil {
		t.Fatalf("seeding pool session bead: %v", err)
	}
	front := sessionFrontDoor(store)
	for k, v := range overrides {
		if err := front.SetMarker(info.ID, k, v); err != nil {
			t.Fatalf("seeding %s=%q on %s: %v", k, v, info.ID, err)
		}
	}
	return info
}

// TestPoolSessionCreate_LegacyBeadIDHolderDoesNotStallTheSlot is the day-one
// migration guard, and the one condition the fix could not ship without.
//
// Failing the create closed is only safe if the names that are actually held on
// a running fleet get released. The live holder on cherry is an OPEN, asleep
// pool bead (gcg-session-a11f2898) that has no pod, holds the pool alias
// "bd.dog-1", and holds a bead-ID-scoped session_name. If that settled holder
// counted as the slot's identity lease, the fail-closed create would convert a
// 115-pods/hour leak into a permanently stalled pool on the first tick after
// deploy: strictly worse, because a leak is visible and a silent stall is not.
//
// It does not block, and this pins why: the holder reserves only the name it
// was minted with, and the identity lease (ensurePoolIdentityNotHeldByOpenRow)
// deliberately ignores settled (asleep/active) rows.
func TestPoolSessionCreate_LegacyBeadIDHolderDoesNotStallTheSlot(t *testing.T) {
	store := beads.NewMemStore()
	identity := poolChurnIdentity()

	holder := seedPoolSessionBead(t, store, identity, nil)
	legacyName := PoolSessionName(poolChurnTemplate, holder.ID)
	front := sessionFrontDoor(store)
	// The live cherry shape: asleep, no runtime box, still open, still holding
	// the pool alias and the bead-ID-scoped runtime name.
	for k, v := range map[string]string{
		"session_name": legacyName,
		"alias":        "bd.dog-1",
		"state":        string(sessionpkg.StateAsleep),
		"sleep_reason": "runtime-missing",
	} {
		if err := front.SetMarker(holder.ID, k, v); err != nil {
			t.Fatalf("seeding %s on holder: %v", k, err)
		}
	}

	open, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	info, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, newSessionBeadSnapshot(open), time.Now().UTC(), identity, "")
	if err != nil {
		t.Fatalf("create stalled behind the legacy bead-ID holder %s (session_name %q): %v — a fail-closed derive that cannot get past the names already on a running fleet is a permanent pool outage, not a fix", holder.ID, legacyName, err)
	}
	got := strings.TrimSpace(info.SessionNameMetadata)
	if want := PoolSessionName(poolChurnTemplate, info.ID); got != want {
		t.Fatalf("session_name = %q, want the bead-scoped %q", got, want)
	}
	if got == legacyName {
		t.Fatalf("new create claimed the legacy holder's own name %q", legacyName)
	}
}

// TestPoolSessionCreate_SweptSlotReleasesItsNameForTheNextAttempt pins the
// release rule the fail-closed derive rests on. A retired pool slot is closed
// with an ordinary reason (orphaned, gc_swept) rather than failed_create, so
// the failed_create release in internal/session/names.go does not apply to it;
// what releases the name is the pool carve-out, and that carve-out reads two
// metadata keys this create path writes. If either stops being written the
// slot's name is reserved by a dead bead forever and the pool never starts
// again.
func TestPoolSessionCreate_SweptSlotReleasesItsNameForTheNextAttempt(t *testing.T) {
	store := beads.NewMemStore()
	identity := poolChurnIdentity()
	front := sessionFrontDoor(store)

	retired := seedPoolSessionBead(t, store, identity, nil)
	retiredName := strings.TrimSpace(retired.SessionNameMetadata)
	if _, err := front.Close(retired.ID, "orphaned", time.Now().UTC()); err != nil {
		t.Fatalf("closing retired slot: %v", err)
	}

	open, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	info, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, newSessionBeadSnapshot(open), time.Now().UTC(), identity, "")
	if err != nil {
		t.Fatalf("create stalled behind swept slot %s: %v", retired.ID, err)
	}
	// Bead-scoped names are never reused: the replacement gets its own.
	if got := strings.TrimSpace(info.SessionNameMetadata); got == retiredName || got != PoolSessionName(poolChurnTemplate, info.ID) {
		t.Fatalf("replacement session_name = %q, want a fresh bead-scoped name (retired %q)", got, retiredName)
	}
}

// TestPoolSessionCreate_FailedCreateHolderReleasesItsName is the other half of
// the availability story, and the one the retry loop depends on every tick: the
// rollback closes a failed attempt as failed_create, and the very next attempt
// must be able to claim the same name. If it could not, the fail-closed derive
// would stall the slot after a single failed start — which is the leak's own
// failure mode wearing different clothes.
func TestPoolSessionCreate_FailedCreateHolderReleasesItsName(t *testing.T) {
	store := beads.NewMemStore()
	identity := poolChurnIdentity()
	front := sessionFrontDoor(store)
	now := time.Now().UTC()

	first := seedPoolSessionBead(t, store, identity, nil)
	firstName := strings.TrimSpace(first.SessionNameMetadata)
	if !closeFailedCreateBead(front, first.ID, now, discardWriter{}) {
		t.Fatalf("closeFailedCreateBead(%s) failed", first.ID)
	}

	open, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	info, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, newSessionBeadSnapshot(open), now, identity, "")
	if err != nil {
		t.Fatalf("retry after a rolled-back attempt: %v", err)
	}
	// A new generation gets a new bead-scoped name; what keeps that from
	// leaking the previous box is releaseBeadScopedPoolRuntime, pinned by
	// TestPoolSessionCreate_FailedCreatesLeaveAtMostOneLiveRuntime.
	if got := strings.TrimSpace(info.SessionNameMetadata); got == firstName || got != PoolSessionName(poolChurnTemplate, info.ID) {
		t.Fatalf("retry session_name = %q, want a fresh bead-scoped name (first %q)", got, firstName)
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// degradedQueryStore answers writes and Get normally but fails every metadata
// query — the shape of a store whose backend is reachable enough to have
// written this tick's beads but cannot serve the reservation scan.
type degradedQueryStore struct {
	beads.Store
	err error
}

func (s degradedQueryStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if len(q.Metadata) > 0 {
		return nil, s.err
	}
	return s.Store.List(q)
}

type selectiveDegradedQueryStore struct {
	beads.Store
	failValue string
	err       error
}

func (s selectiveDegradedQueryStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	for _, value := range q.Metadata {
		if value == s.failValue {
			return nil, s.err
		}
	}
	return s.Store.List(q)
}

// TestPoolSessionCreate_LiveHolderBlocksEvenWhenTheStoreScanCannotAnswer pins
// the snapshot collision result ahead of the failing store scan. Either source
// of uncertainty refuses the create; a known live holder additionally keeps
// the typed collision result used by the retry loop.
func TestPoolSessionCreate_LiveHolderBlocksEvenWhenTheStoreScanCannotAnswer(t *testing.T) {
	mem := beads.NewMemStore()
	identity := poolChurnIdentity()

	live, err := createPoolSessionBeadWithAlias(mem, poolChurnTemplate, nil, nil, time.Now().UTC(), identity, "")
	if err != nil {
		t.Fatalf("live create: %v", err)
	}
	open, err := loadSessionBeads(mem)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	snapshot := newSessionBeadSnapshot(open)

	degraded := degradedQueryStore{Store: mem, err: errors.New("gateway unreachable")}
	second, err := createPoolSessionBeadWithAlias(degraded, poolChurnTemplate, nil, snapshot, time.Now().UTC(), identity, "")
	if err == nil {
		t.Fatalf("create handed %q to a second bead while %s holds it live, because the store scan could not answer", second.SessionNameMetadata, live.ID)
	}
	if !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("create error = %v, want errPoolSessionNameUnavailable", err)
	}
}

// TestPoolSessionCreate_DegradedStoreRefusesWithoutALiveHolder is the
// discriminating control for the test above: an empty snapshot is not proof
// that the runtime identifier is free when the authoritative reservation scan
// cannot answer. The create must fail closed and leave no session bead.
func TestPoolSessionCreate_DegradedStoreRefusesWithoutALiveHolder(t *testing.T) {
	mem := beads.NewMemStore()
	identity := poolChurnIdentity()

	availabilityErr := errors.New("gateway unreachable")
	degraded := degradedQueryStore{Store: mem, err: availabilityErr}
	info, err := createPoolSessionBeadWithAlias(degraded, poolChurnTemplate, nil, newSessionBeadSnapshot(nil), time.Now().UTC(), identity, "")
	if !errors.Is(err, availabilityErr) {
		t.Fatalf("create error = %v, want wrapped availability error", err)
	}
	if info.ID != "" {
		t.Fatalf("refused create returned session %#v", info)
	}
	created, listErr := mem.ListByLabel(sessionBeadLabel, 0)
	if listErr != nil {
		t.Fatalf("listing session beads: %v", listErr)
	}
	if len(created) != 0 {
		t.Fatalf("session beads = %#v, want none after availability error", created)
	}
}

func TestPoolSessionCreate_DegradedTransientIdentityCheckRefusesWithoutCreating(t *testing.T) {
	mem := beads.NewMemStore()
	availabilityErr := errors.New("identity lookup unavailable")
	store := selectiveDegradedQueryStore{
		Store:     mem,
		failValue: "worker-1",
		err:       availabilityErr,
	}
	identity := poolSessionCreateIdentity{
		AgentName:     "worker-1",
		Slot:          1,
		TransientSlot: true,
	}

	info, err := createPoolSessionBeadWithAlias(store, "worker", nil, newSessionBeadSnapshot(nil), time.Now().UTC(), identity, "")
	if !errors.Is(err, availabilityErr) {
		t.Fatalf("create error = %v, want wrapped transient-identity availability error", err)
	}
	if info.ID != "" {
		t.Fatalf("refused create returned session %#v", info)
	}
	created, listErr := mem.ListByLabel(sessionBeadLabel, 0)
	if listErr != nil {
		t.Fatalf("listing session beads: %v", listErr)
	}
	if len(created) != 0 {
		t.Fatalf("session beads = %#v, want none after transient-identity availability error", created)
	}
}
