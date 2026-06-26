package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

// heartbeatMetadataKey is the bead-metadata key freshened by the gc-only
// `gc bd heartbeat <issue-id>` subcommand. The gas-city-dashboard will read
// this exact key — with the `_at` suffix — to tell a live worker from a dead
// one (gastownhall/gascity#1855; reader tracked in dashboard #324). Unrelated
// benchmark/test code writes the suffixless `gc.last_heartbeat` for a
// different purpose; do not unify them.
const heartbeatMetadataKey = beadmeta.LastHeartbeatAtMetadataKey

// bdHeartbeatNow supplies the timestamp stamped by `gc bd heartbeat`. It is a
// package var so tests can pin it to a fixed instant; the rewrite normalizes
// the result to UTC, so an injected non-UTC clock still produces a UTC stamp.
var bdHeartbeatNow = time.Now

// bdSilentFallbackExitCode is the exit code gc bd emits when it detects
// that bd silently fell back to on-disk auto-import mode (managed Dolt
// unreachable). Distinct from bd's own exits so operators and CI can
// tell the loud-fail apart from a real bd error. Covers both the
// bd update path (gastownhall/gascity#2080) and the bd close path
// (gastownhall/gascity#2079) because both subcommands flow through doBd.
const bdSilentFallbackExitCode = 4

const bdSilentFallbackUserMessage = "gc bd: managed Dolt unreachable; bd fell back to on-disk auto-import mode. If this command wrote data, that write was NOT persisted. Restart the managed Dolt server (or check connectivity) and retry. (See gastownhall/gascity#2080.)"

// bdStderrScanLimit caps how much of bd's stderr gc retains to scan for the
// silent-fallback marker. bd emits the marker pair while opening the store —
// before it runs the subcommand — so the marker, when present, always lands
// within the first chunk of stderr. Capping the retained prefix keeps memory
// bounded for bd subcommands that stream large stderr output.
const bdStderrScanLimit = 64 << 10 // 64 KiB

// headLimitedWriter retains only the first limit bytes written to it and
// discards the rest, so scanning bd's stderr for the silent-fallback marker
// never holds an unbounded copy of the stream. It always reports a full
// write so it is safe as an io.MultiWriter sink.
type headLimitedWriter struct {
	buf   []byte
	limit int
}

func (w *headLimitedWriter) Write(p []byte) (int, error) {
	if room := w.limit - len(w.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		w.buf = append(w.buf, p[:room]...)
	}
	return len(p), nil
}

func (w *headLimitedWriter) String() string { return string(w.buf) }

func newBdCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bd [bd-args...]",
		Short: "Run bd in the correct rig directory",
		Long: `Run a bd command routed to the correct rig directory.

When beads belong to a rig (not the city root), bd must run from the
rig directory to find the correct .beads database. This command resolves
the rig automatically from the --rig flag or by detecting the bead prefix
in the arguments.

Use --rig <name> to pin a specific rig store, or --city <path> to pin the
city (HQ) store. An explicit --city is a true scope override: it forces the
city store and disables rig auto-detection (GC_RIG, cwd, bead prefix), so a
deliberate city-scoped query is never silently downgraded to a rig store.

All arguments after "gc bd" are forwarded to bd unchanged, except the
gc-only "heartbeat <issue-id>" subcommand, which rewrites to
"update <issue-id> --set-metadata gc.last_heartbeat_at=<RFC3339 UTC now>"
so long-running workers can signal liveness to the dashboard, and
"release-if-current <issue-id> <assignee>", which conditionally resets an
in-progress assignment only when the bead still has that assignee.

gc bd forces BD_EXPORT_AUTO=false to prevent bd's git auto-export hook
from wedging the wrapper after printing command output. If you need
auto-export behavior, invoke bd directly.`,
		Example: `  gc bd --rig my-project list
  gc bd --rig my-project create "New task"
  gc bd show my-project-abc          # auto-detects rig from bead prefix
  gc bd list --rig my-project -s open
  gc bd --city /path/to/city list    # pins the city (HQ) store, no rig auto-detect
  gc bd heartbeat my-project-abc     # stamp gc.last_heartbeat_at=now
  gc bd release-if-current my-project-abc worker-1`,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			// Plumb doBd's numeric exit code through exitForCode so the
			// process exit code matches the documented contract above
			// (bdSilentFallbackExitCode = 4) and bd's own exit codes are
			// preserved. Returning errExit on any non-zero would collapse
			// every code to 1 and defeat the operator/CI signal the loud-
			// fail was meant to provide.
			return exitForCode(doBd(args, stdout, stderr))
		},
	}
	return cmd
}

