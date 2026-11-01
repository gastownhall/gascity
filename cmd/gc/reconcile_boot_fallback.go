package main

// The O6 safety net: a default-resolved v2 that its boot would refuse falls
// back to legacy.
//
// An unset [daemon] session_reconciler resolves from config alone
// (v2DefaultFallbacks). Two v2 boot checks read the stores instead: C0.7,
// whether the sessions and graph class stores can fence (v2CapabilityVerdict),
// and C11, whether the sessions store holds enterprise-era rows v2 cannot run
// over (readV2SessionMigration). Until now either refusal stopped the city,
// which is right for an operator who asked for v2 and wrong for one who asked
// for nothing: they get a city that will not start because of a default they
// never chose. `gc storage split` moves a city to v2 in the same step, so the
// first boot after it is exactly where this lands.
//
// So, before the latch is handed to the socket or the runtime, a controller
// whose resolution is the v2 DEFAULT probes both checks against its stores. A
// definite refusal falls back to legacy for this boot, loudly: a boot warning
// naming the refusal, and a record (.gc/reconciler-v2-fallback.json) that
// `gc doctor` reports. A read that fails is no verdict, so it leaves v2 in
// place and the boot's own checks retry as they always have. An explicit
// session_reconciler = "v2" is never probed and still refuses.
//
// The probe costs one extra open of the stores and one census, on default-v2
// boots only. Each boot probes again, so the fallback ends by itself once the
// rows are repaired, and the record is removed then.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// reconcilerV2FallbackRecordName is the record a boot that fell back writes.
const reconcilerV2FallbackRecordName = "reconciler-v2-fallback.json"

// reconcilerV2FallbackRecord is what a fallback boot records for doctor.
type reconcilerV2FallbackRecord struct {
	Refusal string `json:"refusal"`
	At      string `json:"at"`
}

func reconcilerV2FallbackRecordPath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", reconcilerV2FallbackRecordName)
}

// readReconcilerV2FallbackRecord reads the record; ok is false without one.
func readReconcilerV2FallbackRecord(cityPath string) (reconcilerV2FallbackRecord, bool) {
	data, err := os.ReadFile(reconcilerV2FallbackRecordPath(cityPath))
	if err != nil {
		return reconcilerV2FallbackRecord{}, false
	}
	var rec reconcilerV2FallbackRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return reconcilerV2FallbackRecord{Refusal: "(the fallback record is unreadable: " + err.Error() + ")"}, true
	}
	return rec, true
}

// v2DefaultBootProbe returns the C0.7 or C11 refusal a v2 boot of the city
// would hit, or nil when it would boot or when nothing could be decided. A
// test seam.
var v2DefaultBootProbe = v2DefaultBootRefusal

// v2BootProbeVerdict is C0.7's boot verdict over the capabilities the probe
// read; a test seam. It takes the capabilities, not the stores, so no raw
// store crosses a package-level seam (storeclass).
var v2BootProbeVerdict = v2CapabilityVerdict

