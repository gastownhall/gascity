package main

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// v2DemandMigrationsCheck counts the rows each of legacy's four demand-pass
// migration repairs would write, per leg (CONTRACT C0.4, as amended for C4).
// Under v2 those repairs are this check's --fix, run before cutover, and the
// counts decide which of them may leave the lane.
//
// The collections are read live as the demand pass reads them, and the repairs
// are legacy's own functions, unchanged and in legacy order. A dry run hands
// them recordingWriteStore stand-ins and writes nothing; --fix hands them the
// real stores. A non-zero count is a warning under v2 and OK under legacy,
// whose demand pass applies these repairs itself every tick.
type v2DemandMigrationsCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newV2DemandMigrationsCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *v2DemandMigrationsCheck {
	return &v2DemandMigrationsCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

// Name implements doctor.Check.
func (c *v2DemandMigrationsCheck) Name() string { return "v2-demand-migrations" }

// CanFix implements doctor.Check.
func (c *v2DemandMigrationsCheck) CanFix() bool { return true }

// WarmupEligible implements doctor.Check: a sweep costs a demand pass of reads.
func (c *v2DemandMigrationsCheck) WarmupEligible() bool { return false }

// Fix implements doctor.Check: the repairs against the real stores. Like
// legacy's tick, it logs a failed write and goes on; the runner's verifying
// dry run reports what is left.
func (c *v2DemandMigrationsCheck) Fix(ctx *doctor.CheckContext) error {
	out := ctx.Output
	if out == nil {
		out = io.Discard
	}
	c.sweep(true, out)
	return nil
}

// Run implements doctor.Check.
func (c *v2DemandMigrationsCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	var log bytes.Buffer
	s := c.sweep(false, &log)
	perRepair, total := map[string]int{}, 0
	var details, parts []string
	for k, n := range s.counts {
		perRepair[k.repair] += n
		total += n
		details = append(details, fmt.Sprintf("%s %s: %d", k.repair, k.leg, n))
	}
	sort.Strings(details)
	for _, repair := range demandMigrationRepairs {
		parts = append(parts, fmt.Sprintf("%s=%d", repair, perRepair[repair]))
	}
	if logged := strings.TrimSpace(log.String()); logged != "" {
		s.problems = append(s.problems, strings.Split(logged, "\n")...)
	}
	details = append(details, s.problems...)
	msg := fmt.Sprintf("%d row(s) need a legacy demand migration repair: %s", total, strings.Join(parts, " "))
	if len(s.problems) > 0 {
		msg += fmt.Sprintf("; %d read problem(s), counts are a lower bound", len(s.problems))
	}
	mode, _, _ := c.cfg.Daemon.SessionReconcilerMode()
	switch {
	case len(s.problems) > 0, total > 0 && mode == config.SessionReconcilerV2:
		return warnCheck(c.Name(), msg, "run gc doctor --check v2-demand-migrations --fix", details)
	case total > 0:
		msg += " (the legacy demand pass applies these every tick)"
	}
	r := okCheck(c.Name(), msg)
	r.Details = details
	return r
}

// demandMigrationRepairs names the four repairs in the order the sweep runs them.
var demandMigrationRepairs = []string{
	"repairPoolSlotWorkDirClobber",
	"canonicalizeLegacyBoundAssignedWork",
	"canonicalizeLegacyBoundUnassignedRoutedWork",
	"collapseSlotSuffixedRoutedWork",
}

type demandMigrationKey struct{ repair, leg string }

type demandMigrationSweep struct {
	counts   map[demandMigrationKey]int
	problems []string
}

// sweep runs legacy's demand-pass migration repairs over the city's work legs
// as runBackstopDemandRepairs does, without the two writers that stay lane
// steps (the session stamp and the control-dispatcher route repair). With fix
// false each repair writes into a recordingWriteStore per row and the writes
// are counted; with fix true the repairs get the real stores.
func (c *v2DemandMigrationsCheck) sweep(fix bool, stderr io.Writer) demandMigrationSweep {
	s := demandMigrationSweep{counts: map[demandMigrationKey]int{}}
	city, err := c.newStore(c.cityPath)
	if err != nil {
		s.problems = append(s.problems, fmt.Sprintf("city store: %v", err))
		return s
	}
	suspended := buildSuspendedRigPathsForCity(c.cfg, c.cityPath)
	rigs := map[string]beads.Store{}
	for _, rig := range c.cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" || suspended[filepath.Clean(rig.Path)] {
			continue
		}
		store, err := c.newStore(rig.Path)
		if err != nil {
			s.problems = append(s.problems, fmt.Sprintf("rig %s store: %v", rig.Name, err))
			continue
		}
		rigs[rig.Name] = store
	}
	sessions, err := loadSessionBeadSnapshot(cliSessionStore(city, c.cfg, c.cityPath))
	if err != nil {
		// canonicalizeLegacyBoundAssignedWork then refuses to run, counting 0.
		s.problems = append(s.problems, fmt.Sprintf("session snapshot: %v", err))
		sessions = newSessionBeadSnapshotWithError(err)
	}
	writes := func(repair string, stores []beads.Store, refs []string) []beads.Store {
		if fix {
			return stores
		}
		out := make([]beads.Store, len(stores))
		for i, store := range stores {
			if store != nil {
				out[i] = recordingWriteStore{counts: s.counts, key: demandMigrationKey{repair, demandMigrationLeg(refs[i])}}
			}
		}
		return out
	}
	assigned, assignedStores, assignedRefs, _, assignedPartial := collectAssignedWorkBeadsWithStores(c.cityPath, c.cfg, city, rigs, suspended, sessions, newReadyDemandCache())
	if assignedPartial {
		s.problems = append(s.problems, "assigned collection partial")
	}
	repairPoolSlotWorkDirClobber(c.cfg, assigned, writes(demandMigrationRepairs[0], assignedStores, assignedRefs), stderr)
	canonicalizeLegacyBoundAssignedWork(c.cfg, assigned, writes(demandMigrationRepairs[1], assignedStores, assignedRefs), sessions, stderr)

	routed, routedStores, routedRefs, routedPartial := collectOpenUnassignedRoutedWork(c.cityPath, c.cfg, city, rigs, suspended, stderr, nil, nil)
	if routedPartial {
		s.problems = append(s.problems, "unassigned routed collection partial")
	}
	repairPoolSlotWorkDirClobber(c.cfg, routed, writes(demandMigrationRepairs[0], routedStores, routedRefs), stderr)
	canonicalizeLegacyBoundUnassignedRoutedWork(c.cfg, routed, writes(demandMigrationRepairs[2], routedStores, routedRefs), stderr)
	collapseSlotSuffixedRoutedWork(c.cfg, routed, writes(demandMigrationRepairs[3], routedStores, routedRefs), stderr)
	return s
}

// demandMigrationLeg spells a leg the same way whichever collection named it:
// the assigned collection uses bare refs ("" for the city, a rig's name), the
// routed one scoped refs ("city:<name>", "rig:<name>").
func demandMigrationLeg(ref string) string {
	switch {
	case ref == "" || strings.HasPrefix(ref, "city:"):
		return "city"
	case strings.Contains(ref, ":"):
		return ref
	}
	return "rig:" + ref
}

// recordingWriteStore stands in for a leg's store in a dry run. It counts the
// two writes the repairs make and has nothing behind it: any other call, read
// or write, panics on the nil embedded Store, which the doctor runner reports
// as a failed check. So a repair that grows a new write fails the dry run
// instead of reaching a real store.
type recordingWriteStore struct {
	beads.Store // always nil
	counts      map[demandMigrationKey]int
	key         demandMigrationKey
}

func (s recordingWriteStore) Update(string, beads.UpdateOpts) error {
	s.counts[s.key]++
	return nil
}

func (s recordingWriteStore) SetMetadataBatch(string, map[string]string) error {
	s.counts[s.key]++
	return nil
}
