package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// storageSplitCheck is the city's split verdict (S5 §7.1): one line per
// city, with storage-rigs as the per-rig detail. It is read-only. The city
// binding's verdict is `gc storage status`'s own (doStorageStatusCity), so
// the two cannot disagree. On a city with no [storage] it stays OK until the
// default flips (O11) and reports only what is cheap and opens nothing: the
// reconciler the split city resolves to and whether the provider is up. The
// full rehearsal is `gc storage preflight --split`; doctor runs it nowhere,
// so a check that times out leaves no window or store open behind it.
type storageSplitCheck struct {
	cfg      *config.City
	cityPath string
}

func newStorageSplitCheck(cfg *config.City, cityPath string) *storageSplitCheck {
	return &storageSplitCheck{cfg: cfg, cityPath: cityPath}
}

func (c *storageSplitCheck) Name() string                   { return "storage-split" }
func (c *storageSplitCheck) CanFix() bool                   { return false }
func (c *storageSplitCheck) WarmupEligible() bool           { return false }
func (c *storageSplitCheck) Fix(*doctor.CheckContext) error { return nil }

// storageSplitLegacySatellites are the per-class SQLite stores the five-store
// layout kept under the binding root (MC's .gc/store) before it was retired.
var storageSplitLegacySatellites = []string{"sessions.db", "messaging.db", "orders.db", "nudges.db"}

// storageSplitFindings collects one run's verdicts.
type storageSplitFindings struct {
	errs, warns, details []string
}

func (c *storageSplitCheck) Run(*doctor.CheckContext) *doctor.CheckResult {
	var f storageSplitFindings
	c.versionFloor(&f)
	var ok string
	switch stage, staged, err := readRemoteStage(c.cityPath); {
	case err != nil:
		f.errs = append(f.errs, err.Error())
	case staged:
		ok = fmt.Sprintf("remote stage of %s (%s): its controller serves the split; this stage's binding is not checked from here", stage.ControllerCity, stage.ControllerPath)
	case c.cfg == nil || c.cfg.Storage == nil:
		ok = c.unsplit(&f)
	default:
		ok = c.split(&f)
	}
	switch {
	case len(f.errs) > 0:
		return errorCheck(c.Name(), strings.Join(f.errs, "; "), "run `"+storageStatusInstruction()+"` and follow its blocking invariant", f.details)
	case len(f.warns) > 0:
		return warnCheck(c.Name(), strings.Join(f.warns, "; "), "see the details; `gc doctor --check storage-rigs` details each rig", f.details)
	}
	r := okCheck(c.Name(), ok)
	r.Details = f.details
	return r
}

// versionFloor holds the city's gc version floor against this gc and the gc
// on an agent session's PATH (B10).
func (c *storageSplitCheck) versionFloor(f *storageSplitFindings) {
	floor, present, err := readMinGCVersion(c.cityPath)
	switch {
	case err != nil:
		f.errs = append(f.errs, err.Error())
		return
	case !present:
		return
	}
	if gcVersionBelowFloor(version, floor) {
		f.errs = append(f.errs, fmt.Sprintf("this gc is %s, below the city's floor %s (%s)", version, floor, minGCVersionPath(c.cityPath)))
	}
	path, running, err := minGCVersionPathProbe(c.cfg)
	switch {
	case err != nil:
		f.warns = append(f.warns, fmt.Sprintf("the gc on the session PATH could not report its version (%v), so it is not compared with the floor %s", err, floor))
	case path == "":
		f.details = append(f.details, "no gc on the session PATH to compare with the floor "+floor)
	case gcVersionBelowFloor(running, floor):
		f.errs = append(f.errs, fmt.Sprintf("the gc on the session PATH (%s) is %s, below the city's floor %s: agents running it do not read the split", path, running, floor))
	default:
		if _, ok := gcVersionCore(running); !ok {
			f.warns = append(f.warns, fmt.Sprintf("the gc on the session PATH (%s) reports version %s, which cannot be compared with the floor %s", path, running, floor))
		}
		f.details = append(f.details, fmt.Sprintf("session PATH gc %s: %s; floor %s", path, running, floor))
	}
}

// unsplit is the verdict of a city with no [storage].
func (c *storageSplitCheck) unsplit(f *storageSplitFindings) string {
	if blocked, held := revertHoldingNote(storageSplitNone, c.cityPath); held {
		f.errs = append(f.errs, preflightAdviceDetail(blocked))
		return ""
	}
	if c.cfg == nil {
		return "all on work"
	}
	split := splitHypotheticalConfig(c.cfg)
	_, resolved := doctorReconcilerMode(split, c.cityPath, nil)
	f.details = append(f.details, "after the split, "+resolved)
	provider := "up"
	if why, _ := splitStoreReadGate(c.cityPath, c.cfg); why != "" {
		provider = why
	}
	f.details = append(f.details, "provider: "+provider)
	return fmt.Sprintf("all on work; `gc storage %s --%s` rehearses the split", storagePreflightVerb, storagePreflightSplitFlag)
}

