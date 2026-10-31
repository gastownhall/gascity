package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Program-status capture heal (OSC 7501 design §5.4) runs on the runtime
// inventory lane, after each published pass, in the controller only. While
// the city switch is shadow or on, each listed session whose reading calls
// for it (programCaptureHealNeeded) is healed through its routed leaf's
// runtime.ProgramCaptureHealer toward the capture its config declares. While
// the switch is off, the steady state reads nothing: one clean disarm sweep
// per config load (programCaptureDisarmer) takes down whatever capture an
// earlier switch left armed, a failed sweep running again at the next pass.
// The leaf reads under the switch the controller published with the config
// (publishProgramStatusMode), so on->shadow, shadow->on and off->on heal
// from readings taken under the new switch.

// programCaptureDisarmer is a leaf backend's disarm sweep for a city whose
// program-status switch is off: tmux's DisarmProgramCaptures.
type programCaptureDisarmer interface {
	DisarmProgramCaptures() (int, error)
}

// programCaptureHealTally is what one pass's heal did: the actions taken,
// by action, and the heals that failed.
type programCaptureHealTally struct {
	actions map[runtime.ProgramCaptureAction]int
	swept   int
	errs    []string
}

func (t *programCaptureHealTally) add(action runtime.ProgramCaptureAction) {
	if t.actions == nil {
		t.actions = make(map[runtime.ProgramCaptureAction]int)
	}
	t.actions[action]++
}

// acted reports whether heal changed any pane or failed, which the pass
// trace records.
func (t programCaptureHealTally) acted() bool {
	return t.actions[runtime.ProgramCaptureArmed] > 0 || t.actions[runtime.ProgramCaptureDisarmed] > 0 || t.swept > 0 || len(t.errs) > 0
}

// String renders the tally for the pass trace: "armed=1,deferred=2,swept=0;
// <errors>", "" when heal did nothing.
func (t programCaptureHealTally) String() string {
	if len(t.actions) == 0 && t.swept == 0 && len(t.errs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(t.actions)+1)
	for action, n := range t.actions {
		parts = append(parts, fmt.Sprintf("%s=%d", action, n))
	}
	sort.Strings(parts)
	parts = append(parts, fmt.Sprintf("swept=%d", t.swept))
	out := strings.Join(parts, ",")
	if len(t.errs) > 0 {
		out += "; " + strings.Join(t.errs, "; ")
	}
	return out
}

// programCaptureHealNeeded reports whether a session's reading, under the
// capture its config declares (want), calls for a heal: a declared session
// whose pane is not armed or whose reader is lost, or an undeclared session
// whose pane still carries our capture. Unread and exited readings, and a
// declared capture that is working, call for nothing.
func programCaptureHealNeeded(r runtime.ProgramStatus, want runtime.ProgramStatusArm) bool {
	switch r.Validity {
	case runtime.ProgramStatusNotArmed:
		return want.Capture
	case runtime.ProgramStatusReaderLost:
		return true
	case runtime.ProgramStatusValid, runtime.ProgramStatusSilent, runtime.ProgramStatusCleared:
		return !want.Capture
	}
	return false
}

// programCaptureWant is the capture session info's config declares: its
// resolved provider's program-status binding under the city switch and its
// transport (config.ProgramStatusArmFor, the derivation every start uses).
func programCaptureWant(cfg *config.City, info session.Info) runtime.ProgramStatusArm {
	resolved, _ := resolveWorkerRuntimeProviderWithConfig(cfg, info, "")
	arm, _ := config.ProgramStatusArmFor(cfg, resolved, info.Transport)
	return arm
}