var bdBeadExists = func(cityPath string, target execStoreTarget, beadID string) bool {
	store, err := openStoreAtForCity(target.ScopeRoot, cityPath)
	if err != nil {
		return false
	}
	bead, err := store.Get(beadID)
	return err == nil && strings.TrimSpace(bead.ID) != ""
}

func bdCommandEnv(cityPath string, cfg *config.City, target execStoreTarget) ([]string, error) {
	var overrides map[string]string
	var err error
	if target.ScopeKind == "rig" {
		overrides, err = bdRuntimeEnvForRigWithError(cityPath, cfg, target.ScopeRoot)
	} else {
		overrides, err = bdRuntimeEnvWithError(cityPath)
	}
	if err != nil {
		return nil, err
	}
	if target.ScopeKind != "rig" {
		overrides["GC_RIG"] = ""
		overrides["GC_RIG_ROOT"] = ""
		overrides["BEADS_DIR"] = filepath.Join(target.ScopeRoot, ".beads")
	}
	overrides["GC_STORE_ROOT"] = target.ScopeRoot
	overrides["GC_STORE_SCOPE"] = target.ScopeKind
	overrides["GC_BEADS_PREFIX"] = target.Prefix
	applyExportSuppressionEnv(overrides)
	return mergeRuntimeEnv(os.Environ(), overrides), nil
}

func warnExternalBdOverrideDrift(stderr io.Writer, cityPath string, target execStoreTarget) {
	resolved, ok, err := canonicalScopeDoltTarget(cityPath, target.ScopeRoot)
	if err != nil || !ok || !resolved.External {
		return
	}
	var drift []string
	if host := strings.TrimSpace(os.Getenv("GC_DOLT_HOST")); host != "" && host != strings.TrimSpace(resolved.Host) {
		drift = append(drift, fmt.Sprintf("GC_DOLT_HOST=%s (canonical %s)", host, strings.TrimSpace(resolved.Host)))
	}
	if port := strings.TrimSpace(os.Getenv("GC_DOLT_PORT")); port != "" && port != strings.TrimSpace(resolved.Port) {
		drift = append(drift, fmt.Sprintf("GC_DOLT_PORT=%s (canonical %s)", port, strings.TrimSpace(resolved.Port)))
	}
	if len(drift) == 0 {
		return
	}
	_, _ = fmt.Fprintf(stderr, "gc bd: warning: ignoring ambient Dolt host/port override for external target: %s\n", strings.Join(drift, ", "))
}

// rewriteBdHeartbeatArgs expands the gc-only `heartbeat <issue-id>`
// subcommand into the bd command that performs the write:
//
//	update <issue-id> --set-metadata gc.last_heartbeat_at=<RFC3339 UTC>
//
// Long-running workers call `gc bd heartbeat {{issue}}` periodically so the
// dashboard can distinguish a live worker from a dead one
// (gastownhall/gascity#1855). It reuses bd's existing metadata-write path
// rather than adding a new store method, and leaves the issue id in place so
// the generic scope resolver still routes the write to the correct rig store.
// Args that do not begin with "heartbeat" pass through unchanged.
func rewriteBdHeartbeatArgs(bdArgs []string) ([]string, error) {
	if len(bdArgs) == 0 || bdArgs[0] != "heartbeat" {
		return bdArgs, nil
	}
	rest := bdArgs[1:]
	// A bead id never contains whitespace; reject any (leading, trailing, or
	// internal) rather than forwarding a malformed id that would break bd's
	// prefix-based rig auto-detection. Also reject empty and flag-shaped args.
	if len(rest) != 1 || rest[0] == "" || strings.HasPrefix(rest[0], "-") ||
		strings.IndexFunc(rest[0], unicode.IsSpace) >= 0 {
		return nil, fmt.Errorf("usage: gc bd heartbeat <issue-id>")
	}
	stamp := bdHeartbeatNow().UTC().Format(time.RFC3339)
	return []string{"update", rest[0], "--set-metadata", heartbeatMetadataKey + "=" + stamp}, nil
}

