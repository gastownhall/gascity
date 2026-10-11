package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/session"
)

// The runtime-reaper differential harness (spec §6.3). The same fixture runs
// through the reapers twice: once listing live (inv=nil, legacy), once fed by
// an inventory lane pass. Effects (provider Stops and session-row closes) and
// the fresh confirmations preceding each Stop must agree:
//
//   - E1, lag 0: the pass and the tick see the same state; effects are equal,
//     and every Stop is preceded by the same confirmation calls on its name.
//   - E2, lag 1: the pass saw the previous state; lane effects are a subset of
//     what legacy does on an honest listing of the current state.
//   - E3: a fresh pass on the current state does everything legacy does
//     (E1 again, one pass later).

// reaperRuntime is one scripted runtime under a name.
type reaperRuntime struct {
	incarnation string
	dead        bool // every pane dead: a remain-on-exit corpse
	sessionID   string
	token       string
	deadErr     bool // IsDeadRuntimeSession cannot tell
	// recycleTo makes the first GetMeta(GC_INSTANCE_TOKEN) find the name
	// already recycled into a live runtime carrying this token.
	recycleTo string
	// uninventoried leaves the name out of the batched inventory.
	uninventoried bool
	// metaErr fails GetMeta.
	metaErr bool
	// acp hosts the runtime on a composite world's acp backend.
	acp bool
}

type reaperListMode uint8

const (
	reaperListOK reaperListMode = iota
	reaperListPartial
	reaperListServerAbsent
	reaperListFailed
)

// reaperState is one instant of the world: runtimes, listing behavior, and
// the session rows (open and closed) in the store.
type reaperState struct {
	runtimes map[string]reaperRuntime
	list     reaperListMode
	hidden   map[string]bool // names a partial listing drops
	rows     []beads.Bead
	drains   []string // row IDs with an active drain
	nilStore bool
	// unattested makes the listing unable to prove absence; inventoryErr
	// fails the batched inventory.
	unattested   bool
	inventoryErr bool
	// storeOnly rows are in the store but not in the tick's snapshot.
	storeOnly []beads.Bead
	// composite runs the world as auto(tmux, acp). acpList is the acp
	// backend's listing behavior (list is tmux's), and sidecars are the
	// GC_SESSION_ID files acp leaves behind: its GetMeta answers from them
	// after the runtime is gone.
	composite bool
	acpList   reaperListMode
	sidecars  map[string]string
}

func (s reaperState) clone() reaperState {
	out := s
	out.runtimes = make(map[string]reaperRuntime, len(s.runtimes))
	for k, v := range s.runtimes {
		out.runtimes[k] = v
	}
	out.hidden = make(map[string]bool, len(s.hidden))
	for k, v := range s.hidden {
		out.hidden[k] = v
	}
	out.rows = cloneReaperRows(s.rows)
	out.storeOnly = cloneReaperRows(s.storeOnly)
	out.drains = append([]string(nil), s.drains...)
	out.sidecars = make(map[string]string, len(s.sidecars))
	for k, v := range s.sidecars {
		out.sidecars[k] = v
	}
	return out
}

// honest is s with a listing that shows every runtime (a partial or failed
// listing answered completely). An absent server stays absent.
func (s reaperState) honest() reaperState {
	out := s.clone()
	if out.list == reaperListPartial || out.list == reaperListFailed {
		out.list = reaperListOK
	}
	out.acpList = reaperListOK
	out.hidden = map[string]bool{}
	return out
}

func cloneReaperRows(rows []beads.Bead) []beads.Bead {
	out := make([]beads.Bead, len(rows))
	for i, b := range rows {
		b.Metadata = cloneStringMap(b.Metadata)
		b.Labels = append([]string(nil), b.Labels...)
		out[i] = b
	}
	return out
}

// reaperCall is one recorded provider call.
type reaperCall struct {
	phase  string // corpse | closed
	method string
	name   string
	key    string
	result string
}

// reaperLog records provider calls while phase is set. The backends of a
// composite world share one.
type reaperLog struct {
	mu    sync.Mutex
	phase string
	calls []reaperCall
}

func (l *reaperLog) record(method, name, key, result string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.phase != "" {
		l.calls = append(l.calls, reaperCall{phase: l.phase, method: method, name: name, key: key, result: result})
	}
}

func (l *reaperLog) setPhase(phase string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.phase = phase
}

func (l *reaperLog) recorded() []reaperCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]reaperCall(nil), l.calls...)
}

// reaperEnv is a scripted provider over a reaperState: one tmux-shaped
// world, or auto over a tmux-shaped and an acp-shaped one.
type reaperEnv interface {
	runtime.Provider
	apply(reaperState)
	callLog() *reaperLog
}

func newReaperEnv(s reaperState) reaperEnv {
	if s.composite {
		return newReaperAutoWorld(s)
	}
	return newReaperWorld(s)
}

// reaperWorld is a tmux-shaped scripted provider over a reaperState: a
// listing with partial, absent-server and failure modes, a batched inventory
// and environment read for the lane, and the reapers' fresh confirmations.
// With sidecars set it answers GetMeta(GC_SESSION_ID) for gone names, as acp
// does.
type reaperWorld struct {
	*runtime.Fake

	log          *reaperLog
	mu           sync.Mutex
	runtimes     map[string]reaperRuntime
	list         reaperListMode
	hidden       map[string]bool
	unattested   bool
	inventoryErr bool
	sidecars     map[string]string
}

var (
	_ runtime.DeadRuntimeSessionChecker = (*reaperWorld)(nil)
	_ runtime.InventoryProvider         = (*reaperWorld)(nil)
	_ runtime.EnvironmentBatchProvider  = (*reaperWorld)(nil)
	_ runtime.ListingAttestation        = (*reaperWorld)(nil)
)

