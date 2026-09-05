package doctor

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// RigDataPresenceCheck flags likely data loss in a rig's live bead store,
// using an export of the rig's rows as the evidence that it previously held
// data: the local .beads/issues.jsonl, or — because gc deletes that file on
// managed scopes — the durable archived export supplied via
// WithArchiveExportRows. Because the rig identity file is stamped at scope creation (before the
// first bead), identity-present alone does not imply the store should be
// non-empty; the export history is what separates data loss from a fresh rig.
//
// Blocking only on unambiguous loss:
//   - Empty live store + populated issues.jsonl → StatusError, SeverityBlocking
//     (the va incident 2026-06-20: 803 export rows, 0 live).
//
// Non-blocking paths (never gate dispatch):
//   - No identity configured → StatusOK (legacy rig, skip silently).
//   - Empty store + no export history → StatusOK (freshly created rig).
//   - Live rows < export rows → StatusWarning, SeverityAdvisory (a partial
//     deficit can be legitimate retention/archival, not loss).
//   - Store open / list fails → StatusWarning, SeverityAdvisory (Dolt offline).
type RigDataPresenceCheck struct {
	cityPath string
	rig      config.Rig
	newStore func(rigPath string) (beads.Store, error)
	fs       fsys.FS
	// archiveExportRows reports the row count of the rig's durable archived
	// export, and whether one was found. Injected rather than resolved here: the
	// archive lives under a pack-owned layout with its own precedence rules, and
	// this package should not learn where a pack keeps its state. Nil disables
	// the fallback, leaving the local-export behavior unchanged.
	archiveExportRows func(rigPath string) (int, bool)
}

// WithArchiveExportRows injects the durable archived-export oracle used when a
// rig's local .beads/issues.jsonl is absent. See the call site in Run for why
// the local file cannot be relied on for gc-managed scopes.
func (c *RigDataPresenceCheck) WithArchiveExportRows(fn func(rigPath string) (int, bool)) *RigDataPresenceCheck {
	c.archiveExportRows = fn
	return c
}

// NewRigDataPresenceCheck creates a per-rig data-presence check.
func NewRigDataPresenceCheck(cityPath string, rig config.Rig, newStore func(string) (beads.Store, error)) *RigDataPresenceCheck {
	return &RigDataPresenceCheck{
		cityPath: cityPath,
		rig:      rig,
		newStore: newStore,
		fs:       fsys.OSFS{},
	}
}

// Name returns the check identifier ("rig:<name>:data-presence").
func (c *RigDataPresenceCheck) Name() string { return "rig:" + c.rig.Name + ":data-presence" }

// WarmupEligible returns false — this check needs the live Dolt store.
func (c *RigDataPresenceCheck) WarmupEligible() bool { return false }

// CanFix returns false — data restoration is operator policy.
func (c *RigDataPresenceCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *RigDataPresenceCheck) Fix(_ *CheckContext) error { return nil }

