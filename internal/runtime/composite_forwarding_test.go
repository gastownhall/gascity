package runtime_test

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// unforwarded is each composite's reviewed list of optional interfaces a
// leaf implements and it does not, by composite then interface. Each entry
// is deliberate, its reason saying what reads the leaf instead, or a gap,
// with its bead. The list only shrinks: a composite that gains one fails until
// its entry is dropped.
var unforwarded = map[string]map[string]struct{ Class, Reason, Bead string }{
	"auto": {
		"FreshByConstruction":          {"deliberate", "a leaf's own read; readRuntime judges it on the routed leaf (cmd/gc/reconcile_effect_runtime.go freshReadable)", ""},
		"FreshActivityProvider":        {"deliberate", "the fresh activity read is read on the routed leaf (runtime.FreshActivityFor)", ""},
		"HeldFileProvider":             {"deliberate", "the held files are read on the routed leaf (runtime.HeldFilesFor)", ""},
		"IdentityObserver":             {"deliberate", "identity is read on the routed leaf (cmd/gc/runtime_inventory_lane.go identityReadable)", ""},
		"IdentitySidecarProvider":      {"deliberate", "identity is read on the routed leaf (cmd/gc/runtime_inventory_lane.go identityReadable)", ""},
		"IdleProbeBudgetProvider":      {"deliberate", "the idle-proof budget is read on the routed leaf (cmd/gc/session_sleep.go idleSleepProbeTimeoutFor)", ""},
		"InterruptNoteProvider":        {"deliberate", "a soft interrupt's note is kept on the routed leaf that waits for it (runtime.NoteInterrupt)", ""},
		"InventoryGate":                {"deliberate", "the inventory lane reads each backend's leaf through Backends, gated by runtime.InventoryFor on the leaf (cmd/gc/runtime_inventory_lane.go)", ""},
		"InventoryLister":              {"deliberate", "the inventory lane lists each backend's leaf through Backends, and takes its inventory with the listing there (cmd/gc/runtime_inventory_lane.go)", ""},
		"InventoryProvider":            {"deliberate", "the inventory lane reads each backend's leaf through Backends (cmd/gc/runtime_inventory_lane.go)", ""},
		"LocalTranscriptProvider":      {"deliberate", "transcript locality is read on the routed leaf (runtime.LocalTranscriptsFor)", ""},
		"ProgramCaptureHealer":         {"deliberate", "the controller heals capture on the routed leaf (runtime.ProgramCaptureHealerFor)", ""},
		"ProgramStatusHoldProvider":    {"deliberate", "the mid-turn hold's fresh read is taken on the routed leaf (runtime.ProgramStatusHoldFor)", ""},
		"ProgramStatusProvider":        {"deliberate", "the program-status reading is read on the routed leaf (runtime.ProgramStatusFor)", ""},
		"ProgramStatusShadowProvider":  {"deliberate", "the shadow sink is read on the routed leaf with its reading (runtime.ProgramStatusShadowFor)", ""},
		"StartupNudgeHandbackProvider": {"deliberate", "a start's handed-back nudge is taken on the leaf that started it (runtime.TakeStartupNudgeHandback)", ""},
		"TurnRunningProvider":          {"deliberate", "the live turn reading is read on the routed leaf (runtime.TurnRunningFor)", ""},
		"V2ReadinessReporter":          {"deliberate", "v2 readiness is a leaf's handshake, judged per leaf (PAR-EXEC C0.8 walks inventoryLeaves)", ""},
		"SharedListing":                {"deliberate", "runtime.ListRunningShared walks Backends to every leaf (gc stop's confirmation, cmd/gc/cmd_stop.go)", ""},
	},
	"hybrid": {
		"FreshByConstruction":          {"deliberate", "as auto's", ""},
		"FreshActivityProvider":        {"deliberate", "as auto's", ""},
		"HeldFileProvider":             {"deliberate", "as auto's", ""},
		"IdentityObserver":             {"deliberate", "as auto's", ""},
		"IdentitySidecarProvider":      {"deliberate", "as auto's", ""},
		"IdleProbeBudgetProvider":      {"deliberate", "as auto's", ""},
		"InterruptNoteProvider":        {"deliberate", "as auto's", ""},
		"InventoryGate":                {"deliberate", "as auto's", ""},
		"InventoryLister":              {"deliberate", "as auto's", ""},
		"InventoryProvider":            {"deliberate", "as auto's", ""},
		"SharedListing":                {"deliberate", "as auto's", ""},
		"LocalTranscriptProvider":      {"deliberate", "as auto's", ""},
		"ProgramCaptureHealer":         {"deliberate", "as auto's", ""},
		"ProgramStatusHoldProvider":    {"deliberate", "as auto's", ""},
		"ProgramStatusProvider":        {"deliberate", "as auto's", ""},
		"ProgramStatusShadowProvider":  {"deliberate", "as auto's", ""},
		"StartupNudgeHandbackProvider": {"deliberate", "as auto's", ""},
		"TurnRunningProvider":          {"deliberate", "as auto's", ""},
		"V2ReadinessReporter":          {"deliberate", "as auto's", ""},
		"FreshLivenessObserver":        {"deliberate", "hybrid's read is its leaves' cached one, so a hop through it proves absence only by its attested listing (cmd/gc/reconcile_effect_runtime.go unlistedElsewhere)", ""},
		"TransportCapabilityProvider":  {"deliberate", "production's hybrid is tmux over k8s (cmd/gc/providers.go newHybridProvider), and neither has it", ""},
	},
}

