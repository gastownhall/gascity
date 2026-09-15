package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

// migrate-handoff hands a legacy GC-managed city's Dolt lifecycle to bd, by
// driving bd's journaled `migrate ownership-handoff` verbs.
//
// The division of labor is the whole design, and it runs one way: bd never
// calls gc. bd owns the journal, the replacement server, the fences and the
// rollback; gc owns its own server — starting it, stopping it, and knowing
// whether it did — and drives the sequence. Everything bd needs to know about
// gc arrives as a command-line argument.
//
// gc's inputs are hints, never proof. bd re-observes every one of them: the
// endpoint gc publishes is handshaked, the pid gc believes it runs is resolved
// independently from the port holder, and a hint that disagrees with what bd
// finds is recorded and ignored rather than trusted. That is why this command
// can be honest about the one thing it cannot prove — whether its own stop
// worked — and still hand over safely: it asks bd.
//
// Scope is the city root, TCP, direct. Rigs are refused (contract P5); see
// migrateHandoffRigRefusal.

const (
	migrateHandoffStatusHandedOff  = "handed-off"
	migrateHandoffStatusAlready    = "already-handed-off"
	migrateHandoffStatusPlanned    = "would-hand-off"
	migrateHandoffStatusRolledBack = "rolled-back"
	migrateHandoffStatusFailed     = "failed"
)

const (
	migrateHandoffStepOK      = "ok"
	migrateHandoffStepSkipped = "skipped"
	migrateHandoffStepPlanned = "would-run"
	migrateHandoffStepFailed  = "failed"
)

// migrateHandoffRollbackFinishAttempts bounds the rollback-finish retry.
//
// rollback-finish is re-runnable while it refuses, by design: it cannot admit
// the legacy owner back until that owner is answering, and gc has just asked
// its own server to start. A few seconds of readiness is the case this covers.
// It is bounded because an unbounded retry turns "gc cannot restart its server"
// into a hang instead of a report, and the retry has an exit either way — the
// operator re-runs the command.
const (
	migrateHandoffRollbackFinishAttempts = 6
	migrateHandoffRestartTimeout         = 60 * time.Second
)

// migrateHandoffJournalBusyAttempts bounds the retry of a verb that found
// another one holding the journal.
//
// journal_busy is not a refusal of the request, it is "somebody else is
// mid-verb" — and the documented recovery is to run the command again. gc runs
// every bd child in its own process group, precisely so a timeout can kill the
// tree, which also means an operator who kills gc leaves the bd verb it had
// started running to completion. Re-running immediately then hits the lock that
// verb still holds. Handing that back as a failure whose only remedy is to type
// the same command again is work the command can do itself.
const migrateHandoffJournalBusyAttempts = 10

// The two retry backoffs are variables so a test can drive the loops without
// waiting out windows it is not testing.
var (
	migrateHandoffRollbackFinishBackoff = 2 * time.Second
	migrateHandoffJournalBusyBackoff    = 2 * time.Second
)

// The two seams that touch a real process. Production uses the concrete
// functions below; package tests replace them with t.Cleanup and do not run
// those cases in parallel. They exist because the interesting failures of this
// command are a stop that did not stop and a restart that could not start, and
// a unit test cannot produce either against a real sql-server.
var (
	migrateHandoffStopLegacy    = stopLegacyManagedDoltForHandoff
	migrateHandoffRestartLegacy = restartLegacyManagedDoltForHandoff
)

type migrateHandoffOptions struct {
	JSON   bool
	DryRun bool
	Rigs   []string
}

type migrateHandoffStep struct {
	Step      string            `json:"step"`
	Actor     string            `json:"actor"`
	Status    string            `json:"status"`
	Phase     string            `json:"phase,omitempty"`
	ErrorCode string            `json:"error_code,omitempty"`
	Detail    string            `json:"detail,omitempty"`
	Error     string            `json:"error,omitempty"`
	Evidence  map[string]string `json:"evidence,omitempty"`
}

type migrateHandoffReport struct {
	City     string               `json:"city"`
	DryRun   bool                 `json:"dry_run"`
	Endpoint string               `json:"legacy_endpoint,omitempty"`
	Database string               `json:"database,omitempty"`
	Status   string               `json:"status"`
	Steps    []migrateHandoffStep `json:"steps"`
	Failed   int                  `json:"failed"`
}

