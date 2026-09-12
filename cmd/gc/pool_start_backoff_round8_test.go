package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// The cases codex round 8 found missing or wrong (evidence 07-codex-r8.md).

// TestBindNamedSessionWakeTriggerClearsWhenNotWoken: an empty wake request
// clears both trigger keys on a retained named holder — persisted through
// the store when there is one, folded locally on a dry-run build — and a
// later request binds again.
func TestBindNamedSessionWakeTriggerClearsWhenNotWoken(t *testing.T) {
	store := beads.NewMemStore()
	holder, err := store.Create(beads.Bead{
		Title:  "holder",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":                          "solo-holder",
			"state":                                 string(session.StateActive),
			beadmeta.TriggerBeadIDMetadataKey:       "gp-parked",
			beadmeta.TriggerBeadStoreRefMetadataKey: "city",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	info := sessionInfosFromBeads([]beads.Bead{holder})[0]
	if info.TriggerBeadID != "gp-parked" {
		t.Fatalf("fixture: trigger not read back: %+v", info)
	}
	bound, err := bindNamedSessionWakeTrigger(&agentBuildParams{beadStore: store}, info, SessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if bound.TriggerBeadID != "" || bound.TriggerBeadStoreRef != "" {
		t.Fatalf("an empty request must clear the trigger, got %q/%q", bound.TriggerBeadID, bound.TriggerBeadStoreRef)
	}
	row, err := store.Get(holder.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Metadata[beadmeta.TriggerBeadIDMetadataKey] != "" || row.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey] != "" {
		t.Fatalf("the clear must persist on the holder's bead: %v", row.Metadata)
	}
	local, err := bindNamedSessionWakeTrigger(nil, info, SessionRequest{})
	if err != nil || local.TriggerBeadID != "" {
		t.Fatalf("a dry-run build folds the clear locally: %q err=%v", local.TriggerBeadID, err)
	}
	rebound, err := bindNamedSessionWakeTrigger(&agentBuildParams{beadStore: store}, bound, SessionRequest{WorkBeadID: "gp-live", WorkStoreRef: "rig:a"})
	if err != nil || rebound.TriggerBeadID != "gp-live" || rebound.TriggerBeadStoreRef != "rig:a" {
		t.Fatalf("a request binds as before: %q/%q err=%v", rebound.TriggerBeadID, rebound.TriggerBeadStoreRef, err)
	}
}

// TestBuildDesiredState_AlwaysHolderDropsTheTriggerOfParkedWork: a
// mode=always holder is desired every tick regardless of demand. While its
// work is live the trigger stays bound; once the work is parked (gated out of
// the demand) the build clears the holder's trigger, so the restarts the
// always mode makes anyway are charged to nothing and cannot lift the park.
func TestBuildDesiredState_AlwaysHolderDropsTheTriggerOfParkedWork(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "solo",
			StartCommand:      "true",
			WorkQuery:         "printf ''",
			MaxActiveSessions: intPtr(1),
			MaxStartFailures:  intPtr(1),
		}},
		NamedSessions: []config.NamedSession{{Template: "solo", Mode: "always"}},
	}
	identity := cfg.NamedSessions[0].QualifiedName()
	work, err := store.Create(beads.Bead{Title: "solo work", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "solo"}})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &identity}); err != nil {
		t.Fatal(err)
	}
	holder, err := store.Create(beads.Bead{
		Title:  "solo holder",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":                           "solo",
			"agent_name":                         identity,
			"alias":                              identity,
			"session_name":                       "solo-holder",
			"state":                              string(session.StateAsleep),
			session.NamedSessionMetadataKey:      "true",
			session.NamedSessionIdentityMetadata: identity,
			session.NamedSessionModeMetadata:     "always",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := func() (string, string) {
		row, err := store.Get(holder.ID)
		if err != nil {
			t.Fatal(err)
		}
		return row.Metadata[beadmeta.TriggerBeadIDMetadataKey], row.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]
	}
	desiredFor := func(res DesiredStateResult) TemplateParams {
		for _, tp := range res.State {
			if tp.ConfiguredNamedIdentity == identity {
				return tp
			}
		}
		t.Fatalf("no desired state for the always holder %s: %v", identity, mapKeys(res.State))
		return TemplateParams{}
	}
	// Live work: the holder is bound to it.
	res := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if live := desiredFor(res); live.TriggerBeadID != work.ID {
		t.Fatalf("control: the build's verdict rides on the retained holder's params too (workTriggerForStart), got %q", live.TriggerBeadID)
	}
	if id, _ := trigger(); id != work.ID {
		t.Fatalf("control: the holder woken for live work must carry it as its trigger, got %q", id)
	}
	// The work parks (one failed start at max_start_failures=1).
	now := time.Now().UTC().Format(time.RFC3339)
	if err := store.SetMetadataBatch(work.ID, map[string]string{
		beadmeta.ParkedAtMetadataKey:     now,
		beadmeta.ParkReasonMetadataKey:   "pre_start[0]: exit status 1",
		beadmeta.ParkFailuresMetadataKey: "1",
		beadmeta.ParkIDMetadataKey:       "deadbeefdeadbeef",
		beadmeta.ParkMailedAtMetadataKey: now,
	}); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	res = buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)
	tp := desiredFor(res) // always: still desired
	if res.NamedSessionDemand[identity] {
		t.Fatalf("a parked bead is no demand: %v", res.NamedSessionDemand)
	}
	if tp.TriggerBeadID != "" {
		t.Fatalf("the desired holder must carry no trigger for parked work, got %q", tp.TriggerBeadID)
	}
	if id, ref := trigger(); id != "" || ref != "" {
		t.Fatalf("the build must CLEAR the retained holder's trigger once its work is parked, got %q/%q\nstderr:\n%s", id, ref, stderr.String())
	}
}