func doBd(args []string, stdout, stderr io.Writer) int {
	cityName, rigName, bdArgs := extractBdScopeFlags(args)

	bdArgs, err := rewriteBdHeartbeatArgs(bdArgs)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, err := resolveBdCity(cityName)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Use the full config load path (includes pack expansion + site
	// binding overlay) so migrated rigs (path only in .gc/site.toml)
	// resolve to their bound path. A raw config.Load here would make
	// every already-migrated rig look unbound and fail the new guard
	// in resolveBdScopeTarget / bdRigScopeTarget.
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: loading config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	target, err := resolveBdScopeTarget(cfg, cityPath, rigName, bdArgs, cityName != "")
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if id, expectedAssignee, ok, err := parseBdReleaseIfCurrentArgs(bdArgs); ok || err != nil {
		if err != nil {
			fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return doBdReleaseIfCurrent(cityPath, target, id, expectedAssignee, stdout, stderr)
	}
	if provider := rawBeadsProviderForScope(target.ScopeRoot, cityPath); !providerUsesBdStoreContract(provider) {
		fmt.Fprintf(stderr, "gc bd: only supported for bd-backed beads providers (resolved %q for %s)\n", provider, target.ScopeRoot) //nolint:errcheck // best-effort stderr
		if hint := bdProviderMismatchHint(target.ScopeRoot, provider); hint != "" {
			fmt.Fprintf(stderr, "  hint: %s\n", hint) //nolint:errcheck // best-effort stderr
		}
		return 1
	}

	// Pre-flight exact-ID guard for write-mutating subcommands (gcy-g4o).
	// bd's fuzzy/substring resolver can silently match a longer ID that
	// contains the supplied ID as a substring (e.g. "gcy-dv7" → "gcy-wisp-dv78").
	// Verify via BdStore.Get — which already enforces an exact-ID match —
	// before forwarding any mutation to the bd subprocess.
	//
	// Fail-closed: if the arg scanner reports ambiguity (unrecognized
	// value-consuming flag), the command is rejected rather than forwarded
	// unguarded.
	//
	// Tradeoff: only a genuine ErrIDCollision (bd returned a *different* bead
	// than requested) blocks the write. ErrNotFound and store-unavailable are
	// non-fatal — the write falls through to bd, which will produce its own
	// error if the bead truly does not exist. This preserves correctness for
	// legitimate flows (heartbeat metadata writes, silent-fallback paths,
	// ephemeral/wisp rows, projection-lag writes) that proceed even when the
	// bead isn't yet visible through the read seam.
	//
	// Note: gc bd show (read passthrough) does NOT have this guard and still
	// substring-resolves. That is intentional — reads are non-destructive.
	if writeIDs, writeOK, ambiguous := bdMutationWriteIDs(bdArgs); writeOK {
		if ambiguous {
			fmt.Fprintf(stderr, "gc bd: cannot safely verify bead IDs (unrecognized flag in args %v); aborting to prevent substring-resolution mutation of the wrong bead\n", bdArgs) //nolint:errcheck // best-effort stderr
			return 1
		}
		if len(writeIDs) > 0 {
			store, storeErr := openStoreAtForCity(target.ScopeRoot, cityPath)
			// Store-unavailable: we cannot verify, but we must not block
			// legitimate writes. Fall through; bd will error on actual problems.
			if storeErr == nil {
				for _, id := range writeIDs {
					_, getErr := store.Get(id)
					if errors.Is(getErr, beads.ErrIDCollision) {
						// bd resolved a different bead — block the write to prevent
						// mutating the wrong bead via substring resolution.
						fmt.Fprintf(stderr, "gc bd: bead %q resolved to a different bead ID (substring collision); aborting to prevent mutating the wrong bead\n", id) //nolint:errcheck // best-effort stderr
						return 1
					}
					// ErrNotFound or any other error: bead may be absent, ephemeral,
					// or the read seam differs from the write seam — fall through.
				}
			}
		}
	}

	// Work-record close gate (ADR-0009): a close routed through the SDK seam
	// must satisfy the typed work-record contract (gc.work_outcome present;
	// shipped ⇒ gc.work_commit reachable on gc.work_branch). Warn-only by default;
	// blocks the close only when GC_WORK_RECORD_ENFORCE is set.
	if runWorkRecordCloseGate(bdArgs, target.ScopeRoot, cityPath, stderr) {
		return 1
	}

	reapStaleBdExportJSONL(target.ScopeRoot)
	warnExternalBdOverrideDrift(stderr, cityPath, target)

	bdPath, err := exec.LookPath("bd")
	if err != nil {
		fmt.Fprintln(stderr, "gc bd: bd not found in PATH") //nolint:errcheck // best-effort stderr
		return 1
	}

	cmd := exec.Command(bdPath, bdArgs...)
	cmd.Dir = target.ScopeRoot
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	// Tee stderr through a bounded head buffer alongside the operator's
	// pipe so we can scan it post-exec for bd's silent-fallback-to-on-disk
	// marker. Only stderr is teed: bd writes its auto-import banner there,
	// not to stdout. See gastownhall/gascity#2080 (update path) and #2079
	// (close path) — both go through this handoff.
	stderrScan := &headLimitedWriter{limit: bdStderrScanLimit}
	cmd.Stderr = io.MultiWriter(stderr, stderrScan)
	env, err := bdCommandEnv(cityPath, cfg, target)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cmd.Env = workQueryEnvForDir(env, cmd.Dir)

	traceStart := time.Now()
	runErr := cmd.Run()
	traceExit := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			traceExit = exitErr.ExitCode()
		} else {
			traceExit = -1
		}
	}
	beads.TraceBDCall("go:gc-bd-passthrough", target.ScopeRoot, bdArgs, traceStart, traceExit, runErr)

	if runErr != nil {
		if traceExit > 0 {
			return traceExit
		}
		fmt.Fprintf(stderr, "gc bd: %v\n", runErr) //nolint:errcheck // best-effort stderr
		return 1
	}

	// bd exited 0 — but if its stderr shows the silent fallback to on-disk
	// auto-import, the managed Dolt server was unreachable and any write in
	// this command was dropped (managed Gas City sets BD_EXPORT_AUTO=false;
	// see applyExportSuppressionEnv in cmd/gc/bd_env.go). Surface that as a
	// hard error instead of a misleading exit 0. One check here covers the
	// whole bd-write-persistence quad (gastownhall/gascity#2079 / #2080 /
	// #2149 / #2150) because every bd subcommand routes through this
	// handoff. A non-zero bd exit is intentionally left to the block above:
	// the existing transport-retry classifier already handles the
	// timeout+marker case, and overriding a real bd exit code here would
	// mask it. (Root cause fixed upstream in beads post-#3691; this surfaces
	// the symptom for deployments still on stable bd builds.)
	if bdOutputIndicatesSilentFallback(stderrScan.String()) {
		fmt.Fprintln(stderr, bdSilentFallbackUserMessage) //nolint:errcheck // best-effort stderr
		return bdSilentFallbackExitCode
	}

	return 0
}

