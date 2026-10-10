package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The start effect's verb section and the adopt (CONTRACT v5 S1, S2, O1, O4;
// plan C5a1) on the effect test kit: row gc-1 (runtime name s-gc-1) at
// generation 3 holding token tok, the simulator's provider behind a recording
// leaf, the production transaction.

// startKit is the kit over row gc-1 in state, at generation 3 with token tok
// and meta over that, running kind's spec.
func startKit(t *testing.T, kind, state string, meta ...string) *txKit {
	t.Helper()
	row := poolRow("gc-1", "worker", 1, state, append([]string{"generation", "3", "instance_token", "tok"}, meta...)...)
	m := beads.NewMemStoreFrom(0, []beads.Bead{row}, nil)
	k := newTxKitOn(t, m, simBacking{m})
	k.it.Kind = kind
	return k
}

// runtimeAs puts a runtime under s-gc-1 carrying id and token at epoch 3,
// then marks it per mark (a corpse, a zombie).
func (k *txKit) runtimeAs(id, token string, mark func(*simRuntime)) {
	k.sp.mu.Lock()
	defer k.sp.mu.Unlock()
	k.sp.put("s-gc-1", id, "3", token)
	if mark != nil {
		mark(k.sp.rts["s-gc-1"])
	}
}

// runKind runs the kit's intent under its kind's production spec.
func (k *txKit) runKind() settlement {
	return k.run(context.Background(), effectSpecs[k.it.Kind])
}

// Kills a Launch over an alive runtime or a corpse, an adopt that launches,
// and a commit on any verdict but Current: one case per row of v5 S1's
// table, over the verb section's Decide.
func TestStartResolutionTable(t *testing.T) {
	k := startKit(t, intentAdopt, "creating")
	row := k.p.World.Census.Rows[k.it.Key].Info
	prepared := &preparedStart{coreHash: "h"}
	current := runtimeIdentity{Known: true, SessionID: "gc-1", Epoch: "3", Token: "tok"}
	for _, c := range []struct {
		name   string
		adopt  bool
		state  string
		rt     txRuntime
		refuse string
		write  bool
		done   bool
	}{
		{"unsupported", false, "creating", txRuntime{Class: rtUnsupported}, causeLivenessUnsupported, false, false},
		{"unknown", false, "creating", txRuntime{Class: rtUnknown}, causeLivenessUnknown, false, false},
		{"absent launches", false, "active", txRuntime{Class: rtAbsent}, "", false, false},
		{"absent never adopts", true, "creating", txRuntime{Class: rtAbsent}, causeNotPresent, false, false},
		{"corpse", false, "creating", txRuntime{Class: rtCorpse, Identity: current}, causeDead, false, false},
		{"zombie", true, "creating", txRuntime{Class: rtZombie, Identity: current}, causeDead, false, false},
		{"alive, another object", false, "creating", txRuntime{Class: rtAlive, Identity: current}, causeLivenessUnknown, false, false},
		{"current committed", false, "active", txRuntime{Class: rtAlive, Same: true, Identity: current}, "", false, true},
		{"current uncommitted commits", true, "creating", txRuntime{Class: rtAlive, Same: true, Identity: current}, "", true, true},
		{"stale self", false, "creating", txRuntime{Class: rtAlive, Same: true, Identity: runtimeIdentity{Known: true, SessionID: "gc-1", Epoch: "2", Token: "old"}}, causeTokenDrift, false, false},
		{"newer self", false, "creating", txRuntime{Class: rtAlive, Same: true, Identity: runtimeIdentity{Known: true, SessionID: "gc-1", Epoch: "9", Token: "new"}}, causeNewerSelf, false, false},
		{"foreign", false, "creating", txRuntime{Class: rtAlive, Same: true, Identity: runtimeIdentity{Known: true, SessionID: "gc-2", Token: "theirs"}}, causeOccupied, false, false},
		{"ownerless", true, "creating", txRuntime{Class: rtAlive, Same: true, Identity: runtimeIdentity{Known: true}}, causeAttribution, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, v := row, txView{World: k.p.World, RT: &c.rt, Now: gatherNow}
			r.MetadataState = c.state
			v.Row = r
			step := verbStep(c.adopt)(v, prepared, nil)
			if step.Refuse != c.refuse || (len(step.Write) > 0) != c.write || step.Done != c.done {
				t.Fatalf("step %+v, want refuse %q, write %v, done %v", step, c.refuse, c.write, c.done)
			}
			if c.done && (step.Facts.Noted == nil || step.Facts.Noted.Name != "s-gc-1") {
				t.Fatalf("a runtime alive and Current is not noted: %+v", step.Facts)
			}
		})
	}
}

