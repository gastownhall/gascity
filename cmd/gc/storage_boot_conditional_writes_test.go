package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout"
)

// serveConvergedSplitCity migrates a city onto the infra binding and opens the
// routes the controller would serve it from, under the given
// beads.conditional_writes spelling. It returns the routes plus the retained
// work store, so a caller can resolve a class exactly as the runtime does.
func serveConvergedSplitCity(t *testing.T, conditionalWrites string) (*storageRoutes, beads.Store, *config.City, string) {
	t.Helper()
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	cfg.Beads.ConditionalWrites = conditionalWrites
	source := stubInfraMigrationSource(t)

	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("the migration reported %s: %s", got.Outcome, log.String())
	}

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err != nil {
		t.Fatalf("a converged city was refused (conditional_writes=%q): %v", conditionalWrites, err)
	}
	if routes == nil {
		t.Fatal("a converged city resolved no routes")
	}
	t.Cleanup(func() { _ = routes.close() })
	return routes, source, cfg, cityPath
}

// TestSessionClassConditionalWritesAreRequiredOfTheBindingStore is the
// resolution half of ga-f7v2ft.162. The binding provider opens its own engine,
// so nothing about the city's rollout policy ever reached the store the session
// class is served from, and the keyed reconciler saw a nil writer: status
// healing skipped, deadline and zombie writes ran unfenced, and drain-ack
// handed every admission back to legacy.
//
// The fix is the requirement, not a policy value: the session class needs the
// fence, SQLiteStore implements it, so the capability must resolve — in every
// mode, including with the rollout gate off. The store is taken from the
// production opener (storageBootGate → the provider's EngineOpener), not
// hand-built, because the defect lived in that path.
func TestSessionClassConditionalWritesAreRequiredOfTheBindingStore(t *testing.T) {
	for _, mode := range []string{"", "off", "auto", "require"} {
		t.Run(fmt.Sprintf("conditional_writes=%q", mode), func(t *testing.T) {
			routes, source, cfg, cityPath := serveConvergedSplitCity(t, mode)

			sessions := resolveSessionStore(routes, source, cfg, cityPath, nil)
			if sessions == source {
				t.Fatal("the session class still resolves to the work store on a converged city")
			}
			// The defect signature, pinned so the fix cannot quietly regress
			// into it: the MODE-GATED resolve — what the keyed session-start
			// path used to call — yields nothing at all on this store, in
			// every mode including require, because the city's policy value
			// never reaches an engine the provider opened itself. That silence
			// is the whole bug. The session class does not ask that question
			// any more; it states a requirement, which the store meets.
			//
			// A later de-conditionalization may legitimately make this resolve
			// non-nil. Delete this leg deliberately then.
			if gated, _, gatedErr := beads.ResolveConditionalWriter(sessions); gated != nil || gatedErr != nil {
				t.Fatalf("the mode-gated resolve changed shape (writer=%v, err=%v); re-read this test before updating it", gated, gatedErr)
			}

			writer, err := beads.RequiredConditionalWriter(sessions)
			if err != nil {
				t.Fatalf("the relocated session-class store cannot fence: %v", err)
			}
			if writer == nil {
				t.Fatal("the relocated session-class store resolved NO conditional writer: keyed status healing skips, deadline and zombie writes run unfenced, and drain-ack hands back to legacy on every admission")
			}

			// The drain-ack admission needs both halves before it will own an
			// admission; the writer alone still hands back.
			if _, ok := beads.AtomicConditionalCloserFor(sessions); !ok {
				t.Fatal("the relocated session-class store advertises no atomic terminal closer, so keyed drain-ack hands back to legacy")
			}
		})
	}
}

// TestRequiredConditionalWriterRefusesACapabilityLessStore is the control: the
// requirement has to be able to FAIL, or the test above proves only that the
// call compiles. A store with no conditional-write surface must produce a named
// error, never a nil writer with a nil error.
func TestRequiredConditionalWriterRefusesACapabilityLessStore(t *testing.T) {
	store := beads.NewMemStore()
	store.DisableConditionalWrites = true

	writer, err := beads.RequiredConditionalWriter(store)
	if err == nil {
		t.Fatal("a store that cannot fence satisfied the session-class requirement")
	}
	if writer != nil {
		t.Error("a refused requirement still handed back a writer")
	}
	if !beads.IsConditionalWritesUnavailable(err) {
		t.Errorf("error = %v, want *beads.ConditionalWritesUnavailableError", err)
	}
	if !strings.Contains(err.Error(), "MemStore") {
		t.Errorf("the refusal does not name the store: %v", err)
	}
}