// bdHandoffResult is the one JSON object every `bd migrate ownership-handoff`
// verb prints. Only the fields gc acts on or reports are named.
type bdHandoffResult struct {
	SchemaVersion int    `json:"schema_version"`
	Phase         string `json:"phase"`
	Owner         string `json:"owner"`
	Mutates       bool   `json:"mutates"`
	Request       struct {
		Root      string `json:"root"`
		Database  string `json:"database"`
		Workspace string `json:"workspace"`
	} `json:"request"`
	LegacyInstance struct {
		Resolved bool   `json:"resolved"`
		PID      int    `json:"pid"`
		BoundBy  string `json:"bound_by"`
		Reason   string `json:"reason"`
	} `json:"legacy_instance"`
	Target struct {
		PID  int    `json:"pid"`
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"target"`
	Evidence map[string]struct {
		Gates      map[string]string `json:"gates"`
		PortHolder string            `json:"port_holder"`
	} `json:"evidence"`
	ErrorCode          string `json:"error_code"`
	Error              string `json:"error"`
	EvidenceIncomplete bool   `json:"evidence_incomplete"`
}

// legacyHandoffEndpoint is where gc's own server listens, and who gc believes
// is running it. Both come from gc's published runtime state, which is the only
// record that carries a live port.
type legacyHandoffEndpoint struct {
	Host string
	Port int
	PID  int
}

func (e legacyHandoffEndpoint) String() string {
	return e.Host + ":" + strconv.Itoa(e.Port)
}

// bd's forward phases, in the order the verbs reach them. Used to decide what a
// resume may skip.
var migrateHandoffForwardRank = map[string]int{
	"prepared":          1,
	"old_owner_stopped": 2,
	"target_configured": 3,
	"verified":          4,
	"committed":         5,
}

func migrateHandoffIsRollbackPhase(phase string) bool {
	switch phase {
	case "rollback_started", "legacy_config_restored", "rolled_back":
		return true
	}
	return false
}

func newBeadsCityMigrateHandoffCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts migrateHandoffOptions
	cmd := &cobra.Command{
		Use:   "migrate-handoff",
		Short: "Hand a legacy GC-managed city's Dolt lifecycle to bd",
		Long: `Hand a legacy GC-managed city's Dolt lifecycle to the beads provider.

The city keeps its direct ` + "`dolt sql-server`" + ` topology; what changes is who
runs the process. gc stops its own server and bd starts and owns the
replacement, journaling every step in ` + "`.beads/ownership-handoff.json`" + `,
which is what every later gc command reads to see that the scope is no longer
gc's to manage.

bd owns the journal, the replacement server and the rollback. This command
drives bd's phases in order and supplies the two things bd cannot observe for
itself: where gc's server listens, and that gc has stopped it. Nothing here
asks bd to call gc back.

A failure after gc's server is stopped rolls the transfer back: bd restores the
workspace byte-exact, gc restarts its own server, and bd admits it back. The
command is idempotent — re-running it resumes from bd's journal, forward or
backward, whichever the journal is on.

City root only. A rig shares its city's server and is handed over with it.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdBeadsCityMigrateHandoff(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "emit the per-step report as JSON")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "report the plan without handing anything over")
	cmd.Flags().StringArrayVar(&opts.Rigs, "rig", nil, "not supported: the handoff is city-root only")
	return cmd
}

func cmdBeadsCityMigrateHandoff(opts migrateHandoffOptions, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc beads city migrate-handoff: %v\n", err) //nolint:errcheck
		return 1
	}
	return doBeadsCityMigrateHandoff(cityPath, opts, stdout, stderr)
}

// migrateHandoffRigRefusal is the P5 refusal, and it exists to answer the
// question an operator actually has when they reach for --rig.
//
// A legacy rig has no server of its own: its database lives in the city's
// multi-database data dir and it reaches it through the city's endpoint. The
// city's handoff is therefore the rig's handoff — afterwards the rig resolves
// through the city's canonical endpoint, which is bd's replacement, with no
// journal and no config of its own. Moving a rig onto its OWN Dolt topology is
// the second hop, and it is beads bd-qvjt, not this command.
const migrateHandoffRigRefusal = "the ownership handoff is city-root only; a rig has no server of its own and " +
	"is handed over with its city, resolving afterwards through the city's canonical endpoint. " +
	"Moving a rig onto its own topology is the second hop (beads bd-qvjt), not this command"

func doBeadsCityMigrateHandoff(cityPath string, opts migrateHandoffOptions, stdout, stderr io.Writer) int {
	const name = "gc beads city migrate-handoff"
	if !cityUsesBdStoreContract(cityPath) {
		fmt.Fprintf(stderr, "%s: only supported for bd-backed beads providers\n", name) //nolint:errcheck
		return 1
	}
	for _, rig := range opts.Rigs {
		if strings.TrimSpace(rig) != "" {
			fmt.Fprintf(stderr, "%s: %s\n", name, migrateHandoffRigRefusal) //nolint:errcheck
			return 1
		}
	}
	cityPath = normalizePathForCompare(cityPath)
	// Steps is non-nil from the start: a report that refuses before any step
	// runs still has to say "no steps", and a JSON null there is not the empty
	// array the result schema promises.
	report := migrateHandoffReport{City: cityPath, DryRun: opts.DryRun, Steps: []migrateHandoffStep{}}

	code := runMigrateHandoff(cityPath, opts, &report, stderr)
	if opts.JSON {
		// ok:true says the report itself is complete, exactly as `gc doctor
		// --json` does with failing checks. Per-step outcomes live in
		// steps[].status and the verdict in status; the process exit code
		// carries it too.
		if jsonCode := writeCLIJSONLineOrExit(stdout, stderr, name, report); jsonCode != 0 {
			return jsonCode
		}
	} else {
		printMigrateHandoffReport(stdout, report)
	}
	return code
}

