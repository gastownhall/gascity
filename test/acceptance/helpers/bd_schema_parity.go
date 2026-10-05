package acceptancehelpers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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
	dir, err := os.MkdirTemp(bdSchemaProbeTempRoot(), "gc-bd-schema-probe-*")
	if err != nil {
		return 0, fmt.Errorf("bd schema probe: create temp dir: %w", err)
	}
	// bd's HOME (dir/home) lives under dir, so a detached child that bd spawned
	// (the metrics flusher) may still be writing while this removal runs. A
	// leaked probe dir must not fail suite setup, but it must not vanish
	// silently either.
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			fmt.Fprintf(os.Stderr, "bd schema probe: leaked temp dir %s: %v\n", dir, rmErr)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), bdSchemaProbeTimeout)
	defer cancel()

	if err := os.MkdirAll(filepath.Join(dir, "home"), 0o755); err != nil {
		return 0, fmt.Errorf("bd schema probe: create tool home: %w", err)
	}
	// db lives one level under dir, not directly in it: --db dir/probe.db
	// (a flat, non-directory path with nothing already at dir) makes bd treat
	// dir ITSELF as the workspace root, and its gate lock lands as dir's own
	// SIBLING, "dir.gate.lock", outside everything the RemoveAll above
	// reaches (#7105; confirmed empirically — a flat --db path leaks exactly
	// that sibling file, hundreds of which were observed accumulated in
	// /var/tmp). Nesting the db one level deeper makes the gate lock land as
	// a sibling of THAT inner directory instead, which is still inside dir
	// and so still removed by the RemoveAll above.
	dbDir := filepath.Join(dir, "db")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return 0, fmt.Errorf("bd schema probe: create db dir: %w", err)
	}
	cmd := bdSchemaProbeCommand(ctx, bdPath, dir, dbDir)
	// BEADS_TEST_MODE=1 (beadstest.EnvBeadsTestMode) stops bd spawning the
	// detached metrics flusher that would race the RemoveAll of dir above.
	cmd.Env = append(cmd.Env, "BEADS_TEST_MODE=1")
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

// bdSchemaProbeTempRoot returns a RAM-backed temp root for the probe's
// throwaway database, or "" to fall back to os.MkdirTemp's own default (the
// OS temp dir, honoring TMPDIR).
//
// The probe runs a real embedded-Dolt open and migration, which fsyncs. On a
// loaded, copy-on-write filesystem — observed: btrfs-backed /var/tmp under
// concurrent write load — that pushed a sub-second operation past the 60s
// timeout (one measured run: 69.41s wall, 4.8s CPU, 539 fsyncs, threads
// parked in btrfs wait_log_commit; #7105). /dev/shm is tmpfs on every Linux
// this suite targets, so the probe's lack of any real durability requirement
// costs nothing there. Every other platform, and a Linux host where /dev/shm
// is missing or unusable (containers sometimes restrict it), keeps the OS
// default.
func bdSchemaProbeTempRoot() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	return writableDirOrEmpty("/dev/shm")
}

// writableDirOrEmpty returns dir if it exists, is a directory, and a file can
// actually be created in it, or "" otherwise.
//
// The actual-write check matters beyond the stat: a read-only or
// space-exhausted /dev/shm (some containers restrict it) must fall back
// silently rather than let the probe that follows fail on it.
func writableDirOrEmpty(dir string) string {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return ""
	}
	probe, err := os.MkdirTemp(dir, "gc-bd-schema-probe-writable-*")
	if err != nil {
		return ""
	}
	_ = os.RemoveAll(probe)
	return dir
}

// bdSchemaProbeCommand builds the probe's `bd migrate schema` against
// dbDir/probe.db, with dir as the process's working directory and the root of
// its isolated HOME. dbDir must be a subdirectory of dir (see the nesting note
// in bdLatestSchemaVersion) so bd's gate lock for it is removed along with dir.
//
// TestMain runs it before any Env exists, so it cannot borrow one: it runs with
// the test process's environment re-homed under dir (IsolatedToolEnv). With the
// inherited HOME, a host whose user-level bd config says
// `dolt.shared-server: true` turned this "throwaway SQLite" probe into a dial of
// the operator's shared Dolt server, plus machine-id and metrics writes under
// the operator's ~/.beads and ~/.config/bd.
func bdSchemaProbeCommand(ctx context.Context, bdPath, dir, dbDir string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bdPath, "migrate", "schema", "--db", filepath.Join(dbDir, "probe.db")) //nolint:gosec // caller-supplied test binary
	cmd.Dir = dir
	cmd.Env = IsolatedToolEnv(os.Environ(), filepath.Join(dir, "home"))
	return cmd
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