// TestConditionalWritesStatusEnumeratesTheSessionClassAndTheRelocatedStore is
// the disclosure half. The §12.5 block enumerated work stores only, so a split
// city reported a clean block while nothing at all was known about the store
// its session rows live in — an operator's clean status was evidence of
// nothing.
func TestConditionalWritesStatusEnumeratesTheSessionClassAndTheRelocatedStore(t *testing.T) {
	routes, source, cfg, cityPath := serveConvergedSplitCity(t, "require")

	cs := &controllerState{rolloutFlags: rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Require))}
	cs.storageRoutes = routes
	cs.cityBeadStore = source
	cs.cfg = cfg
	cs.cityPath = cityPath

	got := cs.ConditionalWritesStatus()
	if got == nil {
		t.Fatal("nil status block")
	}
	binding, ok := conditionalWritesRow(got.Stores, "storage/infra")
	if !ok {
		t.Fatalf("the status block does not enumerate the relocated binding: %+v", got.Stores)
	}
	if binding.Kind != "sqlite-graph" {
		t.Errorf("relocated store kind = %q, want sqlite-graph", binding.Kind)
	}
	if !binding.Capable || binding.Probe != beads.ConditionalWriteProbeCapable {
		t.Errorf("relocated store verdict = %+v, want a capable probe", binding)
	}

	session, ok := conditionalWritesRow(got.Stores, sessionClassStoreID)
	if !ok {
		t.Fatalf("the status block does not report the session class's required capability: %+v", got.Stores)
	}
	if !session.Capable {
		t.Errorf("session-class requirement verdict = %+v, want the requirement met", session)
	}
	if got.Effective != "active" {
		t.Errorf("effective = %q, want active on a fenced split city", got.Effective)
	}
}

// TestConditionalWritesStatusReportsAMissingSessionClassRequirement is the
// negative, in the vocabulary the requirement demands: a session class whose
// store cannot fence is FATAL and says so in EVERY mode — the gate being off
// does not make a missing requirement acceptable, and a row that reported it
// capable would be an absence of signal dressed as evidence.
func TestConditionalWritesStatusReportsAMissingSessionClassRequirement(t *testing.T) {
	for _, mode := range []rollout.Mode{rollout.Off, rollout.Auto, rollout.Require} {
		t.Run(string(mode), func(t *testing.T) {
			store := beads.NewMemStore()
			store.DisableConditionalWrites = true
			cs := &controllerState{rolloutFlags: rollout.ForTest(rollout.WithBeadsConditionalWrites(mode))}
			cs.cityBeadStore = store

			got := cs.ConditionalWritesStatus()
			row, ok := conditionalWritesRow(got.Stores, sessionClassStoreID)
			if !ok {
				t.Fatalf("no session-class requirement row in mode %s: %+v", mode, got.Stores)
			}
			if row.Capable {
				t.Errorf("a session class that cannot fence reported capable: %+v", row)
			}
			if !strings.Contains(row.Reason, "FATAL") {
				t.Errorf("the missing requirement is not reported as fatal: %q", row.Reason)
			}
			if got.Effective != "fail_closed" {
				t.Errorf("effective = %q in mode %s, want fail_closed: a missing requirement is not mode-dependent", got.Effective, mode)
			}
		})
	}
}

// TestPreflightSessionClassConditionalWritesFailsLoudly pins the boot-time
// half: a session-class store that cannot fence produces a startup ERROR line
// naming the store and what it lacks, in every mode, rather than silence until
// the first drain.
func TestPreflightSessionClassConditionalWritesFailsLoudly(t *testing.T) {
	for _, mode := range []rollout.Mode{rollout.Off, rollout.Require} {
		t.Run(string(mode), func(t *testing.T) {
			store := beads.NewMemStore()
			store.DisableConditionalWrites = true
			var logs []string
			cs := &controllerState{
				rolloutFlags: rollout.ForTest(rollout.WithBeadsConditionalWrites(mode)),
				rolloutLogf:  func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
			}
			cs.cityBeadStore = store
			cs.preflightSessionClassConditionalWrites()

			var errLines []string
			for _, line := range logs {
				if strings.Contains(line, "ERROR") {
					errLines = append(errLines, line)
				}
			}
			if len(errLines) != 1 {
				t.Fatalf("preflight ERROR lines = %v, want exactly one", errLines)
			}
			if !strings.Contains(errLines[0], "session-class") || !strings.Contains(errLines[0], "MemStore") {
				t.Errorf("the ERROR line does not name the class and the store: %q", errLines[0])
			}
		})
	}
}

// TestPreflightSessionClassConditionalWritesIsSilentOnACapableStore is the
// control for the line above: a capable store must not produce a startup
// complaint, or the loud line means nothing.
func TestPreflightSessionClassConditionalWritesIsSilentOnACapableStore(t *testing.T) {
	routes, source, cfg, cityPath := serveConvergedSplitCity(t, "auto")
	var logs []string
	cs := &controllerState{
		rolloutFlags: rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Auto)),
		rolloutLogf:  func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	cs.storageRoutes = routes
	cs.cityBeadStore = source
	cs.cfg = cfg
	cs.cityPath = cityPath

	cs.preflightSessionClassConditionalWrites()
	if len(logs) != 0 {
		t.Fatalf("a capable session-class store logged %v, want silence", logs)
	}
}

// conditionalWritesRow finds one store verdict by id.
func conditionalWritesRow(rows []api.StatusConditionalWriteStoreVerdict, id string) (api.StatusConditionalWriteStoreVerdict, bool) {
	for _, row := range rows {
		if row.StoreID == id {
			return row, true
		}
	}
	return api.StatusConditionalWriteStoreVerdict{}, false
}