// Kills an adopt that skips the identity read, records session.woke (legacy's
// heal records none), or lets its runtime's token drift: an uncommitted row
// whose runtime is alive with its token commits active with the start's
// hashes, notes the read at its since, and records no event.
func TestAdoptCommitsAliveCurrentUncommittedRow(t *testing.T) {
	k := startKit(t, intentAdopt, "creating", "pending_create_claim", "true", "sleep_reason", "idle")
	k.runtimeAs("gc-1", "tok", nil)
	k.p.Clock.(*fakePlannerClock).Advance(7 * time.Second) // the read's since is not the pass's
	since := k.p.Clock.Now()
	s := k.runKind()
	if s.Outcome != settledLanded || len(s.Facts.Events) != 0 || s.Facts.Noted == nil || !s.Facts.Noted.At.Equal(since) {
		t.Fatalf("settlement %+v, want landed, noted at the read's since, with no event", s)
	}
	if k.meta("state") != "active" || k.meta("pending_create_claim") != "" || k.meta("started_config_hash") == "" || k.meta("instance_token") != "tok" || k.meta("sleep_reason") != "" {
		t.Fatalf("row after the adopt: state %q claim %q hash %q token %q sleep_reason %q", k.meta("state"), k.meta("pending_create_claim"), k.meta("started_config_hash"), k.meta("instance_token"), k.meta("sleep_reason"))
	}
}

// Kills the committed-row no-op decided before the identity verdict, a
// committed row read as active only, a failed prepare settled as a no-op,
// and a Note stamped at the pass's time (the C5a1-1 max review): over
// committed rows, S1's identity verdicts refuse; a Current one is a no-op
// noted at the attempt's since; a creating one whose prepare failed fails.
func TestVerbStepJudgesCommittedRowsByIdentityFirst(t *testing.T) {
	k := startKit(t, intentAdopt, "creating")
	row := k.p.World.Census.Rows[k.it.Key].Info
	current := runtimeIdentity{Known: true, SessionID: "gc-1", Epoch: "3", Token: "tok"}
	since := k.p.World.Now.Add(7 * time.Second)
	prepErr := errors.New("prepare failed")
	for _, c := range []struct {
		name     string
		adopt    bool
		state    string
		id       runtimeIdentity
		prepared *preparedStart
		refuse   string
		fail     string
		done     bool
	}{
		{"active, stale self", false, "active", runtimeIdentity{Known: true, SessionID: "gc-1", Epoch: "2", Token: "old"}, &preparedStart{}, causeTokenDrift, "", false},
		{"active, newer self", true, "active", runtimeIdentity{Known: true, SessionID: "gc-1", Epoch: "9", Token: "new"}, &preparedStart{}, causeNewerSelf, "", false},
		{"awake, foreign", false, "awake", runtimeIdentity{Known: true, SessionID: "gc-2", Token: "theirs"}, &preparedStart{}, causeOccupied, "", false},
		{"active, ownerless", true, "active", runtimeIdentity{Known: true}, &preparedStart{}, causeAttribution, "", false},
		{"awake, current", true, "awake", current, &preparedStart{}, "", "", true},
		{"creating, current, prepare failed", true, "creating", current, nil, "", causePrepare, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := row
			r.MetadataState = c.state
			v := txView{World: k.p.World, RT: &txRuntime{Class: rtAlive, Same: true, Identity: c.id}, Now: since, Row: r}
			var err error
			if c.prepared == nil {
				err = prepErr
			}
			step := verbStep(c.adopt)(v, c.prepared, err)
			if step.Refuse != c.refuse || step.Fail != c.fail || step.Done != c.done || len(step.Write) > 0 {
				t.Fatalf("step %+v, want refuse %q fail %q done %v and no write", step, c.refuse, c.fail, c.done)
			}
			if c.done && (step.Facts.Noted == nil || !step.Facts.Noted.At.Equal(since)) {
				t.Fatalf("noted %+v, want the attempt's since %v", step.Facts.Noted, since)
			}
		})
	}
}