func newReaperWorld(s reaperState) *reaperWorld {
	w := &reaperWorld{Fake: runtime.NewFake(), log: &reaperLog{}}
	w.apply(s)
	return w
}

// apply replaces the world's runtimes and listing behavior with s's.
func (w *reaperWorld) apply(s reaperState) {
	s = s.clone()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.runtimes, w.list, w.hidden = s.runtimes, s.list, s.hidden
	w.unattested, w.inventoryErr, w.sidecars = s.unattested, s.inventoryErr, s.sidecars
}

func (w *reaperWorld) callLog() *reaperLog { return w.log }

func (w *reaperWorld) record(method, name, key, result string) {
	w.log.record(method, name, key, result)
}

// IsRunning reports whether name has a runtime; auto's Stop reads it.
func (w *reaperWorld) IsRunning(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.runtimes[name]
	return ok
}

func (w *reaperWorld) listedLocked() []string {
	names := make([]string, 0, len(w.runtimes))
	for name := range w.runtimes {
		if w.list == reaperListPartial && w.hidden[name] {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (w *reaperWorld) ListRunning(prefix string) ([]string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var names []string
	var err error
	switch w.list {
	case reaperListServerAbsent:
		err = serverAbsentListErr()
	case reaperListFailed:
		err = errors.New("listing failed")
	case reaperListPartial:
		err = &runtime.PartialListError{Err: errors.New("one backend down")}
		fallthrough
	default:
		for _, name := range w.listedLocked() {
			if strings.HasPrefix(name, prefix) {
				names = append(names, name)
			}
		}
	}
	w.record("ListRunning", prefix, "", reaperListResult(names, err))
	return names, err
}

func reaperListResult(names []string, err error) string {
	switch {
	case runtime.IsRuntimeServerAbsent(err):
		return "server-absent"
	case runtime.IsPartialListError(err):
		return "partial:" + strings.Join(names, ",")
	case err != nil:
		return "failed"
	default:
		return strings.Join(names, ",")
	}
}

func (w *reaperWorld) ListRunningComplete() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.unattested
}

func (w *reaperWorld) RuntimeInventory(context.Context) (map[string]runtime.InventoryEntry, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.list == reaperListServerAbsent || w.list == reaperListFailed || w.inventoryErr {
		return nil, errors.New("list-panes failed")
	}
	out := make(map[string]runtime.InventoryEntry)
	for _, name := range w.listedLocked() {
		r := w.runtimes[name]
		if r.uninventoried {
			continue
		}
		out[name] = runtime.InventoryEntry{Incarnation: r.incarnation, DeadKnown: true, AllPanesDead: r.dead, AttachedKnown: true}
	}
	return out, nil
}

func (w *reaperWorld) GetAllEnvironment(name string) (map[string]string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r, ok := w.runtimes[name]
	if !ok {
		return nil, fmt.Errorf("session %q: %w", name, runtime.ErrSessionNotFound)
	}
	env := map[string]string{}
	if r.sessionID != "" {
		env["GC_SESSION_ID"] = r.sessionID
	}
	if r.token != "" {
		env["GC_INSTANCE_TOKEN"] = r.token
	}
	return env, nil
}

// IsDeadRuntimeSession matches tmux: a missing session is not dead.
func (w *reaperWorld) IsDeadRuntimeSession(name string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r, ok := w.runtimes[name]
	switch {
	case !ok:
		w.record("IsDeadRuntimeSession", name, "", "missing")
		return false, nil
	case r.deadErr:
		w.record("IsDeadRuntimeSession", name, "", "error")
		return false, errors.New("pane state unavailable")
	default:
		w.record("IsDeadRuntimeSession", name, "", fmt.Sprint(r.dead))
		return r.dead, nil
	}
}

// GetMeta matches tmux: a missing session is an error.
func (w *reaperWorld) GetMeta(name, key string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r, ok := w.runtimes[name]
	if id, sidecar := w.sidecars[name]; !ok && sidecar && key == "GC_SESSION_ID" {
		w.record("GetMeta", name, key, id)
		return id, nil
	}
	if !ok {
		w.record("GetMeta", name, key, "missing")
		return "", fmt.Errorf("session %q: %w", name, runtime.ErrSessionNotFound)
	}
	if r.metaErr {
		w.record("GetMeta", name, key, "error")
		return "", fmt.Errorf("session %q: %w", name, runtime.ErrRuntimeUnavailable)
	}
	var v string
	switch key {
	case "GC_SESSION_ID":
		v = r.sessionID
	case "GC_INSTANCE_TOKEN":
		if r.recycleTo != "" {
			r = reaperRuntime{incarnation: r.incarnation + "+recycled", sessionID: r.sessionID, token: r.recycleTo}
			w.runtimes[name] = r
		}
		v = r.token
	}
	w.record("GetMeta", name, key, v)
	return v, nil
}

func (w *reaperWorld) Stop(name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.runtimes[name]; !ok {
		w.record("Stop", name, "", "gone")
		return fmt.Errorf("session %q: %w", name, runtime.ErrSessionNotFound)
	}
	delete(w.runtimes, name)
	w.record("Stop", name, "", "ok")
	return nil
}

// acpReaperBackend is a reaperWorld as acp shows it: a listing, GetMeta that
// reads sidecar files, and Stop; no inventory, environment read or death
// check.
type acpReaperBackend struct {
	*runtime.Fake
	w *reaperWorld
}

func (a *acpReaperBackend) ListRunning(prefix string) ([]string, error) {
	return a.w.ListRunning(prefix)
}
func (a *acpReaperBackend) ListRunningComplete() bool                { return a.w.ListRunningComplete() }
func (a *acpReaperBackend) GetMeta(name, key string) (string, error) { return a.w.GetMeta(name, key) }
func (a *acpReaperBackend) Stop(name string) error                   { return a.w.Stop(name) }
func (a *acpReaperBackend) IsRunning(name string) bool               { return a.w.IsRunning(name) }

// reaperAutoWorld is auto(tmux, acp) over one reaperState: runtimes marked
// acp live on the acp backend, the rest on tmux. A name routes to acp while
// it runs there, or while only its sidecar is left (a crashed acp runtime
// keeps its route).
type reaperAutoWorld struct {
	*sessionauto.Provider
	tmux, acp *reaperWorld
	routed    []string
}

func newReaperAutoWorld(s reaperState) *reaperAutoWorld {
	log := &reaperLog{}
	tmux := &reaperWorld{Fake: runtime.NewFake(), log: log}
	acp := &reaperWorld{Fake: runtime.NewFake(), log: log}
	w := &reaperAutoWorld{
		Provider: sessionauto.New(tmux, &acpReaperBackend{Fake: runtime.NewFake(), w: acp}),
		tmux:     tmux,
		acp:      acp,
	}
	w.apply(s)
	return w
}

func (w *reaperAutoWorld) callLog() *reaperLog { return w.tmux.log }

func (w *reaperAutoWorld) apply(s reaperState) {
	onTmux, onACP := s.clone(), s.clone()
	onTmux.runtimes, onTmux.sidecars = map[string]reaperRuntime{}, nil
	onACP.runtimes, onACP.list = map[string]reaperRuntime{}, s.acpList
	for name, r := range s.runtimes {
		if r.acp {
			onACP.runtimes[name] = r
		} else {
			onTmux.runtimes[name] = r
		}
	}
	w.tmux.apply(onTmux)
	w.acp.apply(onACP)
	for _, name := range w.routed {
		w.Unroute(name)
	}
	w.routed = w.routed[:0]
	for name := range onACP.runtimes {
		w.routed = append(w.routed, name)
	}
	for name := range s.sidecars {
		if _, running := s.runtimes[name]; !running {
			w.routed = append(w.routed, name)
		}
	}
	for _, name := range w.routed {
		w.RouteACP(name)
	}
}

// reaperEffects are one run's effects and the calls behind them.
type reaperEffects struct {
	stops  []string          // sorted
	closes map[string]string // row ID → state written by the close
	calls  []reaperCall
	// source and closedSource name the listing each reaper used: lane or
	// live.
	source, closedSource string
	// filtered counts names a lane filter skipped, in both reapers.
	filtered int
}

// reaperLaneRuntime is the smallest CityRuntime an inventory lane runs on.
func reaperLaneRuntime(t *testing.T, sp runtime.Provider) *CityRuntime {
	t.Helper()
	cr := &CityRuntime{
		cityName: "test-city",
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Daemon:    config.DaemonConfig{PatrolInterval: inventoryLaneInterval.String()},
		},
		sp:                  sp,
		standaloneCityStore: beads.NewMemStore(),
		rec:                 events.Discard,
		logPrefix:           "gc test",
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
	if cr.initRuntimeInventoryLane() == nil {
		t.Fatal("initRuntimeInventoryLane returned nil with a provider and a store")
	}
	return cr
}

// runLegacyReapers runs both reapers on st, listing live.
func runLegacyReapers(t *testing.T, st reaperState) reaperEffects {
	t.Helper()
	return runReapers(t, st, newReaperEnv(st), nil)
}

// runLaneReapers runs one lane pass over seen, then both reapers on st fed
// by that pass.
func runLaneReapers(t *testing.T, seen, st reaperState) reaperEffects {
	t.Helper()
	lr := newLaneReaperRun(t, seen)
	lr.pass(seen)
	eff, inv := lr.reap(t, st)
	if inv == nil && !seen.mergedListingFails() {
		t.Fatalf("no inventory view after a fresh %v pass", seen.list)
	}
	return eff
}

// mergedListingFails reports whether s's merged listing fails outright, so
// the lane publishes no view.
func (s reaperState) mergedListingFails() bool {
	return s.list == reaperListFailed && (!s.composite || s.acpList == reaperListFailed)
}

// laneReaperRun is one world with an inventory lane over it: passes over a
// sequence of states, then the reapers on the current one.
type laneReaperRun struct {
	w  reaperEnv
	cr *CityRuntime
}

func newLaneReaperRun(t *testing.T, first reaperState) *laneReaperRun {
	t.Helper()
	w := newReaperEnv(first)
	return &laneReaperRun{w: w, cr: reaperLaneRuntime(t, w)}
}

// pass runs one lane pass over st.
func (lr *laneReaperRun) pass(st reaperState) {
	lr.w.apply(st)
	runTestInventoryPass(lr.cr)
}

// reap runs both reapers on st with this tick's view, which it returns.
func (lr *laneReaperRun) reap(t *testing.T, st reaperState) (reaperEffects, *runtimeInventoryView) {
	t.Helper()
	lr.w.apply(st)
	inv := lr.cr.inventoryViewForTick()
	return runReapers(t, st, lr.w, inv), inv
}

func runReapers(t *testing.T, st reaperState, w reaperEnv, inv *runtimeInventoryView) reaperEffects {
	testCity := t.TempDir()
	t.Helper()
	rows := cloneReaperRows(st.rows)
	var store beads.Store
	var mem *beads.MemStore
	if !st.nilStore {
		all := append(cloneReaperRows(rows), cloneReaperRows(st.storeOnly)...)
		mem = beads.NewMemStoreFrom(len(all), all, nil)
		store = mem
	}
	snapshot := newSessionBeadSnapshot(cloneReaperRows(rows))
	dt := newDrainTracker()
	for _, id := range st.drains {
		dt.set(id, &drainState{reason: "user"})
	}

	log := w.callLog()
	from := len(log.recorded())
	log.setPhase("corpse")
	cleanupDeadRuntimeSessionCorpses(testCity, store, nil, nil, snapshot, dt, w, inv, clock.Real{}, io.Discard)
	log.setPhase("closed")
	reapRuntimesBoundToClosedBeads(store, snapshot, dt, w, inv, testCity, io.Discard)
	log.setPhase("")

	eff := reaperEffects{closes: map[string]string{}, calls: log.recorded()[from:], source: inventorySourceLive, closedSource: inventorySourceLive}
	if inv != nil {
		if inv.corpses.source != "" {
			eff.source = inv.corpses.source
		}
		if inv.closedBound.source != "" {
			eff.closedSource = inv.closedBound.source
		}
		eff.filtered = inv.corpses.filtered + inv.closedBound.filtered
	}
	for _, c := range eff.calls {
		if c.method == "Stop" && c.result == "ok" {
			eff.stops = append(eff.stops, c.name)
		}
	}
	sort.Strings(eff.stops)
	if mem != nil {
		for _, before := range rows {
			if before.Status == "closed" {
				continue
			}
			after, err := mem.Get(before.ID)
			if err != nil {
				t.Fatalf("re-read row %s: %v", before.ID, err)
			}
			if after.Status == "closed" {
				eff.closes[before.ID] = after.Metadata["state"]
			}
		}
	}
	return eff
}

// confirmPairs maps each Stop to the calls its reaper made on that name
// before it: "phase/name" → calls.
func confirmPairs(calls []reaperCall) map[string][]string {
	pairs := map[string][]string{}
	for i, c := range calls {
		if c.method != "Stop" {
			continue
		}
		var before []string
		for _, p := range calls[:i] {
			if p.phase == c.phase && p.name == c.name {
				before = append(before, p.method+"("+p.key+")="+p.result)
			}
		}
		pairs[c.phase+"/"+c.name] = before
	}
	return pairs
}

func wantSameReaperEffects(t *testing.T, what string, legacy, lane reaperEffects) {
	t.Helper()
	if !reflect.DeepEqual(legacy.stops, lane.stops) || !reflect.DeepEqual(legacy.closes, lane.closes) {
		t.Fatalf("%s: lane effects stops=%v closes=%v, legacy stops=%v closes=%v\nlane calls=%v\nlegacy calls=%v",
			what, lane.stops, lane.closes, legacy.stops, legacy.closes, lane.calls, legacy.calls)
	}
	if lp, gp := legacy.confirmations(), lane.confirmations(); !reflect.DeepEqual(lp, gp) {
		t.Fatalf("%s: confirm-before-effect pairs differ\nlane=%v\nlegacy=%v", what, gp, lp)
	}
}

func (e reaperEffects) pairs() map[string][]string { return confirmPairs(e.calls) }

// confirmations are e's pairs without exact-name listings: the closed-bead
// reaper's presence check is the lane path's addition to legacy's
// confirmations (wantConfirmPrecedesEveryEffect requires it there).
func (e reaperEffects) confirmations() map[string][]string {
	out := map[string][]string{}
	for key, before := range e.pairs() {
		kept := []string{}
		for _, c := range before {
			if !strings.HasPrefix(c, "ListRunning(") {
				kept = append(kept, c)
			}
		}
		out[key] = kept
	}
	return out
}

// listingShows reports whether a recorded ListRunning result lists name.
func listingShows(result, name string) bool {
	return slices.Contains(strings.Split(strings.TrimPrefix(result, "partial:"), ","), name)
}

// wantReaperEffectsSubset asserts lane effects ⊆ legacy effects (E2), and
// that the lane issues no Stop on a name legacy never tries to stop.
func wantReaperEffectsSubset(t *testing.T, what string, legacy, lane reaperEffects) {
	t.Helper()
	tried := map[string]bool{}
	for _, c := range legacy.calls {
		if c.method == "Stop" {
			tried[c.name] = true
		}
	}
	for _, c := range lane.calls {
		if c.method == "Stop" && !tried[c.name] {
			t.Fatalf("%s: lane issued %s Stop(%q), which legacy on an honest listing never issues\nlane calls=%v\nlegacy calls=%v", what, c.phase, c.name, lane.calls, legacy.calls)
		}
	}
	have := map[string]int{}
	for _, n := range legacy.stops {
		have[n]++
	}
	for _, n := range lane.stops {
		if have[n] == 0 {
			t.Fatalf("%s: lane stopped %q, which legacy on an honest listing does not\nlane calls=%v\nlegacy calls=%v", what, n, lane.calls, legacy.calls)
		}
		have[n]--
	}
	for id, state := range lane.closes {
		if got, ok := legacy.closes[id]; !ok || got != state {
			t.Fatalf("%s: lane closed %s as %q, legacy on an honest listing does not\nlane calls=%v\nlegacy calls=%v", what, id, state, lane.calls, legacy.calls)
		}
	}
}

// wantConfirmPrecedesEveryEffect asserts I-lane-authority on one run: every
// corpse Stop follows a fresh IsDeadRuntimeSession on its name that answered
// dead; every closed-bead Stop follows a fresh GetMeta(GC_SESSION_ID) of its
// name and, on the lane path, then a fresh exact-name listing that shows it;
// every close follows its row's Stop or a fresh listing that found the server
// absent.
func wantConfirmPrecedesEveryEffect(t *testing.T, what string, st reaperState, eff reaperEffects) {
	t.Helper()
	for key, before := range eff.pairs() {
		phase, name, _ := strings.Cut(key, "/")
		var last string
		present := false
		for _, c := range before {
			switch {
			case phase == "corpse" && strings.HasPrefix(c, "IsDeadRuntimeSession("):
				last = c
			case phase == "closed" && strings.HasPrefix(c, "GetMeta(GC_SESSION_ID)="):
				last, present = c, false
			case phase == "closed" && strings.HasPrefix(c, "ListRunning()="):
				present = present || listingShows(strings.TrimPrefix(c, "ListRunning()="), name)
			}
		}
		if phase == "closed" && eff.closedSource == inventorySourceLane && !present {
			t.Fatalf("%s: lane-path %s not preceded by a fresh exact-name listing showing it (calls before: %v)", what, key, before)
		}
		switch {
		case phase == "corpse" && last != "IsDeadRuntimeSession()=true":
			t.Fatalf("%s: %s not preceded by a fresh dead confirmation (calls before: %v)", what, key, before)
		case phase == "closed" && (last == "" || last == "GetMeta(GC_SESSION_ID)="):
			t.Fatalf("%s: %s not preceded by a fresh GetMeta attribution (calls before: %v)", what, key, before)
		}
	}
	freshAbsent := false
	for _, c := range eff.calls {
		if c.phase == "corpse" && c.method == "ListRunning" && c.result == "server-absent" {
			freshAbsent = true
		}
	}
	stopped := map[string]bool{}
	for _, n := range eff.stops {
		stopped[n] = true
	}
	for id := range eff.closes {
		var name string
		for _, r := range st.rows {
			if r.ID == id {
				name = r.Metadata["session_name"]
			}
		}
		if !stopped[name] && !freshAbsent {
			t.Fatalf("%s: row %s closed without its runtime %q stopped or a fresh absent listing; calls=%v", what, id, name, eff.calls)
		}
	}
}

// Session-row builders.

func reaperRow(id, name, state string, extra ...string) beads.Bead {
	meta := map[string]string{"session_name": name, "template": "worker"}
	if state != "" {
		meta["state"] = state
	}
	for i := 0; i+1 < len(extra); i += 2 {
		meta[extra[i]] = extra[i+1]
	}
	return beads.Bead{ID: id, Status: "open", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta}
}

func closedReaperRow(id string) beads.Bead {
	return beads.Bead{ID: id, Status: "closed", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{}}
}

func reaperFixtureState(rows []beads.Bead, runtimes map[string]reaperRuntime) reaperState {
	if runtimes == nil {
		runtimes = map[string]reaperRuntime{}
	}
	return reaperState{runtimes: runtimes, hidden: map[string]bool{}, rows: rows, sidecars: map[string]string{}}
}

// reaperFixture is one case lifted from the reapers' owning tests
// (TestCleanupDeadRuntimeSessionCorpses*, TestReapRuntimesBoundToClosedBeads*
// and the pre-boot reap cases).
type reaperFixture struct {
	name  string
	state func() reaperState
	// stops and closes are what both runs must do; they pin the fixture
	// itself, so an equality over two no-ops proves nothing by accident.
	stops  []string
	closes map[string]string
}

func reaperFixtures(boot time.Time) []reaperFixture {
	killPending := map[string]string{}
	for k, v := range session.KillPendingPatch(time.Now()) {
		killPending[k] = v
	}
	killRow := reaperRow("s-kill", "killed-worker", "")
	for k, v := range killPending {
		killRow.Metadata[k] = v
	}
	return []reaperFixture{
		{
			name: "visible dead, live and untracked",
			state: func() reaperState {
				return reaperFixtureState(
					[]beads.Bead{reaperRow("s1", "dead-worker", "active"), reaperRow("s2", "live-worker", "active"), reaperRow("s3", "absent-worker", "active")},
					map[string]reaperRuntime{
						"dead-worker":      {incarnation: "dead:1", dead: true, sessionID: "s1"},
						"live-worker":      {incarnation: "live:1", sessionID: "s2"},
						"untracked-worker": {incarnation: "untracked:1"},
					})
			},
			stops: []string{"dead-worker"}, closes: map[string]string{"s1": "dead-runtime"},
		},
		{
			name: "liveness uncertainty",
			state: func() reaperState {
				return reaperFixtureState([]beads.Bead{reaperRow("s1", "worker", "active")},
					map[string]reaperRuntime{"worker": {incarnation: "w:1", dead: true, deadErr: true, sessionID: "s1"}})
			},
		},
		{
			name: "partial listing checks visible names only",
			state: func() reaperState {
				st := reaperFixtureState(
					[]beads.Bead{reaperRow("s1", "visible-dead", "active"), reaperRow("s2", "hidden-dead", "active")},
					map[string]reaperRuntime{
						"visible-dead": {incarnation: "v:1", dead: true, sessionID: "s1"},
						"hidden-dead":  {incarnation: "h:1", dead: true, sessionID: "s2"},
					})
				st.list, st.hidden = reaperListPartial, map[string]bool{"hidden-dead": true}
				return st
			},
			stops: []string{"visible-dead"}, closes: map[string]string{"s1": "dead-runtime"},
		},
		{
			name: "lifecycle-owned rows",
			state: func() reaperState {
				st := reaperFixtureState(
					[]beads.Bead{
						reaperRow("pending", "pending-worker", "active", "pending_create_claim", "true"),
						reaperRow("draining", "draining-worker", "active"),
						reaperRow("named", "named-worker", "active",
							session.NamedSessionMetadataKey, "true",
							session.NamedSessionIdentityMetadata, "rig/worker",
							session.NamedSessionModeMetadata, "always"),
						killRow,
						reaperRow("ordinary", "ordinary-worker", "active"),
					},
					map[string]reaperRuntime{
						"pending-worker":  {incarnation: "p:1", dead: true},
						"draining-worker": {incarnation: "d:1", dead: true},
						"named-worker":    {incarnation: "n:1", dead: true},
						"killed-worker":   {incarnation: "k:1", dead: true},
						"ordinary-worker": {incarnation: "o:1", dead: true},
					})
				st.drains = []string{"draining"}
				return st
			},
			stops: []string{"ordinary-worker"}, closes: map[string]string{"ordinary": "dead-runtime"},
		},
		{
			name: "killed asleep and dormant rows keep their row",
			state: func() reaperState {
				return reaperFixtureState(
					[]beads.Bead{
						reaperRow("s-killed", "killed", string(session.StateAsleep), "sleep_reason", "killed", "instance_token", "tok-k"),
						reaperRow("s-drained", "drained", string(session.StateDrained)),
						reaperRow("s-suspended", "suspended", string(session.StateSuspended)),
					},
					map[string]reaperRuntime{
						"killed":    {incarnation: "k:1", dead: true, sessionID: "s-killed", token: "tok-k"},
						"drained":   {incarnation: "d:1", dead: true, sessionID: "s-drained"},
						"suspended": {incarnation: "s:1", dead: true, sessionID: "s-suspended"},
					})
			},
			stops: []string{"drained", "killed", "suspended"}, closes: map[string]string{},
		},
		{
			name: "rows claiming a live runtime close; a mid-restart row is left to its start",
			state: func() reaperState {
				return reaperFixtureState(
					[]beads.Bead{
						reaperRow("s-awake", "awake", string(session.StateAwake), "instance_token", "tok-a"),
						reaperRow("s-creating", "creating", string(session.StateCreating), "instance_token", "tok-new"),
						reaperRow("s-unstamped", "unstamped", string(session.StateActive), "instance_token", "tok-u"),
					},
					map[string]reaperRuntime{
						"awake":     {incarnation: "a:1", dead: true, sessionID: "s-awake", token: "tok-a"},
						"creating":  {incarnation: "c:1", dead: true, sessionID: "s-creating", token: "tok-previous"},
						"unstamped": {incarnation: "u:1", dead: true},
					})
			},
			stops: []string{"awake", "unstamped"}, closes: map[string]string{"s-awake": "dead-runtime", "s-unstamped": "dead-runtime"},
		},
		{
			name: "token re-check sees the start recycle the name",
			state: func() reaperState {
				return reaperFixtureState(
					[]beads.Bead{reaperRow("s1", "worker", string(session.StateCreating), "instance_token", "tok-new")},
					map[string]reaperRuntime{"worker": {incarnation: "w:1", dead: true, sessionID: "s1", token: "tok-previous", recycleTo: "tok-new"}})
			},
		},
		{
			// R8a: the cleanup stops only while the stored row still carries
			// the kill's facts; with no store there is no row to read.
			name: "nil store stops nothing: no row to decide on",
			state: func() reaperState {
				st := reaperFixtureState([]beads.Bead{reaperRow("s1", "dead-worker", "active")},
					map[string]reaperRuntime{"dead-worker": {incarnation: "d:1", dead: true}})
				st.nilStore = true
				return st
			},
			closes: map[string]string{},
		},
		{
			name: "pre-boot reap when the server is absent",
			state: func() reaperState {
				pre := reaperRow("s-preboot", "w-1", "active")
				pre.CreatedAt = boot.Add(-time.Hour)
				post := reaperRow("s-postboot", "w-2", "active")
				post.CreatedAt = boot.Add(time.Minute)
				st := reaperFixtureState([]beads.Bead{pre, post}, nil)
				st.list = reaperListServerAbsent
				return st
			},
			closes: map[string]string{"s-preboot": "stale-session"},
		},
		{
			name: "failed listing does nothing",
			state: func() reaperState {
				st := reaperFixtureState([]beads.Bead{reaperRow("s1", "dead-worker", "active")},
					map[string]reaperRuntime{"dead-worker": {incarnation: "d:1", dead: true, sessionID: "s1"}})
				st.list = reaperListFailed
				return st
			},
		},
		{
			name: "runtimes bound to closed beads",
			state: func() reaperState {
				st := reaperFixtureState(
					[]beads.Bead{
						reaperRow("gm-open", "mayor", "active"),
						reaperRow("gm-healthy", "worker", "active"),
						closedReaperRow("gm-closed"),
						closedReaperRow("gm-draining"),
					},
					map[string]reaperRuntime{
						"mayor":      {incarnation: "m:1", sessionID: "gm-closed"},
						"worker":     {incarnation: "w:1", sessionID: "gm-healthy"},
						"mystery":    {incarnation: "x:1"},
						"foreign":    {incarnation: "f:1", sessionID: "gm-elsewhere"},
						"still-open": {incarnation: "s:1", sessionID: "gm-open-not-in-snapshot"},
						"draining":   {incarnation: "d:1", sessionID: "gm-draining"},
					})
				st.drains = []string{"gm-draining"}
				// Open in the store but not in the tick's snapshot: only the
				// live store read keeps "still-open" running.
				st.storeOnly = []beads.Bead{{ID: "gm-open-not-in-snapshot", Status: "open"}}
				return st
			},
			stops: []string{"mayor"}, closes: map[string]string{},
		},
	}
}

// Kills: any divergence between the lane-fed and the live reapers on the
// owning tests' cases, in effects or in the confirmations behind them.
func TestRuntimeReapers_LaneFedEqualsLegacy_Fixtures(t *testing.T) {
	boot := time.Now().Add(-10 * time.Minute)
	withHostBootTime(t, boot, nil)
	for _, fx := range reaperFixtures(boot) {
		t.Run(fx.name, func(t *testing.T) {
			legacy := runLegacyReapers(t, fx.state())
			lane := runLaneReapers(t, fx.state(), fx.state())
			wantSameReaperEffects(t, fx.name, legacy, lane)
			wantStops := fx.stops
			if wantStops == nil {
				wantStops = []string(nil)
			}
			if !reflect.DeepEqual(legacy.stops, wantStops) {
				t.Fatalf("legacy stops = %v, want %v (fixture drifted); calls=%v", legacy.stops, wantStops, legacy.calls)
			}
			wantCloses := fx.closes
			if wantCloses == nil {
				wantCloses = map[string]string{}
			}
			if !reflect.DeepEqual(legacy.closes, wantCloses) {
				t.Fatalf("legacy closes = %v, want %v (fixture drifted)", legacy.closes, wantCloses)
			}
		})
	}
}

// reaperStepper draws random world steps (spec §6.3): start, die to a
// corpse, stop, rename-reuse, close a bead, restart attributed to a closed
// bead, respawn, sleep, mid-restart, server absent, list error, partial
// listing, and on a composite world an acp backend flap.
type reaperStepper struct {
	rng  *rand.Rand
	boot time.Time
	next int
}

var reaperNamePool = []string{"w-0", "w-1", "w-2", "w-3", "w-4", "w-5"}

func (g *reaperStepper) id(prefix string) string {
	g.next++
	return fmt.Sprintf("%s-%d", prefix, g.next)
}

// openRowFor returns the index of the open row named name, or -1.
func openRowFor(st reaperState, name string) int {
	for i, r := range st.rows {
		if r.Status != "closed" && r.Metadata["session_name"] == name {
			return i
		}
	}
	return -1
}

func (g *reaperStepper) createdAt() time.Time {
	if g.rng.Intn(4) == 0 {
		return g.boot.Add(-time.Hour)
	}
	return g.boot.Add(time.Minute)
}

// newRuntime is a fresh incarnation under name bound to sessionID; on a
// composite world it lands on either backend, and on acp it writes its
// sidecar.
func (g *reaperStepper) newRuntime(st reaperState, name, sessionID, token string) reaperRuntime {
	r := reaperRuntime{incarnation: g.id(name), sessionID: sessionID, token: token}
	if st.composite && g.rng.Intn(2) == 0 {
		r.acp = true
		st.sidecars[name] = sessionID
	}
	return r
}

func (g *reaperStepper) step(prev reaperState) reaperState {
	st := prev.clone()
	if st.list != reaperListServerAbsent {
		st.list, st.hidden = reaperListOK, map[string]bool{}
	}
	st.acpList = reaperListOK
	name := reaperNamePool[g.rng.Intn(len(reaperNamePool))]
	r, running := st.runtimes[name]
	row := openRowFor(st, name)
	kinds := 12
	if st.composite {
		kinds = 13
	}
	switch g.rng.Intn(kinds) {
	case 0, 1: // start: the server comes back if it was gone
		if st.list == reaperListServerAbsent {
			st.list = reaperListOK
		}
		if running {
			break
		}
		if row < 0 {
			b := reaperRow(g.id("s"), name, string(session.StateActive), "instance_token", g.id("tok"))
			b.CreatedAt = g.createdAt()
			st.rows = append(st.rows, b)
			row = len(st.rows) - 1
		}
		st.rows[row].Metadata["state"] = string(session.StateActive)
		st.runtimes[name] = g.newRuntime(st, name, st.rows[row].ID, st.rows[row].Metadata["instance_token"])
	case 2: // die to a corpse
		if running {
			r.dead = true
			st.runtimes[name] = r
		}
	case 3: // stop
		delete(st.runtimes, name)
	case 4: // rename-reuse: a new incarnation under the name, for a new row
		if row >= 0 && g.rng.Intn(2) == 0 {
			st.rows[row].Status = "closed"
		}
		b := reaperRow(g.id("s"), name, string(session.StateActive), "instance_token", g.id("tok"))
		b.CreatedAt = g.createdAt()
		st.rows = append(st.rows, b)
		st.runtimes[name] = g.newRuntime(st, name, b.ID, b.Metadata["instance_token"])
	case 5: // close the bead; its runtime keeps running
		if row >= 0 {
			st.rows[row].Status = "closed"
		}
	case 6: // restart the runtime attributed to a closed bead. GC_SESSION_ID
		// never changes within an incarnation, so a rebind is a restart.
		if running {
			id := g.id("gone")
			st.rows = append(st.rows, closedReaperRow(id))
			st.runtimes[name] = reaperRuntime{incarnation: g.id(name), sessionID: id, token: r.token, acp: r.acp}
			if r.acp {
				st.sidecars[name] = id
			}
		}
	case 7: // respawn: same session, new pane pid
		if running {
			r.incarnation, r.dead = g.id(name), false
			st.runtimes[name] = r
		}
	case 8: // sleep: the row goes dormant
		if row >= 0 {
			st.rows[row].Metadata["state"] = string(session.StateAsleep)
		}
	case 9: // mid-restart: the row rotated its token and is creating
		if row >= 0 {
			st.rows[row].Metadata["state"] = string(session.StateCreating)
			st.rows[row].Metadata["instance_token"] = g.id("tok")
		}
	case 10: // the tmux server goes away, and every tmux runtime with it
		for n, r := range st.runtimes {
			if !r.acp {
				delete(st.runtimes, n)
			}
		}
		st.list = reaperListServerAbsent
	case 11: // a list error or a partial listing
		if st.list == reaperListServerAbsent {
			break
		}
		if g.rng.Intn(2) == 0 {
			st.list = reaperListFailed
		} else {
			st.list, st.hidden = reaperListPartial, map[string]bool{name: true}
		}
	case 12: // the acp backend flaps: its listing fails or drops a name
		if g.rng.Intn(2) == 0 {
			st.acpList = reaperListFailed
		} else {
			st.acpList, st.hidden = reaperListPartial, map[string]bool{name: true}
		}
	}
	return st
}

// Kills: a stale nomination that authorizes an effect or a Stop (E2), and an
// effect the lane-fed reapers lose for good (E3). Each seed runs one lane
// over its whole walk, on tmux for odd seeds and on auto(tmux, acp) for even
// ones. Each step the reapers run on the current state first fed by an
// earlier pass (lag 1: a pass over the previous state; or, one step in
// three, lag 2: no pass since the last tick, which may reuse a pass up to
// two intervals old, so the view predates that tick's effects), then fed by
// a pass over the current state (lag 0).
func TestRuntimeReapers_LaneFedSafeAndLive_Randomized(t *testing.T) {
	const seeds, steps = 20, 50
	boot := time.Now().Add(-10 * time.Minute)
	withHostBootTime(t, boot, nil)
	var laneRuns, laneEffects, filtered, laggedRuns, compositeRuns int
	for seed := int64(1); seed <= seeds; seed++ {
		g := &reaperStepper{rng: rand.New(rand.NewSource(seed)), boot: boot}
		prev := reaperFixtureState(nil, nil)
		prev.composite = seed%2 == 0
		lr := newLaneReaperRun(t, prev)
		for step := 1; step <= steps; step++ {
			cur := g.step(prev)
			what := fmt.Sprintf("seed %d step %d (composite %v)", seed, step, cur.composite)

			if g.rng.Intn(3) != 0 {
				lr.pass(prev)
			}
			lagged, _ := lr.reap(t, cur)
			wantReaperEffectsSubset(t, what+" (E2, lagged)", runLegacyReapers(t, cur.honest()), lagged)
			wantConfirmPrecedesEveryEffect(t, what+" (lagged)", cur, lagged)

			legacy := runLegacyReapers(t, cur)
			lr.pass(cur)
			fresh, inv := lr.reap(t, cur)
			if inv == nil && !cur.mergedListingFails() {
				t.Fatalf("%s: no inventory view after a fresh pass", what)
			}
			wantSameReaperEffects(t, what+" (E1/E3, lag 0)", legacy, fresh)
			wantConfirmPrecedesEveryEffect(t, what+" (lag 0)", cur, fresh)
			if fresh.source == inventorySourceLane {
				laneRuns++
				laneEffects += len(fresh.stops) + len(fresh.closes)
				if cur.composite {
					compositeRuns++
				}
			}
			if lagged.source == inventorySourceLane {
				laggedRuns++
			}
			filtered += fresh.filtered + lagged.filtered

			// Advance the world by what the reapers did, as the next tick
			// would find it.
			prev = applyReaperEffects(cur, legacy)
		}
	}
	// The walk must reach the lane-fed paths, or equality proves nothing.
	if laneRuns == 0 || laggedRuns == 0 || laneEffects == 0 || filtered == 0 || compositeRuns == 0 {
		t.Fatalf("walk too thin: lane runs=%d (composite %d) lagged lane runs=%d lane effects=%d filtered=%d", laneRuns, compositeRuns, laggedRuns, laneEffects, filtered)
	}
	t.Logf("lane runs=%d (composite %d) lagged lane runs=%d lane effects=%d filtered=%d", laneRuns, compositeRuns, laggedRuns, laneEffects, filtered)
}

// applyReaperEffects returns st after eff's stops and closes.
func applyReaperEffects(st reaperState, eff reaperEffects) reaperState {
	out := st.clone()
	for _, n := range eff.stops {
		delete(out.runtimes, n)
	}
	for i, r := range out.rows {
		if _, ok := eff.closes[r.ID]; ok {
			out.rows[i].Status = "closed"
		}
	}
	return out
}

// Kills: fence-order regressions (I-lane-authority): a lane-fed Stop or close
// that no fresh confirmation of the same name precedes.
func TestRuntimeReapers_ConfirmPrecedesEveryEffect(t *testing.T) {
	boot := time.Now().Add(-10 * time.Minute)
	withHostBootTime(t, boot, nil)
	effects := 0
	for _, fx := range reaperFixtures(boot) {
		eff := runLaneReapers(t, fx.state(), fx.state())
		wantConfirmPrecedesEveryEffect(t, fx.name, fx.state(), eff)
		effects += len(eff.stops) + len(eff.closes)
	}
	if effects == 0 {
		t.Fatal("no fixture produced an effect; the ordering check proved nothing")
	}
}

// Kills: the lane path stopping a name that is gone. On auto(tmux, acp) the
// lane nominates an acp runtime that has since crashed; acp's GetMeta still
// answers from its sidecar with a closed bead, so only the fresh exact-name
// listing before Stop keeps the reaper from stopping a gone name, which
// legacy, listing live, never even nominates.
func TestReapRuntimesBoundToClosedBeads_LaneSkipsGoneNameItsSidecarAnswersFor(t *testing.T) {
	seen := reaperFixtureState([]beads.Bead{closedReaperRow("gm-closed")},
		map[string]reaperRuntime{"w-acp": {incarnation: "a:1", sessionID: "gm-closed", acp: true}})
	seen.composite = true
	seen.sidecars["w-acp"] = "gm-closed"
	gone := seen.clone()
	delete(gone.runtimes, "w-acp")

	lane := runLaneReapers(t, seen, gone)
	if lane.closedSource != inventorySourceLane {
		t.Fatalf("closed-bead reaper source = %q, want the lane", lane.closedSource)
	}
	answered := false
	for _, c := range lane.calls {
		if c.phase == "closed" && c.method == "GetMeta" && c.name == "w-acp" && c.result == "gm-closed" {
			answered = true
		}
		if c.method == "Stop" {
			t.Fatalf("lane path issued %s Stop(%q) on a gone name; calls=%v", c.phase, c.name, lane.calls)
		}
	}
	if !answered {
		t.Fatalf("the sidecar never answered GetMeta for the gone name, so the test proved nothing; calls=%v", lane.calls)
	}
	legacy := runLegacyReapers(t, gone)
	wantReaperEffectsSubset(t, "gone acp name", legacy, lane)
}
