package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// Pool members identified by a rebinding numeric slot ("rig/claude-1") must
// never carry that slot in an IDENTITY channel — not in the session bead's
// metadata["alias"], and not in the spawn env's GC_ALIAS/GC_AGENT/BEADS_ACTOR.
//
// #4981 made `gc hook --claim` prefer GC_ALIAS. That is right for named agents
// (a stable identity bd can match actor-to-assignee on) and wrong for pool
// slots, which the controller rebinds to a fresh session whenever a holder
// dies. With the slot in GC_ALIAS, pool workers claimed under a name no
// reconciler guard enumerates, so the drain guard saw no assigned work and
// drained live claim-holders ~2min in. Restoring the original unaliased-pool
// design (bc2ee15ac4) puts the claim back on the session name, which every
// guard already enumerates — no guard change needed.
//
// Slot bookkeeping does NOT ride the alias: agent_name, title, the agent:<name>
// label, pool_slot, and the canonical-instance record all still carry it, and
// nonExpandingPoolIdentitySlot reads every one of them.

func transientSlotPoolConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "claude",
			Dir:               "rig",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "printf 1",
		}},
	}
}

// TestBuildDesiredState_PoolSlotSessionBeadCarriesNoSlotAlias inverts the
// pre-fix pin (TestBuildDesiredState_NewPoolSessionBeadCreatedWithConcreteIdentity
// asserted alias == "rig/claude-1"). The slot identity must survive in every
// bookkeeping channel and in none of the identity channels.
func TestBuildDesiredState_PoolSlotSessionBeadCarriesNoSlotAlias(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), transientSlotPoolConfig(), runtime.NewFake(), store, io.Discard)

	sessionBeads, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	if len(sessionBeads) != 1 {
		t.Fatalf("session beads = %d, want 1", len(sessionBeads))
	}
	got := sessionBeads[0]

	if alias := got.Metadata["alias"]; alias != "" {
		t.Fatalf("pool slot session bead alias = %q, want empty: a rebinding slot must not be an ownership identity", alias)
	}
	// Slot accounting channels must all survive the unaliasing.
	if got.Metadata["agent_name"] != "rig/claude-1" {
		t.Fatalf("agent_name = %q, want concrete slot identity", got.Metadata["agent_name"])
	}
	if got.Metadata["pool_slot"] != "1" {
		t.Fatalf("pool_slot = %q, want 1", got.Metadata["pool_slot"])
	}
	if got.Title != "rig/claude-1" {
		t.Fatalf("title = %q, want concrete slot identity", got.Title)
	}
	if !containsString(got.Labels, "agent:rig/claude-1") {
		t.Fatalf("labels = %#v, want concrete slot agent label", got.Labels)
	}
	if got.Metadata[sessionpkg.CanonicalInstanceNameMetadata] != "rig/claude-1" {
		t.Fatalf("canonical instance record = %q, want concrete slot identity", got.Metadata[sessionpkg.CanonicalInstanceNameMetadata])
	}

	tp, ok := dsResult.State[got.Metadata["session_name"]]
	if !ok {
		t.Fatalf("desired state missing created session %q; keys=%v", got.Metadata["session_name"], mapKeys(dsResult.State))
	}
	if tp.Alias != "" {
		t.Fatalf("pool slot TemplateParams.Alias = %q, want empty", tp.Alias)
	}
	if env := tp.Env["GC_ALIAS"]; env != "" {
		t.Fatalf("pool slot GC_ALIAS = %q, want empty; template_resolve seeds the bare template here, so it must be blanked, not skipped", env)
	}
	if env := tp.Env["GC_AGENT"]; env != tp.SessionName {
		t.Fatalf("pool slot GC_AGENT = %q, want the session name %q", env, tp.SessionName)
	}
	if tp.PoolSlot != 1 {
		t.Fatalf("pool slot TemplateParams.PoolSlot = %d, want 1", tp.PoolSlot)
	}
}