func parseBdReleaseIfCurrentArgs(args []string) (id, expectedAssignee string, ok bool, err error) {
	if len(args) == 0 || args[0] != "release-if-current" {
		return "", "", false, nil
	}
	if len(args) != 3 || invalidBdReleaseIfCurrentArg(args[1]) || invalidBdReleaseIfCurrentArg(args[2]) {
		return "", "", true, fmt.Errorf("usage: gc bd release-if-current <issue-id> <assignee>")
	}
	return args[1], args[2], true, nil
}

func invalidBdReleaseIfCurrentArg(value string) bool {
	return value == "" || strings.IndexFunc(value, unicode.IsSpace) >= 0
}

// bdMutationWriteIDs extracts all positional bead IDs from a bd write-mutation
// command (update, close, reopen, delete) and reports whether the scan was
// unambiguous.
//
// Returns:
//   - ids: all positional (non-flag) tokens after the subcommand; may be empty.
//   - ok: false if args is empty or the subcommand is not a write-mutation.
//   - ambiguous: true if the scanner encountered an unrecognized flag that
//     might consume the next argument as its value. In that case the caller
//     must fail-closed — forwarding the command unguarded risks the original
//     substring-resolution bug (gcy-g4o).
//
// The scanner has complete knowledge of every value-consuming flag for each
// subcommand (sourced from `bd <sub> --help`). Unknown flags that start with
// "-" and do not contain "=" are treated as potentially value-consuming, which
// triggers ambiguous=true. Boolean flags (no value) are fine to ignore.
// The "--" terminator is respected: everything after it is positional.
//
// All returned IDs must be verified via BdStore.Get (exact-ID guard) before
// the mutation is forwarded to the bd subprocess.
func bdMutationWriteIDs(args []string) (ids []string, ok bool, ambiguous bool) {
	if len(args) == 0 {
		return nil, false, false
	}
	sub := args[0]
	switch sub {
	case "update", "close", "reopen", "delete":
	default:
		return nil, false, false
	}

	// valueFlags is the complete set of flags that consume the next argument as
	// their value for this subcommand, in both long and short form.
	// Sourced from `bd <sub> --help` (2026-06-10).
	valueFlags := bdSubcmdValueFlags(sub)

	// boolFlags is the complete set of boolean (no-value) flags. Unknown flags
	// not in either set trigger ambiguous=true.
	boolFlags := bdSubcmdBoolFlags(sub)

	positional := false // true after "--"
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if positional {
			if arg != "" {
				ids = append(ids, arg)
			}
			continue
		}
		if arg == "--" {
			positional = true
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			// Positional token — a bead ID (or batch of IDs).
			if arg != "" {
				ids = append(ids, arg)
			}
			continue
		}
		// Flag token.
		// --flag=value form: value is embedded, no next-arg consumed.
		if strings.Contains(arg, "=") {
			continue
		}
		// Strip leading dashes to get the flag name for lookup.
		flagName := strings.TrimLeft(arg, "-")
		// Reconstruct the canonical long or short form for set membership.
		longForm := "--" + flagName
		shortForm := "-" + flagName // only meaningful when flagName is 1 char

		if valueFlags[longForm] || (len(flagName) == 1 && valueFlags[shortForm]) {
			// Known value-consuming flag: skip its value argument.
			i++
			continue
		}
		if boolFlags[longForm] || (len(flagName) == 1 && boolFlags[shortForm]) {
			// Known boolean flag: no value to skip.
			continue
		}
		// Unknown flag. It might consume a value argument that looks like a
		// bead ID. Fail-closed: report ambiguity so the caller can reject.
		return nil, true, true
	}
	return ids, true, false
}