// runMigrateHandoff fills the report and returns the exit code.
func runMigrateHandoff(cityPath string, opts migrateHandoffOptions, report *migrateHandoffReport, stderr io.Writer) int {
	const name = "gc beads city migrate-handoff"

	journal, journalErr := readMigrateHandoffJournalPhase(cityPath)
	if journalErr != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, journalErr) //nolint:errcheck
		report.Status = migrateHandoffStatusFailed
		report.Failed++
		return 1
	}
	if journal == "committed" {
		report.Status = migrateHandoffStatusAlready
		report.Steps = append(report.Steps, migrateHandoffStep{
			Step: "status", Actor: "bd", Status: migrateHandoffStepSkipped, Phase: journal,
			Detail: "bd's journal already records a committed transfer",
		})
		return 0
	}

	// A journal on the rollback track is finished as a rollback, never resumed
	// forward. Whatever went wrong the first time is still what happened, and
	// the exit from bd's fence is the rollback's own tail.
	if migrateHandoffIsRollbackPhase(journal) {
		scope, scopeErr := resolveMigrateHandoffScopeFromJournal(cityPath)
		if scopeErr != nil {
			fmt.Fprintf(stderr, "%s: %v\n", name, scopeErr) //nolint:errcheck
			report.Status = migrateHandoffStatusFailed
			report.Failed++
			return 1
		}
		if opts.DryRun {
			report.Status = migrateHandoffStatusPlanned
			report.Steps = append(report.Steps, migrateHandoffStep{
				Step: "resume-rollback", Actor: "gc", Status: migrateHandoffStepPlanned, Phase: journal,
				Detail: "finish the rollback bd's journal is already on",
			})
			return 0
		}
		finishMigrateHandoffRollback(cityPath, scope, report, stderr)
		// A rollback that completes is a rollback that completed, not a
		// transfer that happened. The exit status says the city was not handed
		// over; the report's status says how far the compensation got.
		return 1
	}

	scope, err := classifyMigrateHandoffCity(cityPath, journal)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err) //nolint:errcheck
		report.Status = migrateHandoffStatusFailed
		report.Failed++
		return 1
	}
	report.Endpoint = scope.Endpoint.String()
	report.Database = scope.Database

	if opts.DryRun {
		report.Status = migrateHandoffStatusPlanned
		for _, step := range []struct{ step, actor, detail string }{
			{"prepare", "bd", "snapshot the workspace and prove " + scope.Endpoint.String() + " serves " + scope.Database},
			{"stop-legacy", "gc", "stop gc's managed sql-server and retire its runtime publication"},
			{"legacy-gone", "bd", "prove the legacy owner is gone"},
			{"configure", "bd", "launch bd's replacement server"},
			{"verify", "bd", "prove the replacement serves the same data"},
			{"commit", "bd", "point the workspace at the replacement"},
		} {
			report.Steps = append(report.Steps, migrateHandoffStep{
				Step: step.step, Actor: step.actor, Status: migrateHandoffStepPlanned, Detail: step.detail,
			})
		}
		return 0
	}
	return migrateHandoffForward(cityPath, scope, journal, report, stderr)
}

// migrateHandoffScope is the city being handed over, resolved once.
type migrateHandoffScope struct {
	Path      string
	Database  string
	Workspace string
	Endpoint  legacyHandoffEndpoint
}