// split is the verdict of a city that authors [storage].
func (c *storageSplitCheck) split(f *storageSplitFindings) string {
	var out, errOut bytes.Buffer
	if doStorageStatusCity(storageOperatorRequest{CityPath: c.cityPath, Cfg: c.cfg}, &out, &errOut) != 0 {
		// A stranded city whose binding names no id twice serves degraded
		// (infraMigrationReport.servesDegraded) rather than refusing, and the
		// error says so: nothing stops it any more, so this is where an
		// operator who does not watch the event stream finds out.
		if serving := storageStatusServingLine(out.String()); strings.HasPrefix(serving, "degraded") {
			f.errs = append(f.errs, fmt.Sprintf("the city serves %s: `%s` exits non-zero and names the repair", serving, storageStatusInstruction()))
		} else {
			f.errs = append(f.errs, fmt.Sprintf("the city binding is not serving: `%s` exits non-zero", storageStatusInstruction()))
		}
		f.details = append(f.details, storageSplitLines(out.String(), errOut.String())...)
		return ""
	}
	target, ok, err := resolveInfraBindingTarget(c.cityPath, c.cfg)
	if err != nil || !ok {
		// Status cleared it: the born-split discipline or every class on work.
		return "the city's [storage] serves as `" + storageStatusInstruction() + "` reports"
	}
	if name, remote := cityRunsRemoteSessions(c.cfg, c.cityPath); remote {
		if storageAttestationRecorded(c.cityPath, storageHazardRemoteRuntime) {
			f.warns = append(f.warns, fmt.Sprintf("split on the remote runtime %s under the recorded --%s=%s attestation: remote sessions refuse infrastructure-class commands", name, storageForceFlag, storageHazardRemoteRuntime))
		} else {
			f.errs = append(f.errs, fmt.Sprintf("split on the remote runtime %s with no --%s=%s attestation recorded", name, storageForceFlag, storageHazardRemoteRuntime))
		}
	}
	if _, recorded, err := readMarkerWorkStoreIdentity(target.MarkerPath()); err == nil && !recorded {
		f.warns = append(f.warns, "the convergence marker records no work-store identity (a marker an earlier build wrote); the next converged boot records it")
	}
	if manifest, recorded, err := readInfraCopyManifestRecord(target); err == nil && recorded {
		ids := make([]string, 0, manifest.Len())
		for id := range manifest.IDs {
			ids = append(ids, id)
		}
		if plan, err := planCityIDFloor(c.cityPath, ids); err == nil && plan.below() {
			f.warns = append(f.warns, cityIDFloorBelowWarning(plan))
		}
	}
	awaiting := 0
	for _, r := range censusRigs(c.cityPath, c.cfg, target, false) {
		if r.blocking() {
			awaiting++
		}
	}
	if awaiting > 0 {
		f.warns = append(f.warns, fmt.Sprintf("%d rig(s) await import or hold findings (storage-rigs)", awaiting))
	}
	root := target.Root
	var satellites []string
	for _, name := range storageSplitLegacySatellites {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			satellites = append(satellites, name)
		}
	}
	if len(satellites) > 0 {
		f.warns = append(f.warns, fmt.Sprintf("legacy per-class stores remain in %s (%s), which nothing reads", root, strings.Join(satellites, ", ")))
	}
	if _, err := os.Stat(filepath.Join(c.cityPath, ".gc", "infra", ".beads")); err == nil {
		f.warns = append(f.warns, "a legacy combined infrastructure store remains at .gc/infra/.beads, which nothing reads")
	}
	return "split, converged, and serving"
}

// storageAttestationRecorded reports whether the attestation log records
// hazard.
func storageAttestationRecorded(cityPath, hazard string) bool {
	data, err := os.ReadFile(filepath.Join(cityPath, ".gc", storageAttestationLog))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "#gc:attested" && fields[1] == hazard {
			return true
		}
	}
	return false
}

// storageStatusServingLine is the value of the "serving:" line a stranded
// city's `gc storage status` prints (infraStrandedServingLine), or "".
func storageStatusServingLine(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "serving:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// storageSplitLines is a report's non-empty lines, stdout then stderr.
func storageSplitLines(stdout, stderr string) []string {
	var out []string
	for _, text := range []string{stdout, stderr} {
		for _, line := range strings.Split(text, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				out = append(out, line)
			}
		}
	}
	return out
}

// cityIDFloorBelowWarning is the finding for a city work store whose floor is
// under what its carried ids need, with the command that raises it, or for
// hosted bd the out-of-band setting (O10).
func cityIDFloorBelowWarning(plan cityIDFloorPlan) string {
	below := fmt.Sprintf("the city work store's %s is %d, below the %d the carried ids need", bdMinHashLengthKey, plan.Current, plan.Need)
	if plan.Hosted != "" {
		return fmt.Sprintf("%s: it is hosted bd at %s, so set %s=%d (and %s at least %d) on the hosted project through the bd serve admin path, out of band (owner decision O10)", below, plan.Hosted, bdMinHashLengthKey, plan.Need, bdMaxHashLengthKey, plan.Need)
	}
	return fmt.Sprintf("%s: run `gc storage %s --%s` with the city stopped", below, storageRepairSequenceVerb, storageIDFloorFlag)
}