// v2DefaultBootRefusal opens the stores the v2 boot reads, as the runtime
// would, and runs C0.7 and C11 over them.
//
// The storage boot gate it runs gets the city's event recorder: a genesis it
// performs is the city's real genesis, and its outcome event must not be lost.
func v2DefaultBootRefusal(cityPath, cityName string, cfg *config.City, rec events.Recorder) error {
	// The work store the migration and the boot census open (its seam lives
	// with the migration engine).
	work, err := openInfraMigrationSource(cityPath)
	if err != nil {
		return nil // no verdict: the runtime reports its own open
	}
	defer closeBeadStoreHandle(work) //nolint:errcheck // best-effort close
	routes, err := storageBootGate(cityPath, cfg, "gc v2 boot probe", rec, io.Discard)
	if err != nil {
		return nil // no verdict: the runtime's own gate refuses, with the reason
	}
	if routes != nil {
		defer routes.close() //nolint:errcheck // best-effort close
	}
	sessions := resolveSessionStore(routes, work, cfg, cityPath, nil)
	graph := resolveGraphStore(routes, work, cfg, cityPath, nil)
	if verdict := v2BootProbeVerdict(v2ClassStoreCapabilities(sessions, graph, false)); verdict != nil {
		var unread *v2CapabilityUnreadError
		if !errors.As(verdict, &unread) {
			return verdict
		}
	}
	rigs := map[string]beads.Store{}
	defer func() {
		for _, store := range rigs {
			_ = closeBeadStoreHandle(store)
		}
	}()
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		store, err := openStoreAtForCity(rig.Path, cityPath)
		if err != nil {
			return nil // an unread leg is no verdict
		}
		rigs[rig.Name] = store
	}
	m, err := readV2SessionMigration(cityPath, cityName, cfg, sessions, rigs)
	if err != nil {
		return nil
	}
	return m.refusal()
}

// applyV2DefaultBootFallback probes a default-v2 wiring and demotes it to
// legacy on a refusal. It must run before the wiring reaches the controller
// socket or the city runtime, and with the controller lock held. It returns
// the refusal it fell back over, or nil.
func applyV2DefaultBootFallback(w *controllerWiring, cityPath, cityName string, cfg *config.City, rec events.Recorder, bootLog io.Writer) error {
	if w == nil || w.mode != reconcilerV2 || w.reason != reconcilerDefaultV2 {
		return nil
	}
	refusal := v2DefaultBootProbe(cityPath, cityName, cfg, rec)
	if refusal == nil {
		_ = os.Remove(reconcilerV2FallbackRecordPath(cityPath))
		return nil
	}
	w.fallBackToLegacy()
	fmt.Fprintf(bootLog, "WARNING: session reconciler: the default v2 would refuse to boot this city (%v); FALLING BACK TO LEGACY for this boot. Repair what is named (gc doctor lists it), or set session_reconciler = \"legacy\" to keep legacy and silence this warning; an explicit session_reconciler = \"v2\" still refuses\n", refusal) //nolint:errcheck // best-effort log
	data, err := json.Marshal(reconcilerV2FallbackRecord{Refusal: refusal.Error(), At: time.Now().UTC().Format(time.RFC3339)})
	if err == nil {
		err = os.WriteFile(reconcilerV2FallbackRecordPath(cityPath), data, 0o644)
	}
	if err != nil {
		fmt.Fprintf(bootLog, "warning: session reconciler: recording the v2 fallback for gc doctor: %v\n", err) //nolint:errcheck // best-effort log
	}
	return refusal
}

// fallBackToLegacy demotes an unused v2 wiring to legacy: no planner, and its
// wake routing to the legacy channels. Only valid before the socket or a city
// runtime has seen the wiring.
func (w *controllerWiring) fallBackToLegacy() {
	w.mode = reconcilerLegacy
	w.reason = reconcilerFallbackLegacy
	w.fellBack = true
	w.v2 = nil
	// The wake newControllerWiringLoggingTo built, made legacy in place: the
	// same channels, no planner to route to.
	w.wake.mode = reconcilerLegacy
	w.wake.planner, w.wake.waitDepClosed, w.wake.wakeReads = nil, nil, nil
}

// newStandaloneControllerWiring is runController's latch (gc start
// --foreground): the wiring, with the O6 fallback applied before the
// controller socket publishes it.
func newStandaloneControllerWiring(cityPath string, cfg *config.City, rec events.Recorder, stderr io.Writer) (*controllerWiring, error) {
	w, err := newControllerWiring(cfg, reconcilerModeLookupEnv, stderr)
	if err != nil {
		return nil, err
	}
	_ = applyV2DefaultBootFallback(w, cityPath, loadedCityName(cfg, cityPath), cfg, rec, stderr)
	return w, nil
}