// bdSubcmdValueFlags returns the set of value-consuming flag names (in
// "--long" / "-s" form) for the given bd write-mutation subcommand.
// Sourced from `bd <sub> --help` output (2026-06-10).
func bdSubcmdValueFlags(sub string) map[string]bool {
	// Global flags shared by all bd subcommands that take a value.
	global := map[string]bool{
		"--actor": true, "--db": true, "--directory": true, "-C": true,
		"--dolt-auto-commit": true,
	}
	var subFlags map[string]bool
	switch sub {
	case "update":
		subFlags = map[string]bool{
			"--acceptance": true,
			"--add-label":  true, "--append-notes": true,
			"-a": true, "--assignee": true,
			"--await-id":  true,
			"--body-file": true,
			"--defer":     true,
			"-d":          true, "--description": true,
			"--design": true, "--design-file": true,
			"--due": true,
			"-e":    true, "--estimate": true,
			"--external-ref": true,
			"--metadata":     true,
			"--notes":        true,
			"--parent":       true,
			"-p":             true, "--priority": true,
			"--remove-label": true,
			"--session":      true,
			"--set-labels":   true,
			"--set-metadata": true,
			"-s":             true, "--status": true,
			"-t": true, "--type": true,
			"--title":          true,
			"--spec-id":        true,
			"--unset-metadata": true,
		}
	case "close":
		subFlags = map[string]bool{
			"-r": true, "--reason": true,
			"--reason-file": true,
			"--session":     true,
		}
	case "reopen":
		subFlags = map[string]bool{
			"-r": true, "--reason": true,
		}
	case "delete":
		subFlags = map[string]bool{
			"--from-file": true,
		}
	}
	merged := make(map[string]bool, len(global)+len(subFlags))
	for k := range global {
		merged[k] = true
	}
	for k := range subFlags {
		merged[k] = true
	}
	return merged
}