// TestPoolSlotStaysUnaliasedAcrossReconcileTicks is the CROSS-TICK pin, and the
// one that matters: every other test in this file is unit-shaped and nothing in
// them crosses build -> sync -> spawn.
//
// A production tick is buildDesiredState -> syncSessionBeads -> session start
// (city_runtime.go). Creating the bead unaliased is not enough, because
// syncSessionBeads re-derives an alias of its own from tp.InstanceName /
// agent_name — both still slot-form on purpose, since the slot IS the logical
// instance name. If those fallbacks are not gated on the same predicate the
// create path uses, tick 1 writes the slot straight back onto the bead and the
// session starts from a re-aliased bead with GC_ALIAS=slot baked in. The claim
// is slot-form again and the treadmill continues, with every unit test still
// green.
//
// So this runs the real order twice and asserts the alias stays empty through
// both. skipClose=true matches the reconciler's own call.
func TestPoolSlotStaysUnaliasedAcrossReconcileTicks(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := transientSlotPoolConfig()
	clk := &clock.Fake{Time: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}

	var beadID string
	for tick := 1; tick <= 2; tick++ {
		var stderr bytes.Buffer
		dsResult := buildDesiredState("test-city", cityPath, clk.Now(), cfg, runtime.NewFake(), store, &stderr)
		syncSessionBeads(cityPath, store, dsResult.State, runtime.NewFake(), allConfiguredDS(dsResult.State), cfg, clk, &stderr, true)

		sessionBeads, err := loadSessionBeads(store)
		if err != nil {
			t.Fatalf("tick %d: load session beads: %v", tick, err)
		}
		if len(sessionBeads) != 1 {
			t.Fatalf("tick %d: session beads = %d, want 1; beads=%#v", tick, len(sessionBeads), sessionBeads)
		}
		got := sessionBeads[0]
		if beadID == "" {
			beadID = got.ID
		} else if got.ID != beadID {
			t.Fatalf("tick %d: session bead id = %q, want the tick-1 bead %q (sync minted a duplicate)", tick, got.ID, beadID)
		}

		if alias := got.Metadata["alias"]; alias != "" {
			t.Fatalf("tick %d: pool slot bead alias = %q, want empty — syncSessionBeads re-stamped the slot as an "+
				"ownership identity, so the session starts with GC_ALIAS=%s and claims slot-form again; stderr=%q",
				tick, alias, alias, stderr.String())
		}
		if history := got.Metadata["alias_history"]; strings.TrimSpace(history) != "" {
			t.Fatalf("tick %d: pool slot bead alias_history = %q, want empty", tick, history)
		}
		// The slot must still be recorded where it belongs, or the unaliasing
		// would have destroyed slot accounting instead of relocating it.
		if got.Metadata["agent_name"] != "rig/claude-1" || got.Metadata["pool_slot"] != "1" {
			t.Fatalf("tick %d: slot accounting lost: agent_name=%q pool_slot=%q",
				tick, got.Metadata["agent_name"], got.Metadata["pool_slot"])
		}

		// The env the session would actually spawn with, read through the same
		// runtime projection session start uses (it overrides tp.Env).
		info, err := sessionFrontDoor(store).Get(got.ID)
		if err != nil {
			t.Fatalf("tick %d: project session info: %v", tick, err)
		}
		env := sessionpkg.RuntimeEnvWithSessionContext(info, 1, 1, "tok")
		if env["GC_ALIAS"] != "" {
			t.Fatalf("tick %d: spawn GC_ALIAS = %q, want empty", tick, env["GC_ALIAS"])
		}
		if want := got.Metadata["session_name"]; env["BEADS_ACTOR"] != want {
			t.Fatalf("tick %d: spawn BEADS_ACTOR = %q, want the session name %q — this is the string the claim writes",
				tick, env["BEADS_ACTOR"], want)
		}
	}
}