// TestNamedSessionReopenTriggerMetadataClearsTheClosedBeadsTrigger: a closed
// named holder reopened for no work is reopened with both trigger keys
// cleared; reopened for work it carries that work. The create-time metadata
// stays nil for no trigger (a fresh bead has nothing to clear).
func TestNamedSessionReopenTriggerMetadataClearsTheClosedBeadsTrigger(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "solo", StartCommand: "true", MaxActiveSessions: intPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "solo", Mode: "always"}},
	}
	identity := cfg.NamedSessions[0].QualifiedName()
	spec, ok := findNamedSessionSpec(cfg, "test-city", identity)
	if !ok {
		t.Fatalf("no named spec for %s", identity)
	}
	closed, err := store.Create(beads.Bead{
		Title:  "solo holder",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":                              "solo",
			"agent_name":                            identity,
			"alias":                                 identity,
			"session_name":                          spec.SessionName,
			"state":                                 "closed",
			session.NamedSessionMetadataKey:         "true",
			session.NamedSessionIdentityMetadata:    identity,
			session.NamedSessionModeMetadata:        "always",
			beadmeta.TriggerBeadIDMetadataKey:       "gp-parked",
			beadmeta.TriggerBeadStoreRefMetadataKey: "city",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatal(err)
	}
	meta := namedSessionReopenTriggerMetadata(TemplateParams{})
	if meta[beadmeta.TriggerBeadIDMetadataKey] != "" || meta[beadmeta.TriggerBeadStoreRefMetadataKey] != "" || len(meta) != 2 {
		t.Fatalf("reopen metadata for no work must carry both keys, empty: %v", meta)
	}
	if got := namedSessionReopenTriggerMetadata(TemplateParams{TriggerBeadID: "gp-live", TriggerBeadStoreRef: "rig:a"}); got[beadmeta.TriggerBeadIDMetadataKey] != "gp-live" || got[beadmeta.TriggerBeadStoreRefMetadataKey] != "rig:a" {
		t.Fatalf("reopen metadata for work carries it: %v", got)
	}
	if namedSessionTriggerMetadata(TemplateParams{}) != nil {
		t.Fatal("create metadata for no work stays nil")
	}
	var stderr bytes.Buffer
	reopened, _, ok := reopenClosedConfiguredNamedSessionBead(cityPath, store, cfg, "test-city", identity, spec.SessionName, string(session.StateStartPending), time.Now().UTC(), meta, &stderr)
	if !ok {
		t.Fatalf("the closed holder must reopen\nstderr:\n%s", stderr.String())
	}
	row, err := store.Get(reopened.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Metadata[beadmeta.TriggerBeadIDMetadataKey] != "" || row.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey] != "" {
		t.Fatalf("the reopened holder must not inherit the closed bead's trigger: %v", row.Metadata)
	}
}