func migrateHandoffForward(cityPath string, scope migrateHandoffScope, journal string, report *migrateHandoffReport, stderr io.Writer) int {
	const name = "gc beads city migrate-handoff"
	reached := func(phase string) bool {
		return migrateHandoffForwardRank[journal] >= migrateHandoffForwardRank[phase]
	}

	// 1. prepare. Nothing is mutated outside bd's journal, so a resume simply
	//    re-reports the phase it already reached.
	if !reached("prepared") {
		// The pid hint is prepare's alone. bd resolves the legacy process from
		// the port holder itself and records gc's belief beside what it found;
		// repeating it later would only re-assert a belief about a process
		// that is, by then, supposed to be gone.
		//
		// There is no birth hint. bd's process-birth identity is its own
		// format — a platform-versioned boot id and start tick — and gc has no
		// way to mint one that bd would recognize. The contract makes hints
		// optional precisely so a caller can decline to invent one.
		result, step := runMigrateHandoffVerb(cityPath, scope, "prepare",
			"--legacy-pid", strconv.Itoa(scope.Endpoint.PID),
			"--caller", "gc")
		report.Steps = append(report.Steps, step)
		if step.Status == migrateHandoffStepFailed {
			// Nothing has been stopped and nothing has been written outside
			// bd's own journal, so there is nothing to roll back.
			report.Status = migrateHandoffStatusFailed
			report.Failed++
			fmt.Fprintf(stderr, "%s: bd refused to prepare the transfer [%s]: %s\n", name, result.ErrorCode, result.Error) //nolint:errcheck
			return 1
		}
	} else {
		report.Steps = append(report.Steps, migrateHandoffStep{
			Step: "prepare", Actor: "bd", Status: migrateHandoffStepSkipped, Phase: journal,
			Detail: "bd's journal already records the snapshot",
		})
	}

	// 2. gc stops its own server. This is the one step bd cannot do and cannot
	//    ask for: bd never calls gc.
	stopFailure := ""
	if !reached("old_owner_stopped") {
		step := migrateHandoffStopLegacy(cityPath, scope.Endpoint)
		report.Steps = append(report.Steps, step)
		if step.Status == migrateHandoffStepFailed {
			// Deliberately not fatal on its own. gc knows what it asked for,
			// not what happened: the stop can report a failure the server did
			// not have (a descendant still holding the store lock is the
			// common one) and it can report success over a server that is
			// still serving. bd is the side that proves this, and the next
			// step is that proof. The failure is reported either way, so the
			// operator sees gc's reason alongside bd's verdict.
			stopFailure = step.Error
			report.Failed++
		}
	} else {
		report.Steps = append(report.Steps, migrateHandoffStep{
			Step: "stop-legacy", Actor: "gc", Status: migrateHandoffStepSkipped, Phase: journal,
			Detail: "bd's journal already records the legacy owner gone",
		})
	}

	// 3. Everything from here is after the stop, so every failure compensates.
	for _, verb := range []string{"legacy-gone", "configure", "verify", "commit"} {
		phase := map[string]string{
			"legacy-gone": "old_owner_stopped", "configure": "target_configured",
			"verify": "verified", "commit": "committed",
		}[verb]
		if reached(phase) {
			report.Steps = append(report.Steps, migrateHandoffStep{
				Step: verb, Actor: "bd", Status: migrateHandoffStepSkipped, Phase: journal,
				Detail: "bd's journal already reached " + phase,
			})
			continue
		}
		result, step := runMigrateHandoffVerb(cityPath, scope, verb)
		report.Steps = append(report.Steps, step)
		if step.Status != migrateHandoffStepFailed {
			continue
		}
		report.Failed++
		if stopFailure != "" && result.ErrorCode == "legacy_alive" {
			fmt.Fprintf(stderr, "%s: gc did not stop its own server (%s), and bd confirms it: %s\n", //nolint:errcheck
				name, stopFailure, result.Error)
		} else {
			fmt.Fprintf(stderr, "%s: bd refused %s [%s]: %s\n", name, verb, result.ErrorCode, result.Error) //nolint:errcheck
		}
		// gc's server is stopped and the scope has no settled owner. Put it
		// back rather than leaving the city owned by nobody — and clear bd's
		// journal, which otherwise fences every gc lifecycle command on this
		// city with no documented way out.
		fmt.Fprintf(stderr, "%s: rolling the transfer back\n", name) //nolint:errcheck
		rollbackCode := finishMigrateHandoffRollback(cityPath, scope, report, stderr)
		if rollbackCode == 0 {
			report.Status = migrateHandoffStatusRolledBack
		}
		// A rollback that worked is still a transfer that did not happen.
		return 1
	}

	report.Status = migrateHandoffStatusHandedOff
	return 0
}

// finishMigrateHandoffRollback runs the compensation tail: bd restores the
// workspace, gc restarts its own server, and bd admits it back.
//
// It is also the resume path for a journal already on the rollback track, which
// is why it starts from `rollback` rather than assuming a phase: bd's verbs are
// idempotent, and re-running one that has already reached its phase reports the
// journal instead of redoing the work.
func finishMigrateHandoffRollback(cityPath string, scope migrateHandoffScope, report *migrateHandoffReport, stderr io.Writer) int {
	const name = "gc beads city migrate-handoff"
	endpoint := scope.Endpoint

	result, step := runMigrateHandoffVerb(cityPath, scope, "rollback")
	report.Steps = append(report.Steps, step)
	if step.Status == migrateHandoffStepFailed {
		report.Status = migrateHandoffStatusFailed
		report.Failed++
		fmt.Fprintf(stderr, "%s: bd could not roll the transfer back [%s]: %s\n", name, result.ErrorCode, result.Error) //nolint:errcheck
		return 1
	}

	restart := migrateHandoffRestartLegacy(cityPath, endpoint)
	report.Steps = append(report.Steps, restart)
	if restart.Status == migrateHandoffStepFailed {
		report.Status = migrateHandoffStatusFailed
		report.Failed++
		fmt.Fprintf(stderr, "%s: the workspace is restored but gc could not restart its own server: %s\n", //nolint:errcheck
			name, restart.Error)
		fmt.Fprintf(stderr, "%s: re-run this command once it can start; bd's journal is still waiting to admit it\n", name) //nolint:errcheck
		return 1
	}

	// rollback-finish refuses without advancing until the legacy owner is
	// answering and provably not bd's. gc has just asked for that server, so
	// the refusal is expected for as long as it takes to come up.
	var last bdHandoffResult
	var lastStep migrateHandoffStep
	for attempt := 1; attempt <= migrateHandoffRollbackFinishAttempts; attempt++ {
		last, lastStep = runMigrateHandoffVerb(cityPath, scope, "rollback-finish")
		if lastStep.Status != migrateHandoffStepFailed {
			report.Steps = append(report.Steps, lastStep)
			report.Status = migrateHandoffStatusRolledBack
			return 0
		}
		if attempt < migrateHandoffRollbackFinishAttempts {
			time.Sleep(migrateHandoffRollbackFinishBackoff)
		}
	}
	lastStep.Detail = fmt.Sprintf("refused %d times over %s; re-run this command to retry",
		migrateHandoffRollbackFinishAttempts,
		time.Duration(migrateHandoffRollbackFinishAttempts)*migrateHandoffRollbackFinishBackoff)
	report.Steps = append(report.Steps, lastStep)
	report.Status = migrateHandoffStatusFailed
	report.Failed++
	fmt.Fprintf(stderr, "%s: bd would not admit gc's server back [%s]: %s\n", name, last.ErrorCode, last.Error) //nolint:errcheck
	return 1
}