// Kills a composite that drops a capability a leaf has, unreviewed: auto and
// hybrid implement every optional interface any production backend does,
// but for their reviewed entries, and no entry names one they implement.
func TestCompositesForwardEveryLeafCapability(t *testing.T) {
	var leaves []string
	for _, p := range productionBackends(t) {
		for _, name := range implemented(p) {
			if !slices.Contains(leaves, name) {
				leaves = append(leaves, name)
			}
		}
	}
	slices.Sort(leaves)
	f := func() runtime.Provider { return runtime.NewFake() }
	composites := map[string]runtime.Provider{
		"auto":   auto.New(f(), f()),
		"hybrid": hybrid.New(f(), f(), func(string) bool { return false }),
	}
	for name, c := range composites {
		has := implemented(c)
		for _, iface := range leaves {
			e, listed := unforwarded[name][iface]
			switch forwards := slices.Contains(has, iface); {
			case !forwards && !listed:
				t.Errorf("%s does not implement %s, which a leaf does: forward it, or list it with a reason", name, iface)
			case forwards && listed:
				t.Errorf("%s implements %s now: drop its unforwarded entry", name, iface)
			case listed && (e.Reason == "" || e.Class != "deliberate" && e.Class != "gap" || (e.Bead == "") == (e.Class == "gap")):
				t.Errorf("%s's %s entry: a class, a reason, and a bead exactly when it is a gap", name, iface)
			}
		}
		for iface := range unforwarded[name] {
			if !slices.Contains(leaves, iface) {
				t.Errorf("%s's entry %s: no leaf implements it", name, iface)
			}
		}
	}
}

// Kills a composite that reads the exec leaf's scanner methods as the
// capability: every exec provider implements the scanner, but one whose pack
// does not share the host cannot scan, and a composite over it as its default
// (auto) or local (hybrid) backend must read as having no scanner, exactly
// as it did before exec implemented one; over a leaf that scans, it scans.
func TestCompositesReadAScannerlessExecLeafAsNoScanner(t *testing.T) {
	full := func() runtime.FullFake { return runtime.FullFake{Fake: runtime.NewFake()} }
	for _, tc := range []struct {
		leaf runtime.Profile
		want bool
	}{{runtime.ProfileExec, false}, {runtime.ProfileExecV2, false}, {runtime.ProfileSubprocess, true}} {
		leaf := func() runtime.Provider { return runtime.NewFakeProfile(tc.leaf, full()) }
		composites := map[string]runtime.Provider{
			"auto":   auto.New(leaf(), runtime.NewFakeProfile(runtime.ProfileACP, full())),
			"hybrid": hybrid.New(leaf(), runtime.NewFakeProfile(runtime.ProfileK8s, full()), func(string) bool { return false }),
		}
		for name, c := range composites {
			if _, ok := runtime.AsProcessTableScanner(c); ok != tc.want {
				t.Errorf("%s over a %s leaf: AsProcessTableScanner ok = %t, want %t", name, tc.leaf, ok, tc.want)
			}
		}
	}
}
