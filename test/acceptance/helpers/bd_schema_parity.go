package acceptancehelpers

import (
	"context"
	"errors"
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

// bdSchemaProbeTimeout bounds the probe. It creates a throwaway embedded Dolt
// database and migrates it, a few seconds of CPU once the database lives in RAM
// (see bdSchemaProbeRoots); the timeout is here so a wedged bd cannot hang every
// acceptance run at startup.
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
// migrating a throwaway embedded Dolt database. `bd migrate schema` needs no
// `bd init` and no server when handed an explicit --db, so this costs one
// process and touches nothing the suite cares about.
func bdLatestSchemaVersion(bdPath string) (int, error) {
	return bdLatestSchemaVersionUnder(bdPath, bdSchemaProbeRoots())
}

// bdSchemaProbeRoots lists where the probe's throwaway database may be created,
// best first; "" is the default temp directory.
//
// The database is deleted the moment bd answers, so durability buys it
// nothing, yet bd's embedded Dolt fsyncs it hundreds of times while creating it
// (539 fsyncs for one probe with bd 1.1.0). On a durable filesystem each of
// those waits for the filesystem's log commit, which a busy host does not
// bound: on the btrfs /var/tmp the pre-push gate uses as TMPDIR, one probe ran
// 69s on its usual ~5s of CPU, its threads parked in fsync, and overran
// bdSchemaProbeTimeout with nothing wedged (ga-01i5ul). On a RAM-backed
// filesystem fsync is a no-op and the probe is CPU-bound: under the same load
// it measured p50 4.1s and max 8.0s against p50 9.0s and max 20.9s on /var/tmp.
//
// /dev/shm is that filesystem on Linux hosts and CI runners. The default temp
// directory stays the fallback where it is missing or not writable (macOS,
// sandboxes that mount it read-only). The probe needs about 2MB.
func bdSchemaProbeRoots() []string {
	if runtime.GOOS == "linux" {
		return []string{"/dev/shm", ""}
	}
	return []string{""}
}

// bdLatestSchemaVersionUnder is bdLatestSchemaVersion with its temp directory
// created under the first of roots that accepts one.
func bdLatestSchemaVersionUnder(bdPath string, roots []string) (int, error) {
	dir, err := mkdirTempUnder(roots, "gc-bd-schema-probe-*")
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
	cmd := bdSchemaProbeCommand(ctx, bdPath, dir)
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

// mkdirTempUnder is os.MkdirTemp under the first of roots that accepts the
// directory. The earlier roots are fast paths, so a root that refuses is
// skipped; only when every root refuses is that an error, naming each refusal.
func mkdirTempUnder(roots []string, pattern string) (string, error) {
	if len(roots) == 0 {
		roots = []string{""}
	}
	var errs []error
	for _, root := range roots {
		dir, err := os.MkdirTemp(root, pattern)
		if err == nil {
			return dir, nil
		}
		errs = append(errs, err)
	}
	return "", errors.Join(errs...)
}

// bdSchemaProbeCommand builds the probe's `bd migrate schema` under dir.
//
// TestMain runs it before any Env exists, so it cannot borrow one: it runs with
// the test process's environment re-homed under dir (IsolatedToolEnv). With the
// inherited HOME, a host whose user-level bd config says
// `dolt.shared-server: true` turned this "throwaway SQLite" probe into a dial of
// the operator's shared Dolt server, plus machine-id and metrics writes under
// the operator's ~/.beads and ~/.config/bd.
//
// The database sits one level below dir, not in it. bd keeps a workspace gate
// file BESIDE the directory that holds the database and never deletes it
// (beads internal/workspacegate), so with the database directly in dir every
// probe left a <dir>.gate.lock in the shared temp root — 238 had piled up in
// /var/tmp between 2026-09-25 and 2026-10-02 (ga-01i5ul). Nested, anything bd
// puts beside the database's directory is inside dir, where the caller's
// RemoveAll reaches it.
func bdSchemaProbeCommand(ctx context.Context, bdPath, dir string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bdPath, "migrate", "schema", "--db", filepath.Join(dir, "db", "probe.db")) //nolint:gosec // caller-supplied test binary
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