// bdSubcmdBoolFlags returns the set of boolean (no-value) flag names for the
// given bd write-mutation subcommand.
// Sourced from `bd <sub> --help` output (2026-06-10).
func bdSubcmdBoolFlags(sub string) map[string]bool {
	// Global boolean flags shared by all bd subcommands.
	global := map[string]bool{
		"--global": true, "--ignore-schema-skew": true,
		"--json": true, "--profile": true,
		"-q": true, "--quiet": true,
		"--readonly": true, "--sandbox": true,
		"-v": true, "--verbose": true,
		"-h": true, "--help": true,
	}
	var subFlags map[string]bool
	switch sub {
	case "update":
		subFlags = map[string]bool{
			"--allow-empty-description": true,
			"--claim":                   true, "--ephemeral": true,
			"--history": true, "--no-history": true,
			"--persistent": true, "--stdin": true,
		}
	case "close":
		subFlags = map[string]bool{
			"--claim-next": true, "--continue": true,
			"-f": true, "--force": true,
			"--no-auto": true, "--suggest-next": true,
		}
	case "reopen":
		subFlags = map[string]bool{}
	case "delete":
		subFlags = map[string]bool{
			"--cascade": true, "--dry-run": true,
			"-f": true, "--force": true,
		}
	}
	merged := make(map[string]bool, len(global)+len(subFlags))
	for k := range global {
		merged[k] = true
	}
	for k := range subFlags {
		merged[k] = true
	}
	return merged
}

// bdMutationWriteID is a compatibility shim retained for callers that only
// need the first ID. Prefer bdMutationWriteIDs for new code.
func bdMutationWriteID(args []string) (string, bool) {
	ids, ok, ambiguous := bdMutationWriteIDs(args)
	if !ok || ambiguous || len(ids) == 0 {
		return "", false
	}
	return ids[0], true
}

func doBdReleaseIfCurrent(cityPath string, target execStoreTarget, id, expectedAssignee string, stdout, stderr io.Writer) int {
	store, err := openStoreAtForCity(target.ScopeRoot, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd release-if-current: opening store: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	releaser, ok := store.(beads.ConditionalAssignmentReleaser)
	if !ok {
		fmt.Fprintf(stderr, "gc bd release-if-current: %v for %T\n", beads.ErrConditionalReleaseUnsupported, store) //nolint:errcheck // best-effort stderr
		return 1
	}
	released, err := releaser.ReleaseIfCurrent(id, expectedAssignee)
	if err != nil {
		if errors.Is(err, beads.ErrBDSilentFallback) {
			fmt.Fprintf(stderr, "gc bd release-if-current: %v\n", err) //nolint:errcheck // best-effort stderr
			fmt.Fprintln(stderr, bdSilentFallbackUserMessage)          //nolint:errcheck // best-effort stderr
			return bdSilentFallbackExitCode
		}
		fmt.Fprintf(stderr, "gc bd release-if-current: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if released {
		fmt.Fprintln(stdout, "released") //nolint:errcheck // best-effort stdout
		return 0
	}
	fmt.Fprintln(stdout, "skipped") //nolint:errcheck // best-effort stdout
	return 0
}

func resolveBdCity(cityName string) (string, error) {
	if strings.TrimSpace(cityName) != "" {
		return validateCityPath(cityName)
	}
	return resolveCity()
}

// extractBdScopeFlags extracts gc-owned --city/--rig flags from the raw
// argument list and returns the requested city, rig, and remaining bd args.
// It also falls back to cobra's persistent globals for "gc --city X --rig Y bd".
func extractBdScopeFlags(args []string) (string, string, []string) {
	var cityName string
	var rigName string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--city" && i+1 < len(args):
			cityName = args[i+1]
			i++
			continue
		case strings.HasPrefix(args[i], "--city="):
			cityName = strings.TrimPrefix(args[i], "--city=")
			continue
		case args[i] == "--rig" && i+1 < len(args):
			rigName = args[i+1]
			i++
			continue
		case strings.HasPrefix(args[i], "--rig="):
			rigName = strings.TrimPrefix(args[i], "--rig=")
			continue
		}
		rest = append(rest, args[i])
	}
	if cityName == "" && cityFlag != "" {
		cityName = cityFlag
	}
	if rigName == "" && rigFlag != "" {
		rigName = rigFlag
	}
	return cityName, rigName, rest
}

// extractRigFlag extracts --rig <name> from the argument list and returns
// the rig name and remaining args. Also checks the global rigFlag set by
// cobra's persistent flag parsing (for "gc --rig foo bd list" syntax).
func extractRigFlag(args []string) (string, []string) {
	_, rigName, rest := extractBdScopeFlags(args)
	return rigName, rest
}

