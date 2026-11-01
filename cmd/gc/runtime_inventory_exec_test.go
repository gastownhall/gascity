package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
)

// scriptedInventory is a full fake whose batched inventory is entries, and
// which counts its inventory reads.
type scriptedInventory struct {
	runtime.FullFake
	mu      *sync.Mutex
	entries map[string]runtime.InventoryEntry
	reads   *int
}

func newScriptedInventory(entries map[string]runtime.InventoryEntry) scriptedInventory {
	return scriptedInventory{FullFake: runtime.FullFake{Fake: runtime.NewFake()}, mu: &sync.Mutex{}, entries: entries, reads: new(int)}
}

func (s scriptedInventory) RuntimeInventory(context.Context) (map[string]runtime.InventoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.reads++
	return s.entries, nil
}

func (s scriptedInventory) inventoryReads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.reads
}

// gatedOff is a leaf with a static inventory its provider gates off, as an
// exec pack that is not v2-ready has.
type gatedOff struct{ runtime.Provider }

func (gatedOff) RuntimeInventory(context.Context) (map[string]runtime.InventoryEntry, error) {
	return nil, errors.New("inventory gated off")
}

func (gatedOff) InventoryAvailable() bool { return false }

// Kills (PAR-EXEC §13.5 16): a lane step that takes an inventory a leaf
// gated off, by type assertion instead of runtime.InventoryFor. An exec
// pack that is not v2-ready then keeps the no-inventory path exactly: its
// names are enriched as unsupported (no inventory read), and its activity
// and sidecar restart markers are still read per name.
func TestLaneTakesInventoryOnlyThroughInventoryFor(t *testing.T) {
	backend := newScriptedInventory(map[string]runtime.InventoryEntry{"s": {Incarnation: "i", DeadKnown: true}})
	leaf := runtime.NewFakeProfile(runtime.ProfileExec, backend)
	l := &runtimeInventoryLane{}
	attrs, hosts, errs := l.enrich(context.Background(), []inventoryBackend{{label: "exec", provider: leaf, names: []string{"s"}}}, []BackendPass{completeBackend("exec", "s")})
	if a, ok := attrs["s"]; !ok || !reflect.DeepEqual(a, InventoryAttrs{}) || len(errs) != 0 || hosts["s"] == nil {
		t.Fatalf("enrich = %+v (present %v), %v; want the unsupported entry of a leaf with no inventory", a, ok, errs)
	}
	if n := backend.inventoryReads(); n != 0 {
		t.Fatalf("%d inventory reads on a gated-off leaf, want none", n)
	}

	act := newActivityLeaf(t)
	act.at["a"] = gatherNow.Add(-time.Hour)
	actAttrs := map[string]InventoryAttrs{"a": {}}
	if n := readActivities(context.Background(), actAttrs, map[string]runtime.Provider{"a": gatedOff{act}}, 4); n != 1 || !actAttrs["a"].ActivityKnown {
		t.Fatalf("readActivities on a gated-off leaf read %d names (%+v), want the per-name read", n, actAttrs["a"])
	}

	side := sidecarProvider([]string{"m"}, map[string]map[string]string{"m": {"GC_RESTART_REQUESTED": "1"}})
	sideAttrs := map[string]InventoryAttrs{"m": {}}
	readSidecarRestartMarkers(context.Background(), sideAttrs, map[string]runtime.Provider{"m": gatedSidecar{gatedOff{side}}}, 2)
	if !sideAttrs["m"].RestartRequested {
		t.Fatal("a gated-off sidecar leaf's restart marker was not read")
	}
}

// gatedSidecar is a gated-off leaf whose GetMeta reads a local sidecar.
type gatedSidecar struct{ gatedOff }

func (gatedSidecar) LocalIdentitySidecar() bool { return true }