// Kills an adopt that reads every adoption as a first start (SESS-542): its
// prepare probes the key's transcript read-only, so a launch that already
// created its conversation is adopted with no priming stamp, and only one
// whose transcript is gone is primed.
func TestAdoptProbesTheTranscript(t *testing.T) {
	for _, present := range []bool{true, false} {
		stubTranscript(t, present)
		k := startKit(t, intentAdopt, "creating", "session_key", "k-1")
		k.runtimeAs("gc-1", "tok", nil)
		row := k.p.World.Census.Rows[k.it.Key]
		tp := TemplateParams{
			TemplateName: "worker", Command: "agent", Prompt: "hello", WorkDir: t.TempDir(),
			ResolvedProvider: &config.ResolvedProvider{Name: "claude", SessionIDFlag: "--session-id", ResumeFlag: "--resume", ResumeStyle: "flag", PromptMode: "arg"},
		}
		k.p.World.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: tp}}}
		if s := k.runKind(); s.Outcome != settledLanded {
			t.Fatalf("transcript present %t: settlement %+v, want landed", present, s)
		}
		if primed := k.meta(session.PrimedAtMetadataKey) != ""; primed == present {
			t.Errorf("transcript present %t: primed_at %q; want priming only on a first start", present, k.meta(session.PrimedAtMetadataKey))
		}
	}
}

// Kills Note's wiring dropped from the host (O4): bindHost hands the planner
// the host's observation cache.
func TestBindHostWiresTheNoteCache(t *testing.T) {
	cache := NewObservationCache(&clock.Fake{Time: gatherNow}, time.Minute, "e1")
	rt := newDefaultPlanner(io.Discard)
	rt.bindHost(plannerHost{gather: gatherEnv{CityPath: t.TempDir(), Observations: func() *ObservationCache { return cache }}})
	if rt.planner.observations == nil || rt.planner.observations() != cache {
		t.Fatal("bindHost dropped the observation cache the planner notes on")
	}
}

// Kills committing a runtime that does not carry the row's token (I3), and
// a read taken as alive when it is not: another token of the row, another
// row's runtime, no identity, a corpse, a zombie, a failed read and nothing
// present each refuse, and the row is not written.
func TestAdoptRefusesWhatItCannotCommit(t *testing.T) {
	for _, c := range []struct {
		name, id, token, cause string
		mark                   func(*simRuntime)
	}{
		{"stale self", "gc-1", "older", causeTokenDrift, nil},
		{"foreign", "gc-2", "theirs", causeOccupied, nil},
		{"ownerless", "", "", causeAttribution, nil},
		{"corpse", "gc-1", "tok", causeDead, func(r *simRuntime) { r.corpse = true }},
		{"zombie", "gc-1", "tok", causeDead, func(r *simRuntime) { r.zombie = true }},
		{"probe error", "gc-1", "tok", causeLivenessUnknown, func(r *simRuntime) { r.probeErr = true }},
		{"absent", "-", "", causeNotPresent, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := startKit(t, intentAdopt, "creating")
			if c.id != "-" {
				k.runtimeAs(c.id, c.token, c.mark)
			}
			if s := k.runKind(); s.Outcome != settledRefused || s.Cause != c.cause {
				t.Fatalf("settlement %+v, want refused %s", s, c.cause)
			}
			if k.meta("state") != "creating" || k.meta("started_config_hash") != "" {
				t.Fatal("a refused adopt wrote the row")
			}
		})
	}
}