// stopLegacyManagedDoltForHandoff stops gc's own Dolt server and retires the
// runtime state gc publishes for it.
//
// The published state is retired separately, and deliberately: the stop leaves
// it alone (clearPublishedState is false) because clearing it syncs the port
// mirrors under .beads, and those are artifacts bd has already snapshotted for
// the rollback to restore. Left behind entirely, though, it still says
// running:true for a pid that is gone, and every gc path that asks "is this
// city mine" reads it as yes. So the two files under .gc go, and nothing under
// .beads is touched.
func stopLegacyManagedDoltForHandoff(cityPath string, endpoint legacyHandoffEndpoint) migrateHandoffStep {
	step := migrateHandoffStep{Step: "stop-legacy", Actor: "gc"}
	port := strconv.Itoa(endpoint.Port)
	stopReport, err := stopManagedDoltProcessWithOptions(cityPath, port, false)
	if err != nil {
		step.Status = migrateHandoffStepFailed
		step.Error = err.Error()
		step.Detail = "gc's own stop did not complete; bd's next phase is the proof of what actually happened"
		return step
	}
	if err := retireManagedDoltRuntimePublication(cityPath); err != nil {
		step.Status = migrateHandoffStepFailed
		step.Error = err.Error()
		step.Detail = "the server stopped but gc still publishes runtime state for it"
		return step
	}
	step.Status = migrateHandoffStepOK
	if stopReport.HadPID {
		step.Detail = fmt.Sprintf("stopped pid %d on %s and retired gc's runtime publication", stopReport.PID, endpoint)
	} else {
		step.Detail = "no controllable gc-managed process; retired gc's runtime publication"
	}
	return step
}

// restartLegacyManagedDoltForHandoff puts gc's own server back after a
// rollback.
//
// The admission gate runs first and is the same one every managed lifecycle
// verb takes: at this point bd has restored the workspace byte-exact, which is
// what makes the scope legacy-owned again, so the gate passing IS the proof
// that the restore landed.
//
// A server already listening on the legacy endpoint is the legacy_alive shape —
// gc's stop never took it down — so there is nothing to start and starting
// anyway would race a live server for the store lock. Republishing gc's runtime
// state is the whole of what is missing in that case.
func restartLegacyManagedDoltForHandoff(cityPath string, endpoint legacyHandoffEndpoint) migrateHandoffStep {
	step := migrateHandoffStep{Step: "restart-legacy", Actor: "gc"}
	port := strconv.Itoa(endpoint.Port)
	if err := admitLegacyManagedDoltLifecycle(cityPath); err != nil {
		step.Status = migrateHandoffStepFailed
		step.Error = err.Error()
		step.Detail = "bd's journal does not yet hand the scope back"
		return step
	}
	if loopbackPortAccepts(port) {
		if err := publishManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
			step.Status = migrateHandoffStepFailed
			step.Error = err.Error()
			return step
		}
		step.Status = migrateHandoffStepOK
		step.Detail = "gc's server never stopped; republished its runtime state for " + endpoint.String()
		return step
	}
	lock, _, lockErr := openManagedDoltLifecycleLock(cityPath)
	if lockErr != nil {
		step.Status = migrateHandoffStepFailed
		step.Error = lockErr.Error()
		return step
	}
	locked, lockErr := tryManagedDoltLifecycleLock(lock)
	if lockErr != nil || !locked {
		releaseManagedDoltLifecycleLock(lock)
		if lockErr == nil {
			lockErr = errors.New("managed dolt lifecycle is busy")
		}
		step.Status = migrateHandoffStepFailed
		step.Error = lockErr.Error()
		return step
	}
	defer releaseManagedDoltLifecycleLock(lock)
	if _, err := startManagedDoltProcess(cityPath, endpoint.Host, port, "", "warning", migrateHandoffRestartTimeout); err != nil {
		step.Status = migrateHandoffStepFailed
		step.Error = err.Error()
		return step
	}
	step.Status = migrateHandoffStepOK
	step.Detail = "restarted gc's managed server on " + endpoint.String()
	return step
}

