package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

// CleanupSchemaVersion is the stable schema identifier for the JSON output of
// `gc dolt-cleanup --json`. Documented in AD-04 designer Wireframe 6.
const CleanupSchemaVersion = "gc.dolt.cleanup.v1"

// CleanupReport is the typed JSON output of `gc dolt-cleanup`.
//
// Fields are populated incrementally: the port section is filled from the
// AD-04 §4.1 discovery chain; rigs_protected, dropped, purge, reaped are
// populated by their respective steps as they come online. The shape is
// stable from day one — empty arrays and zero structs render as `[]` /
// `{...}` so callers can rely on the schema across versions.
type CleanupReport struct {
	Schema        string                 `json:"schema"`
	Port          CleanupPortReport      `json:"port"`
	RigsProtected []CleanupRigProtection `json:"rigs_protected"`
	ForceBlockers []CleanupForceBlocker  `json:"force_blockers"`
	Dropped       CleanupDroppedReport   `json:"dropped"`
	Purge         CleanupPurgeReport     `json:"purge"`
	Reaped        CleanupReapedReport    `json:"reaped"`
	Summary       CleanupSummary         `json:"summary"`
	Errors        []CleanupError         `json:"errors"`
}

// CleanupPortReport is the resolved-port section of the JSON envelope.
type CleanupPortReport struct {
	Resolved int    `json:"resolved"`
	Source   string `json:"source"`
	Fallback bool   `json:"fallback"`
}

// CleanupRigProtection records a registered rig DB whose name will not be
// dropped even if it appears in the orphan scan.
type CleanupRigProtection struct {
	Rig string `json:"rig"`
	DB  string `json:"db"`
}

// CleanupForceBlocker records a condition that would block a future forced
// cleanup but does not make dry-run output an error.
type CleanupForceBlocker struct {
	Kind  string `json:"kind"`
	Name  string `json:"name,omitempty"`
	Error string `json:"error"`
}

// CleanupDroppedReport summarizes the drop step.
type CleanupDroppedReport struct {
	Count      int   `json:"count"`
	BytesFreed int64 `json:"bytes_freed"`
	// Names lists the databases the drop step targeted: the candidates in
	// dry-run, the actually-dropped names in --force. Order follows the
	// SHOW DATABASES result.
	Names   []string             `json:"names"`
	Failed  []CleanupDropFailure `json:"failed"`
	Skipped []DoltDropSkip       `json:"skipped"`
}

// CleanupDropFailure records a single drop step that did not complete.
type CleanupDropFailure struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// CleanupPurgeReport summarizes the purge step.
type CleanupPurgeReport struct {
	OK bool `json:"ok"`
	// BytesReclaimed is an estimate in dry-run mode and confirmed reclaimed
	// bytes in --force mode. Failed forced purge calls do not contribute.
	BytesReclaimed int64 `json:"bytes_reclaimed"`
}

// CleanupReapedReport summarizes the orphan-process reap step.
type CleanupReapedReport struct {
	Count         int   `json:"count"`
	ProtectedPIDs []int `json:"protected_pids"`
	// VanishedPIDs records reap targets missing before any signal was sent.
	// Post-SIGTERM disappearance is counted as a successful reap because this
	// process sent the termination signal and the process exited before SIGKILL.
	VanishedPIDs []int `json:"vanished_pids"`
	// Targets records the PIDs the reaper identified as test orphans (the
	// reap candidates). Populated in both dry-run and --force; --force
	// additionally drives Count to reflect actually-killed processes.
	Targets []CleanupReapTarget `json:"targets"`
	Errors  []string            `json:"errors"`
}

// CleanupReapTarget is a single orphan dolt sql-server process the reaper
// identified for termination.
type CleanupReapTarget struct {
	PID        int    `json:"pid"`
	ConfigPath string `json:"config_path"`
}

// CleanupSummary aggregates totals across the three steps.
type CleanupSummary struct {
	BytesFreedDisk int64 `json:"bytes_freed_disk"`
	BytesFreedRSS  int64 `json:"bytes_freed_rss"`
	ErrorsTotal    int   `json:"errors_total"`
}

// CleanupError is a single error entry tagged with the stage that produced
// it. Stage values are e.g. "drop", "purge", "reap", "port".
type CleanupError struct {
	Stage string `json:"stage"`
	Kind  string `json:"kind,omitempty"`
	Name  string `json:"name,omitempty"`
	Error string `json:"error"`
}

const (
	cleanupErrorKindInvalidMaxOrphanDBs = "invalid-max-orphan-dbs"
	cleanupErrorKindMaxOrphanRefusal    = "max-orphan-refusal"
	cleanupErrorKindRigProtection       = "rig-protection"
)