// TestPoolSlotSessionRuntimeIdentityIsSessionName pins the end of the chain the
// claim actually reads. The spawn env is mergeEnv(tp.Env, RuntimeEnvWithSessionContext),
// so the RUNTIME projection wins — blanking tp.Env alone would be inert. With the
// bead unaliased, runtimePublicAlias falls to empty and AssigneeIdentifier falls to
// the persisted session name, which is what `gc hook --claim` then writes as the
// assignee (hookSessionAgentForQuery: GC_ALIAS -> BEADS_ACTOR -> ...).
func TestPoolSlotSessionRuntimeIdentityIsSessionName(t *testing.T) {
	const sessionName = "claude-gcg-session-x"
	info := sessiontest.SeedBead(t, beads.Bead{
		ID:     "gcg-session-x",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:rig/claude-1"},
		Metadata: map[string]string{
			"template":             "rig/claude",
			"agent_name":           "rig/claude-1",
			"session_name":         sessionName,
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})

	if got := sessionpkg.AssigneeIdentifier(info); got != sessionName {
		t.Fatalf("AssigneeIdentifier = %q, want the session name %q — this is the string BEADS_ACTOR and the claim both use", got, sessionName)
	}
	env := sessionpkg.RuntimeEnvWithSessionContext(info, 1, 1, "tok")
	if env["GC_ALIAS"] != "" {
		t.Fatalf("runtime GC_ALIAS = %q, want empty for an unaliased pool slot", env["GC_ALIAS"])
	}
	if env["BEADS_ACTOR"] != sessionName {
		t.Fatalf("runtime BEADS_ACTOR = %q, want the session name %q", env["BEADS_ACTOR"], sessionName)
	}
	if env["GC_AGENT"] != sessionName {
		t.Fatalf("runtime GC_AGENT = %q, want the session name %q", env["GC_AGENT"], sessionName)
	}
}

// TestNamedSessionRuntimeIdentityStaysAliasFirst is the #4981 / ga-i44k control:
// unaliasing pool slots must not touch named agents, whose stable alias is what
// keeps bd's actor==assignee close check passing.
func TestNamedSessionRuntimeIdentityStaysAliasFirst(t *testing.T) {
	info := sessiontest.SeedBead(t, beads.Bead{
		ID:     "gcg-session-mayor",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":                  "mayor",
			"session_name":              "mayor",
			"alias":                     "mayor",
			"configured_named_session":  "true",
			"configured_named_identity": "mayor",
		},
	})
	if got := sessionpkg.AssigneeIdentifier(info); got != "mayor" {
		t.Fatalf("named AssigneeIdentifier = %q, want alias-first \"mayor\"", got)
	}
	if env := sessionpkg.RuntimeEnvWithSessionContext(info, 1, 1, "tok"); env["GC_ALIAS"] != "mayor" || env["BEADS_ACTOR"] != "mayor" {
		t.Fatalf("named runtime identity = GC_ALIAS %q / BEADS_ACTOR %q, want both \"mayor\"", env["GC_ALIAS"], env["BEADS_ACTOR"])
	}
}

// TestCanonicalSingletonPoolKeepsStableAlias is the carve-out control. A
// max_active_sessions=1 pool member is identified by the bare agent name, which
// never rebinds and doubles as its mail/nudge address, so it stays aliased —
// only rebinding slot names are unaliased.
func TestCanonicalSingletonPoolKeepsStableAlias(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "printf 1",
		}},
	}

	buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)

	sessionBeads, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	if len(sessionBeads) != 1 {
		t.Fatalf("session beads = %d, want 1", len(sessionBeads))
	}
	if got := sessionBeads[0].Metadata["alias"]; got != "cashmaster/refinery" {
		t.Fatalf("canonical singleton alias = %q, want the stable canonical identity", got)
	}
}

// TestReleaseOrphanedPoolAssignmentsReopensStaleSlotFormClaim is the deploy
// self-heal pin. Beads still assigned slot-form when the fix lands map to no
// live session (pool beads no longer answer to slot names), so orphan release
// reopens them and a fresh worker re-claims under its session name. No manual
// bead surgery.
func TestReleaseOrphanedPoolAssignmentsReopensStaleSlotFormClaim(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "live pool session, unaliased",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":             "worker",
			"agent_name":           "worker-1",
			"session_name":         "worker-gcg-session-x",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "in-flight work claimed slot-form before the fix",
		Type:     "task",
		Status:   "open",
		Assignee: "worker-1",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	if work, err = store.Get(work.ID); err != nil {
		t.Fatalf("reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		beads.SessionStore{Store: store},
		testPoolReleaseConfig(),
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{store},
		nil,
		nil,
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s] — a stale slot-form assignee names no live session once pool beads are unaliased", released, work.ID)
	}
}