// runMigrateHandoffVerb runs one bd phase and turns its object into a step.
func runMigrateHandoffVerb(cityPath string, scope migrateHandoffScope, verb string, extra ...string) (bdHandoffResult, migrateHandoffStep) {
	step := migrateHandoffStep{Step: verb, Actor: "bd"}
	// The whole request goes on every verb, not just the first. bd rebuilds it
	// from the command line each time and checks it against the journal, which
	// is what makes a resume refuse a request for a different scope instead of
	// merging it. Only the hints are prepare's.
	args := append([]string{
		"migrate", "ownership-handoff", verb, "--json",
		"--root", scope.Path,
		"--database", scope.Database,
		"--workspace", scope.Workspace,
		"--legacy-endpoint", scope.Endpoint.String(),
	}, extra...)
	out, runErr := runBdScopeCommand(cityPath, scope.Path, args...)
	// A verb that finds the journal locked has not been refused; it has been
	// asked to wait. Waiting is this command's job, not the operator's.
	for attempt := 1; attempt < migrateHandoffJournalBusyAttempts && handoffResultIsJournalBusy(out); attempt++ {
		time.Sleep(migrateHandoffJournalBusyBackoff)
		out, runErr = runBdScopeCommand(cityPath, scope.Path, args...)
	}
	result, decodeErr := decodeBdHandoffResult(out)
	if decodeErr != nil {
		step.Status = migrateHandoffStepFailed
		step.Error = fmt.Sprintf("%v: %s", decodeErr, strings.TrimSpace(string(out)))
		if runErr != nil {
			step.Error = fmt.Sprintf("bd migrate ownership-handoff %s: %v: %s", verb, runErr, step.Error)
		}
		return result, step
	}
	step.Phase = result.Phase
	step.ErrorCode = result.ErrorCode
	step.Evidence = result.gatesFor(result.Phase)
	if runErr != nil || result.ErrorCode != "" {
		step.Status = migrateHandoffStepFailed
		step.Error = strings.TrimSpace(result.Error)
		if step.Error == "" && runErr != nil {
			step.Error = runErr.Error()
		}
		return result, step
	}
	step.Status = migrateHandoffStepOK
	step.Detail = migrateHandoffStepDetail(verb, result)
	return result, step
}

// handoffResultIsJournalBusy reports whether bd refused because another verb
// holds the journal lock. It reads the decoded code rather than the text so a
// reworded message cannot silently turn the wait back into a failure.
func handoffResultIsJournalBusy(out []byte) bool {
	result, err := decodeBdHandoffResult(out)
	return err == nil && result.ErrorCode == "journal_busy"
}

func migrateHandoffStepDetail(verb string, result bdHandoffResult) string {
	switch verb {
	case "prepare":
		if result.LegacyInstance.Resolved {
			return fmt.Sprintf("snapshot captured; bd resolved the legacy server as pid %d (bound by %s)",
				result.LegacyInstance.PID, result.LegacyInstance.BoundBy)
		}
		return "snapshot captured; bd could not resolve the legacy process identity (" + result.LegacyInstance.Reason + ")"
	case "configure":
		return fmt.Sprintf("replacement server pid %d on %s:%d", result.Target.PID, result.Target.Host, result.Target.Port)
	case "commit":
		if result.EvidenceIncomplete {
			return "committed, with gates this platform could not evaluate (see evidence)"
		}
		return "committed"
	default:
		return result.Phase
	}
}

// gatesFor returns the gate outcomes bd journaled for one phase, so a refusal
// reports which gate said what rather than only that something said no.
func (r bdHandoffResult) gatesFor(phase string) map[string]string {
	evidence, ok := r.Evidence[phase]
	if !ok || len(evidence.Gates) == 0 {
		return nil
	}
	gates := make(map[string]string, len(evidence.Gates)+1)
	for gate, outcome := range evidence.Gates {
		gates[gate] = outcome
	}
	if evidence.PortHolder != "" {
		gates["port_holder"] = evidence.PortHolder
	}
	return gates
}

// decodeBdHandoffResult reads the one JSON object a handoff verb prints.
//
// The suite's usual last-line helper cannot: bd pretty-prints its object across
// many lines, so the last line is `}`. The object starts at the first line that
// opens one and runs to the end of the output.
func decodeBdHandoffResult(out []byte) (bdHandoffResult, error) {
	var result bdHandoffResult
	lines := strings.Split(string(out), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "{") {
			continue
		}
		if err := json.Unmarshal([]byte(strings.Join(lines[i:], "\n")), &result); err != nil {
			return result, fmt.Errorf("bd emitted an object that does not decode: %w", err)
		}
		return result, nil
	}
	return result, errors.New("bd emitted no JSON object")
}

// readMigrateHandoffJournalPhase reports the phase bd's journal is on, or "" if
// there is no journal.
//
// It reads the file rather than running `bd ... status` on purpose: status
// needs the full request on its command line to build one, and the endpoint
// half of that request is exactly what gc may no longer be able to resolve
// mid-transfer — gc's publication is retired between the stop and the commit.
// The phase is the only thing needed to decide where to resume, and the
// projection already owns reading it safely, version first.
func readMigrateHandoffJournalPhase(cityPath string) (string, error) {
	path := filepath.Join(cityPath, ".beads", handoffJournalName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read ownership handoff journal %s: %w", path, err)
	}
	var journal handoffProjectionJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return "", fmt.Errorf("parse ownership handoff journal %s: %w", path, err)
	}
	if journal.SchemaVersion != handoffJournalSchemaVersion {
		return "", fmt.Errorf("unsupported handoff journal version %d at %s: gc drives version %d",
			journal.SchemaVersion, path, handoffJournalSchemaVersion)
	}
	if _, forward := migrateHandoffForwardRank[journal.Phase]; !forward && !migrateHandoffIsRollbackPhase(journal.Phase) {
		return "", fmt.Errorf("ownership handoff journal %s records unknown phase %q", path, journal.Phase)
	}
	return journal.Phase, nil
}