// Kills an adopt over a row that moved after the pass: `gc session kill`'s
// fence landing before the attempt's row read refuses with the premise, and
// nothing commits.
func TestAdoptRefusesAKillFenceAfterThePass(t *testing.T) {
	k := startKit(t, intentAdopt, "creating")
	k.runtimeAs("gc-1", "tok", nil)
	k.on(seamAfterReads, func() { k.outside("state", "asleep", "sleep_reason", "killed") })
	if s := k.runKind(); s.Outcome != settledRefused || s.Cause != causePremise || k.meta("started_config_hash") != "" {
		t.Fatalf("settlement %+v, want the premise refusal and nothing committed", s)
	}
}

// Kills a launch or commit over a runtime alive and Current on a committed
// row: the adopt is a no-op noting the read, and writes nothing.
func TestAdoptOverCommittedRowIsANoop(t *testing.T) {
	k := startKit(t, intentAdopt, "active")
	k.runtimeAs("gc-1", "tok", nil)
	if s := k.runKind(); s.Outcome != settledNoop || s.Cause != causeAlreadyRunning || s.Facts.Noted == nil || k.meta("started_config_hash") != "" {
		t.Fatalf("settlement %+v, want a noted no-op", s)
	}
}

// Kills presence read on the composite paired with identity read on the
// leaf: with the routed backend gone and the other backend alive, a sidecar
// left on the leaf carrying the row's token is not taken as the row's
// runtime, nor the gone leaf as absence; the adopt refuses.
func TestAdoptOverAStaleRouteRefuses(t *testing.T) {
	k := startKit(t, intentAdopt, "creating")
	other := newSimProvider()
	other.mu.Lock()
	other.put("s-gc-1", "gc-2", "1", "theirs")
	other.mu.Unlock()
	k.p.Runtime = fallThrough{recordingLeaf: k.leaf, other: other}
	if s := k.runKind(); s.Outcome != settledRefused || s.Cause != causeLivenessUnknown || k.meta("started_config_hash") != "" {
		t.Fatalf("settlement %+v, want refused liveness-unknown and nothing committed", s)
	}
}

// Kills a second start proposed while the inventory lags a commit (scenario
// R36, I2; v5 O4): the planner notes the effect's fresh read at its issue
// time, a pass that began before that read cannot conclude the name gone
// nor its process dead, and a pass that began after it decides either way.
func TestJustStartedRowNotReproposedWhileInventoryLags(t *testing.T) {
	c := observeRows(t, map[string]string{"gc-1": "s1"})
	cache := newObserveCache()
	cache.publish(censusNow, nil, completeBackend("tmux"))
	p := newPlanner(newFakePlannerClock(plannerT0), func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, io.Discard)
	p.observations = func() *ObservationCache { return cache.ObservationCache }
	read := censusNow.Add(2 * time.Second)
	p.applyFacts(effectFacts{Noted: &notedRuntime{Name: "s1", At: read}}, read.Add(time.Hour))
	for _, kind := range []FactKind{FactListed, FactRunning, FactProcessAlive} {
		obs := cache.Snapshot().ByName["s1"]
		if f := obs.fact(kind); f.Value != ObsYes || !f.ObservedAt.Equal(read) {
			t.Fatalf("fact %d noted as %+v, want Yes at the read", kind, *f)
		}
	}
	lagging := cache.publish(read.Add(-time.Second), nil, completeBackend("tmux"))
	if got := observed(t, lagging, c, read, "gc-1"); got.Liveness.startCandidate() {
		t.Fatalf("a pass that began before the read: %+v, want no start candidate", got)
	}
	// A pass that began before the read and listed the name over dead panes
	// (the runtime the start replaced) does not override the read either.
	dead := map[string]InventoryAttrs{"s1": {DeadKnown: true, AllPanesDead: true}}
	listed := cache.publish(read.Add(-time.Second), dead, completeBackend("tmux", "s1"))
	obs := listed.ByName["s1"]
	for _, kind := range []FactKind{FactRunning, FactProcessAlive} {
		if f := obs.fact(kind); f.Value != ObsYes || !f.ObservedAt.Equal(read) {
			t.Fatalf("fact %d after a lagging listing: %+v, want the read's Yes", kind, *f)
		}
	}
	later := cache.publish(read.Add(time.Second), nil, completeBackend("tmux"))
	if got := observed(t, later, c, read.Add(time.Second), "gc-1"); got.Liveness != livenessGone {
		t.Fatalf("a pass that began after the read: %+v, want gone", got)
	}
}