// MarshalJSON ensures slices serialize as `[]` rather than `null` for empty
// values. The JSON contract documents these as always-present arrays.
func (r CleanupReport) MarshalJSON() ([]byte, error) {
	type alias CleanupReport
	if r.RigsProtected == nil {
		r.RigsProtected = []CleanupRigProtection{}
	}
	if r.ForceBlockers == nil {
		r.ForceBlockers = []CleanupForceBlocker{}
	}
	if r.Dropped.Failed == nil {
		r.Dropped.Failed = []CleanupDropFailure{}
	}
	if r.Dropped.Skipped == nil {
		r.Dropped.Skipped = []DoltDropSkip{}
	}
	if r.Reaped.ProtectedPIDs == nil {
		r.Reaped.ProtectedPIDs = []int{}
	}
	if r.Reaped.VanishedPIDs == nil {
		r.Reaped.VanishedPIDs = []int{}
	}
	if r.Reaped.Targets == nil {
		r.Reaped.Targets = []CleanupReapTarget{}
	}
	if r.Reaped.Errors == nil {
		r.Reaped.Errors = []string{}
	}
	if r.Dropped.Names == nil {
		r.Dropped.Names = []string{}
	}
	if r.Errors == nil {
		r.Errors = []CleanupError{}
	}
	return json.Marshal(alias(r))
}

// cleanupOptions bundles the inputs to runDoltCleanup so the command body
// stays Cobra-free and testable. The Cobra command builds an options value
// from flags and city state and hands it off.
//
// DiscoverProcesses and KillProcess are injection points for tests; in
// production they default to the /proc walker and syscall.Kill respectively.
// HomeDir defaults to the live $HOME and seeds ~/.gotmp/Test* recognition.
// TempDir defaults to the live os.TempDir() and lets the reaper recognize
// Go test temp roots and known Gas City test prefixes on hosts where TMPDIR
// is not /tmp.
type cleanupOptions struct {
	Flag           string
	CityPort       int
	PortResolution PortResolution
	Rigs           []resolverRig
	FS             fsys.FS
	JSON           bool
	Probe          bool
	Force          bool
	Host           string
	HomeDir        string
	TempDir        string
	MaxOrphanDBs   int

	// StalePrefixes overrides defaultStaleDatabasePrefixes when non-empty.
	// Set by tests; production passes nil and falls back to the built-in.
	StalePrefixes []string

	// DoltClient is the SQL surface used by the drop and purge stages. When
	// nil, those stages no-op (the report still renders, just without DB
	// operations) — useful for tests that exercise the port resolver and
	// reaper in isolation.
	DoltClient CleanupDoltClient
	// DoltClientOpenErr records a failed attempt to open the production SQL
	// client. Tests that intentionally omit DoltClient leave this nil.
	DoltClientOpenErr error

	DiscoverProcesses func() ([]DoltProcInfo, error)
	ActiveTestRoots   []string
	KillProcess       func(pid int, sig syscall.Signal) error
	ReapGracePeriod   time.Duration
}

// runDoltCleanup is the testable core of the `gc dolt-cleanup` command. It
// applies the AD-04 §4.1 port-resolution chain, optionally probes the
// resolved port, runs the orphan-process reaper, and writes either a
// CleanupReport JSON envelope or a human-readable summary to stdout.
// Returns the exit code.
//
// Drop and purge stages are populated when a Dolt SQL client is available;
// otherwise the report still renders with errors describing the unreachable
// data plane.
func runDoltCleanup(opts cleanupOptions, stdout, stderr io.Writer) int {
	if opts.MaxOrphanDBs < 0 {
		report := CleanupReport{Schema: CleanupSchemaVersion}
		recordCleanupErrorKind(
			&report,
			"config",
			cleanupErrorKindInvalidMaxOrphanDBs,
			"--max-orphan-dbs",
			fmt.Errorf("--max-orphan-dbs must be non-negative, got %d", opts.MaxOrphanDBs),
		)
		emitReport(report, PortResolution{}, opts, stdout, stderr)
		return 1
	}

	resolution := cleanupPortResolution(opts)
	opts.PortResolution = resolution
	protections, protectionErrors := rigProtections(opts.Rigs, opts.FS)

	report := CleanupReport{
		Schema: CleanupSchemaVersion,
		Port: CleanupPortReport{
			Resolved: resolution.Port,
			Source:   resolution.Source,
			Fallback: resolution.Fallback,
		},
		RigsProtected: protections,
	}
	for _, e := range protectionErrors {
		recordCleanupForceBlocker(&report, cleanupErrorKindRigProtection, e.rig, e.err)
	}
	if opts.Force {
		for _, e := range protectionErrors {
			recordCleanupErrorKind(&report, "rig", cleanupErrorKindRigProtection, e.rig, e.err)
		}
	}
	recordUnsafeRigDatabaseNames(&report)

	if fatalAttempt, err := fatalPortResolutionAttempt(resolution); err != nil {
		fatalResolution := resolution
		fatalResolution.Port = 0
		fatalResolution.Source = fatalAttempt.Source
		fatalResolution.Fallback = false
		report.Port = CleanupPortReport{
			Resolved: 0,
			Source:   fatalAttempt.Source,
			Fallback: false,
		}
		recordCleanupError(&report, "port", fatalAttempt.Source, err)
		emitReport(report, fatalResolution, opts, stdout, stderr)
		return 1
	}

	if opts.Probe {
		host := opts.Host
		if host == "" {
			host = "127.0.0.1"
		}
		if err := probeDoltPort(host, resolution.Port); err != nil {
			report.Errors = append(report.Errors, CleanupError{
				Stage: "port",
				Error: err.Error(),
			})
			report.Summary.ErrorsTotal++
			emitReport(report, resolution, opts, stdout, stderr)
			return 1
		}
	}

	if runDropStage(&report, opts) {
		runPurgeStage(&report, opts)
		runReapStage(&report, opts)
	}
	report.Summary.BytesFreedDisk = report.Purge.BytesReclaimed

	emitReport(report, resolution, opts, stdout, stderr)
	if opts.DoltClientOpenErr != nil {
		return 1
	}
	return 0
}