// classifyMigrateHandoffCity decides whether this city is one gc may hand over,
// and resolves everything bd's request needs.
//
// The shape it admits is narrow on purpose: a legacy GC-managed direct city,
// still gc's by every record gc keeps. Anything else either is not gc's to hand
// over or has no legacy server to transfer.
func classifyMigrateHandoffCity(cityPath, journalPhase string) (migrateHandoffScope, error) {
	scope := migrateHandoffScope{Path: cityPath}
	metadataPath := scopeMetadataJSONPath(cityPath)
	metadata, ok, err := contract.LoadMetadataState(fsys.OSFS{}, metadataPath)
	if err != nil {
		return scope, fmt.Errorf("read %s: %w", metadataPath, err)
	}
	if !ok {
		return scope, fmt.Errorf("the city has no %s; there is no initialized beads store to hand over", metadataPath)
	}
	if !contract.IsDoltBackend(strings.TrimSpace(metadata.Backend)) {
		return scope, fmt.Errorf("the city uses beads backend %q; only a dolt backend has a server to hand over", metadata.Backend)
	}
	switch mode := strings.ToLower(strings.TrimSpace(metadata.DoltMode)); mode {
	case "server", "":
		// The transferable shape. An empty mode is the pre-dolt_mode legacy
		// direct server (see freshScopeCanonicalDoltMode).
	case "proxied-server":
		return scope, errors.New("the city already runs bd's proxied-server topology; there is no gc-managed server to hand over")
	case "embedded":
		return scope, errors.New("the city is an embedded Dolt scope; it has no server to hand over")
	default:
		return scope, fmt.Errorf("the city records unsupported dolt_mode %q", metadata.DoltMode)
	}

	// Ownership, by every record gc keeps. The handoff journal arm answers
	// first because it is the most specific evidence, and because a pending
	// journal is a resume rather than a refusal.
	if journalPhase == "" {
		if _, journaled, err := providerScopeOwnership(cityPath, cityPath); err != nil {
			return scope, err
		} else if journaled {
			return scope, errors.New("the city is recorded in .gc/scope-ownership.json as provider-owned; " +
				"it was not initialized the legacy GC-managed way and has no gc-managed server to hand over")
		}
		if bound, err := scopeBindingIsProviderOwnedProxied(cityPath); err != nil {
			return scope, err
		} else if bound {
			return scope, errors.New("the city's beads metadata already binds it to bd's proxied topology; it is not gc's to hand over")
		}
	}

	cfg, configured, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "config.yaml"))
	if err != nil {
		return scope, err
	}
	if configured && journalPhase == "" {
		switch cfg.EndpointOrigin {
		case contract.EndpointOriginManagedCity, "":
		default:
			return scope, fmt.Errorf("the city tracks a %q Dolt endpoint, not a gc-managed one; only a gc-managed server is gc's to hand over",
				cfg.EndpointOrigin)
		}
	}

	database := strings.TrimSpace(metadata.DoltDatabase)
	if database == "" {
		database = strings.TrimSpace(metadata.Database)
	}
	if database == "" {
		return scope, fmt.Errorf("%s names no dolt database; bd cannot be told which database the legacy server serves", metadataPath)
	}
	workspace, ok, err := contract.ReadMetadataProjectID(fsys.OSFS{}, metadataPath)
	if err != nil {
		return scope, err
	}
	if !ok {
		return scope, fmt.Errorf("%s carries no project_id; bd verifies the workspace identity against it and cannot be handed one gc invented", metadataPath)
	}
	scope.Database, scope.Workspace = database, workspace

	// A resume takes its request from bd's journal, not from gc's publication.
	// The publication is retired as part of the stop, so past that point there
	// is nothing left for gc to re-derive the endpoint from — and bd refuses a
	// request that disagrees with its journal, which gc must not be the one to
	// cause. A first run has no journal and reads gc's own live record.
	if journalPhase != "" {
		resumed, err := resolveMigrateHandoffScopeFromJournal(cityPath)
		if err != nil {
			return scope, err
		}
		scope.Endpoint = resumed.Endpoint
		scope.Database, scope.Workspace = resumed.Database, resumed.Workspace
		return scope, nil
	}
	endpoint, err := resolveMigrateHandoffLegacyEndpoint(cityPath)
	if err != nil {
		return scope, err
	}
	scope.Endpoint = endpoint
	return scope, nil
}