// healProgramCaptureReadings heals, through sp's routed leaves, each session
// in programs whose reading calls for it under the capture wants declares
// for it. A session wants does not know is left alone; when wants fails,
// nothing is healed.
func healProgramCaptureReadings(sp runtime.Provider, programs map[string]runtime.ProgramStatus, wants func() (map[string]runtime.ProgramStatusArm, error)) programCaptureHealTally {
	var tally programCaptureHealTally
	var names []string
	for name, r := range programs {
		if r.Validity != runtime.ProgramStatusUnread && r.Validity != runtime.ProgramStatusExited {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return tally
	}
	sort.Strings(names)
	declared, err := wants()
	if err != nil {
		tally.errs = append(tally.errs, fmt.Sprintf("reading session capture declarations: %v", err))
		return tally
	}
	for _, name := range names {
		want, known := declared[name]
		if !known || !programCaptureHealNeeded(programs[name], want) {
			continue
		}
		h, ok := runtime.ProgramCaptureHealerFor(sp, name)
		if !ok {
			continue
		}
		action, err := h.HealProgramCapture(name, want)
		if err != nil {
			tally.errs = append(tally.errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		tally.add(action)
	}
	return tally
}

// disarmProgramCaptures runs the disarm sweep on every leaf backend of sp
// that has one.
func disarmProgramCaptures(sp runtime.Provider) programCaptureHealTally {
	var tally programCaptureHealTally
	for _, leaf := range inventoryLeaves(sp, "") {
		d, ok := leaf.provider.(programCaptureDisarmer)
		if !ok {
			continue
		}
		n, err := d.DisarmProgramCaptures()
		tally.swept += n
		if err != nil {
			tally.errs = append(tally.errs, fmt.Sprintf("disarm sweep %s: %v", inventoryLabelForTrace(leaf.label), err))
		}
	}
	return tally
}

// healProgramCaptures is the pass's heal step (see the file comment). The
// lane's passMu is held. Failures are logged and recorded in the pass trace;
// the leaf's per-pane backoff paces retries.
func (cr *CityRuntime) healProgramCaptures(lane *runtimeInventoryLane, cfg *config.City, sp runtime.Provider, report *inventoryPassReport) {
	if cfg == nil || sp == nil {
		return
	}
	if cfg.Session.ProgramStatusMode() == runtime.ProgramStatusOff {
		if lane.captureSweptCfg != cfg {
			report.captureHeals = disarmProgramCaptures(sp)
			// Only a sweep that finished clean is done for this load: a
			// failed one runs again at the next pass.
			if len(report.captureHeals.errs) == 0 {
				lane.captureSweptCfg = cfg
			}
		}
	} else {
		lane.captureSweptCfg = nil
		report.captureHeals = healProgramCaptureReadings(sp, report.programs, func() (map[string]runtime.ProgramStatusArm, error) {
			return cr.programCaptureWants(cfg)
		})
	}
	for _, e := range report.captureHeals.errs {
		fmt.Fprintf(lane.stderr, "%s: program-status capture heal: %s\n", lane.logPrefix, e) //nolint:errcheck // best-effort stderr
	}
}

// programCaptureWants is the capture cfg declares for every open session,
// by runtime session name, read from the session beads.
func (cr *CityRuntime) programCaptureWants(cfg *config.City) (map[string]runtime.ProgramStatusArm, error) {
	store := resolveSessionStore(cr.storageRoutes, cr.cityBeadStore(), cfg, cr.cityPath, cr.rec)
	if store == nil {
		return nil, errors.New("no session bead store")
	}
	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		return nil, fmt.Errorf("loading session beads: %w", err)
	}
	return programCaptureWantsFrom(cfg, snapshot.OpenInfos()), nil
}

// programCaptureWantsFrom is the capture cfg declares for each info with a
// runtime session name.
func programCaptureWantsFrom(cfg *config.City, infos []session.Info) map[string]runtime.ProgramStatusArm {
	wants := make(map[string]runtime.ProgramStatusArm, len(infos))
	for _, info := range infos {
		if name := strings.TrimSpace(info.SessionName); name != "" {
			wants[name] = programCaptureWant(cfg, info)
		}
	}
	return wants
}