// Kills the transcript option leaking into legacy: legacy's prepare still
// mints a key through the store; with the option, prepare writes nothing.
func TestPrepareNilTranscriptOptionIsLegacy(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		store := beads.NewMemStore()
		b, err := store.Create(sessionRow("a", "template", "worker", "session_name", "s-a", "instance_token", "tok"))
		if err != nil {
			t.Fatal(err)
		}
		info, err := sessionFrontDoor(store).Get(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		tp := TemplateParams{TemplateName: "worker", Command: "agent", ResolvedProvider: &config.ResolvedProvider{SessionIDFlag: "--session-id"}}
		candidate := startCandidate{info: info, tp: tp}
		if legacy {
			_, _, err = buildPreparedStartWithWorkDirResolver(candidate, "", &config.City{}, store, nil, dispatchOptionSources{})
		} else {
			_, _, err = buildPreparedStartWithTranscript(candidate, "", &config.City{}, store, nil, dispatchOptionSources{}, new(sessTranscriptState))
		}
		if err != nil {
			t.Fatal(err)
		}
		after, _ := sessionFrontDoor(store).Get(b.ID)
		if minted := after.SessionKey != ""; minted != legacy {
			t.Fatalf("legacy %v: minted=%v, want only legacy to mint", legacy, minted)
		}
	}
}