// Run executes the data-presence check.
func (c *RigDataPresenceCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	rigPath := c.rig.Path
	if !filepath.IsAbs(rigPath) {
		rigPath = filepath.Join(c.cityPath, rigPath)
	}

	_, ok, err := contract.ReadProjectIdentity(c.fs, rigPath)
	if err != nil {
		r.Status = StatusWarning
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("cannot read identity for data-presence check: %v", err)
		return r
	}
	if !ok {
		// No identity configured — legacy rig; skip silently.
		r.Status = StatusOK
		r.Message = fmt.Sprintf("rig %q: no identity configured (legacy rig, skip)", c.rig.Name)
		return r
	}

	store, err := c.newStore(rigPath)
	if err != nil {
		r.Status = StatusWarning
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("cannot open store for data-presence check: %v", err)
		r.FixHint = "run gc doctor again when Dolt is running"
		return r
	}

	rows, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		r.Status = StatusWarning
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("cannot list store rows for data-presence check: %v", err)
		r.FixHint = "run gc doctor again when Dolt is running"
		return r
	}

	// issues.jsonl is the local export history — the evidence that the rig
	// *previously* held data. The identity file is written at scope creation, before
	// the first bead exists (cmd/gc/dolt_project_id.go), so identity-present plus
	// an empty store cannot by itself distinguish data loss from a freshly
	// created rig. Read the export count first and gate the blocking signals on
	// it.
	jsonlLines, jsonlErr := CountJSONLLines(filepath.Join(rigPath, ".beads", "issues.jsonl"))
	haveExportHistory := jsonlErr == nil && jsonlLines > 0
	evidence := "issues.jsonl"

	// The local export is not durable on a gc-managed rig. Managed scopes get
	// export.auto=false baked in, and gc then deletes .beads/issues.jsonl
	// (reapStaleBdExportJSONL, and again after EnsureCanonicalConfig) because the
	// re-import cycle stalls bd writes on large datasets. So on exactly the
	// configuration this check targets, the local file is absent and the blocking
	// branch below could never fire — the check would report "fresh rig, skip"
	// for a rig that had just lost its rows.
	//
	// Fall back to the durable archive export, which lives outside the scope and
	// is not reaped. Only consulted when the local file yields nothing, so a
	// present local export still wins as the more current signal.
	if !haveExportHistory && c.archiveExportRows != nil {
		if archived, ok := c.archiveExportRows(rigPath); ok && archived > 0 {
			jsonlLines, haveExportHistory, evidence = archived, true, "archived export"
		}
	}

	if len(rows) == 0 {
		if !haveExportHistory {
			// Fresh rig: identity stamped at scope creation, no beads slung yet,
			// no export history. Not data loss — pass silently rather than gate
			// dispatch on a brand-new or just-added rig.
			r.Status = StatusOK
			r.Message = fmt.Sprintf("rig %q: no rows yet and no export history (fresh rig, skip)", c.rig.Name)
			return r
		}
		// Empty live store with a populated export is unambiguous data loss —
		// retention/archival never empties a store that still has an export
		// (the va incident 2026-06-20: 803 rows in issues.jsonl, 0 live).
		r.Status = StatusError
		r.Severity = SeverityBlocking
		r.Message = fmt.Sprintf("rig %s: live store is empty but %s has %d rows — data loss",
			c.rig.Name, evidence, jsonlLines)
		r.FixHint = dataPresenceFixHint()
		return r
	}

	// Secondary signal: a partial deficit (live rows < export rows) is advisory,
	// not blocking. Active retention/archival (#3342 / #3772 archive-then-delete)
	// legitimately deletes live rows while issues.jsonl may lag or retain the
	// archived rows, so a deficit does not prove loss and must not gate dispatch.
	// Surface it for operator review until the export/retention contract
	// guarantees the two counts track.
	if haveExportHistory && len(rows) < jsonlLines {
		r.Status = StatusWarning
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("rig %s: live store has %d rows but %s has %d — possible row deficit (advisory; retention/archival can cause this)",
			c.rig.Name, len(rows), evidence, jsonlLines)
		r.FixHint = dataPresenceFixHint()
		return r
	}

	r.Status = StatusOK
	r.Message = fmt.Sprintf("rig %s: data present (%d rows)", c.rig.Name, len(rows))
	return r
}

// CountJSONLLines counts non-empty lines in a JSONL file. Exported so a caller
// supplying an alternative export source (see WithArchiveExportRows) counts
// rows exactly as this check does — two counters with different notions of a
// row would make the live-vs-export comparison meaningless.
// Returns (0, os.ErrNotExist) when the file is absent — callers treat that as "no signal".
func CountJSONLLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, fs.ErrNotExist
		}
		return 0, err
	}
	defer f.Close() //nolint:errcheck
	count := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			count++
		}
	}
	return count, sc.Err()
}

func dataPresenceFixHint() string {
	return "restore the rig from its last backup (`bd dolt pull` or `mol-dog restore`), " +
		"then run `gc stop && gc start` to re-stamp identity — " +
		"do NOT edit the identity or metadata files to match an empty store"
}
