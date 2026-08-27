package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// triggerRepointGuardEnv is one active pool member bound to "wb-A", plus the
// build params that would re-point it.
type triggerRepointGuardEnv struct {
	bp     *agentBuildParams
	info   sessionpkg.Info
	store  beads.Store
	stderr *bytes.Buffer
}

func newTriggerRepointGuardEnv(t *testing.T) triggerRepointGuardEnv {
	t.Helper()
	mem := beads.NewMemStore()
	created, err := mem.Create(beads.Bead{
		Title:  "worker-1",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":                          "s-worker",
			"template":                              "city/worker",
			"state":                                 string(sessionpkg.StateActive),
			beadmeta.TriggerBeadIDMetadataKey:       "wb-A",
			beadmeta.TriggerBeadStoreRefMetadataKey: "city:city",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	info, err := sessionFrontDoor(mem).Get(created.ID)
	if err != nil {
		t.Fatalf("read session info: %v", err)
	}
	cfg := &config.City{Workspace: config.Workspace{Name: "city"}}
	stderr := &bytes.Buffer{}
	bp := newAgentBuildParams("city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), mem, stderr)
	return triggerRepointGuardEnv{bp: bp, info: info, store: mem, stderr: stderr}
}

func (e triggerRepointGuardEnv) repoint(t *testing.T) sessionpkg.Info {
	t.Helper()
	bound, err := bindPoolSessionTriggerBead(e.bp, &config.Agent{Name: "worker"}, "city/worker", e.info, SessionRequest{
		WorkBeadID:   "wb-B",
		WorkStoreRef: "city:city",
	})
	if err != nil {
		t.Fatalf("bind pool session trigger bead: %v", err)
	}
	return bound
}

func (e triggerRepointGuardEnv) durableTrigger(t *testing.T) string {
	t.Helper()
	current, err := sessionFrontDoor(e.store).Get(e.info.ID)
	if err != nil {
		t.Fatalf("read current session info: %v", err)
	}
	return strings.TrimSpace(current.TriggerBeadID)
}

// TestPoolTriggerRepointSkipsAcknowledgedMember is the ga-f7v2ft.131 Commit 2
// red. The candidate is decided from a per-tick snapshot in which the member is
// still active; between that snapshot and the write it acknowledges its drain
// and the controller stamps drain-ack-stop-pending. Committing the re-point
// then re-targets a member that is already retiring, and its trigger binding is
// load-bearing on the keyed stop path — the acknowledged drain never finalizes.
func TestPoolTriggerRepointSkipsAcknowledgedMember(t *testing.T) {
	env := newTriggerRepointGuardEnv(t)

	// The keyed stop-pending transition lands after the snapshot the candidate
	// was decided on.
	if err := sessionFrontDoor(env.store).ApplyPatch(env.info.ID,
		sessionpkg.AgentDrainAckStopPendingPatch(time.Now().UTC(), env.info.ID, "token-1")); err != nil {
		t.Fatalf("mark drain-ack stop pending: %v", err)
	}

	bound := env.repoint(t)

	if got := env.durableTrigger(t); got != "wb-A" {
		t.Fatalf("durable trigger = %q, want the acknowledged %q: legacy re-pointed a member that is already retiring (ga-f7v2ft.131)", got, "wb-A")
	}
	if strings.TrimSpace(bound.TriggerBeadID) != "wb-A" {
		t.Fatalf("returned Info trigger = %q, want the unchanged %q", bound.TriggerBeadID, "wb-A")
	}
	if !strings.Contains(env.stderr.String(), "superseded") {
		t.Fatalf("stderr = %q, want a traced supersede for the skipped re-point", env.stderr.String())
	}
}

// TestPoolTriggerRepointStillRetargetsFreeMember is the negative that keeps the
// guard narrow: re-targeting a freed member onto the next ready work item is the
// intended system response (ga-f7v2ft.112 round-4 ruling 3), and only a member
// that already acknowledged its drain is exempt.
func TestPoolTriggerRepointStillRetargetsFreeMember(t *testing.T) {
	env := newTriggerRepointGuardEnv(t)

	bound := env.repoint(t)

	if got := env.durableTrigger(t); got != "wb-B" {
		t.Fatalf("durable trigger = %q, want the re-pointed %q: a free member must still re-point", got, "wb-B")
	}
	if strings.TrimSpace(bound.TriggerBeadID) != "wb-B" {
		t.Fatalf("returned Info trigger = %q, want %q", bound.TriggerBeadID, "wb-B")
	}
	if strings.Contains(env.stderr.String(), "superseded") {
		t.Fatalf("stderr = %q, want no supersede for an ordinary re-target", env.stderr.String())
	}
}

// The ga-vumr7 race fixture's vocabulary: work 1 is what the member was started
// and bound to, work 2 is the ready routed item that arrives afterwards.
const (
	repointRaceWorkOne  = "g2-249"
	repointRaceWorkTwo  = "g2-to8"
	repointRaceStoreRef = "city:test-city"
)

// routedRepointRaceEnv is the ga-vumr7 shape, serialized: one ACTIVE pool member
// that was started and BOUND to work 1 before work 2 existed, plus the legacy
// re-point that would put work 2 on it.
//
// The member's own work is still open, which is exactly what makes the keyed
// allocator refuse to reuse it (authorizeRoutedWorkPoolReuse calls it busy) and
// GROW a second member for work 2 instead. Committing the re-point therefore
// leaves TWO rows stamped with work 2, and every subsequent
// findRoutedWorkPoolSession read fails "ambiguous routed-work pool sessions".
// reusablePoolSessionInfo cannot see that: it screens the member's own row, not
// the keyed growth for the same work item.
type routedRepointRaceEnv struct {
	bp     *agentBuildParams
	cfg    *config.City
	store  beads.Store
	member sessionpkg.Info
	stderr *bytes.Buffer
}

func newRoutedRepointRaceEnv(t *testing.T) routedRepointRaceEnv {
	t.Helper()
	isolateKeyedRoutedWorkAllocations(t)
	store := beads.NewMemStore()
	cfg := poolMemberDemandCity()
	stderr := &bytes.Buffer{}
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, stderr)
	// The plan is decided from a per-tick snapshot; nothing the keyed lane did
	// after it was taken is in there.
	bp.sessionBeads = newSessionBeadSnapshot(nil)
	id, err := sessionFrontDoor(store).CreateSession(sessionpkg.CreateSpec{
		Title:     "worker",
		AgentName: "worker",
		Metadata: map[string]string{
			"template":                              "worker",
			"session_name":                          "worker-1",
			"state":                                 string(sessionpkg.StateActive),
			"pool_slot":                             "1",
			poolManagedMetadataKey:                  boolMetadata(true),
			beadmeta.TriggerBeadIDMetadataKey:       repointRaceWorkOne,
			beadmeta.TriggerBeadStoreRefMetadataKey: repointRaceStoreRef,
		},
	})
	if err != nil {
		t.Fatalf("seed bound pool member: %v", err)
	}
	member, err := sessionFrontDoor(store).Get(id)
	if err != nil {
		t.Fatalf("read bound pool member: %v", err)
	}
	return routedRepointRaceEnv{bp: bp, cfg: cfg, store: store, member: member, stderr: stderr}
}

// repointToNewWork is legacy's re-point: the snapshot still shows the member
// bound to work 1 and free, so buildDesiredState hands it the new work item.
func (e routedRepointRaceEnv) repointToNewWork(t *testing.T) sessionpkg.Info {
	t.Helper()
	bound, err := bindPoolSessionTriggerBead(e.bp, &e.cfg.Agents[0], "worker-1", e.member, SessionRequest{
		Template:     "worker",
		Tier:         "new",
		WorkBeadID:   repointRaceWorkTwo,
		WorkStoreRef: repointRaceStoreRef,
	})
	if err != nil {
		t.Fatalf("bind pool session trigger bead: %v", err)
	}
	return bound
}

func (e routedRepointRaceEnv) durableTrigger(t *testing.T) string {
	t.Helper()
	current, err := sessionFrontDoor(e.store).Get(e.member.ID)
	if err != nil {
		t.Fatalf("read current session info: %v", err)
	}
	return strings.TrimSpace(current.TriggerBeadID)
}

// claimantIDs is the exact population findRoutedWorkPoolSession reads; more than
// one of them is the ambiguity the journey observes.
func (e routedRepointRaceEnv) claimantIDs(t *testing.T) []string {
	t.Helper()
	claims := livePoolMembersForWork(t, e.store, e.cfg, repointRaceWorkTwo, repointRaceStoreRef)
	ids := make([]string, 0, len(claims))
	for _, claim := range claims {
		ids = append(ids, claim.ID)
	}
	return ids
}

// TestPoolTriggerRepointStandsDownForKeyedClaimOnSameWork is the ga-vumr7 red in
// the keyed-allocation-first ordering: the keyed lane already grew its member for
// work 2 and stamped the durable claim, and legacy's re-point commits afterwards
// off a snapshot taken before that member existed. Two rows then carry work 2 and
// the routed-work start fails with `ambiguous routed-work pool sessions`.
func TestPoolTriggerRepointStandsDownForKeyedClaimOnSameWork(t *testing.T) {
	env := newRoutedRepointRaceEnv(t)
	keyed := seedKeyedRoutedWorkPoolMember(t, env.store, repointRaceWorkTwo, repointRaceStoreRef)

	bound := env.repointToNewWork(t)

	if got := env.claimantIDs(t); len(got) != 1 || got[0] != keyed {
		t.Fatalf("claimants on %s = %v, want exactly the keyed member %q: a committed re-point makes findRoutedWorkPoolSession ambiguous (ga-vumr7)", repointRaceWorkTwo, got, keyed)
	}
	if got := env.durableTrigger(t); got != repointRaceWorkOne {
		t.Fatalf("durable trigger = %q, want the member's own %q", got, repointRaceWorkOne)
	}
	if strings.TrimSpace(bound.TriggerBeadID) != repointRaceWorkOne {
		t.Fatalf("returned Info trigger = %q, want the unchanged %q", bound.TriggerBeadID, repointRaceWorkOne)
	}
	if !strings.Contains(env.stderr.String(), "superseded") {
		t.Fatalf("stderr = %q, want a traced supersede for the skipped re-point", env.stderr.String())
	}
}

// TestPoolTriggerRepointStandsDownForKeyedAllocationReservation is the same red
// in the legacy-re-point-first ordering: the work item has entered the keyed lane
// but its member does not exist yet, so there is no durable claim to see. That
// pre-create window is the one first-creator-wins hands to legacy, and it is the
// window the reservation exists to close (the ga-f7v2ft.126 stand-down, applied
// at the trigger-binding write instead of the member create).
func TestPoolTriggerRepointStandsDownForKeyedAllocationReservation(t *testing.T) {
	env := newRoutedRepointRaceEnv(t)
	if got := env.claimantIDs(t); len(got) != 0 {
		t.Fatalf("premise broken: claimants on %s before the keyed create = %v, want none", repointRaceWorkTwo, got)
	}
	keyedRoutedWorkAllocations.reserve(
		routedWorkAllocationKeyFor(repointRaceWorkTwo, "worker", repointRaceStoreRef), time.Now())

	env.repointToNewWork(t)

	if got := env.durableTrigger(t); got != repointRaceWorkOne {
		t.Fatalf("durable trigger = %q, want the member's own %q: legacy re-pointed inside the keyed allocation window", got, repointRaceWorkOne)
	}
	// The keyed lane then materializes the member it reserved the key for.
	keyed := seedKeyedRoutedWorkPoolMember(t, env.store, repointRaceWorkTwo, repointRaceStoreRef)
	if got := env.claimantIDs(t); len(got) != 1 || got[0] != keyed {
		t.Fatalf("claimants on %s = %v, want exactly the keyed member %q", repointRaceWorkTwo, got, keyed)
	}
}

// TestPoolTriggerRepointIsNotSupersededByItsOwnClaim is the guard on the guard.
// The member itself can be the row that already claims the target work — the
// keyed lane re-binds and re-starts an existing member — and legacy's stale
// snapshot still computes a re-point patch for it. Counting that claim would make
// every such re-point supersede itself and drop the rest of the cluster
// (pack/workspace/work dir) with it.
func TestPoolTriggerRepointIsNotSupersededByItsOwnClaim(t *testing.T) {
	env := newRoutedRepointRaceEnv(t)
	// The keyed re-bind landed after the snapshot: this member now claims work 2
	// and consumes new demand while its start is in flight.
	if err := sessionFrontDoor(env.store).ApplyPatch(env.member.ID, sessionpkg.MetadataPatch{
		beadmeta.TriggerBeadIDMetadataKey: repointRaceWorkTwo,
		"state":                           string(sessionpkg.StateStartPending),
		"pending_create_claim":            "true",
	}); err != nil {
		t.Fatalf("apply keyed re-bind: %v", err)
	}
	if got := env.claimantIDs(t); len(got) != 1 || got[0] != env.member.ID {
		t.Fatalf("premise broken: claimants on %s = %v, want exactly the member itself %q", repointRaceWorkTwo, got, env.member.ID)
	}

	bound, err := bindPoolSessionTriggerBead(env.bp, &env.cfg.Agents[0], "worker-1", env.member, SessionRequest{
		Template:     "worker",
		Tier:         "new",
		WorkBeadID:   repointRaceWorkTwo,
		WorkStoreRef: repointRaceStoreRef,
		WorkPack:     "demo",
	})
	if err != nil {
		t.Fatalf("bind pool session trigger bead: %v", err)
	}
	if strings.TrimSpace(bound.Pack) != "demo" {
		t.Fatalf("returned Info pack = %q, want %q: the member's own claim must not supersede its own binding", bound.Pack, "demo")
	}
	current, err := sessionFrontDoor(env.store).Get(env.member.ID)
	if err != nil {
		t.Fatalf("read current session info: %v", err)
	}
	if strings.TrimSpace(current.Pack) != "demo" || strings.TrimSpace(current.TriggerBeadID) != repointRaceWorkTwo {
		t.Fatalf("durable row pack=%q trigger=%q, want the whole cluster committed for %q", current.Pack, current.TriggerBeadID, repointRaceWorkTwo)
	}
}

// TestPoolTriggerRepointStillRetargetsUncontendedWork keeps the widening narrow:
// with no keyed reservation and no other claimant, re-targeting a freed member
// onto the next ready item is still the intended system response.
func TestPoolTriggerRepointStillRetargetsUncontendedWork(t *testing.T) {
	env := newRoutedRepointRaceEnv(t)

	env.repointToNewWork(t)

	if got := env.durableTrigger(t); got != repointRaceWorkTwo {
		t.Fatalf("durable trigger = %q, want the re-pointed %q: an uncontended member must still re-point", got, repointRaceWorkTwo)
	}
	if got := env.claimantIDs(t); len(got) != 1 || got[0] != env.member.ID {
		t.Fatalf("claimants on %s = %v, want exactly the re-pointed member %q", repointRaceWorkTwo, got, env.member.ID)
	}
	if strings.Contains(env.stderr.String(), "superseded") {
		t.Fatalf("stderr = %q, want no supersede for an uncontended re-target", env.stderr.String())
	}
}

// TestPoolTriggerClearIsNotGatedByDraining pins the arm boundary: the guard is
// on the REASSIGN arm. Dropping a retiring member's trigger cluster is a
// release, not a re-target, and must still commit.
func TestPoolTriggerClearIsNotGatedByDraining(t *testing.T) {
	env := newTriggerRepointGuardEnv(t)
	if err := sessionFrontDoor(env.store).ApplyPatch(env.info.ID,
		sessionpkg.AgentDrainAckStopPendingPatch(time.Now().UTC(), env.info.ID, "token-1")); err != nil {
		t.Fatalf("mark drain-ack stop pending: %v", err)
	}

	if _, err := bindPoolSessionTriggerBead(env.bp, &config.Agent{Name: "worker"}, "city/worker", env.info, SessionRequest{WorkBeadID: ""}); err != nil {
		t.Fatalf("clear pool session trigger bead: %v", err)
	}

	if got := env.durableTrigger(t); got != "" {
		t.Fatalf("durable trigger = %q, want the cluster cleared", got)
	}
}