// stateWriters are the functions outside the planner that write a row's
// state key, or a key named state, each with its class (SC R1): the commit
// premise (open, this token, creating/active/awake) refuses a start whenever
// one of them moves a row off those states, which only operator lifecycle
// verbs should do.
var stateWriters = map[string]string{
	"cmd/gc/city_runtime.go:CityRuntime.recordReconcileTraceInputs":           "trace payload, not a row",
	"cmd/gc/city_runtime.go:CityRuntime.recordReconcileTraceResults":          "trace payload, not a row",
	"cmd/gc/cmd_registry_auth.go:registryBrowserLogin":                        "OAuth state, not a row",
	"cmd/gc/cmd_session_wake.go:doSessionWake":                                "operator: gc session wake",
	"cmd/gc/endpoint_capacity.go:endpointCapacityGuard.recordTick":            "trace payload, not a row",
	"cmd/gc/session_bead_cycle.go:cycleAliveSessionForFreshReassign":          "legacy reconciler",
	"cmd/gc/session_beads.go:reopenNamedSessionBatch":                         "legacy reconciler",
	"cmd/gc/session_identity.go:desiredSessionIdentity":                       "create's initial metadata",
	"cmd/gc/session_index.go:sessionIndex.occupancy":                          "read model, not a write",
	"cmd/gc/session_lifecycle_parallel.go:executePlannedStartsTraced":         "legacy reconciler (trace payload)",
	"cmd/gc/session_name_lookup.go:createPoolSessionBeadWithIdentifiers":      "create's initial metadata",
	"cmd/gc/session_reconcile.go:healStatePatchWithRollbackInfo":              "legacy reconciler",
	"cmd/gc/session_reconcile.go:providerTerminalErrorPatch":                  "patch builder (C5a2's abandon)",
	"cmd/gc/session_reconciler.go:reconcileSessionBeadsTracedWithNamedDemand": "legacy reconciler",
	"internal/session/chat.go:Manager.confirmLiveSessionState":                "Manager: CLI/API start confirms active",
	"internal/session/holds.go:holdMeta":                                      "read model, not a write",
	"internal/session/resume_user_hold.go:consumeUserHoldPatch":               "patch builder (operator resume consumes a user hold)",
	"internal/session/lifecycle_exits.go:RateLimitQuarantinePatch":            "patch builder",
	"internal/session/lifecycle_transition.go:AcknowledgeDrainPatch":          "patch builder",
	"internal/session/lifecycle_transition.go:ArchivePatch":                   "patch builder",
	"internal/session/lifecycle_transition.go:BeginDrainPatch":                "patch builder",
	"internal/session/lifecycle_transition.go:ClearWakeBlockersPatch":         "patch builder",
	"internal/session/lifecycle_transition.go:ClosePatch":                     "patch builder",
	"internal/session/lifecycle_transition.go:CommitStartedPatch":             "patch builder (S2's commit)",
	"internal/session/lifecycle_transition.go:ConfigDriftResetPatch":          "patch builder",
	"internal/session/lifecycle_transition.go:ConfirmStartedPatch":            "patch builder",
	"internal/session/lifecycle_transition.go:PreWakePatch":                   "patch builder (S1's PreWake)",
	"internal/session/lifecycle_transition.go:OperatorSuspendPatch":           "patch builder (operator suspend)",
	"internal/session/lifecycle_transition.go:QuarantinePatch":                "patch builder",
	"internal/session/lifecycle_transition.go:ReactivatePatch":                "patch builder",
	"internal/session/lifecycle_transition.go:RequestWakePatch":               "patch builder",
	"internal/session/lifecycle_transition.go:SleepPatch":                     "patch builder (gc session kill, idle sleep)",
	"internal/session/manager.go:Manager.createBeadOnly":                      "create's initial metadata",
	"internal/session/manager.go:Manager.createStarted":                       "create's initial metadata",
	"internal/session/manager.go:Manager.suspend":                             "operator: suspend",
	"internal/session/store.go:Store.SetState":                                "operator verbs' blind setter",
	"internal/session/wait_store.go:Store.CancelWait":                         "wait bead, not a row",
	"internal/session/wait_store.go:Store.CloseWaitFromNudge":                 "wait bead, not a row",
	"internal/session/wait_store.go:Store.CreateWait":                         "wait bead, not a row",
	"internal/session/wait_store.go:Store.ExpireWait":                         "wait bead, not a row",
	"internal/session/wait_store.go:Store.FailWait":                           "wait bead, not a row",
	"internal/session/wait_store.go:Store.FailWaitFromNudge":                  "wait bead, not a row",
	"internal/session/wait_store.go:Store.MarkWaitReady":                      "wait bead, not a row",
	"internal/session/wait_store.go:Store.MarkWaitReadyForRedelivery":         "wait bead, not a row",
	"internal/session/wait_store.go:Store.RetryClosedWait":                    "wait bead, not a row",
	"internal/session/wait_store.go:Store.cancelWaitsAndCollectNudgeIDs":      "wait bead, not a row",
}