// Kills (PAR-EXEC §5 item 5, SHOULD-1): a batched agent-process fact
// ignored, which costs one observe fork per name per pass, or taken for a
// name the lane would not probe: an unattributed box (identity null) whose
// script reads its agent dead would read as a zombie. probeProcesses
// answers the names it would probe from the batch, probes the rest, and
// leaves every other name's fact alone.
func TestLaneTakesTheBatchedProcessFact(t *testing.T) {
	backend := newScriptedInventory(map[string]runtime.InventoryEntry{
		"zombie":       {Incarnation: "i1", DeadKnown: true, ProcessKnown: true},
		"live":         {Incarnation: "i2", DeadKnown: true, ProcessKnown: true, ProcessAlive: true},
		"quiet":        {Incarnation: "i3", DeadKnown: true},
		"unattributed": {Incarnation: "i4", DeadKnown: true, ProcessKnown: true},
	})
	for _, name := range []string{"zombie", "live", "quiet", "unattributed"} {
		if err := backend.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	leaf := runtime.NewFakeProfile(runtime.ProfileExecV2, backend)
	l := &runtimeInventoryLane{}
	names := []string{"live", "quiet", "unattributed", "zombie"}
	attrs, hosts, errs := l.enrich(context.Background(), []inventoryBackend{{label: "exec", provider: leaf, names: names}}, []BackendPass{completeBackend("exec", names...)})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, name := range []string{"zombie", "live", "quiet"} {
		a := attrs[name]
		a.Identity = runtimeIdentity{Known: true, ProcessNames: []string{"claude"}}
		attrs[name] = a
	}
	if n := probeProcesses(context.Background(), attrs, hosts, 4); n != 1 {
		t.Fatalf("probeProcesses probed %d names, want only the one the inventory left unknown", n)
	}
	for name, want := range map[string][3]bool{"zombie": {true, true, false}, "live": {true, true, true}, "unattributed": {false, false, false}} {
		a := attrs[name]
		if got := [3]bool{a.ProcessProbed, a.ProcessKnown, a.ProcessAlive}; got != want {
			t.Errorf("%s: probed/known/alive = %v, want %v", name, got, want)
		}
	}
	for _, name := range []string{"zombie", "live", "unattributed"} {
		if got := backend.CountCalls("ObserveLivenessWithError", name); got != 0 {
			t.Errorf("%s: %d liveness probes, want none", name, got)
		}
	}
	obs := RuntimeObservation{}
	applyInventoryAttrs(&obs, attrs["unattributed"], obsTestEpoch)
	if obs.ProcessAlive.Value == ObsNo {
		t.Fatal("an unattributed box's batched dead agent read as a dead process")
	}
}

// Kills (PAR-EXEC §5 item 5): a batched identity that spends the identity
// read budget, or is not taken at all: a Known identity in the inventory
// is the name's identity for that incarnation with no read, even with the
// interval's budget spent; a name without one waits for the budget.
func TestLaneTakesTheBatchedIdentityWithoutTheBudget(t *testing.T) {
	backend := newScriptedInventory(map[string]runtime.InventoryEntry{
		"batched": {Incarnation: "i1", DeadKnown: true, IdentityKnown: true, Identity: map[string]string{"GC_SESSION_ID": "gc-1", "GC_RUNTIME_EPOCH": "2", "GC_INSTANCE_TOKEN": "tok-1", "GT_PROCESS_NAMES": "claude,node"}},
		"unread":  {Incarnation: "i2", DeadKnown: true},
	})
	for _, name := range []string{"batched", "unread"} {
		if err := backend.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	leaf := runtime.NewFakeProfile(runtime.ProfileExecV2, backend)
	clk := &clock.Fake{Time: obsTestEpoch}
	l := &runtimeInventoryLane{clock: clk, interval: time.Minute, identity: map[string]inventoryIdentity{}}
	l.envWindowAt, l.envWindowReads = clk.Now(), inventoryAttributionBudget
	names := []string{"batched", "unread"}
	attrs, hosts, _ := l.enrich(context.Background(), []inventoryBackend{{label: "exec", provider: leaf, names: names}}, []BackendPass{completeBackend("exec", names...)})
	before := backend.CountCalls("GetMeta", "batched")
	reads, _ := l.readIdentities(context.Background(), attrs, hosts)
	if reads != 0 {
		t.Fatalf("readIdentities made %d reads with the budget spent, want none", reads)
	}
	got := attrs["batched"].Identity
	if !got.Known || got.SessionID != "gc-1" || got.Epoch != "2" || got.Token != "tok-1" || !slices.Equal(got.ProcessNames, []string{"claude", "node"}) {
		t.Fatalf("batched identity = %+v, want the inventory's", got)
	}
	if backend.CountCalls("GetMeta", "batched") != before {
		t.Fatal("the batched identity was read again")
	}
	if attrs["unread"].Identity.Known {
		t.Fatalf("unread identity = %+v, want unknown until the budget allows a read", attrs["unread"].Identity)
	}
	if a := attrs["batched"]; a.OwnerState != OwnerUnknown {
		t.Fatalf("owner state %v: the reapers' owner fact stays EnvironmentBatchProvider-only", a.OwnerState)
	}
	if l.identity["batched"].incarnation != "i1" {
		t.Fatalf("lane identity %+v, want the batch recorded for incarnation i1", l.identity["batched"])
	}
}

// Kills (PAR-EXEC §13.5 6, design gap 1): an exec inventory that is
// partial or failed classified complete, or a dead box left out of an
// attested listing, which would prove it gone. Through the production
// seam-backed provider.
func TestLaneClassifiesTheExecInventory(t *testing.T) {
	for _, tc := range []struct {
		name, inventory string
		want            InventoryOutcome
		listed          []string
	}{
		{"complete", `echo '{"complete":true,"sessions":[{"name":"mc-dead","state":"dead","object":{"id":"c","created":"t"}}]}'`, OutcomeComplete, []string{"mc-dead"}},
		{"unprefixed", `echo '{"complete":true,"sessions":[{"name":"other","state":"running","object":{"id":"c","created":"t"}}]}'`, OutcomeComplete, []string{"other"}},
		{"partial", `echo '{"complete":false,"sessions":[{"name":"mc-a","state":"running","object":{"id":"c","created":"t"}}]}'`, OutcomePartial, []string{"mc-a"}},
		{"failed", `exit 1`, OutcomeFailed, nil},
		{"unknown op", `exit 2`, OutcomeFailed, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := writeRPPScript(t, `case "$1" in
  protocol) echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}' ;;
  inventory) `+tc.inventory+` ;;
  *) exit 2 ;;
esac
`)
			leaf := listInventoryBackends(sessionexec.NewSeamBacked(script), "exec").leaves[0]
			b := classifyInventoryBackend(leaf)
			if b.Outcome != tc.want || !slices.Equal(b.Names, tc.listed) {
				t.Fatalf("outcome %v names %v (err %v), want %v %v", b.Outcome, b.Names, leaf.err, tc.want, tc.listed)
			}
		})
	}
}

// plainInventory is a leaf with a batched inventory (entries, or err) and
// no identity read of its own: neither a batched environment, an identity
// observer nor a local sidecar.
type plainInventory struct {
	*runtime.Fake
	entries map[string]runtime.InventoryEntry
	err     error
}

func (p plainInventory) RuntimeInventory(context.Context) (map[string]runtime.InventoryEntry, error) {
	return p.entries, p.err
}

// Kills (PAR-EXEC X3): a batched identity taken from a leaf whose identity
// is not readable, which never promised to seed identity before listing a
// name, so a box listed before its identity would read ownerless.
func TestLaneTakesNoBatchedIdentityFromAnUnreadableLeaf(t *testing.T) {
	leaf := plainInventory{Fake: runtime.NewFake(), entries: map[string]runtime.InventoryEntry{
		"s": {Incarnation: "i1", DeadKnown: true, IdentityKnown: true, Identity: map[string]string{}},
	}}
	if identityReadable(leaf) {
		t.Fatal("fixture: the leaf reads identity")
	}
	l := &runtimeInventoryLane{}
	attrs, _, _ := l.enrich(context.Background(), []inventoryBackend{{label: "x", provider: leaf, names: []string{"s"}}}, []BackendPass{completeBackend("x", "s")})
	if attrs["s"].Identity.Known {
		t.Fatalf("identity %+v from an unreadable leaf, want unknown", attrs["s"].Identity)
	}
	leaf.err = errors.New("inventory failed")
	if attrs, _, errs := l.enrich(context.Background(), []inventoryBackend{{label: "x", provider: leaf, names: []string{"s"}}}, []BackendPass{completeBackend("x", "s")}); len(errs) != 1 || len(attrs) != 0 {
		t.Fatalf("a failed inventory: attrs %+v, errs %v; want no facts and the error", attrs, errs)
	}
}

// flippingLeaf is a leaf whose listing attestation turns on during its
// first listing, as an exec pack's does when its failed handshake recovers
// mid-call: that first listing is blind (list-running's, here nothing),
// and every later one is the complete listing, unless staysBlind keeps
// every listing blind, so a test sees one site's reading of the first one
// without a later listing repairing it.
type flippingLeaf struct {
	*runtime.Fake
	on         *atomic.Bool
	staysBlind bool
}

func newFlippingLeaf(f *runtime.Fake) flippingLeaf {
	return flippingLeaf{Fake: f, on: new(atomic.Bool)}
}

func (f flippingLeaf) ListRunning(prefix string) ([]string, error) {
	if f.on.Swap(true) && !f.staysBlind {
		return f.Fake.ListRunning(prefix)
	}
	return nil, nil
}

func (f flippingLeaf) ListRunningComplete() bool { return f.on.Load() }

// Kills (PAR-EXEC MUST-1, lane): a listing classified complete on an
// attestation read after it, which proves every running name gone.
func TestLaneListingAttestationIsReadAroundTheListing(t *testing.T) {
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	leaf := listInventoryBackends(newFlippingLeaf(fake), "x").leaves[0]
	if b := classifyInventoryBackend(leaf); b.Outcome == OutcomeComplete || b.Attested {
		t.Fatalf("a listing taken before the attestation turned on: %+v, want unattested", b)
	}
}

// Kills (PAR-EXEC MUST-1, unlistedElsewhere): a fall-through backend's
// blind listing read as attested absence.
func TestUnlistedElsewhereReadsAttestationAroundTheListing(t *testing.T) {
	routed, _ := fakeProfile(runtime.ProfileTmux)
	other := runtime.NewFake()
	if err := other.Start(context.Background(), "s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	hop := auto.New(routed, newFlippingLeaf(other))
	hop.SeedRoutes(nil)
	if unlistedElsewhere(context.Background(), hop, "s") {
		t.Fatal("a blind listing taken before the attestation turned on proved s absent")
	}
}

// Kills (PAR-EXEC SHOULD-2): a v2-ready exec leaf read twice per pass. The
// lane's listing carries its inventory (runtime.InventoryLister), so
// enrich makes no second `inventory` call, under no second bound.
func TestLaneReadsTheExecInventoryOncePerPass(t *testing.T) {
	ops := filepath.Join(t.TempDir(), "ops")
	script := writeRPPScript(t, `case "$1" in
  protocol) echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}' ;;
  inventory) echo inventory >> `+ops+`; echo '{"complete":true,"sessions":[{"name":"mc-a","state":"running","object":{"id":"c","created":"t"},"attached":null,"agent":"alive"}]}' ;;
  *) exit 2 ;;
esac
`)
	listing := listInventoryBackends(sessionexec.NewSeamBacked(script), "exec")
	backends := []BackendPass{classifyInventoryBackend(listing.leaves[0])}
	l := &runtimeInventoryLane{}
	attrs, _, errs := l.enrich(context.Background(), listing.leaves, backends)
	if len(errs) != 0 || attrs["mc-a"].Incarnation != "c@t" {
		t.Fatalf("enrich = %+v, %v; want the listing's inventory", attrs, errs)
	}
	data, err := os.ReadFile(ops)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "inventory"); n != 1 {
		t.Fatalf("%d inventory calls for one pass, want 1", n)
	}

	// SHOULD-3: the leaf reports attachment, and its inventory did not
	// answer it (attached: null): attachment is uncertain, not "no".
	obs := RuntimeObservation{}
	applyInventoryAttrs(&obs, attrs["mc-a"], obsTestEpoch)
	if obs.Attached.Value != ObsUnknown || obs.Attached.Reason == obsReasonUnsupported {
		t.Fatalf("attached:null on a reporting leaf = %+v, want unknown, not unsupported", obs.Attached)
	}
}
