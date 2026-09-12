package acceptancehelpers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/steveyegge/beads/schema"
)

// bdSchemaProbeTimeout bounds the probe. It opens a throwaway SQLite database
// and applies migrations to it, which is a sub-second operation; the timeout is
// here so a wedged bd cannot hang every acceptance run at startup.
const bdSchemaProbeTimeout = 60 * time.Second

// bdSchemaVersionPattern matches the version bd reports from `migrate schema`
// ("Schema already at v66", "Schema migrated to v66", "v65 -> v66"). The
// highest number in the line is the version bd ended up at.
var bdSchemaVersionPattern = regexp.MustCompile(`\bv(\d+)\b`)

// RequireBdSchemaParity fails when the bd binary the acceptance suite runs and
// the beads library linked into gc disagree about the latest Dolt schema
// version.
//
// This guard exists because that skew does not announce itself as a version
// problem — it announces itself as a product bug, twice over, and only on the
// shapes where one database is shared between gc's native path and the bd CLI
// (the external topologies):
//
//   - gc refuses to migrate a shared server database it is ahead of (correct,
//     beads #5920), the store silently falls back to the bd CLI front door, and
//     doctor reports a `beads-store` warning that reads like a gc defect; or
//   - gc migrates it anyway, and the co-resident bd is locked out of its own
//     database with errors like `table "leases" does not have column
//     "granted_node"` — which reads like a beads bug.
//
// Both were observed on this suite (2026-09-12) from a bd binary built out of a
// different checkout's go.mod, one migration behind the module this tree pins.
// Two engineers' worth of triage went into a stale binary, so the suite now
// answers the question up front, by name and by number.
//
// Parity is required in both directions. A bd behind the library is the case
// above; a bd ahead of it writes a schema gc's native open cannot read. Neither
// is a topology this suite is meant to characterize.
func RequireBdSchemaParity(bdPath string) error {
	bdVersion, err := bdLatestSchemaVersion(bdPath)
	if err != nil {
		return err
	}
	libVersion := schema.LatestVersion()
	if bdVersion == libVersion {
		return nil
	}
	relation := "behind"
	if bdVersion > libVersion {
		relation = "ahead of"
	}
	return fmt.Errorf(
		"bd schema skew: %s tops out at schema v%d, %d migration(s) %s the beads library linked into gc (v%d).\n"+
			"The acceptance matrix cannot characterize a topology through a mismatched pair — on the external shapes it "+
			"shows up as a gc store fallback or as bd locked out of its own database, not as a version error.\n"+
			"Build bd from the module this tree pins:\n"+
			"  GOFLAGS=-mod=mod go build -o <path>/bd github.com/steveyegge/beads/cmd/bd\n"+
			"then point GC_ACCEPTANCE_BD_BIN at it",
		bdPath, bdVersion, abs(bdVersion-libVersion), relation, libVersion)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// bdLatestSchemaVersion asks the binary what schema version it migrates to, by
// migrating a throwaway SQLite database. `bd migrate schema` needs no `bd init`
// and no server when handed an explicit --db, so this costs one sub-second
// process and touches nothing the suite cares about.
func bdLatestSchemaVersion(bdPath string) (int, error) {
	dir, err := os.MkdirTemp("", "gc-bd-schema-probe-*")
	if err != nil {
		return 0, fmt.Errorf("bd schema probe: create temp dir: %w", err)
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best-effort probe cleanup

	ctx, cancel := context.WithTimeout(context.Background(), bdSchemaProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bdPath, "migrate", "schema", "--db", filepath.Join(dir, "probe.db")) //nolint:gosec // caller-supplied test binary
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("bd schema probe: %s migrate schema: %w\n%s", bdPath, err, out)
	}
	version, ok := parseBdSchemaVersion(string(out))
	if !ok {
		return 0, fmt.Errorf("bd schema probe: no schema version in %s migrate schema output:\n%s", bdPath, out)
	}
	return version, nil
}

// parseBdSchemaVersion returns the highest vN in bd's output, which is the
// version it ended at whether it reported "already at v66" or "v65 -> v66".
func parseBdSchemaVersion(out string) (int, bool) {
	best, found := 0, false
	for _, match := range bdSchemaVersionPattern.FindAllStringSubmatch(out, -1) {
		n, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		if n > best {
			best, found = n, true
		}
	}
	return best, found
}