func cleanupPortResolution(opts cleanupOptions) PortResolution {
	if opts.PortResolution.Port != 0 || opts.PortResolution.Source != "" || len(opts.PortResolution.Tried) != 0 {
		return opts.PortResolution
	}
	return ResolveDoltPort(PortResolverInput{
		Flag:     opts.Flag,
		CityPort: opts.CityPort,
		Rigs:     opts.Rigs,
		FS:       opts.FS,
	})
}

func recordCleanupError(report *CleanupReport, stage, name string, err error) {
	recordCleanupErrorKind(report, stage, "", name, err)
}

func recordCleanupErrorKind(report *CleanupReport, stage, kind, name string, err error) {
	entry := CleanupError{Stage: stage, Kind: kind, Error: err.Error()}
	if name != "" {
		entry.Name = name
	}
	report.Errors = append(report.Errors, entry)
	report.Summary.ErrorsTotal++
}

func recordCleanupForceBlocker(report *CleanupReport, kind, name string, err error) {
	entry := CleanupForceBlocker{Kind: kind, Error: err.Error()}
	if name != "" {
		entry.Name = name
	}
	report.ForceBlockers = append(report.ForceBlockers, entry)
}

// runReapStage discovers live `dolt sql-server` processes, classifies them
// against the rig-port and test-config-path allowlists, and (when --force is
// set) sends SIGTERM followed by SIGKILL after a grace period. Errors are
// recorded into the CleanupReport but do not abort the run — partial reap
// progress is more useful than failing the whole stage.
func runReapStage(report *CleanupReport, opts cleanupOptions) {
	discover := opts.DiscoverProcesses
	if discover == nil {
		discover = discoverDoltProcesses
	}
	procs, err := discover()
	if err != nil {
		report.Errors = append(report.Errors, CleanupError{Stage: "reap", Error: err.Error()})
		report.Summary.ErrorsTotal++
		report.Reaped.Errors = append(report.Reaped.Errors, err.Error())
		return
	}

	rigPorts := protectedDoltPortsForReap(opts)
	tempDir := opts.TempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	activeTestRoots := opts.ActiveTestRoots
	if activeTestRoots == nil {
		activeTestRoots = discoverActiveTestRoots(opts.HomeDir, tempDir)
	}
	plan := planOrphanReap(procs, rigPorts, opts.HomeDir, tempDir, activeTestRoots)

	report.Reaped.ProtectedPIDs = nil
	for _, p := range plan.Protected {
		report.Reaped.ProtectedPIDs = append(report.Reaped.ProtectedPIDs, p.PID)
	}
	report.Reaped.Targets = nil
	for _, t := range plan.Reap {
		report.Reaped.Targets = append(report.Reaped.Targets, CleanupReapTarget{PID: t.PID, ConfigPath: t.ConfigPath})
	}

	if !opts.Force {
		report.Reaped.Count = len(plan.Reap)
		report.Summary.BytesFreedRSS = sumReapTargetRSS(plan.Reap, nil)
		return
	}

	killFn := opts.KillProcess
	if killFn == nil {
		killFn = killProcess
	}
	grace := opts.ReapGracePeriod
	if grace <= 0 {
		grace = 250 * time.Millisecond
	}

	reaped := 0
	gone := make(map[int]bool, len(plan.Reap))
	sigtermSent := make(map[int]bool, len(plan.Reap))
	for _, target := range plan.Reap {
		switch revalidateReapTarget(report, discover, target, rigPorts, opts.HomeDir, tempDir, activeTestRoots, "SIGTERM") {
		case reapRevalidationEligible:
		case reapRevalidationVanished:
			appendVanishedPID(report, target.PID)
			continue
		default:
			continue
		}
		if err := killFn(target.PID, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				gone[target.PID] = true
			} else {
				recordReapSignalError(report, target.PID, syscall.SIGTERM, err)
			}
			continue
		}
		sigtermSent[target.PID] = true
	}
	if grace > 0 {
		time.Sleep(grace)
	}

	for _, target := range plan.Reap {
		if gone[target.PID] || !sigtermSent[target.PID] {
			continue
		}
		switch revalidateReapTarget(report, discover, target, rigPorts, opts.HomeDir, tempDir, activeTestRoots, "SIGKILL") {
		case reapRevalidationEligible:
		case reapRevalidationVanished:
			gone[target.PID] = true
			continue
		default:
			continue
		}
		if err := killFn(target.PID, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				gone[target.PID] = true
			} else {
				recordReapSignalError(report, target.PID, syscall.SIGKILL, err)
			}
			continue
		}
		gone[target.PID] = true
	}
	for _, target := range plan.Reap {
		if gone[target.PID] {
			reaped++
		}
	}
	report.Reaped.Count = reaped
	report.Summary.BytesFreedRSS = sumReapTargetRSS(plan.Reap, gone)
}