// TestPoolStartBackoffUnfencedResetLandsBehindAStaleCache: the caching
// store's SetMetadataBatch skips the backing write when the CACHED row
// already carries the patch's values. Here the cache still says "cleared"
// while four failures landed on the backing behind its back: the unfenced
// reset, computed from the live backing row, must still reach the backing
// — and the cache agrees afterwards.
func TestPoolStartBackoffUnfencedResetLandsBehindAStaleCache(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1), MaxStartFailures: intPtr(5)}}}
	backing := beads.NewMemStore()
	caching := beads.NewCachingStoreForTest(backing, nil)
	work, err := caching.Create(beads.Bead{Title: "cached work", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := caching.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	four := map[string]string{beadmeta.StartFailuresMetadataKey: "4", beadmeta.StartFailedAtMetadataKey: time.Now().UTC().Format(time.RFC3339), beadmeta.StartFailureMetadataKey: "boom"}
	if err := backing.SetMetadataBatch(work.ID, four); err != nil {
		t.Fatal(err)
	}
	wrapped := &beadPolicyStore{Store: caching, cfg: cfg}
	if cached, _ := wrapped.Get(work.ID); readWorkStartFailureState(cached.Metadata).Failures != 0 {
		t.Fatalf("fixture: the cache must still serve the cleared row, got %v", cached.Metadata)
	}
	// Control: the cache's guard swallows the clear on its own.
	if err := caching.SetMetadataBatch(work.ID, workStartFailureClearPatch(four)); err != nil {
		t.Fatal(err)
	}
	if row, _ := backing.Get(work.ID); readWorkStartFailureState(row.Metadata).Failures != 4 {
		t.Fatalf("control: a write through the cache is skipped as already satisfied, backing=%v", row.Metadata)
	}
	var stderr bytes.Buffer
	policy := &workStartFailurePolicy{
		workStore:         wrapped,
		templateOf:        func(b beads.Bead) string { return poolTemplateForWorkBead(cfg, b) },
		canonicalTemplate: func(template string) string { return normalizeAgentTemplateIdentity(cfg, template) },
		limitFor:          func(string) int { return 5 },
		resolveWriter:     func(beads.Store) (beads.ConditionalWriter, error) { return nil, nil }, // unfenced
		stderr:            &stderr,
	}
	policy.recordStartSuccess(workTrigger{BeadID: work.ID, StoreRef: "city"}, "worker")
	row, err := backing.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(workStartFailureClearPatch(row.Metadata)) != 0 {
		t.Fatalf("the unfenced reset must land on the backing behind a stale cache: %v\nstderr:\n%s", row.Metadata, stderr.String())
	}
	if !strings.Contains(stderr.String(), "record cleared") {
		t.Fatalf("the clear is reported once it landed:\n%s", stderr.String())
	}
	if cached, _ := wrapped.Get(work.ID); readWorkStartFailureState(cached.Metadata).Failures != 0 {
		t.Fatalf("the cache agrees with the backing afterwards: %v", cached.Metadata)
	}
	// The other direction — the cached row differs from the patch — writes
	// through the cache, and the cache refreshes with it.
	if err := backing.SetMetadataBatch(work.ID, four); err != nil {
		t.Fatal(err)
	}
	if err := caching.SetMetadataBatch(work.ID, four); err != nil { // cache now says four too
		t.Fatal(err)
	}
	policy.recordStartFailure(workTrigger{BeadID: work.ID, StoreRef: "city"}, "worker", errPreStartFailure, time.Now().UTC())
	row, _ = backing.Get(work.ID)
	cached, _ := wrapped.Get(work.ID)
	if readWorkStartFailureState(row.Metadata).ParkFailures != 5 || readWorkStartFailureState(cached.Metadata).ParkFailures != 5 {
		t.Fatalf("a fifth failure parks on the backing (%v) and the cache follows (%v)", row.Metadata, cached.Metadata)
	}
}

// TestLegacyParkReceiptsCarryTheBeadID: two parks written before gc.park_id
// existed, on different beads in the same second, must not share a receipt:
// the mail that landed for one must not stamp the other as mailed.
func TestLegacyParkReceiptsCarryTheBeadID(t *testing.T) {
	at := time.Date(2026, 9, 12, 2, 0, 0, 0, time.UTC)
	legacy := readWorkStartFailureState(map[string]string{beadmeta.ParkedAtMetadataKey: at.Format(time.RFC3339), beadmeta.ParkFailuresMetadataKey: "5", beadmeta.ParkReasonMetadataKey: "boom"})
	a, b := legacy.parkIdentity("gp-a"), legacy.parkIdentity("gp-b")
	if a == "" || a == b || a != legacy.parkIdentity("gp-a") {
		t.Fatalf("legacy identities must be per bead and stable: %q vs %q", a, b)
	}
	if !strings.Contains((parkedWorkNotice{ParkID: a}).Tag(), "gp-a") {
		t.Fatalf("the receipt tag names the bead: %q", (parkedWorkNotice{ParkID: a}).Tag())
	}
	withID := readWorkStartFailureState(map[string]string{beadmeta.ParkedAtMetadataKey: at.Format(time.RFC3339), beadmeta.ParkIDMetadataKey: "cafe"})
	if withID.parkIdentity("gp-a") != "cafe" || withID.parkIdentity("gp-b") != "cafe" {
		t.Fatal("a park with gc.park_id is identified by it alone")
	}
	if (workStartFailureState{}).parkIdentity("gp-a") != "" {
		t.Fatal("no park, no identity")
	}
	// Through the mailer: A's landed mail is found by A's tag only.
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
	store := beads.NewMemStore()
	park := map[string]string{beadmeta.RoutedToMetadataKey: "worker", beadmeta.ParkedAtMetadataKey: at.Format(time.RFC3339), beadmeta.ParkFailuresMetadataKey: "5", beadmeta.ParkReasonMetadataKey: "boom"}
	var ids []string
	for _, title := range []string{"legacy park A", "legacy park B"} {
		wb, err := store.Create(beads.Bead{Title: title, Type: "task", Metadata: park})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, wb.ID)
	}
	landed := map[string]bool{}
	var mails []string
	var stderr bytes.Buffer
	policy := &workStartFailurePolicy{
		workStore:         store,
		templateOf:        func(b beads.Bead) string { return poolTemplateForWorkBead(cfg, b) },
		canonicalTemplate: func(template string) string { return normalizeAgentTemplateIdentity(cfg, template) },
		limitFor:          func(string) int { return 5 },
		notify:            func(n parkedWorkNotice) error { mails = append(mails, n.BeadID); landed[n.Tag()] = true; return nil },
		lookup:            func(n parkedWorkNotice) (bool, error) { return landed[n.Tag()], nil },
		stderr:            &stderr,
	}
	for _, id := range ids {
		policy.mailPark(store, id, "worker", true)
	}
	if strings.Join(mails, ",") != strings.Join(ids, ",") {
		t.Fatalf("each legacy park gets its own mail, got %v for %v\nstderr:\n%s", mails, ids, stderr.String())
	}
	for _, id := range ids {
		row, _ := store.Get(id)
		if readWorkStartFailureState(row.Metadata).ParkMailedAt.IsZero() {
			t.Fatalf("park %s must be stamped by its own mail: %v", id, row.Metadata)
		}
	}
}