// resolveMigrateHandoffLegacyEndpoint reads where gc's own server listens.
//
// gc's published runtime state is the only record that carries a live port
// together with the pid gc believes is serving it. .beads/dolt-server.port is a
// compatibility mirror for raw bd, not a control-plane input, and it outlives
// the process it names.
func resolveMigrateHandoffLegacyEndpoint(cityPath string) (legacyHandoffEndpoint, error) {
	statePath := managedDoltStatePath(cityPath)
	state, err := readDoltRuntimeStateFile(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return legacyHandoffEndpoint{}, fmt.Errorf("gc publishes no managed Dolt runtime state at %s; "+
				"the handoff transfers a running server, so start the city first", statePath)
		}
		return legacyHandoffEndpoint{}, fmt.Errorf("read %s: %w", statePath, err)
	}
	if !state.Running || state.PID <= 0 || state.Port <= 0 {
		return legacyHandoffEndpoint{}, fmt.Errorf("gc's managed Dolt runtime state at %s records no live server (running=%v pid=%d port=%d); "+
			"the handoff transfers a running server, so start the city first", statePath, state.Running, state.PID, state.Port)
	}
	host, err := migrateHandoffLoopbackHost()
	if err != nil {
		return legacyHandoffEndpoint{}, err
	}
	return legacyHandoffEndpoint{Host: host, Port: state.Port, PID: state.PID}, nil
}

// migrateHandoffLoopbackHost resolves the host gc's managed server binds, and
// refuses anything that is not a literal loopback address.
//
// bd refuses the same thing one step later — a transfer it cannot prove is
// local is a transfer it cannot prove anything about — and it refuses a
// hostname too, because resolution can change between the check and the
// connection. Saying so here means the refusal names the variable that caused
// it instead of arriving as an unsupported_scope from another program.
func migrateHandoffLoopbackHost() (string, error) {
	host := defaultManagedDoltBindHost
	if override := strings.TrimSpace(os.Getenv(contract.ManagedCityHostEnv)); override != "" {
		host = override
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("gc's managed Dolt host is %q; the ownership handoff transfers a local server only, "+
			"and bd refuses anything that is not a literal loopback address (unset %s to hand this city over)",
			host, contract.ManagedCityHostEnv)
	}
	return host, nil
}

// resolveMigrateHandoffScopeFromJournal recovers bd's request for a transfer
// already past gc's stop, where gc's own publication has been retired.
//
// bd's journal is the record then: it holds the exact request gc supplied, and
// the rollback restores the workspace to the state that request belongs to.
// Reading it back rather than re-deriving it is also what keeps a resume from
// silently re-scoping the transfer — bd refuses a request that disagrees with
// its journal, and gc should not be the one making them disagree. The pid is
// not recovered: it names a process that is gone, and it was only ever a hint.
func resolveMigrateHandoffScopeFromJournal(cityPath string) (migrateHandoffScope, error) {
	path := filepath.Join(cityPath, ".beads", handoffJournalName)
	data, err := os.ReadFile(path)
	if err != nil {
		return migrateHandoffScope{}, fmt.Errorf("read ownership handoff journal %s: %w", path, err)
	}
	var journal handoffProjectionJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return migrateHandoffScope{}, fmt.Errorf("parse ownership handoff journal %s: %w", path, err)
	}
	request := journal.Request
	if request.Endpoint.Host == "" || request.Endpoint.Port <= 0 || request.Database == "" || request.Workspace == "" {
		return migrateHandoffScope{}, fmt.Errorf("ownership handoff journal %s records no request to resume", path)
	}
	return migrateHandoffScope{
		Path:      cityPath,
		Database:  request.Database,
		Workspace: request.Workspace,
		Endpoint:  legacyHandoffEndpoint{Host: request.Endpoint.Host, Port: request.Endpoint.Port},
	}, nil
}

func printMigrateHandoffReport(stdout io.Writer, report migrateHandoffReport) {
	if report.DryRun {
		fmt.Fprintf(stdout, "DRY RUN: no files written (%s)\n", report.City) //nolint:errcheck
	}
	width := len("STEP")
	for _, step := range report.Steps {
		if len(step.Step) > width {
			width = len(step.Step)
		}
	}
	fmt.Fprintf(stdout, "%-*s  %-4s  %-10s  %s\n", width, "STEP", "WHO", "STATUS", "DETAIL") //nolint:errcheck
	for _, step := range report.Steps {
		detail := step.Detail
		if step.Error != "" {
			detail = step.Error
			if step.ErrorCode != "" {
				detail = "[" + step.ErrorCode + "] " + detail
			}
			if step.Detail != "" {
				detail += " — " + step.Detail
			}
		}
		fmt.Fprintf(stdout, "%-*s  %-4s  %-10s  %s\n", width, step.Step, step.Actor, step.Status, detail) //nolint:errcheck
		if gates := migrateHandoffGateLine(step.Evidence); gates != "" {
			fmt.Fprintf(stdout, "%-*s  %-4s  %-10s  %s\n", width, "", "", "", gates) //nolint:errcheck
		}
	}
	fmt.Fprintf(stdout, "%s: %s\n", report.City, report.Status) //nolint:errcheck
}

// migrateHandoffGateLine renders bd's evidence in a stable order, so an
// operator comparing two runs is comparing the same line.
func migrateHandoffGateLine(gates map[string]string) string {
	if len(gates) == 0 {
		return ""
	}
	names := make([]string, 0, len(gates))
	for gate := range gates {
		names = append(names, gate)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, gate := range names {
		parts = append(parts, gate+"="+gates[gate])
	}
	return "evidence: " + strings.Join(parts, " ")
}