func protectedDoltPortsForReap(opts cleanupOptions) map[int]string {
	ports := loadRigDoltPorts(opts.Rigs, opts.FS)
	if opts.PortResolution.Port <= 0 {
		return ports
	}
	if opts.PortResolution.Fallback {
		return ports
	}
	source := opts.PortResolution.Source
	if source == "" {
		source = "selected"
	}
	if _, ok := ports[opts.PortResolution.Port]; !ok {
		ports[opts.PortResolution.Port] = source
	}
	return ports
}

type reapRevalidationStatus int

const (
	reapRevalidationEligible reapRevalidationStatus = iota
	reapRevalidationProtected
	reapRevalidationVanished
	reapRevalidationError
)

func revalidateReapTarget(report *CleanupReport, discover func() ([]DoltProcInfo, error), target ReapTarget, rigPorts map[int]string, homeDir, tempDir string, activeTestRoots []string, signalName string) reapRevalidationStatus {
	refreshed, err := discover()
	if err != nil {
		recordReapRevalidationError(report, signalName, err)
		return reapRevalidationError
	}
	for _, proc := range refreshed {
		if proc.PID != target.PID {
			continue
		}
		recheck := classifyDoltProcess(proc, rigPorts, homeDir, tempDir, activeTestRoots)
		if recheck.Action != "reap" || recheck.ConfigPath != target.ConfigPath || !sameReapProcessIdentity(target, proc) {
			appendProtectedPID(report, target.PID)
			return reapRevalidationProtected
		}
		return reapRevalidationEligible
	}
	return reapRevalidationVanished
}

func sameReapProcessIdentity(target ReapTarget, proc DoltProcInfo) bool {
	return target.StartTimeTicks != 0 && proc.StartTimeTicks == target.StartTimeTicks
}

func recordReapRevalidationError(report *CleanupReport, signalName string, err error) {
	msg := fmt.Sprintf("revalidate before %s: %v", signalName, err)
	report.Reaped.Errors = append(report.Reaped.Errors, msg)
	report.Errors = append(report.Errors, CleanupError{
		Stage: "reap",
		Error: msg,
	})
	report.Summary.ErrorsTotal++
}

func sumReapTargetRSS(targets []ReapTarget, include map[int]bool) int64 {
	var total int64
	for _, target := range targets {
		if include != nil && !include[target.PID] {
			continue
		}
		if target.RSSBytes > 0 {
			total += target.RSSBytes
		}
	}
	return total
}

func fatalPortResolutionError(resolution PortResolution) error {
	_, err := fatalPortResolutionAttempt(resolution)
	return err
}

func fatalPortResolutionAttempt(resolution PortResolution) (PortResolutionAttempt, error) {
	for _, attempt := range resolution.Tried {
		if attempt.Status != "error" {
			continue
		}
		if attempt.Source != flagDoltPortSource && attempt.Source != cityConfigDoltPortSource && !isRigPortFileSource(attempt.Source) {
			continue
		}
		if attempt.Detail != "" {
			return attempt, errors.New(attempt.Detail)
		}
		return attempt, fmt.Errorf("%s resolution failed", attempt.Source)
	}
	return PortResolutionAttempt{}, nil
}

func isRigPortFileSource(source string) bool {
	return filepath.Base(source) == "dolt-server.port" && filepath.Base(filepath.Dir(source)) == ".beads"
}

func appendProtectedPID(report *CleanupReport, pid int) {
	for _, existing := range report.Reaped.ProtectedPIDs {
		if existing == pid {
			return
		}
	}
	report.Reaped.ProtectedPIDs = append(report.Reaped.ProtectedPIDs, pid)
}