// resolveBdScopeTarget determines the canonical scope root for a bd command.
// Priority: explicit rig name > explicit city > bead prefix auto-detection > GC_RIG env > enclosing rig > city root.
func resolveBdScopeTarget(cfg *config.City, cityPath, rigName string, args []string, cityExplicit bool) (execStoreTarget, error) {
	resolveRigPaths(cityPath, cfg.Rigs)
	if rigName != "" {
		rig, ok := rigByName(cfg, rigName)
		if !ok {
			return execStoreTarget{}, fmt.Errorf("rig %q not found", rigName)
		}
		if strings.TrimSpace(rig.Path) == "" {
			return execStoreTarget{}, fmt.Errorf("rig %q is declared but has no path binding — run `gc rig add <dir> --name %s` to bind it before scoping bd commands", rig.Name, rig.Name)
		}
		return bdRigScopeTarget(cityPath, rig), nil
	}

	cityTarget := bdCityScopeTarget(cityPath, cfg)

	// An explicit --city pins the city store, symmetric with explicit --rig:
	// a deliberate city scope must never be silently downgraded to a rig store
	// by bead-prefix / GC_RIG-env / cwd auto-detection below. Without this,
	// `gc bd --city <path> list` returned cwd/rig-scoped results, mis-scoping
	// scripts that trusted the flag. (gastownhall/gascity#3410)
	if cityExplicit {
		return cityTarget, nil
	}

	cityPrefix := config.EffectiveHQPrefix(cfg)
	if cityPrefix != "" {
		for _, arg := range args {
			if strings.HasPrefix(arg, "-") || beadPrefix(cfg, arg) != cityPrefix {
				continue
			}
			if bdBeadExists(cityPath, cityTarget, arg) {
				return cityTarget, nil
			}
		}
	}

	// Auto-detect from bead IDs in args, but only accept candidates that
	// actually exist in the resolved rig store. This keeps hyphenated flag
	// values and other non-ID args from silently retargeting the command.
	// Unbound rigs are skipped so we don't alias them to the city store.
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if rig, ok := bdRigForArg(cfg, arg); ok {
			if strings.TrimSpace(rig.Path) == "" {
				continue
			}
			target := bdRigScopeTarget(cityPath, rig)
			if bdBeadExists(cityPath, target, arg) {
				return target, nil
			}
		}
	}

	// Honor GC_RIG env (set by the controller on every rig agent) when no
	// explicit --rig flag was given and no bead-ID in the args matched a
	// specific store. This is a weaker signal than an explicit flag or a
	// bead-prefix hit, but a stronger default than cwd: the controller sets
	// GC_RIG reliably, while cwd detection fails for polecat worktrees (they
	// live under .gc/worktrees/, not the configured rig path).
	// Priority: explicit --rig > bead-prefix detect > GC_RIG env > cwd > city.
	if gcRig := strings.TrimSpace(os.Getenv("GC_RIG")); gcRig != "" {
		if rig, ok := rigByName(cfg, gcRig); ok && strings.TrimSpace(rig.Path) != "" {
			return bdRigScopeTarget(cityPath, rig), nil
		}
		// GC_RIG names an unknown or unbound rig — fall through to cwd/city
		// rather than erroring, so cross-city queries still work from rig agents.
	}

	if rig, ok, err := bdRigFromCwd(cfg, cityPath); err != nil {
		return execStoreTarget{}, err
	} else if ok {
		// resolveRigForDir already skips unbound rigs, so rig.Path is
		// guaranteed non-empty here.
		return bdRigScopeTarget(cityPath, rig), nil
	}

	return cityTarget, nil
}

func bdRigForArg(cfg *config.City, arg string) (config.Rig, bool) {
	if prefix := beadPrefix(cfg, arg); prefix != "" {
		return findRigByPrefix(cfg, prefix)
	}
	return config.Rig{}, false
}

func bdRigFromCwd(cfg *config.City, cityPath string) (config.Rig, bool, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return config.Rig{}, false, nil
	}
	return resolveRigForDir(cfg, cityPath, cwd)
}

func bdRigScopeTarget(cityPath string, rig config.Rig) execStoreTarget {
	return execStoreTarget{
		ScopeRoot: resolveStoreScopeRoot(cityPath, rig.Path),
		ScopeKind: "rig",
		Prefix:    rig.EffectivePrefix(),
		RigName:   rig.Name,
	}
}

func bdCityScopeTarget(cityPath string, cfg *config.City) execStoreTarget {
	return execStoreTarget{
		ScopeRoot: resolveStoreScopeRoot(cityPath, cityPath),
		ScopeKind: "city",
		Prefix:    config.EffectiveHQPrefix(cfg),
	}
}