// TestRecoverRunningPendingCreate_ClearsTheTriggerBeadRecord: the recovery
// path's confirmed start clears the trigger work bead's record like the
// ordinary commit does (a nil policy stays a no-op).
func TestRecoverRunningPendingCreate_ClearsTheTriggerBeadRecord(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "helper", MaxActiveSessions: intPtr(1)}}}
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{Title: "helper work", Type: "task", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey:          "helper",
		beadmeta.StartFailuresMetadataKey:     "4",
		beadmeta.StartFailedAtMetadataKey:     "2026-09-12T02:00:00Z",
		beadmeta.StartFailureMetadataKey:      "boom",
		beadmeta.StartBackoffUntilMetadataKey: "2026-09-12T02:01:20Z",
	}})
	if err != nil {
		t.Fatal(err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":                    "sky",
			"pending_create_claim":            "true",
			"state":                           "active",
			"state_reason":                    "creation_complete",
			beadmeta.TriggerBeadIDMetadataKey: work.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tp := TemplateParams{SessionName: "sky", TemplateName: "helper"}
	clkTime := time.Date(2026, 3, 18, 12, 0, 1, 0, time.UTC)
	var stderr bytes.Buffer
	policy := &workStartFailurePolicy{
		workStore:         store,
		templateOf:        func(b beads.Bead) string { return poolTemplateForWorkBead(cfg, b) },
		canonicalTemplate: func(template string) string { return normalizeAgentTemplateIdentity(cfg, template) },
		limitFor:          func(string) int { return 5 },
		stderr:            &stderr,
	}
	if ok, _ := recoverRunningPendingCreate(sessiontest.SeedBead(t, bead), tp, cfg, store, &clock.Fake{Time: clkTime}, nil, policy); !ok {
		t.Fatal("recoverRunningPendingCreate returned false, want true")
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(workStartFailureClearPatch(got.Metadata)) != 0 {
		t.Fatalf("recovery's confirmed start must clear the trigger bead's record: %v\nstderr:\n%s", got.Metadata, stderr.String())
	}
	healed, _ := store.Get(bead.ID)
	if healed.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared", healed.Metadata["pending_create_claim"])
	}
}