func appendVanishedPID(report *CleanupReport, pid int) {
	for _, existing := range report.Reaped.VanishedPIDs {
		if existing == pid {
			return
		}
	}
	report.Reaped.VanishedPIDs = append(report.Reaped.VanishedPIDs, pid)
}

func recordReapSignalError(report *CleanupReport, pid int, sig syscall.Signal, err error) {
	sigName := reapSignalName(sig)
	report.Reaped.Errors = append(report.Reaped.Errors, fmt.Sprintf("pid %d %s: %v", pid, sigName, err))
	report.Errors = append(report.Errors, CleanupError{
		Stage: "reap",
		Name:  fmt.Sprintf("pid %d", pid),
		Error: fmt.Sprintf("%s: %v", sigName, err),
	})
	report.Summary.ErrorsTotal++
}

func reapSignalName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGKILL:
		return "SIGKILL"
	default:
		return sig.String()
	}
}

func emitReport(report CleanupReport, resolution PortResolution, opts cleanupOptions, stdout, stderr io.Writer) {
	if opts.JSON {
		data, err := json.Marshal(report)
		if err != nil {
			fmt.Fprintf(stderr, "gc dolt-cleanup: marshal report: %v\n", err) //nolint:errcheck
			return
		}
		fmt.Fprintln(stdout, string(data)) //nolint:errcheck
		return
	}

	emitHumanReport(report, resolution, opts, stdout)
}

// emitHumanReport writes the operator-facing wireframe to stdout. Output is
// plain text with small unicode glyphs (⚠ ✓ ✖) — no ANSI escapes — so it
// behaves correctly under NO_COLOR or when piped to a file.
func emitHumanReport(report CleanupReport, resolution PortResolution, opts cleanupOptions, stdout io.Writer) {
	host := opts.Host
	if host == "" {
		host = "127.0.0.1"
	}
	switch {
	case resolution.Port <= 0:
		fmt.Fprintln(stdout, "✖ Dolt server port: unresolved") //nolint:errcheck
		fmt.Fprintln(stdout, "  Tried sources, in order:")     //nolint:errcheck
		for _, attempt := range resolution.Tried {
			fmt.Fprintf(stdout, "    %-46s  %s\n", attempt.Source, attemptStatusLabel(attempt)) //nolint:errcheck
		}
	case resolution.Fallback:
		fmt.Fprintf(stdout, "⚠ Dolt server port: %d (legacy default — fallback)\n", resolution.Port) //nolint:errcheck
		fmt.Fprintln(stdout, "  Tried sources, in order:")                                           //nolint:errcheck
		for _, attempt := range resolution.Tried {
			fmt.Fprintf(stdout, "    %-46s  %s\n", attempt.Source, attemptStatusLabel(attempt)) //nolint:errcheck
		}
	default:
		fmt.Fprintf(stdout, "Dolt server: %s:%d (resolved from %s)\n", host, resolution.Port, resolution.Source) //nolint:errcheck
	}

	emitDroppedSection(report, stdout)
	emitOrphansSection(report, stdout)
	emitProtectedSection(report, stdout)
	emitForceBlockersSection(report, stdout)
	emitErrorsOrSummary(report, opts, stdout)
	if !opts.Force {
		fmt.Fprintln(stdout, "")                              //nolint:errcheck
		fmt.Fprintln(stdout, "Re-run with --force to apply.") //nolint:errcheck
	}
}

func emitDroppedSection(report CleanupReport, stdout io.Writer) {
	fmt.Fprintln(stdout, "")                                                         //nolint:errcheck
	fmt.Fprintf(stdout, "DROPPED-DATABASE DIRECTORIES (%d)\n", report.Dropped.Count) //nolint:errcheck
	if len(report.Dropped.Names) == 0 {
		fmt.Fprintln(stdout, "  (none)") //nolint:errcheck
		return
	}
	for _, name := range report.Dropped.Names {
		fmt.Fprintf(stdout, "  %s\n", name) //nolint:errcheck
	}
	for _, f := range report.Dropped.Failed {
		fmt.Fprintf(stdout, "  ✖ %s — %s\n", f.Name, f.Error) //nolint:errcheck
	}
	for _, s := range report.Dropped.Skipped {
		fmt.Fprintf(stdout, "  skipped %s — %s\n", s.Name, s.Reason) //nolint:errcheck
	}
}

func emitOrphansSection(report CleanupReport, stdout io.Writer) {
	fmt.Fprintln(stdout, "")                                                                   //nolint:errcheck
	fmt.Fprintf(stdout, "ORPHAN dolt sql-server PROCESSES (%d)\n", len(report.Reaped.Targets)) //nolint:errcheck
	if len(report.Reaped.Targets) == 0 {
		fmt.Fprintln(stdout, "  (none)") //nolint:errcheck
		return
	}
	for _, t := range report.Reaped.Targets {
		path := t.ConfigPath
		if path == "" {
			path = "(no --config flag)"
		}
		fmt.Fprintf(stdout, "  PID %d  %s\n", t.PID, path) //nolint:errcheck
	}
}