// Kills an unclassified writer of state outside the planner (SC R1): every
// function in the CLI, API, Manager and legacy reconciler that writes a
// "state" key, by map literal, index assignment, SetMetadata or SetMarker,
// or calls SetState, is in stateWriters, and every entry still writes.
func TestStateWritersOutsideThePlannerArePinned(t *testing.T) {
	found := make(map[string]bool)
	isState := func(e ast.Expr) bool { b, ok := e.(*ast.BasicLit); return ok && b.Value == `"state"` }
	root := repoRootForLint(t)
	for _, dir := range []string{"cmd/gc", "internal/session", "internal/api", "internal/worker"} {
		files, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no sources under %s (%v)", dir, err)
		}
		for _, path := range files {
			rel, base := dir+"/"+filepath.Base(path), filepath.Base(path)
			if strings.HasSuffix(base, "_test.go") || dir == "cmd/gc" && (strings.HasPrefix(base, "reconcile_") || strings.HasPrefix(base, "allocator_")) {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				ast.Inspect(fn, func(n ast.Node) bool {
					hit := false
					switch n := n.(type) {
					case *ast.KeyValueExpr:
						hit = isState(n.Key)
					case *ast.AssignStmt:
						for _, l := range n.Lhs {
							ix, ok := l.(*ast.IndexExpr)
							hit = hit || ok && isState(ix.Index)
						}
					case *ast.CallExpr:
						if sel, ok := n.Fun.(*ast.SelectorExpr); ok && len(n.Args) >= 2 {
							hit = sel.Sel.Name == "SetState" || (sel.Sel.Name == "SetMetadata" || sel.Sel.Name == "SetMarker") && isState(n.Args[1])
						}
					}
					if hit {
						found[rel+":"+funcDeclName(fn)] = true
					}
					return true
				})
			}
		}
	}
	for w := range found {
		if _, ok := stateWriters[w]; !ok {
			t.Errorf("unclassified writer of state: %s (classify it in stateWriters; the start commit's premise depends on it)", w)
		}
	}
	for w := range stateWriters {
		if !found[w] {
			t.Errorf("stateWriters lists %s, which no longer writes state: drop it", w)
		}
	}
}

// concretePoolKit is the kit over a concrete pool row (alias
// gastown.capable of template gastown.polecat) in state whose work_dir still
// names its template's directory, the pass's config and template naming the
// concrete one; it returns the kit and both directories.
func concretePoolKit(t *testing.T, kind, state string) (k *txKit, templateDir, concreteDir string) {
	t.Helper()
	city := t.TempDir()
	rigPath := filepath.Join(city, "repos", "aot-mobile")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	templateDir = filepath.Join(city, ".gc", "worktrees", "aot-mobile", "polecats", "gastown.polecat")
	concreteDir = filepath.Join(city, ".gc", "worktrees", "aot-mobile", "polecats", "gastown.capable")
	k = startKit(t, kind, state, "template", "aot-mobile/gastown.polecat", "agent_name", "aot-mobile/gastown.capable",
		"alias", "aot-mobile/gastown.capable", beadmeta.WorkDirMetadataKey, templateDir, beadmeta.LegacyWorkDirMetadataKey, templateDir)
	k.p.World.CityPath = city
	k.p.World.Env.Cfg = &config.City{
		Workspace: config.Workspace{Name: "aot"}, Rigs: []config.Rig{{Name: "aot-mobile", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "gastown.polecat", Dir: "aot-mobile", Provider: "test-agent", StartCommand: "true",
			WorkDir: ".gc/worktrees/{{.Rig}}/polecats/{{.AgentBase}}", NamepoolNames: []string{"gastown.capable"},
		}},
	}
	row := k.p.World.Census.Rows[k.it.Key]
	tp := TemplateParams{TemplateName: "aot-mobile/gastown.polecat", InstanceName: "aot-mobile/gastown.capable", SessionName: "s-gc-1", WorkDir: concreteDir}
	k.p.World.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: tp}}}
	return k, templateDir, concreteDir
}

// Kills a prepare that writes (S-1): an adopt over a concrete pool row whose
// work_dir needs repair commits, its prepare folding the repair in memory
// only (the read-only stores would fail a write), and leaves the work_dir to
// the row-metadata verb.
func TestAdoptPrepareWritesNothing(t *testing.T) {
	k, templateDir, _ := concretePoolKit(t, intentAdopt, "creating")
	k.runtimeAs("gc-1", "tok", nil)
	if s := k.runKind(); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	if got := k.meta(beadmeta.WorkDirMetadataKey); got != templateDir {
		t.Fatalf("work_dir %q, want the adopt to leave it", got)
	}
}

// stubTranscript makes every keyed transcript present or absent.
func stubTranscript(t *testing.T, present bool) {
	t.Helper()
	old := staleResumeKeyProbe
	staleResumeKeyProbe = func(string, string, string) (bool, bool) { return present, true }
	t.Cleanup(func() { staleResumeKeyProbe = old })
}