func emitProtectedSection(report CleanupReport, stdout io.Writer) {
	fmt.Fprintln(stdout, "")          //nolint:errcheck
	fmt.Fprintln(stdout, "PROTECTED") //nolint:errcheck
	if len(report.RigsProtected) == 0 && len(report.Reaped.ProtectedPIDs) == 0 {
		fmt.Fprintln(stdout, "  (none)") //nolint:errcheck
		return
	}
	for _, rp := range report.RigsProtected {
		fmt.Fprintf(stdout, "  rig %q → DB %q\n", rp.Rig, rp.DB) //nolint:errcheck
	}
	for _, pid := range report.Reaped.ProtectedPIDs {
		fmt.Fprintf(stdout, "  PID %d (active server or non-test path)\n", pid) //nolint:errcheck
	}
}

func emitForceBlockersSection(report CleanupReport, stdout io.Writer) {
	if len(report.ForceBlockers) == 0 {
		return
	}
	fmt.Fprintln(stdout, "")                                                //nolint:errcheck
	fmt.Fprintf(stdout, "FORCE BLOCKERS (%d)\n", len(report.ForceBlockers)) //nolint:errcheck
	for _, blocker := range report.ForceBlockers {
		if blocker.Name != "" {
			fmt.Fprintf(stdout, "  [%s] %s - %s\n", blocker.Kind, blocker.Name, blocker.Error) //nolint:errcheck
		} else {
			fmt.Fprintf(stdout, "  [%s] %s\n", blocker.Kind, blocker.Error) //nolint:errcheck
		}
	}
}

func emitErrorsOrSummary(report CleanupReport, opts cleanupOptions, stdout io.Writer) {
	fmt.Fprintln(stdout, "") //nolint:errcheck
	if len(report.Errors) > 0 {
		fmt.Fprintf(stdout, "ERRORS (%d)\n", len(report.Errors)) //nolint:errcheck
		for _, e := range report.Errors {
			if e.Name != "" {
				fmt.Fprintf(stdout, "  [%s] %s — %s\n", e.Stage, e.Name, e.Error) //nolint:errcheck
			} else {
				fmt.Fprintf(stdout, "  [%s] %s\n", e.Stage, e.Error) //nolint:errcheck
			}
		}
		fmt.Fprintln(stdout, "") //nolint:errcheck
	}

	fmt.Fprintln(stdout, "SUMMARY") //nolint:errcheck
	verb := "would free"
	if opts.Force {
		verb = "freed"
	}
	fmt.Fprintf(stdout, "  Disk %s:    %s\n", verb, formatBytes(report.Purge.BytesReclaimed))                   //nolint:errcheck
	fmt.Fprintf(stdout, "  Drops:         %d (failed: %d)\n", report.Dropped.Count, len(report.Dropped.Failed)) //nolint:errcheck
	purgeStatus := "skipped"
	if opts.Force {
		if report.Purge.OK {
			purgeStatus = "ok"
		} else {
			purgeStatus = "failed"
		}
	}
	fmt.Fprintf(stdout, "  Purge:         %s\n", purgeStatus)                                                           //nolint:errcheck
	fmt.Fprintf(stdout, "  Reaped:        %d (protected: %d)\n", report.Reaped.Count, len(report.Reaped.ProtectedPIDs)) //nolint:errcheck
	fmt.Fprintf(stdout, "  Errors:        %d\n", report.Summary.ErrorsTotal)                                            //nolint:errcheck
}

// formatBytes formats a byte count as "N B", "N.N KiB", "N.N MiB", or
// "N.N GiB" — the binary-prefix scale operators expect for disk
// reclamation reports.
func formatBytes(n int64) string {
	const (
		KiB int64 = 1 << 10
		MiB int64 = 1 << 20
		GiB int64 = 1 << 30
	)
	switch {
	case n >= GiB:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(GiB))
	case n >= MiB:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(MiB))
	case n >= KiB:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(KiB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func attemptStatusLabel(a PortResolutionAttempt) string {
	switch a.Status {
	case "found":
		return "← " + a.Detail
	case "error":
		if a.Detail != "" {
			return "error: " + a.Detail
		}
		return "error"
	case "not-provided":
		return "not provided"
	case "not-set":
		return "not set"
	case "not-found":
		return "not found"
	default:
		return a.Status
	}
}

func probeDoltPort(host string, port int) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
	if err != nil {
		return fmt.Errorf("dolt server at %s unreachable: %w", addr, err)
	}
	_ = conn.Close()
	return nil
}

// newDoltCleanupCmd builds the `gc dolt-cleanup` Cobra command.
//
// Top-level (not under a `dolt` parent) because the existing `dolt` pack
// binding owns that namespace. The pack's `gc dolt cleanup` script can
// delegate to this Go-side command once feature parity lands.
func newDoltCleanupCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		portFlag     string
		jsonOut      bool
		probe        bool
		force        bool
		maxOrphanDBs int
	)

	cmd := &cobra.Command{
		Use:   "dolt-cleanup",
		Short: "Find and remove orphaned Dolt databases (Go-side core)",
		Long: `gc dolt-cleanup is the Go-side implementation of the operational Dolt
cleanup tool. It resolves the Dolt server port via the AD-04 chain
(--port > city dolt.port > <rigRoot>/.beads/dolt-server.port > 3307),
drops stale test/agent databases, calls DOLT_PURGE_DROPPED_DATABASES
to reclaim disk, and reaps orphaned dolt sql-server processes left
over from leaked test harnesses. Invalid explicit ports and unreadable
or invalid city/rig port settings fail closed before cleanup stages run;
only absent rig port files can reach the legacy default. The legacy
default is a connection fallback only; it does not protect port 3307
from orphan-process reaping.

Dry-run by default. Pass --force to actually drop, purge, and kill.
Pass --max-orphan-dbs with --force to refuse all destructive cleanup
stages if the live apply-time stale database count exceeds the
scan-time threshold. The default 0 disables this guard; negative values
are rejected before any city lookup or cleanup stage runs.
Active rig dolt servers, registered rig databases, active test temp roots,
and processes outside the test-config-path allowlist (/tmp/Test*,
os.TempDir()/Test*, known Gas City test prefixes, ~/.gotmp/Test*) are always
protected — see the PROTECTED section of the
report. Destructive drops are limited to known stale test database name
shapes and conservative SQL identifier characters; skipped stale matches
are reported in dropped.skipped. Rig dolt_database names used for purge
must use the same identifier shape: ASCII letters, digits, underscores,
and non-leading hyphens. Missing or silent rig metadata disables forced
drop/purge because the live database name cannot be proven safe.

JSON envelope schema is stable: gc.dolt.cleanup.v1. Automation that
uses --json must inspect summary.errors_total and errors, and must also
refuse to invoke --force when dry-run force_blockers is non-empty.
force_blockers reports conditions that would block forced cleanup without
incrementing errors_total. The rig-protection blocker is intentionally
global: missing or silent rig metadata prevents forced drop/purge because
the command cannot prove all registered rig databases are protected.
Cleanup stage errors are reported in the envelope even when the command
can still return successfully after emitting the report.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if maxOrphanDBs < 0 {
				err := fmt.Errorf("--max-orphan-dbs must be >= 0")
				if jsonOut {
					report := CleanupReport{Schema: CleanupSchemaVersion}
					recordCleanupErrorKind(&report, "options", cleanupErrorKindInvalidMaxOrphanDBs, "", err)
					emitReport(report, PortResolution{}, cleanupOptions{JSON: true}, stdout, stderr)
				} else {
					fmt.Fprintf(stderr, "gc dolt-cleanup: %v\n", err) //nolint:errcheck
				}
				return errExit
			}
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-cleanup: %v\n", err) //nolint:errcheck
				return errExit
			}
			cfg, err := loadCityConfig(cityPath, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-cleanup: %v\n", err) //nolint:errcheck
				return errExit
			}
			rigs := loadResolverRigs(cityPath, cfg)
			homeDir, _ := os.UserHomeDir()
			opts := cleanupOptions{
				Flag:         portFlag,
				CityPort:     cfg.Dolt.Port,
				Rigs:         rigs,
				FS:           fsys.OSFS{},
				JSON:         jsonOut,
				Probe:        probe,
				Force:        force,
				Host:         cfg.Dolt.Host,
				HomeDir:      homeDir,
				TempDir:      os.TempDir(),
				MaxOrphanDBs: maxOrphanDBs,
			}

			// Resolve the port first so we can open a Dolt connection at the
			// right address. Failed opens are reported by runDoltCleanup inside
			// the typed cleanup envelope.
			resolution := ResolveDoltPort(PortResolverInput{
				Flag: opts.Flag, CityPort: opts.CityPort, Rigs: opts.Rigs, FS: opts.FS,
			})
			opts.PortResolution = resolution
			host := opts.Host
			if host == "" {
				host = "127.0.0.1"
			}
			if fatalPortResolutionError(resolution) == nil {
				client, openErr := newSQLCleanupDoltClient(host, strconv.Itoa(resolution.Port))
				if openErr != nil {
					opts.DoltClientOpenErr = openErr
				} else {
					opts.DoltClient = client
					defer client.Close() //nolint:errcheck
				}
			}

			if code := runDoltCleanup(opts, stdout, stderr); code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&portFlag, "port", "", "override the resolved Dolt port")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON envelope (gc.dolt.cleanup.v1)")
	cmd.Flags().BoolVar(&probe, "probe", false, "TCP-probe the resolved port; fail if unreachable")
	cmd.Flags().BoolVar(&force, "force", false, "actually drop, purge, and kill orphaned resources (default: dry-run)")
	cmd.Flags().IntVar(&maxOrphanDBs, "max-orphan-dbs", 0, "with --force, refuse cleanup when live stale database count exceeds this limit")
	return cmd
}

// rigProtections projects the resolver's rig list into the JSON-envelope
// rigs_protected entries. The DB name is read from each rig's
// <rigPath>/.beads/metadata.json `dolt_database` field. Missing, silent,
// unreadable, or corrupt metadata is returned as an error so forced destructive
// work can fail closed instead of pretending the fallback is the live DB
// identity. Order is HQ-first to match the port-resolution preference.
func rigProtections(rigs []resolverRig, fs fsys.FS) ([]CleanupRigProtection, []rigProtectionError) {
	out := make([]CleanupRigProtection, 0, len(rigs))
	var errs []rigProtectionError
	for _, r := range orderRigsHQFirst(rigs) {
		resolution := resolveRigDoltDatabase(r, fs)
		out = append(out, CleanupRigProtection{Rig: r.Name, DB: resolution.name})
		if resolution.err != nil {
			errs = append(errs, rigProtectionError{rig: r.Name, err: resolution.err})
		}
	}
	return out, errs
}

type rigProtectionError struct {
	rig string
	err error
}

func recordUnsafeRigDatabaseNames(report *CleanupReport) {
	for _, rp := range report.RigsProtected {
		if validDoltDatabaseIdentifier(rp.DB) {
			continue
		}
		err := fmt.Errorf("rig %q dolt_database %q is not cleanup-safe", rp.Rig, rp.DB)
		recordCleanupForceBlocker(report, cleanupErrorKindRigProtection, rp.Rig, err)
		recordCleanupErrorKind(report, "rig", cleanupErrorKindRigProtection, rp.Rig, err)
	}
}

func hasRigProtectionError(report *CleanupReport) bool {
	for _, e := range report.Errors {
		if e.Kind == cleanupErrorKindRigProtection || e.Stage == "rig" {
			return true
		}
	}
	return false
}

// rigDoltDatabaseName returns the rig's dolt database name as recorded in its
// metadata.json, falling back to rig.Name only as a report label when metadata
// is missing or silent.
func rigDoltDatabaseName(r resolverRig, fs fsys.FS) string {
	return resolveRigDoltDatabase(r, fs).name
}

type rigDoltDatabaseResolution struct {
	name string
	err  error
}

func resolveRigDoltDatabase(r resolverRig, fs fsys.FS) rigDoltDatabaseResolution {
	if fs == nil {
		return rigDoltDatabaseResolution{
			name: r.Name,
			err:  fmt.Errorf("missing filesystem for rig metadata; cannot verify live dolt database name"),
		}
	}
	metadataPath := filepath.Join(r.Path, ".beads", "metadata.json")
	data, err := fs.ReadFile(metadataPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rigDoltDatabaseResolution{
				name: r.Name,
				err:  fmt.Errorf("missing rig metadata %s; cannot verify live dolt database name", metadataPath),
			}
		}
		return rigDoltDatabaseResolution{
			name: r.Name,
			err:  fmt.Errorf("read rig metadata %s: %w", metadataPath, err),
		}
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return rigDoltDatabaseResolution{
			name: r.Name,
			err:  fmt.Errorf("parse rig metadata %s: %w", metadataPath, err),
		}
	}
	if db, ok := meta["dolt_database"]; ok {
		s := strings.TrimSpace(fmt.Sprint(db))
		if s != "" && s != "<nil>" {
			return rigDoltDatabaseResolution{name: s}
		}
	}
	return rigDoltDatabaseResolution{
		name: r.Name,
		err:  fmt.Errorf("rig metadata %s lacks dolt_database; cannot verify live dolt database name", metadataPath),
	}
}

// loadResolverRigs builds the resolver's rig list from a city config. The HQ
// rig (the city itself) is added first so it wins the AD-04 §4.1 tie when
// multiple <rigRoot>/.beads/dolt-server.port files exist; non-HQ rigs follow
// in city.toml order. Paths are resolved to absolute form via
// resolveRigPaths so the resolver's filesystem reads work regardless of how
// the rig was registered.
func loadResolverRigs(cityPath string, cfg *config.City) []resolverRig {
	rigs := make([]config.Rig, len(cfg.Rigs))
	copy(rigs, cfg.Rigs)
	resolveRigPaths(cityPath, rigs)

	out := make([]resolverRig, 0, len(rigs)+1)
	out = append(out, resolverRig{
		Name: cfg.EffectiveCityName(),
		Path: cityPath,
		HQ:   true,
	})
	for _, r := range rigs {
		out = append(out, resolverRig{
			Name: r.Name,
			Path: r.Path,
			HQ:   false,
		})
	}
	return out
}
