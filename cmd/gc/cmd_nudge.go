package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/nudgepoller"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gastownhall/gascity/internal/worker"
	"github.com/spf13/cobra"
)

const (
	defaultQueuedNudgeTTL           = 24 * time.Hour
	defaultQueuedNudgeClaimTTL      = 2 * time.Minute
	defaultQueuedNudgeRetryDelay    = 15 * time.Second
	defaultQueuedNudgeMaxAttempts   = 5
	defaultQueuedNudgeDeadRetention = 1 * time.Hour

	// nudgeEnqueueMaintenanceBudget bounds the wall-clock time the foreground
	// `gc sling --nudge` enqueue path spends on best-effort nudge-queue
	// maintenance (expiring/pruning stale entries) while holding the
	// withNudgeQueueState flock. Without this, maintenance does O(backlog)
	// serial store writes with no cap, so a large backlog turns a sub-second
	// foreground call into a multi-minute hang. Items skipped once the budget
	// is exceeded are left untouched for the next enqueue, the per-session
	// poller, or the doctor reaper to handle — never dropped.
	nudgeEnqueueMaintenanceBudget = 2 * time.Second

	defaultNudgePollInterval   = 2 * time.Second
	defaultNudgePollQuiescence = 3 * time.Second
	// A controller wake can legitimately take a couple of minutes when the
	// session has to rematerialize a worktree and complete startup dialog.
	defaultNudgePollStartGrace = 5 * time.Minute

	// nudgePollIdleObserveEvery bounds how many consecutive ticks the poll
	// loop may skip nudgeObserveTarget while nothing is queued for this
	// target before forcing a fresh observation anyway. obs.Running is the
	// only signal that detects a dead session, so this cadence must stay
	// small enough that an idle target still exits promptly once its session
	// ends -- it only needs to be large enough to matter, not to eliminate
	// every idle-tick observation (ga-8x82f4).
	nudgePollIdleObserveEvery = 5

	// defaultNudgePollMemLimitMB is the soft Go runtime memory limit
	// (debug.SetMemoryLimit) installed for the long-lived `gc nudge poll`
	// sidecar. The sidecar re-parses the whole-file city bead store on every
	// poll tick that sees a changed file; on the file backend a large city
	// beads.json (100MB+) inflates the heap to several hundred MB per parse and
	// the runtime otherwise retains ~2x that as unreturned arena, so RSS
	// plateaus near 1GB per sidecar (gc-3ftcq). With one sidecar per active
	// session, the fleet-wide total dominates host memory and drives OOM. The
	// limit caps that arena growth without starving a single live parse (~400MB
	// for a 124MB file). Override with nudgePollMemLimitEnv; an operator-set
	// GOMEMLIMIT takes precedence and disables this default.
	defaultNudgePollMemLimitMB = 512

	// nudgePollMemLimitEnv overrides defaultNudgePollMemLimitMB (in MiB). Set to
	// "0" to disable the soft limit entirely.
	nudgePollMemLimitEnv = "GC_NUDGE_POLL_MEMLIMIT_MB"

	// nudgePollPprofAddrEnv sets the listen address for the sidecar's pprof
	// endpoint. The endpoint only starts when GC_PPROF=1 (see api.StartPprof);
	// pass a distinct address per sidecar to avoid colliding with the
	// supervisor's pprof server on the default 127.0.0.1:6060.
	nudgePollPprofAddrEnv = "GC_NUDGE_POLL_PPROF_ADDR"

	// nudgePollFreeOSInterval throttles debug.FreeOSMemory in the poll loop so
	// transient per-parse garbage is returned to the OS without paying a full
	// GC + scavenge on every short idle tick.
	nudgePollFreeOSInterval = 30 * time.Second
)

// errNudgeSessionFenceMismatch is the contract's fence-mismatch cause, so every
// queue dead-letters on it at once. Its text is what state.json last_error
// has always carried.
var errNudgeSessionFenceMismatch = storebinding.ErrNudgeSessionFenceMismatch

// nudgeManualDropCause is the failure cause `gc nudge drop` passes to
// recordQueuedNudgeFailureWithStore. Its Is method reports equivalence to
// errNudgeSessionFenceMismatch so failedQueuedNudge's unconditional
// dead-letter branch fires on the first call, the same branch drain already
// relies on for a nudge whose target fence no longer matches, rather than
// the attempts/TTL branch, which would just requeue a fresh item instead of
// dropping it.
type nudgeManualDropCause struct{}

func (nudgeManualDropCause) Error() string { return "dropped by operator (gc nudge drop)" }

func (nudgeManualDropCause) Is(target error) bool { return target == errNudgeSessionFenceMismatch }

var errNudgeManualDrop error = nudgeManualDropCause{}

var (
	// Test seams for cmd_nudge_test.go. Tests that replace these package
	// variables must stay serial; do not use t.Parallel in those tests.
	nudgeCityUsesManagedReconciler           = cityUsesManagedReconciler
	nudgePokeController                      = enqueueController
	nudgeObserveTarget                       = workerObserveNudgeTarget
	nudgePollDeliverQueued                   = tryDeliverQueuedNudgesByPoller
	nudgeWarningWriter             io.Writer = os.Stderr
)

type nudgeDeliveryMode string

const (
	nudgeDeliveryImmediate nudgeDeliveryMode = "immediate"
	nudgeDeliveryWaitIdle  nudgeDeliveryMode = "wait-idle"
	nudgeDeliveryQueue     nudgeDeliveryMode = "queue"
)

type queuedNudge = nudgequeue.Item

type nudgeQueueState = nudgequeue.State

type nudgeTarget struct {
	cityPath          string
	cityName          string
	cfg               *config.City
	alias             string
	aliasHistory      []string
	identity          string
	transport         string
	agent             config.Agent
	resolved          *config.ResolvedProvider
	sessionID         string
	continuationEpoch string
	sessionName       string
	// fromClaim marks a target built from a contract ClaimTarget
	// (fileClaimTarget): its queue keys are exactly claimKeys.
	fromClaim bool
	claimKeys []string
	// fenceCensus, when set, is the Leg-B census claimTarget hands a claim
	// instead of listing the city's sessions itself: the dispatcher's
	// snapshot census, which only its skip probe uses.
	fenceCensus func() (map[string]struct{}, bool)
}

type nudgeStatusJSON struct {
	SchemaVersion string            `json:"schema_version"`
	Command       string            `json:"command"`
	CityPath      string            `json:"city_path"`
	Agent         string            `json:"agent"`
	Session       string            `json:"session"`
	SessionID     string            `json:"session_id,omitempty"`
	Counts        nudgeStatusCounts `json:"counts"`
	Pending       []queuedNudge     `json:"pending"`
	InFlight      []queuedNudge     `json:"in_flight"`
	Dead          []queuedNudge     `json:"dead"`

	// DispatchSkips is the dispatch tick's running, city-wide (not
	// agent-scoped) count of silent skips by reason, since the queue state
	// file was created. Omitted when empty (legacy-mode cities never
	// populate it; a fresh queue has no ticks recorded yet).
	DispatchSkips map[string]int64 `json:"dispatch_skips,omitempty"`
}

type nudgeStatusCounts struct {
	Pending  int `json:"pending"`
	InFlight int `json:"in_flight"`
	Dead     int `json:"dead"`
}

func (t nudgeTarget) agentKey() string {
	if t.alias != "" {
		return t.alias
	}
	if t.sessionID != "" {
		return t.sessionID
	}
	if qn := t.agent.QualifiedName(); qn != "" {
		return qn
	}
	if t.identity != "" {
		return t.identity
	}
	return t.sessionName
}

func (t nudgeTarget) pollerKey() string {
	// Queue items keep alias-oriented Agent values for matching/readability;
	// poller ownership prefers the concrete session ID so live generations that
	// share one runtime session name do not reuse each other's sidecar. Keep
	// the concrete-ID precedence aligned with session.PollerKeyFromBead for
	// session-bead-derived poller launches.
	if t.sessionID != "" {
		return t.sessionID
	}
	return t.agentKey()
}

func (t nudgeTarget) queueKeys() []string {
	sources := [][]string{t.claimKeys}
	if !t.fromClaim {
		sources = [][]string{{t.alias, t.sessionID, t.agent.QualifiedName(), t.identity, t.sessionName}, t.aliasHistory}
	}
	var keys []string
	seen := map[string]bool{}
	for _, source := range sources {
		for _, key := range source {
			key = strings.TrimSpace(key)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			keys = append(keys, key)
		}
	}
	return keys
}

func (t nudgeTarget) matchesQueueAgent(agent string) bool {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return false
	}
	for _, key := range t.queueKeys() {
		if key == agent {
			return true
		}
	}
	return false
}

func (t nudgeTarget) sessionTransport() string {
	if t.transport != "" {
		return t.transport
	}
	return t.agent.Session
}

// processNames are the agent processes the target's provider runs, by
// which a runtime handle tells a crashed agent in a pane still up.
func (t nudgeTarget) processNames() []string {
	if t.resolved == nil {
		return nil
	}
	return t.resolved.ProcessNames
}

func (t nudgeTarget) providerName() string {
	if t.resolved != nil && strings.TrimSpace(t.resolved.Name) != "" {
		return strings.TrimSpace(t.resolved.Name)
	}
	if strings.TrimSpace(t.agent.Provider) != "" {
		return strings.TrimSpace(t.agent.Provider)
	}
	if t.cfg != nil {
		return strings.TrimSpace(t.cfg.Workspace.Provider)
	}
	return ""
}

// providerFamily returns the builtin provider family the target runs, which is
// what the claude wait-idle gates compare against. A resolved provider reports
// its BuiltinAncestor; otherwise the family is derived from the configured
// provider name through the city's providers, so a wrapped alias such as a
// provider with base = "builtin:claude" is still claude-family. A name with no
// builtin family falls back to the raw configured name.
func (t nudgeTarget) providerFamily() string {
	if t.resolved != nil {
		if family := strings.TrimSpace(t.resolved.BuiltinAncestor); family != "" {
			return family
		}
	}
	name := t.providerName()
	if t.cfg != nil {
		if family := strings.TrimSpace(config.BuiltinFamily(name, t.cfg.Providers)); family != "" {
			return family
		}
	}
	return name
}

type queuedNudgeOptions struct {
	ID                string
	SessionID         string
	ContinuationEpoch string
	Reference         *nudgeReference
}

func newNudgeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nudge",
		Short: "Inspect and deliver deferred nudges",
		Long: `Inspect and deliver deferred nudges.

Deferred nudges are reminders that were queued because the target agent
was asleep or was not at a safe interactive boundary yet.`,
	}
	cmd.AddCommand(
		newNudgeStatusCmd(stdout, stderr),
		newNudgeDrainCmd(stdout, stderr),
		newNudgePollCmd(stdout, stderr),
		newNudgeDropCmd(stdout, stderr),
	)
	return cmd
}

func newNudgeStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status [session]",
		Short: "Show queued and dead-letter nudges for a session",
		Long: `Show queued and dead-letter nudges for a session.

Defaults to $GC_ALIAS or $GC_SESSION_ID when run inside a session.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdNudgeStatus(args, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func newNudgeDrainCmd(stdout, stderr io.Writer) *cobra.Command {
	var inject bool
	var hookFormat string
	cmd := &cobra.Command{
		Use:    "drain [session]",
		Short:  "Deliver queued nudges for a session",
		Long:   "Deliver queued nudges for a session. Used by runtime hooks.",
		Args:   cobra.MaximumNArgs(1),
		Hidden: true,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdNudgeDrainWithFormat(args, inject, hookFormat, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&inject, "inject", false, "emit <system-reminder> output for hook injection")
	cmd.Flags().StringVar(&hookFormat, "hook-format", "", "format hook output for a provider")
	return cmd
}

func newNudgePollCmd(stdout, stderr io.Writer) *cobra.Command {
	var sessionName string
	var interval time.Duration
	var quiescence time.Duration
	cmd := &cobra.Command{
		Use:    "poll [session]",
		Short:  "Poll and deliver queued nudges for sessions that need out-of-band delivery",
		Long:   "Poll and deliver queued nudges for sessions that need an out-of-band delivery fallback. Used internally.",
		Args:   cobra.MaximumNArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmdNudgePoll(args, sessionName, interval, quiescence, cmd.Flags().Changed("interval"), stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sessionName, "session", "", "runtime session name (defaults to $GC_SESSION_NAME)")
	cmd.Flags().DurationVar(&interval, "interval", defaultNudgePollInterval, "poll interval (overrides [session] nudge_poll_interval)")
	cmd.Flags().DurationVar(&quiescence, "quiescence", defaultNudgePollQuiescence, "minimum inactivity before injecting")
	return cmd
}

func newNudgeDropCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "drop <id>...",
		Short: "Dead-letter one or more pending or in-flight nudges",
		Long: `Dead-letter one or more pending or in-flight nudges by ID.

Each dropped nudge is terminalized through the same dead-letter path a
failed delivery attempt uses, so it lands in "gc nudge status" as dead
rather than disappearing silently. Find IDs with "gc nudge status".

Dropping an in-flight nudge dead-letters it even if it was already
injected into the session but not yet acked.

This only accepts explicit nudge IDs; it does not do bulk or age-based
selection.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdNudgeDrop(args, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func cmdNudgeStatus(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	targetID := os.Getenv("GC_ALIAS")
	if targetID == "" {
		targetID = os.Getenv("GC_SESSION_ID")
	}
	if len(args) > 0 {
		targetID = args[0]
	}
	if targetID == "" {
		fmt.Fprintln(stderr, "gc nudge status: session not specified (set $GC_ALIAS/$GC_SESSION_ID or pass an alias/id)") //nolint:errcheck
		return 1
	}

	target, err := resolveNudgeTarget(targetID, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge status: %v\n", err) //nolint:errcheck
		return 1
	}

	pending, inFlight, dead, err := listQueuedNudgesForTarget(target.cityPath, target, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge status: %v\n", err) //nolint:errcheck
		return 1
	}

	// City-wide (not agent-scoped) skip-reason totals from the supervisor
	// dispatch tick, read directly off the persisted queue state — a raw
	// read so this doesn't re-run the maintenance sweep listQueuedNudgesForTarget
	// above already ran. Best-effort: a load failure here must not fail the
	// whole status command over a diagnostics-only field.
	var dispatchSkips map[string]int64
	if skips, skipsErr := openNudgeQueue(target.cityPath, beads.NudgesStore{}).Skips.DispatchSkips(); skipsErr == nil {
		dispatchSkips = skips
	}

	if jsonOutput {
		if err := writeCLIJSONLine(stdout, nudgeStatusJSON{
			SchemaVersion: "1",
			Command:       "nudge status",
			CityPath:      target.cityPath,
			Agent:         target.agentKey(),
			Session:       target.sessionName,
			SessionID:     target.sessionID,
			Counts: nudgeStatusCounts{
				Pending:  len(pending),
				InFlight: len(inFlight),
				Dead:     len(dead),
			},
			Pending:       nudgeStatusItems(pending),
			InFlight:      nudgeStatusItems(inFlight),
			Dead:          nudgeStatusItems(dead),
			DispatchSkips: dispatchSkips,
		}); err != nil {
			fmt.Fprintf(stderr, "gc nudge status: writing JSON: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "AGENT\tPENDING\tIN_FLIGHT\tDEAD\tSESSION\n") //nolint:errcheck
	_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\n",
		target.agentKey(), len(pending), len(inFlight), len(dead), target.sessionName)
	_ = tw.Flush()

	if len(pending) > 0 {
		fmt.Fprintln(stdout, "") //nolint:errcheck
		for _, item := range pending {
			_, _ = fmt.Fprintf(stdout, "pending  %s  due=%s  source=%s  %s\n",
				item.ID, formatDueTime(item.DeliverAfter), item.Source, item.Message)
		}
	}
	if len(inFlight) > 0 {
		fmt.Fprintln(stdout, "") //nolint:errcheck
		for _, item := range inFlight {
			_, _ = fmt.Fprintf(stdout, "in-flight  %s  lease=%s  source=%s  %s\n",
				item.ID, formatDueTime(item.LeaseUntil), item.Source, item.Message)
		}
	}
	if len(dead) > 0 {
		fmt.Fprintln(stdout, "") //nolint:errcheck
		for _, item := range dead {
			_, _ = fmt.Fprintf(stdout, "dead     %s  reason=%s  source=%s  %s\n",
				item.ID, deadReason(item), item.Source, item.Message)
		}
	}
	if len(dispatchSkips) > 0 {
		fmt.Fprintln(stdout, "")                                                         //nolint:errcheck
		fmt.Fprintln(stdout, "dispatch-tick skips (city-wide, all agents, cumulative):") //nolint:errcheck
		for _, reason := range sortedNudgeDispatchSkipReasons(dispatchSkips) {
			_, _ = fmt.Fprintf(stdout, "  %s=%d\n", reason, dispatchSkips[reason])
		}
	}
	return 0
}

// sortedNudgeDispatchSkipReasons returns counts's keys in deterministic
// (alphabetical) order for stable CLI output.
func sortedNudgeDispatchSkipReasons(counts map[string]int64) []string {
	reasons := make([]string, 0, len(counts))
	for reason := range counts {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	return reasons
}

// nudgeStatusItems is items as gc nudge status --json lists them: never null,
// and without the queue's internal shadow-debt bookkeeping, which is not part
// of the command's item shape.
func nudgeStatusItems(items []queuedNudge) []queuedNudge {
	out := make([]queuedNudge, len(items))
	for i, item := range items {
		item.Shadow = nil
		out[i] = item
	}
	return out
}

// nudgeDropStatus classifies an ID passed to `gc nudge drop` against the
// current queue state, before the drop's write pass runs.
type nudgeDropStatus int

const (
	nudgeDropStatusNotFound nudgeDropStatus = iota
	nudgeDropStatusDroppable
	nudgeDropStatusAlreadyDead
)

type nudgeDropResult struct {
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Outcome string `json:"outcome,omitempty"`
	Error   string `json:"error,omitempty"`
}

type nudgeDropJSON struct {
	SchemaVersion string            `json:"schema_version"`
	OK            bool              `json:"ok"`
	Command       string            `json:"command"`
	CityPath      string            `json:"city_path"`
	Results       []nudgeDropResult `json:"results"`
}

func cmdNudgeDrop(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	var ids []string
	for _, arg := range args {
		id := strings.TrimSpace(arg)
		if id == "" {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		fmt.Fprintln(stderr, "gc nudge drop: no nudge ID given") //nolint:errcheck
		return 1
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge drop: %v\n", err) //nolint:errcheck
		return 1
	}
	return doNudgeDrop(cityPath, ids, jsonOutput, stdout, stderr)
}

func doNudgeDrop(cityPath string, ids []string, jsonOutput bool, stdout, stderr io.Writer) int {
	now := time.Now()
	statuses, err := classifyQueuedNudgeIDs(cityPath, ids, now)
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge drop: %v\n", err) //nolint:errcheck
		return 1
	}

	var droppable []string
	for _, id := range ids {
		if statuses[id] == nudgeDropStatusDroppable {
			droppable = append(droppable, id)
		}
	}
	// Classification and the write pass take the queue lock separately -- the
	// flock is per-process and non-reentrant -- so an ID can be delivered and
	// acked in between. Report from the write pass's own dead-lettered slice
	// rather than the classification, or a nudge nobody dropped is still
	// reported as dropped.
	dropped := make(map[string]bool, len(droppable))
	if len(droppable) > 0 {
		store, opened, err := openNudgeBeadStoreOwned(cityPath)
		if err != nil {
			fmt.Fprintf(stderr, "gc nudge drop: %v\n", err) //nolint:errcheck
			return 1
		}
		defer closeBeadStoreHandle(opened) //nolint:errcheck // best-effort
		deadLettered, err := recordQueuedNudgeFailureDetailed(cityPath, store, droppable, errNudgeManualDrop, now)
		if err != nil {
			fmt.Fprintf(stderr, "gc nudge drop: %v\n", err) //nolint:errcheck
			return 1
		}
		for _, item := range deadLettered {
			dropped[item.ID] = true
		}
	}

	exit := 0
	results := make([]nudgeDropResult, 0, len(ids))
	for _, id := range ids {
		switch statuses[id] {
		case nudgeDropStatusDroppable:
			if !dropped[id] {
				exit = 1
				results = append(results, nudgeDropResult{ID: id, OK: false, Error: "no longer queued (delivered or acked concurrently)"})
				fmt.Fprintf(stderr, "gc nudge drop %s: no longer queued (delivered or acked concurrently)\n", id) //nolint:errcheck
				continue
			}
			results = append(results, nudgeDropResult{ID: id, OK: true, Outcome: "dropped"})
			if !jsonOutput {
				fmt.Fprintf(stdout, "Dropped nudge %s\n", id) //nolint:errcheck
			}
		case nudgeDropStatusAlreadyDead:
			exit = 1
			results = append(results, nudgeDropResult{ID: id, OK: false, Error: "already dead-lettered"})
			fmt.Fprintf(stderr, "gc nudge drop %s: already dead-lettered\n", id) //nolint:errcheck
		default:
			exit = 1
			results = append(results, nudgeDropResult{ID: id, OK: false, Error: "no such queued nudge"})
			fmt.Fprintf(stderr, "gc nudge drop %s: no such queued nudge\n", id) //nolint:errcheck
		}
	}

	if jsonOutput {
		if err := writeCLIJSONLine(stdout, nudgeDropJSON{
			SchemaVersion: "1",
			OK:            exit == 0,
			Command:       "nudge drop",
			CityPath:      cityPath,
			Results:       results,
		}); err != nil {
			fmt.Fprintf(stderr, "gc nudge drop: writing JSON: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	return exit
}

// classifyQueuedNudgeIDs reports, for each of the given IDs, whether it is
// currently droppable (pending or in-flight), already dead-lettered, or not
// found in the queue at all. The lookup runs the same maintenance every other
// queue read does, so a since-expired or since-recovered item is classified
// against current state rather than a stale snapshot.
func classifyQueuedNudgeIDs(cityPath string, ids []string, now time.Time) (map[string]nudgeDropStatus, error) {
	buckets, err := openNudgeQueue(cityPath, beads.NudgesStore{}).Queue.Lookup(ids, now)
	statuses := make(map[string]nudgeDropStatus, len(buckets))
	for id, bucket := range buckets {
		switch bucket {
		case nudgequeue.BucketPending, nudgequeue.BucketInFlight:
			statuses[id] = nudgeDropStatusDroppable
		case nudgequeue.BucketDead:
			statuses[id] = nudgeDropStatusAlreadyDead
		}
	}
	return statuses, err
}

func cmdNudgeDrainWithFormat(args []string, inject bool, hookFormat string, stdout, stderr io.Writer) int {
	// One composed city-config load for the whole invocation: the target
	// resolution, the step reminder and every store open each loaded it again
	// (see invocationConfigMemo).
	defer beginInvocationConfigMemo()()
	// --inject writes a <system-reminder> straight into a provider's system
	// prompt, and gc stages the overlays that call it into the session work
	// directory — commonly a city or rig root — so a human who opens the same
	// provider there gets them too. With no gc identity this did not come from
	// a session gc started, so there is no session to drain for. Naming a
	// session explicitly is a deliberate request and is still served; the plain
	// non-inject form is the human-facing one and is untouched (#5304).
	if inject && len(args) == 0 && !hookHasManagedIdentity() {
		return 0
	}
	// On every prompt, emit a live clock (operator-local + UTC + epoch) and
	// the agent's active formula step (if any) as UserPromptSubmit hook context.
	// When a nudge also fires we fold everything into that nudge's single
	// provider-formatted payload (see the combined write below); otherwise this
	// deferred fallback emits clock+step on their own. Either way exactly one
	// provider hook context is written per invocation, so JSON formats
	// (codex/gemini) stay one valid document rather than two concatenated objects.
	// See clock_inject.go and wisp_step_inject.go.
	var wispExtra string // step reminder; captured by defer closure
	emittedHookContext := false
	var injectPrefix string
	var contextUsage contextUsageSample
	// hookInput is the provider's hook payload; laneConv is the conversation
	// the in-band lane pass serves (conversation_lane_hook.go), laneDue that
	// the fast path's probe already found an open lane op there, and lane what
	// the pass did, so the drain rings the lane host once it is done.
	var hookInput []byte
	var laneConv, laneCity string
	var laneDue bool
	var lane hookLaneOutcome
	// laneRows are the journal rows the pass wrote, rung with the drain's
	// last doorbell rather than as the pass writes them.
	var laneRows *journalRingQueue
	if inject {
		// Registered first so it runs last: after the fallback hook context
		// below and after every ack and stamp of the drain.
		defer func() { hookLaneRing(laneCity, laneConv, lane, laneRows) }()
		// Read the provider hook input once (UserPromptSubmit JSON on stdin,
		// pipe-only — see readHookStdin) and build the shared inject prefix:
		// the clock line plus, when context pressure crosses its threshold,
		// the context-usage guidance (see context_inject.go).
		hookInput = readHookStdin()
		contextUsage = readContextUsageSample(hookInput)
		laneConv = hookLaneConversation(args, hookFormat)
		injectPrefix = clockInjectLine()
		defer func() {
			if !emittedHookContext {
				line := injectPrefix + wispExtra
				if line != "" {
					_ = writeProviderHookContextForEvent(stdout, hookFormat, "UserPromptSubmit", line)
				}
			}
		}()
	}
	targetID := os.Getenv("GC_ALIAS")
	if targetID == "" {
		targetID = os.Getenv("GC_SESSION_ID")
	}
	if len(args) > 0 {
		targetID = args[0]
	}
	if targetID == "" {
		if inject {
			injectPrefix += contextInjectLineForSample(contextUsage, nil, nil)
			return 0
		}
		fmt.Fprintln(stderr, "gc nudge drain: session not specified (set $GC_ALIAS/$GC_SESSION_ID or pass an alias/id)") //nolint:errcheck
		return 1
	}

	// The step reminder is the slowest read a drain makes: in process it opens
	// the rig store at GC_RIG_ROOT and scans that rig by gc.routed_to once per
	// identity. Ask the controller for it now, so the request runs while the
	// target resolves; both paths below take it for the city the explicit
	// environment names, which is the city they resolve.
	var stepLookup *wispStepLookup
	explicitCity, explicit := "", false
	if inject {
		if explicitCity, explicit = nudgeDrainExplicitCityPath(); explicit {
			stepLookup = startWispStepLookup(explicitCity, stderr)
			defer stepLookup.stop()
		}
	}

	// Empty-queue fast path. Resolving the target loads the city config and
	// opens the session store, and on an empty queue the only things it would
	// feed are the context advisory (which renders nothing for a payload with
	// no usage sample, whatever the agent's policy) and the step reminder
	// (which depends only on the city and the hook's identity env). The hook
	// context written is therefore the same as the full path's, so skip the
	// resolution; a queued item, an open lane op of the hook's conversation
	// (one keyed probe, asked only while the controller is down:
	// hookLaneDueWithoutController) or a usage sample takes the full path.
	wispPrefetched := false
	if inject && !contextUsage.ok {
		if explicit && nudgeQueueIsEmpty(explicitCity) {
			wispExtra = stepLookup.content()
			wispPrefetched = true
			// The full path writes the step reminder only once the target
			// resolves, so a non-empty reminder still needs that proof. Recheck
			// the queue too: a nudge enqueued during the lookup must be claimed.
			if wispExtra == "" && nudgeQueueIsEmpty(explicitCity) {
				laneDue = hookLaneDueWithoutController(laneConv, explicitCity, stepLookup, stderr)
				if !laneDue {
					return 0
				}
			}
		}
	}

	// With no explicit target, the drain is the hook's own: GC_SESSION_ID
	// names its session bead exactly, so it is preferred over GC_ALIAS.
	target, err := resolveNudgeTargetPreferringHookIdentity(targetID, len(args) == 0, stderr)
	if err != nil {
		if inject {
			switch {
			case errors.Is(err, errHookSessionClosed):
				fmt.Fprintf(stderr, "gc nudge drain: %v; not draining its nudges\n", err) //nolint:errcheck
			case isControlPlaneDescriptorError(err):
				fmt.Fprintf(stderr, "gc nudge drain: %v\n", err) //nolint:errcheck
			}
			wispExtra = ""
			injectPrefix += contextInjectLineForSample(contextUsage, nil, nil)
			return 0
		}
		fmt.Fprintf(stderr, "gc nudge drain: %v\n", err) //nolint:errcheck
		return 1
	}
	if inject {
		injectPrefix += contextInjectLineForSample(contextUsage, target.cfg.AgentDefaults.ContextAdvisory, target.agent.ContextAdvisory)
		if !wispPrefetched {
			wispExtra = stepLookup.contentFor(target.cityPath)
		}
	}

	// injectLane runs the in-band lane pass for the hook's conversation
	// (conversation_lane_hook.go). Its text joins the one hook context, after
	// the legacy nudges out carries (legacy of them), in the room the rest of
	// that context leaves.
	injectLane := func(out string, legacy int) hookLaneOutcome {
		if laneConv == "" {
			return hookLaneOutcome{}
		}
		laneCity = target.cityPath
		sessions := hookLaneSessionStore(target.cityPath, target.cfg, stderr)
		laneRows = deferJournalRings(sessions)
		return hookDrainLane(target.cityPath, sessions, laneConv, laneDue, hookInput, legacy, hookLaneTextRoom(injectPrefix, out, wispExtra), func(laneText string) error {
			emittedHookContext = true
			return writeProviderHookContextForEvent(stdout, hookFormat, "UserPromptSubmit", injectPrefix+out+laneText+wispExtra)
		}, stderr)
	}
	// injectLaneOnly is the injecting drain with no legacy nudge to deliver.
	injectLaneOnly := func() int {
		lane = injectLane("", 0)
		if lane.emitErr != nil {
			fmt.Fprintf(stderr, "gc nudge drain: writing the hook context: %v\n", lane.emitErr) //nolint:errcheck
		}
		return 0
	}

	now := time.Now()
	items, err := claimDueQueuedNudgesForTarget(target.cityPath, target, now)
	if err != nil {
		if inject {
			fmt.Fprintf(stderr, "gc nudge drain: claiming queued nudges: %v\n", err) //nolint:errcheck
			return injectLaneOnly()
		}
		fmt.Fprintf(stderr, "gc nudge drain: %v\n", err) //nolint:errcheck
		return 1
	}
	// A deferred user submit is the user's prompt, which hook output cannot
	// deliver: it would reach the agent as context. Leave it for the poller,
	// which types it (tryDeliverQueuedNudgesByPoller).
	var submits []queuedNudge
	items, submits = splitQueuedUserSubmits(items)
	if len(submits) > 0 {
		if err := releaseQueuedNudgeClaims(target.cityPath, queuedNudgeIDs(submits)); err != nil {
			fmt.Fprintf(stderr, "gc nudge drain: leaving deferred submits for the poller: %v; their lease expiry returns them\n", err) //nolint:errcheck
		}
	}
	if len(items) == 0 {
		if inject {
			return injectLaneOnly()
		}
		return 1
	}
	deliveryStore := openNudgeBeadStore(target.cityPath)
	// Two-store split: the nudge-queue delivery store stays on the nudges class
	// (openNudgeBeadStore), while the session-class ops — wait-bead reads in
	// splitQueuedNudgesForDelivery and the last-nudge-delivered stamp — route
	// through the session store. Identity today (single backend).
	deliverySessStore := cliSessionStore(deliveryStore.Store, target.cfg, target.cityPath)
	var deliverySessFront *session.Store
	var deliveryMailProvider mail.Provider
	if deliveryStore.Store != nil {
		deliverySessFront = sessionFrontDoor(deliverySessStore)
		// Same split for the mail gate: the message re-read in
		// splitQueuedNudgesForDelivery is messaging-class (cliMailStore), while the
		// provider's session-addressing half reuses the session store resolved above.
		deliveryMailProvider = newMailProviderWithSessionStore(cliMailStore(deliveryStore.Store, target.cfg, target.cityPath).Store, deliverySessStore)
	}
	items, rejected := splitQueuedNudgesForTarget(target, items)
	if len(rejected) > 0 {
		_ = recordQueuedNudgeFailureWithStore(target.cityPath, deliveryStore, queuedNudgeIDs(rejected), errNudgeSessionFenceMismatch, time.Now())
	}
	candidates := items
	items, blocked, err := splitQueuedNudgesForDelivery(sessionFrontDoor(deliverySessStore), deliveryMailProvider, candidates)
	if err != nil {
		// Release the claims so the next drain or poller pass retries
		// promptly instead of waiting out the in-flight lease.
		_ = releaseQueuedNudgeClaims(target.cityPath, queuedNudgeIDs(candidates))
		if inject {
			fmt.Fprintf(stderr, "gc nudge drain: validating claimed nudges: %v\n", err) //nolint:errcheck
			return injectLaneOnly()
		}
		fmt.Fprintf(stderr, "gc nudge drain: validating claimed nudges: %v\n", err) //nolint:errcheck
		return 1
	}
	if len(blocked) > 0 {
		if err := terminalizeBlockedQueuedNudges(target.cityPath, blocked); err != nil {
			// Best-effort: blocked-item bookkeeping must not abort delivery
			// of the remaining items. The blocked items stay in-flight and
			// lease expiry returns them to pending for a later pass.
			fmt.Fprintf(stderr, "gc nudge drain: withdrawing blocked nudges: %v\n", err) //nolint:errcheck
		}
	}
	if len(items) == 0 {
		if inject {
			return injectLaneOnly()
		}
		return 1
	}

	var out string
	if inject {
		out = formatNudgeInjectOutput(items)
	} else {
		out = formatNudgeRuntimeMessage(items)
	}
	var writeErr error
	if inject {
		// Fold the clock, the due lane notices and the active formula step
		// into the nudge so a single provider-formatted payload carries all;
		// the lane pass writes it when it takes a batch, and otherwise this is
		// the one place the combined context is written.
		lane = injectLane(out, len(items))
		if lane.emitted {
			writeErr = lane.emitErr
		} else {
			emittedHookContext = true
			writeErr = writeProviderHookContextForEvent(stdout, hookFormat, "UserPromptSubmit", injectPrefix+out+wispExtra)
		}
	} else {
		_, writeErr = io.WriteString(stdout, out)
	}
	if writeErr != nil {
		_ = recordQueuedNudgeFailureWithStore(target.cityPath, deliveryStore, queuedNudgeIDs(items), writeErr, time.Now())
		if inject {
			return 0
		}
		fmt.Fprintf(stderr, "gc nudge drain: writing output: %v\n", writeErr) //nolint:errcheck
		return 1
	}
	if inject {
		// gc hook run discards this output if the hook overruns its deadline,
		// so ack only while there is time to finish; otherwise release the
		// claims so the next drain delivers the nudges again.
		if !hookDeadlineAllowsNudgeAck(fmt.Sprintf("%d injected nudge(s)", len(items)), stderr) {
			if err := releaseQueuedNudgeClaims(target.cityPath, queuedNudgeIDs(items)); err != nil {
				fmt.Fprintf(stderr, "gc nudge drain: releasing injected nudges: %v; their lease expiry returns them\n", err) //nolint:errcheck
			}
			return 0
		}
		if err := ackQueuedNudgesWithOutcome(target.cityPath, queuedNudgeIDs(items), "accepted_for_injection", "", "hook-transport-accepted"); err != nil {
			fmt.Fprintf(stderr, "gc nudge drain: recording injection ack: %v\n", err) //nolint:errcheck
			return 0
		}
		// The delivery stamp is best-effort bookkeeping after the destructive
		// ack; it is skipped rather than run into the deadline.
		if hookDeadlineStillLeavesTime() {
			stampLastNudgeDeliveredAt(deliverySessFront, target.sessionID, time.Now())
		}
		return 0
	}
	if err := ackQueuedNudges(target.cityPath, queuedNudgeIDs(items)); err != nil {
		fmt.Fprintf(stderr, "gc nudge drain: %v\n", err) //nolint:errcheck
		return 1
	}
	stampLastNudgeDeliveredAt(deliverySessFront, target.sessionID, time.Now())
	return 0
}

func queuedNudgeOptionsFromTarget(target nudgeTarget) queuedNudgeOptions {
	return queuedNudgeOptions{
		SessionID:         target.sessionID,
		ContinuationEpoch: target.continuationEpoch,
	}
}

// configureNudgePollRuntime bounds the long-lived nudge-poll sidecar's memory
// footprint and, when GC_PPROF=1, exposes a pprof endpoint for diagnosing it.
//
// Root cause (gc-3ftcq): each poll tick that observes a changed city beads.json
// re-parses the entire whole-file store. On the file backend a 100MB+ beads.json
// parses to ~400MB of live heap, and across reparses the Go runtime retains the
// arena (default GOGC headroom), so RSS plateaus near 1GB per sidecar. One
// sidecar runs per active session, so the fleet total is the dominant host-OOM
// driver. A soft memory limit makes the GC return freed arena to the OS instead
// of holding ~2x headroom; the per-parse working set itself is irreducible
// without a query-capable read path (tracked as follow-up).
//
// It returns a cleanup func the caller must defer.
func configureNudgePollRuntime(stderr io.Writer) func() {
	// Respect an operator-provided GOMEMLIMIT; debug.SetMemoryLimit would
	// otherwise silently override it. Only install our default when none is set.
	if strings.TrimSpace(os.Getenv("GOMEMLIMIT")) == "" {
		limitMB := defaultNudgePollMemLimitMB
		if raw := strings.TrimSpace(os.Getenv(nudgePollMemLimitEnv)); raw != "" {
			switch v, err := strconv.Atoi(raw); {
			case err != nil || v < 0:
				// An unparseable or negative override is almost certainly an
				// operator typo (e.g. "512mb"). Warn instead of silently
				// falling back so the misconfiguration is visible; "0" remains
				// a valid, intentional disable handled by the v>0 check below.
				fmt.Fprintf(stderr, "gc nudge poll: ignoring invalid %s=%q; using default %dMiB\n", //nolint:errcheck
					nudgePollMemLimitEnv, raw, defaultNudgePollMemLimitMB)
			default:
				limitMB = v
			}
		}
		if limitMB > 0 {
			debug.SetMemoryLimit(int64(limitMB) * 1024 * 1024)
		}
	}

	srv, err := api.StartPprof(strings.TrimSpace(os.Getenv(nudgePollPprofAddrEnv)))
	if err != nil {
		// pprof is a diagnostic nicety; a bind failure (e.g. address already in
		// use by another sidecar) must not stop the poller from delivering.
		fmt.Fprintf(stderr, "gc nudge poll: pprof: %v\n", err) //nolint:errcheck
	}
	if srv == nil {
		return func() {}
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx) //nolint:errcheck // best-effort shutdown on exit
	}
}

// resolveNudgePollInterval picks the poller cycle interval: an explicitly
// passed --interval flag wins; otherwise a positive [session]
// nudge_poll_interval from the city config; otherwise the passed default.
func resolveNudgePollInterval(cityPath string, flagValue time.Duration, flagExplicit bool) time.Duration {
	if flagExplicit {
		return flagValue
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil || cfg == nil {
		return flagValue
	}
	if d := cfg.Session.NudgePollIntervalDuration(); d > 0 {
		return d
	}
	return flagValue
}

func cmdNudgePoll(args []string, sessionName string, interval, quiescence time.Duration, intervalExplicit bool, _ io.Writer, stderr io.Writer) int {
	targetID := os.Getenv("GC_ALIAS")
	if targetID == "" {
		targetID = os.Getenv("GC_SESSION_ID")
	}
	if len(args) > 0 {
		targetID = args[0]
	}
	if targetID == "" {
		fmt.Fprintln(stderr, "gc nudge poll: session not specified (set $GC_ALIAS/$GC_SESSION_ID or pass an alias/id)") //nolint:errcheck
		return 1
	}
	target, err := resolveNudgeTarget(targetID, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge poll: %v\n", err) //nolint:errcheck
		return 1
	}
	if sessionName != "" {
		target.sessionName = sessionName
	}
	if target.sessionName == "" {
		fmt.Fprintln(stderr, "gc nudge poll: session name unavailable") //nolint:errcheck
		return 1
	}
	interval = resolveNudgePollInterval(target.cityPath, interval, intervalExplicit)

	release, err := acquireNudgePollerLease(target.cityPath, target.sessionName, target.pollerKey())
	if err != nil {
		if errors.Is(err, errNudgePollerRunning) {
			return 0
		}
		fmt.Fprintf(stderr, "gc nudge poll: %v\n", err) //nolint:errcheck
		return 1
	}
	defer release()

	stopRuntime := configureNudgePollRuntime(stderr)
	defer stopRuntime()

	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge poll: %v\n", err) //nolint:errcheck
		return 1
	}
	store, err := openNudgeBeadStoreErr(target.cityPath)
	if err != nil || store.Store == nil {
		fmt.Fprintf(stderr, "gc nudge poll: opening the nudge store for %q: %v\n", target.agentKey(), err) //nolint:errcheck
		return 1
	}
	// Session-class store for the observe read (nudgeObserveTarget); the raw
	// nudges store keeps flowing to the queue-delivery path. Identity today.
	sessStore := cliSessionStore(store.Store, target.cfg, target.cityPath)
	var missingSince time.Time
	var lastFreeOS time.Time
	var idleTicksSinceObserve int
	gate := newNudgePollGate()
	skips := &nudgePollSkips{cityPath: target.cityPath}
	defer func() { skips.flush(time.Now(), stderr) }()
	for {
		// Each tick that observes a changed beads.json re-parses the whole-file
		// store, leaving several hundred MB of transient garbage. The soft
		// memory limit caps live arena, but proactively returning freed pages to
		// the OS on a throttled cadence keeps steady-state RSS near the live
		// working set rather than the GC's high-water mark.
		if now := time.Now(); now.Sub(lastFreeOS) >= nudgePollFreeOSInterval {
			debug.FreeOSMemory()
			lastFreeOS = now
		}

		// Cheap gate, read before paying for the expensive observe/deliver
		// pair: this target's queue state alone (no tmux/session probe) tells
		// us whether there is anything to deliver this tick. When nothing is
		// due, most ticks skip nudgeObserveTarget entirely -- but never for
		// more than nudgePollIdleObserveEvery consecutive ticks, so a poller
		// whose session died while its queue sat empty still notices and
		// exits instead of being immortalized (ga-8x82f4).
		hasDue := gate.hasDue(target, time.Now())
		observeThisTick := hasDue
		if !observeThisTick {
			idleTicksSinceObserve++
			if idleTicksSinceObserve >= nudgePollIdleObserveEvery {
				observeThisTick = true
			}
		}
		if !observeThisTick {
			time.Sleep(interval)
			continue
		}
		idleTicksSinceObserve = 0

		// Observing reads the bead store, and on a stopped city that read
		// restarts the bd and Dolt servers `gc stop` just retired (#6857).
		if nudgePollCityStopped(target, sp) {
			fmt.Fprintf(stderr, "gc nudge poll: city controller and session %q are not running; exiting and leaving queued nudges for the next start\n", target.sessionName) //nolint:errcheck
			return 0
		}
		obs, err := nudgeObserveTarget(target, sessStore, sp)
		if err != nil {
			fmt.Fprintf(stderr, "gc nudge poll: %v\n", err) //nolint:errcheck
			// Transient observation failures (store hiccup, runtime probe
			// race) must not kill the poller while queued work is pending:
			// for an idle session this sidecar is the only delivery path, so
			// exiting here strands the queue until something re-launches a
			// poller. Reuse the missing-session grace window; persistent
			// failures still exit once it elapses or the queue drains.
			now := time.Now()
			if shouldKeepNudgePollerAlive(target, missingSince, now) {
				if missingSince.IsZero() {
					missingSince = now
				}
				time.Sleep(interval)
				continue
			}
			return 1
		}
		if !obs.Running {
			now := time.Now()
			if shouldKeepNudgePollerAlive(target, missingSince, now) {
				if missingSince.IsZero() {
					missingSince = now
				}
				time.Sleep(interval)
				continue
			}
			return 0
		}
		missingSince = time.Time{}
		if !hasDue {
			// Observed only to satisfy the idle-cadence liveness check above;
			// the cheap gate already established there is nothing to deliver.
			time.Sleep(interval)
			continue
		}
		delivered, pollErr := nudgePollDeliverQueued(target, store.Store, cliSessionStore(store.Store, target.cfg, target.cityPath), sp, quiescence, obs)
		if pollErr != nil {
			fmt.Fprintf(stderr, "gc nudge poll: %v\n", pollErr) //nolint:errcheck
		}
		if delivered {
			continue
		}
		countNudgePollSkip(gate, skips, target, pollErr, time.Now(), stderr)
		time.Sleep(interval)
	}
}

// countNudgePollSkip is the poll loop's skip accounting after a tick that
// delivered nothing. It mirrors dispatchAllQueuedNudges' (nudge_dispatcher.go),
// gate included: "not-delivered" means this target HAD work it could claim and
// still got nothing this tick -- most commonly
// tryDeliverQueuedNudgesByPoller's quiescence gate rejecting a continuously
// busy session. A running session with nothing it may claim (nothing queued,
// or only items fenced to a live sibling) is the dispatcher's separate
// "not-matched" class and is deliberately not counted: the poll loop never
// exits while the session runs, so counting it would turn this into a tick
// counter. See #5317. The gate is asked afresh (not the tick's pre-observe
// answer) because the delivery attempt just run can itself have consumed the
// only due item. The counts are flushed at most every
// nudgePollSkipFlushInterval, not once per tick.
func countNudgePollSkip(gate *nudgePollGate, skips *nudgePollSkips, target nudgeTarget, pollErr error, now time.Time, stderr io.Writer) {
	if !gate.hasDue(target, now) {
		return
	}
	reason := "not-delivered"
	if pollErr != nil {
		reason = "not-delivered-error"
	}
	skips.add(reason, now, stderr)
}

// nudgePollTargetHasDueWork reports whether a claim for this target would
// claim anything now: a due item it may take, or an in-flight item whose lease
// has expired and that it may take once recovered. An item fenced to another
// live session is not due work for this target (ga-fsp5w9): every session of
// one agent matches that agent's items by key, and a sibling that cannot claim
// them must not observe, claim or count a skip for them. It reads a lock-free
// snapshot and takes no lock; the census of the city's sessions is read only
// when a due candidate is fenced to another session.
func nudgePollTargetHasDueWork(target nudgeTarget, now time.Time) bool {
	return newNudgePollGate().hasDue(target, now)
}

// nudgePollCensusTTL is how long the poll gate reuses a census that showed
// every candidate fenced to a live session.
const nudgePollCensusTTL = 30 * time.Second

// nudgePollSkipFlushInterval is how often the poll loop flushes the skip
// counts it accumulated into the queue state.
const nudgePollSkipFlushInterval = 30 * time.Second

// nudgePollGate is the poll loop's due-work gate (nudgePollTargetHasDueWork)
// with a short census cache, so N sibling pollers do not each list the city's
// sessions every tick. The cache is used only to skip a tick: when the gate
// sees claimable work, including an adoption, the poller claims with a fresh
// census (claimDueQueuedNudgeBatchForTarget), so an adoption decision is as
// fresh as an uncached one.
type nudgePollGate struct {
	census   map[string]struct{}
	censusOK bool
	censusAt time.Time
}

func newNudgePollGate() *nudgePollGate { return &nudgePollGate{} }

func (g *nudgePollGate) hasDue(target nudgeTarget, now time.Time) bool {
	ct := target.claimTarget(target.cityPath)
	fresh := ct.LiveFenceSessions
	ct.LiveFenceSessions = func() (map[string]struct{}, bool) {
		if g.censusOK && !g.censusAt.IsZero() && now.Sub(g.censusAt) < nudgePollCensusTTL {
			return g.census, true
		}
		live, ok := fresh()
		g.census, g.censusOK, g.censusAt = live, ok, now
		return live, ok
	}
	would, err := openNudgeQueue(target.cityPath, beads.NudgesStore{}).Probe.WouldClaimBatch(ct, now)
	return err == nil && would
}

// nudgePollSkips accumulates the poll loop's skip counts and flushes them at
// most every nudgePollSkipFlushInterval, and at exit, so a poller takes the
// queue lock for its counters once per interval, not once per tick.
type nudgePollSkips struct {
	cityPath  string
	counts    map[string]int64
	lastFlush time.Time
}

func (s *nudgePollSkips) add(reason string, now time.Time, stderr io.Writer) {
	if s.counts == nil {
		s.counts = map[string]int64{}
		if s.lastFlush.IsZero() {
			s.lastFlush = now
		}
	}
	s.counts[reason]++
	if now.Sub(s.lastFlush) >= nudgePollSkipFlushInterval {
		s.flush(now, stderr)
	}
}

func (s *nudgePollSkips) flush(now time.Time, stderr io.Writer) {
	if len(s.counts) == 0 {
		return
	}
	if err := openNudgeQueue(s.cityPath, beads.NudgesStore{}).Skips.RecordDispatchSkips(s.counts); err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "gc nudge poll: recording dispatch skip counter: %v\n", err) //nolint:errcheck
		}
		return
	}
	s.counts = nil
	s.lastFlush = now
}

func shouldKeepNudgePollerAlive(target nudgeTarget, missingSince, now time.Time) bool {
	// Lock-free read (ga-2kzci3 FR5): this is a liveness check, not a
	// maintenance operation, and must not wait on the queue's writer lock.
	pending, inFlight, _, err := listQueuedNudgesForTargetSnapshot(target.cityPath, target, now)
	if err != nil || (len(pending) == 0 && len(inFlight) == 0) {
		return false
	}
	if missingSince.IsZero() {
		return true
	}
	return now.Sub(missingSince) < defaultNudgePollStartGrace
}

func deliverSessionNudge(target nudgeTarget, message string, mode nudgeDeliveryMode, key string, jsonOutput bool, stdout, stderr io.Writer) int {
	store, err := openNudgeBeadStoreErr(target.cityPath)
	if err != nil || store.Store == nil {
		fmt.Fprintf(stderr, "gc session nudge: opening the nudge store for %q: %v\n", target.agentKey(), err) //nolint:errcheck
		return 1
	}
	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck
		return 1
	}
	return deliverSessionNudgeKeyed(target, store.Store, sp, message, mode, key, jsonOutput, stdout, stderr)
}

// sessionNudgeResult is how a session nudge's legacy path carried it out:
// delivered, or queued, with undelivered naming why the live leg was skipped
// (empty for a nudge queued by request).
type sessionNudgeResult struct {
	queued      bool
	undelivered worker.NudgeUndeliveredReason
	// note, when set, is the downgrade wording for a queued nudge in place of
	// the one undelivered names (a question dialog is not a permission prompt).
	note string
}

// deliverSessionNudgeKeyed records the nudge as a message{role: notice} op
// under key (empty mints one), as DES-O0API §8.4 has it, before its legacy
// path (deliverSessionNudgeLegacy) runs, and closes the op from that path's
// result before it reports: delivered, queued (succeeded{via:
// legacy.queued}), or refused. A same-key rerun replays the first run's
// outcome without delivering again. The delivered text is the message,
// unchanged in every ops.content mode. When [ops] authoritative lists message
// and a lane host can type into the session, the nudge is a conversation-lane
// op instead (deliverSessionNudgeLane), which ops.content does not govern.
func deliverSessionNudgeKeyed(target nudgeTarget, store beads.Store, sp runtime.Provider, message string, mode nudgeDeliveryMode, key string, jsonOutput bool, stdout, stderr io.Writer) int {
	if handled, code := deliverSessionNudgeLane(target, store, sp, message, mode, key, jsonOutput, stdout, stderr); handled {
		return code
	}
	op, start := startSessionNudgeOp(target, store, message, mode, key, jsonOutput, stdout, stderr)
	switch start {
	case cliOpRefused:
		return 1
	case cliOpReplayed:
		return writeSessionNudgeResult(target, mode, jsonOutput, sessionNudgeResult{queued: op.replayedQueued()}, op, stdout, stderr)
	}
	defer op.end()
	res, err := deliverSessionNudgeLegacy(target, store, sp, message, mode, stderr)
	op.finishMessage(res.queued, err)
	if err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck
		return 1
	}
	return writeSessionNudgeResult(target, mode, jsonOutput, res, op, stdout, stderr)
}

// startSessionNudgeOp starts the nudge's op over the session store store
// backs. A nudge with no store, or to a target with no session bead, cannot
// be recorded: auto runs it unrecorded, require refuses it.
func startSessionNudgeOp(target nudgeTarget, store beads.Store, message string, mode nudgeDeliveryMode, key string, jsonOutput bool, stdout, stderr io.Writer) (*cliOp, cliOpStart) {
	req := sessionNudgeOpRequest(message, mode, key, jsonOutput, stdout, stderr)
	req.conversation = target.sessionID
	switch {
	case store == nil:
		req.unrecordable = legacyop.ReasonStoreUnavailable
	case target.sessionID == "":
		req.unrecordable = legacyop.ReasonUnmaterializedTarget
	}
	return startCLIOp(target.cfg, cliSessionStore(store, target.cfg, target.cityPath), req)
}

// sessionNudgeOpRequest is a nudge's op request before its target resolves.
func sessionNudgeOpRequest(message string, mode nudgeDeliveryMode, key string, jsonOutput bool, stdout, stderr io.Writer) cliOpRequest {
	return cliOpRequest{
		verb: "gc session nudge", kind: opjournal.KindMessage, key: key,
		payload: message, policy: sessionNudgeOpPolicy(mode),
		asJSON: jsonOutput, stdout: stdout, stderr: stderr,
	}
}

// sessionNudgeOpPolicy is a nudge's message policy (DES-O0API §3.1): an
// immediate nudge steers, falling back to the boundary on a runtime that
// cannot steer (D-STEER); wait-idle and queue deliver at the boundary.
func sessionNudgeOpPolicy(mode nudgeDeliveryMode) opjournal.Policy {
	m := opjournal.MessagePolicy{When: opjournal.WhenBoundary, Wake: opjournal.WakeIfNeeded, Role: opjournal.RoleNotice}
	if mode == nudgeDeliveryImmediate {
		m.When, m.OnUnsupported = opjournal.WhenSteer, opjournal.FallbackBoundary
	}
	return opjournal.Policy{Message: m}
}

// deliverSessionNudgeLegacy delivers a session nudge live, or queues it when
// the live leg cannot or must not deliver it. It reports nothing on stdout;
// its notes and warnings go to stderr, and a refusal is a cliMessageRefusal.
func deliverSessionNudgeLegacy(target nudgeTarget, store beads.Store, sp runtime.Provider, message string, mode nudgeDeliveryMode, stderr io.Writer) (sessionNudgeResult, error) {
	if mode == nudgeDeliveryQueue {
		return queueSessionNudgeWithWorker(target, store, sp, message, "")
	}
	// Two-store split: the raw store keeps threading the nudge-enqueue currency,
	// while the session-class observe/handle reads below route through sessStore.
	// nil store -> cliSessionStore returns nil (identity), preserving the
	// store-less WithProvider path.
	sessStore := cliSessionStore(store, target.cfg, target.cityPath)
	queueManagedWake, err := shouldQueueManagedNudgeWake(target, store, sp)
	if err != nil {
		return sessionNudgeResult{}, err
	}
	if queueManagedWake {
		return queueManagedSessionNudgeWake(target, store, message, stderr)
	}
	// Never type into a pending permission prompt (#2892): its highlighted
	// "❯ 1. Yes" reads as an idle composer, and the nudge's Enter would
	// approve the tool call. wait-idle already means "deliver at a safe
	// boundary", so queue it behind the prompt; immediate cannot wait, so
	// refuse with an explanation.
	pending, err := nudgeTargetPendingInteraction(target, sessStore, sp, mode, stderr)
	if err != nil {
		return sessionNudgeResult{}, err
	}
	if pending != nil {
		return refuseOrQueueNudgeBehindPendingInteraction(target, store, sp, message, mode, describeWorkerPendingInteraction(pending), isQuestionInteraction(pending))
	}
	// A wait-idle nudge to a RUNNING-but-busy target must not block the caller in
	// the worker's synchronous WaitForIdle (runtimeHandleWaitIdleTimeout, 30s):
	// the session never reports idle for the whole window, so the caller stalls
	// ~30s while the city store's Dolt connection sits idle in scope and gets
	// reaped server-side ("[mysql] unexpected EOF"). Detect "busy" without
	// blocking (LastActivity, the same quiescence signal the nudge poller uses)
	// and queue immediately instead — the supervisor dispatcher (or the
	// per-session poller in legacy mode) delivers at the next idle boundary, off
	// the caller's critical path. An idle target still delivers live below (its
	// WaitForIdle returns promptly); an unknown LastActivity is treated as
	// not-busy so untracked sessions keep the existing behavior.
	//
	// Restrict this to the claude, non-ACP transport — the ONLY case
	// RuntimeHandle.nudgeWaitIdle actually blocks on WaitForIdle. ACP delivers
	// live in-process and non-claude returns immediately (see
	// internal/worker/runtime_handle.go nudgeWaitIdle), so short-circuiting
	// those would needlessly downgrade live delivery to queued. (gco-90ui)
	if mode == nudgeDeliveryWaitIdle && target.sessionTransport() != "acp" && target.providerFamily() == "claude" {
		if obs, obsErr := nudgeObserveTarget(target, sessStore, sp); obsErr == nil && obs.Running && nudgeObservationBusy(obs) {
			return queueSessionNudgeWithWorker(target, store, sp, message, worker.NudgeUndeliveredNoIdleBoundary)
		}
	}
	delivery, ok := workerNudgeDeliveryForMode(mode)
	if !ok {
		return sessionNudgeResult{}, fmt.Errorf("unknown delivery mode %q", mode)
	}
	handle, err := workerHandleForNudgeTarget(target, sessStore, sp)
	if err != nil {
		return sessionNudgeResult{}, err
	}
	result, err := handle.Nudge(context.Background(), sessionActor(session.ActorAgent, target.cityPath), worker.NudgeRequest{
		Text:     message,
		Delivery: delivery,
		Source:   "session",
	})
	if err != nil && errors.Is(err, tmux.ErrNudgeSubmitDeliveredUnobserved) {
		// The submit Enter was delivered and the composer drained; only the
		// busy-indicator OBSERVATION timed out. Delivery is proven, so this
		// must report success like any other delivered nudge, not a CLI
		// failure — fall through to the normal success path below.
		result.Delivered = true
		err = nil
	}
	if err != nil {
		if errors.Is(err, runtime.ErrSessionNotFound) && target.sessionTransport() == "acp" {
			if mode == nudgeDeliveryWaitIdle {
				return queueSessionNudgeWithWorker(target, store, sp, message, "")
			}
			if mode == nudgeDeliveryImmediate {
				return sessionNudgeResult{}, &cliMessageRefusal{why: cliOpRefusedACPNotOwned, err: fmt.Errorf("live ACP delivery failed for %s because this process does not own the ACP connection; retry with --delivery=wait-idle or --delivery=queue so the queued dispatcher can deliver it", target.agentKey())}
			}
		}
		if errors.Is(err, runtime.ErrPendingInteraction) {
			// The prompt appeared between the probe above and delivery; the
			// runtime refused to type into it. Read what it is again to word
			// the answer: a refusal carries no kind, and a prompt answered in
			// the meantime reads as the permission prompt this path used to be.
			what, question := err.Error(), false
			if pending, _ := nudgeTargetPendingInteraction(target, sessStore, sp, nudgeDeliveryWaitIdle, io.Discard); pending != nil {
				what, question = describeWorkerPendingInteraction(pending), isQuestionInteraction(pending)
			}
			return refuseOrQueueNudgeBehindPendingInteraction(target, store, sp, message, mode, what, question)
		}
		if errors.Is(err, runtime.ErrSessionBusy) {
			// The runtime typed nothing rather than interrupt the session's
			// running turn; the queue delivers it at the next idle boundary.
			return queueSessionNudgeWithWorker(target, store, sp, message, worker.NudgeUndeliveredNoIdleBoundary)
		}
		return sessionNudgeResult{}, err
	}
	if result.Undelivered == worker.NudgeQueued {
		// The session could not take it now and queued the nudge itself
		// (CONTRACT v5.9 D8); a held wait-idle nudge (NudgeUndeliveredHeld) is
		// queued below.
		return sessionNudgeResult{queued: true, undelivered: result.Undelivered}, nil
	}
	if !result.Delivered {
		// Not delivered and not queued: a wait-idle nudge that found no
		// boundary, or any nudge to an agent dead in its pane (mc-48az4).
		return queueSessionNudgeWithWorker(target, store, sp, message, result.Undelivered)
	}
	// Stamp every delivered nudge: a resumed session's manager already
	// reports a delivered-but-unobserved submit as plain delivery.
	if result.Delivered && store != nil {
		stampLastNudgeDeliveredAt(sessionFrontDoor(sessStore), target.sessionID, time.Now())
	}
	return sessionNudgeResult{}, nil
}

// writeSessionNudgeResult reports a nudge's success: delivered or queued, and
// under --json the op that backs the run (op may be nil).
func writeSessionNudgeResult(target nudgeTarget, mode nudgeDeliveryMode, jsonOutput bool, res sessionNudgeResult, op *cliOp, stdout, stderr io.Writer) int {
	if res.queued {
		note := res.note
		if note == "" {
			note = queuedNudgeDowngradeNote(target, res.undelivered)
		}
		return writeQueuedSessionNudgeNote(target, mode, jsonOutput, note, op, stdout, stderr)
	}
	if jsonOutput {
		result := sessionNudgeJSON{
			SchemaVersion: "1",
			OK:            true,
			Target:        target.agentKey(),
			SessionID:     target.sessionID,
			SessionName:   target.sessionName,
			Delivery:      string(mode),
			Queued:        false,
			Outcome:       "delivered",
		}
		result.OpID, result.Key, result.Replayed = op.jsonFields()
		return writeCLIJSONLineOrExit(stdout, stderr, "gc session nudge", result)
	}
	fmt.Fprintf(stdout, "Nudged %s\n", target.agentKey()) //nolint:errcheck
	return 0
}

// nudgeTargetPendingInteraction reports the interaction the target session is
// waiting on, if any. Providers that cannot report interactions report none.
// A look that fails is an error for an immediate nudge, which cannot wait for
// an answer. A wait-idle nudge reports the failure on stderr and goes on: its
// delivery looks for a prompt again before it types, and the nudge is queued
// behind one it finds.
func nudgeTargetPendingInteraction(target nudgeTarget, sessStore beads.Store, sp runtime.Provider, mode nudgeDeliveryMode, stderr io.Writer) (*worker.PendingInteraction, error) {
	handle, err := workerHandleForNudgeTarget(target, sessStore, sp)
	if err != nil {
		return nil, err
	}
	pending, err := handle.Pending(context.Background())
	if err == nil {
		return pending, nil
	}
	err = fmt.Errorf("checking %s for a pending permission prompt: %w", target.agentKey(), err)
	if mode != nudgeDeliveryWaitIdle {
		return nil, err
	}
	fmt.Fprintf(stderr, "gc session nudge: %v; delivering when idle, which looks again before it types\n", err) //nolint:errcheck
	return nil, nil
}

func describeWorkerPendingInteraction(pending *worker.PendingInteraction) string {
	what := strings.TrimSpace(pending.Kind)
	if what == "" {
		what = "interaction"
	}
	if prompt := strings.TrimSpace(pending.Prompt); prompt != "" {
		what += " " + strconv.Quote(prompt)
	}
	if pending.RequestID != "" {
		what += ", request " + pending.RequestID
	}
	return what
}

// isQuestionInteraction reports whether pending is a question dialog: no
// response verb answers it, only a person at the terminal can.
func isQuestionInteraction(pending *worker.PendingInteraction) bool {
	return pending.Kind == "question"
}

// refuseOrQueueNudgeBehindPendingInteraction handles a nudge to a session that
// is waiting on a permission prompt or a question dialog: wait-idle queues it
// for delivery after the interaction is answered; immediate is refused with an
// explanation of how it is answered.
func refuseOrQueueNudgeBehindPendingInteraction(target nudgeTarget, store beads.Store, sp runtime.Provider, message string, mode nudgeDeliveryMode, what string, question bool) (sessionNudgeResult, error) {
	if mode == nudgeDeliveryWaitIdle {
		if question {
			return queueSessionNudgeWithNote(target, store, sp, message, worker.NudgeUndeliveredPendingInteraction, queuedNudgeQuestionNote)
		}
		return queueSessionNudgeWithWorker(target, store, sp, message, worker.NudgeUndeliveredPendingInteraction)
	}
	if question {
		attach := target.sessionID
		if attach == "" {
			attach = target.agentKey()
		}
		return sessionNudgeResult{}, &cliMessageRefusal{why: cliOpRefusedPendingInteraction, err: fmt.Errorf("%s is waiting on a question (%s); typing now would answer it. Answer it in the terminal (gc session attach %s), or retry with --delivery=wait-idle or --delivery=queue to deliver after it is answered", target.agentKey(), what, attach)}
	}
	return sessionNudgeResult{}, &cliMessageRefusal{why: cliOpRefusedPendingInteraction, err: fmt.Errorf("%s is waiting on a permission prompt (%s); typing now would answer it. Approve or deny it first (POST .../session/{id}/respond), or retry with --delivery=wait-idle or --delivery=queue to deliver after it is answered", target.agentKey(), what)}
}

func shouldQueueManagedNudgeWake(target nudgeTarget, store beads.Store, sp runtime.Provider) (bool, error) {
	if !canRequestManagedNudgeWake(target, store) {
		return false, nil
	}
	// The observe is a session-class read; route it through the session store
	// (identity today). The nil-check above keeps the raw store deliberately.
	obs, err := nudgeObserveTarget(target, cliSessionStore(store, target.cfg, target.cityPath), sp)
	if err != nil {
		return false, fmt.Errorf("observing managed session before wake routing: %w", err)
	}
	return !obs.Running, nil
}

// nudgeObservationBusy reports, WITHOUT blocking, whether the observed session
// is actively busy right now. It uses the LastActivity timestamp — the same
// quiescence signal the nudge poller relies on (defaultNudgePollQuiescence). An
// unknown (nil/zero) LastActivity returns false (treat as not-busy) so callers
// fall back to the existing wait-idle path rather than queue a session whose
// activity cannot be observed.
//
// This is the near-inverse of pollerSessionIdleEnough's LastActivity branch
// (idle when time-since >= quiescence); they intentionally differ on the
// unknown-LastActivity case — the poller falls back to a blocking WaitForIdle,
// whereas this non-blocking caller-side check must not, so it defaults to
// not-busy.
func nudgeObservationBusy(obs worker.LiveObservation) bool {
	if obs.LastActivity == nil || obs.LastActivity.IsZero() {
		return false
	}
	return time.Since(*obs.LastActivity) < defaultNudgePollQuiescence
}

func canRequestManagedNudgeWake(target nudgeTarget, store beads.Store) bool {
	return managedNudgeWakeSkipReason(target, store != nil) == ""
}

// queueManagedSessionNudgeWake queues a nudge for a managed session that is
// not running and asks the controller to wake it, never over an operator's
// hold: a held session keeps the nudge queued.
func queueManagedSessionNudgeWake(target nudgeTarget, store beads.Store, message string, stderr io.Writer) (sessionNudgeResult, error) {
	item := newQueuedNudgeWithOptions(target.agentKey(), message, "session", time.Now(), queuedNudgeOptionsFromTarget(target))
	outcome, err := enqueueManagedNudgeThenWake(target, store, item)
	if err != nil {
		return sessionNudgeResult{}, err
	}
	var undelivered worker.NudgeUndeliveredReason
	if outcome == session.WakeHeld {
		undelivered = worker.NudgeQueuedHeld
	} else if err := nudgePokeController(target.cityPath, reconcilekey.Session(target.sessionID)); err != nil {
		fmt.Fprintf(stderr, "gc session nudge: warning: poke failed: %v\n", err) //nolint:errcheck
	}
	return sessionNudgeResult{queued: true, undelivered: undelivered}, nil
}

func enqueueManagedNudgeThenWake(target nudgeTarget, store beads.Store, item queuedNudge) (session.WakeRequestOutcome, error) {
	// store is class-mixed here: the enqueue/rollback arms are nudge-class (wrap
	// into the typed NudgesStore), while the wake arm reads the session bead and
	// wakes it (sessions class), so it routes through the session coordination-class
	// store via cliSessionStore (identity today). enqueue (NudgesStore wrap) and
	// rollback stay nudges.
	nudges := beads.NudgesStore{Store: store}
	if err := enqueueQueuedNudgeWithStore(target.cityPath, nudges, item); err != nil {
		return 0, err
	}
	outcome, err := requestManagedNudgeWake(target, cliSessionFrontDoor(store, target.cfg, target.cityPath))
	if err != nil {
		if rollbackErr := rollbackQueuedNudge(target.cityPath, nudges, item, "managed wake failed: "+err.Error()); rollbackErr != nil {
			return 0, managedWakeRollbackError(item.ID, err, rollbackErr)
		}
		return 0, err
	}
	return outcome, nil
}

// managedWakeRollbackError reports a managed wake that failed and the rollback
// of its queued nudge that failed too. Only the wake's error is wrapped, so it
// alone classifies the op: the rollback's (a busy queue lock, say) is named in
// the text but must not make the op read as "nothing was queued" while the
// nudge it failed to roll back is still queued.
func managedWakeRollbackError(itemID string, wakeErr, rollbackErr error) error {
	return errors.Join(wakeErr, fmt.Errorf("rolling back queued nudge %q after managed wake failure: %s", itemID, rollbackErr.Error()))
}

// requestManagedNudgeWake asks the controller to wake a managed session that
// owns queued nudges, never over an operator's hold (CONTRACT v5.9 D8): a
// held row keeps its nudge queued until an operator resumes it. sessFront is
// the session-class front door (cliSessionFrontDoor).
func requestManagedNudgeWake(target nudgeTarget, sessFront *session.Store) (session.WakeRequestOutcome, error) {
	if !sessFront.Backed() || target.sessionID == "" {
		// The item IS enqueued; what did not happen is the wake that gets a
		// stopped session to drain it. Returning nil silently made those two
		// facts indistinguishable, so a nudge to a stopped session with no
		// session-store backing looked delivered and simply sat in the queue.
		// Report the downgrade rather than failing the enqueue: the queued item
		// is still valid and the controller's own patrol tick can deliver it.
		if nudgeWarningWriter != nil {
			fmt.Fprintf(nudgeWarningWriter, //nolint:errcheck // best-effort warning
				"gc session nudge: queued for %s but no managed wake was requested (%s); it is delivered when the session next runs\n",
				target.agentKey(), managedNudgeWakeSkipReason(target, sessFront.Backed()))
		}
		return 0, nil
	}
	return sessFront.RequestWakeUnlessHeld(target.sessionID, false, time.Now().UTC())
}

// managedNudgeWakeSkipReason names the first missing precondition of the
// managed wake, or "" when it can be requested, so a downgrade line is
// actionable rather than merely present. backed reports whether a store backs
// the command. It probes the city's controller at most once.
func managedNudgeWakeSkipReason(target nudgeTarget, backed bool) string {
	switch {
	case !backed:
		return "no session store is backing this command"
	case target.sessionID == "":
		return "the target resolved to no session bead id"
	case strings.TrimSpace(target.cityPath) == "":
		return "the target resolved to no city"
	case !nudgeCityUsesManagedReconciler(target.cityPath):
		return fmt.Sprintf("no controller or supervisor was detected for %s", target.cityPath)
	default:
		return ""
	}
}

// requestWorkerSiteNudgeWake is a courtesy nudge's worker-site arm (gc sling's
// "Work slung", gc mail's new-mail notice). The box queues no text, since its
// nudge queue is a local file the remote controller never reads, and its
// guard types into no runtime. What reaches the remote controller is the
// session's wake on the ledger, recorded never over an operator's hold. A
// target with no session bead, or a command with no store, records nothing
// and reports zero: the routed bead or the mail is on the ledger either way.
func requestWorkerSiteNudgeWake(target nudgeTarget, store beads.Store) (session.WakeRequestOutcome, error) {
	if store == nil || target.sessionID == "" {
		return 0, nil
	}
	sessFront := cliSessionFrontDoor(store, target.cfg, target.cityPath)
	if !sessFront.Backed() {
		return 0, nil
	}
	outcome, err := sessFront.RequestWakeUnlessHeld(target.sessionID, false, time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("recording the wake for %s: %w", target.agentKey(), err)
	}
	return outcome, nil
}

func workerHandleForNudgeTarget(target nudgeTarget, store beads.Store, sp runtime.Provider) (worker.Handle, error) {
	if target.sessionName != "" {
		if target.sessionID != "" || target.continuationEpoch != "" {
			obs, err := workerObserveSessionTargetWithConfig(target.cityPath, store, sp, target.cfg, target.sessionName)
			if err == nil {
				matches, matchErr := nudgeTargetLiveGenerationMatches(target, obs, sp)
				if matchErr != nil {
					return nil, matchErr
				}
				if !matches {
					return nil, fmt.Errorf("%w: live runtime %q no longer matches session generation %q", runtime.ErrSessionNotFound, target.sessionName, target.pollerKey())
				}
				if !obs.Running && target.sessionID != "" {
					return workerHandleForSessionWithConfig(target.cityPath, store, sp, target.cfg, target.sessionID)
				}
			}
			if err != nil && !errors.Is(err, runtime.ErrSessionNotFound) && !errors.Is(err, session.ErrSessionNotFound) {
				return nil, err
			}
		}
		handle, err := runtimeWorkerHandleWithConfig(
			target.cityPath,
			store,
			sp,
			target.cfg,
			target.sessionName,
			strings.TrimSpace(target.providerFamily()),
			strings.TrimSpace(target.sessionTransport()),
			target.processNames(),
		)
		if err == nil {
			return handle, nil
		}
		if target.sessionID == "" || !errors.Is(err, runtime.ErrSessionNotFound) {
			return nil, err
		}
	}
	if target.sessionID != "" {
		return workerHandleForSessionWithConfig(target.cityPath, store, sp, target.cfg, target.sessionID)
	}
	return runtimeWorkerHandleWithConfig(
		target.cityPath,
		store,
		sp,
		target.cfg,
		target.sessionName,
		strings.TrimSpace(target.providerFamily()),
		strings.TrimSpace(target.sessionTransport()),
		target.processNames(),
	)
}

func workerObserveNudgeTarget(target nudgeTarget, store beads.Store, sp runtime.Provider) (worker.LiveObservation, error) {
	if target.sessionName != "" {
		obs, err := workerObserveSessionTargetWithConfig(target.cityPath, store, sp, target.cfg, target.sessionName)
		if err != nil {
			return worker.LiveObservation{}, err
		}
		matches, err := nudgeTargetLiveGenerationMatches(target, obs, sp)
		if err != nil {
			return worker.LiveObservation{}, err
		}
		if !matches {
			obs.Running = false
			obs.Alive = false
			obs.Attached = false
			obs.AttachedErr = nil
			obs.LastActivity = nil
		}
		return obs, nil
	}
	if target.sessionID != "" {
		return workerObserveSessionTargetWithConfig(target.cityPath, store, sp, target.cfg, target.sessionID)
	}
	return workerObserveSessionTargetWithConfig(target.cityPath, store, sp, target.cfg, target.sessionName)
}

func nudgeTargetLiveGenerationMatches(target nudgeTarget, obs worker.LiveObservation, sp runtime.Provider) (bool, error) {
	if !obs.Running || (target.sessionID == "" && target.continuationEpoch == "") {
		return true, nil
	}
	if target.sessionID != "" {
		for _, liveID := range []string{obs.SessionID, obs.RuntimeSessionID} {
			liveID = strings.TrimSpace(liveID)
			if liveID != "" && liveID != target.sessionID {
				return false, nil
			}
		}
	}
	if sp == nil || target.sessionName == "" {
		return true, nil
	}
	if target.sessionID != "" {
		liveID, err := sp.GetMeta(target.sessionName, "GC_SESSION_ID")
		if err != nil && !runtime.IsSessionGone(err) {
			return false, err
		}
		if strings.TrimSpace(liveID) != "" && strings.TrimSpace(liveID) != target.sessionID {
			return false, nil
		}
	}
	if target.continuationEpoch != "" {
		liveEpoch, err := sp.GetMeta(target.sessionName, "GC_CONTINUATION_EPOCH")
		if err != nil && !runtime.IsSessionGone(err) {
			return false, err
		}
		if strings.TrimSpace(liveEpoch) != "" && strings.TrimSpace(liveEpoch) != target.continuationEpoch {
			return false, nil
		}
	}
	return true, nil
}

func deliverSessionNudgeWithProvider(target nudgeTarget, sp runtime.Provider, mode nudgeDeliveryMode, stdout, stderr io.Writer) int {
	return deliverSessionNudgeKeyed(target, nil, sp, "check deploy status", mode, "", false, stdout, stderr)
}

// queueSessionNudgeWithWorker enqueues a nudge the live leg could not (or must
// not) deliver. undelivered names why live delivery was skipped so the operator
// line can say it; it is empty for a caller that queued by request rather than
// by downgrade.
func queueSessionNudgeWithWorker(target nudgeTarget, store beads.Store, sp runtime.Provider, message string, undelivered worker.NudgeUndeliveredReason) (sessionNudgeResult, error) {
	return queueSessionNudgeWithNote(target, store, sp, message, undelivered, "")
}

// queueSessionNudgeWithNote is queueSessionNudgeWithWorker with note, the
// downgrade wording that replaces the one undelivered names ("" keeps it).
func queueSessionNudgeWithNote(target nudgeTarget, store beads.Store, sp runtime.Provider, message string, undelivered worker.NudgeUndeliveredReason, note string) (sessionNudgeResult, error) {
	if err := enqueueQueuedNudge(target.cityPath, newQueuedNudgeWithOptions(target.agentKey(), message, "session", time.Now(), queuedNudgeOptionsFromTarget(target))); err != nil {
		return sessionNudgeResult{}, err
	}
	// The observe is a session-class read; route through the session store
	// (identity today). The enqueue above stays on its own nudge store.
	if obs, err := workerObserveNudgeTarget(target, cliSessionStore(store, target.cfg, target.cityPath), sp); err == nil && obs.Running {
		maybeStartNudgePoller(target)
	}
	return sessionNudgeResult{queued: true, undelivered: undelivered, note: note}, nil
}

// writeQueuedSessionNudgeResult reports a queued nudge truthfully: WHERE it was
// queued (the flock'd state.json that is the queue's authority — the shadow bead
// is a projection of it, so an operator looking for the item needs this path)
// and, when the live leg was skipped rather than tried, WHY. op (nil when none)
// is the op that backs the run, which --json names.
func writeQueuedSessionNudgeResult(target nudgeTarget, mode nudgeDeliveryMode, jsonOutput bool, undelivered worker.NudgeUndeliveredReason, op *cliOp, stdout, stderr io.Writer) int {
	return writeQueuedSessionNudgeNote(target, mode, jsonOutput, queuedNudgeDowngradeNote(target, undelivered), op, stdout, stderr)
}

// writeQueuedSessionNudgeNote is writeQueuedSessionNudgeResult with the
// rendered downgrade note.
func writeQueuedSessionNudgeNote(target nudgeTarget, mode nudgeDeliveryMode, jsonOutput bool, note string, op *cliOp, stdout, stderr io.Writer) int {
	if jsonOutput {
		result := sessionNudgeJSON{
			SchemaVersion: "1",
			OK:            true,
			Target:        target.agentKey(),
			SessionID:     target.sessionID,
			SessionName:   target.sessionName,
			Delivery:      string(mode),
			Queued:        true,
			Outcome:       "queued",
		}
		result.OpID, result.Key, result.Replayed = op.jsonFields()
		return writeCLIJSONLineOrExit(stdout, stderr, "gc session nudge", result)
	}
	fmt.Fprintf(stdout, //nolint:errcheck // best-effort stdout
		"Queued nudge for %s in %s%s\n",
		target.agentKey(), openNudgeQueue(target.cityPath, beads.NudgesStore{}).Location, note)
	return 0
}

// queuedNudgeQuestionNote is the downgrade note for a nudge queued behind a
// question dialog, which is answered in the terminal rather than with respond.
const queuedNudgeQuestionNote = " (the session is waiting on a question; the queued dispatcher delivers it after the question is answered in the terminal)"

// queuedNudgeDowngradeNote renders the reason the live leg was skipped, or "" for
// a nudge that was queued by request. It names the provider for the unsupported
// case because that is the actionable half: the operator's next question is
// always "unsupported by what".
func queuedNudgeDowngradeNote(target nudgeTarget, undelivered worker.NudgeUndeliveredReason) string {
	switch undelivered {
	case worker.NudgeUndeliveredProviderUnsupported:
		provider := strings.TrimSpace(target.providerName())
		if provider == "" {
			provider = "this provider"
		}
		return fmt.Sprintf(" (live delivery is unsupported for %s; the queued dispatcher delivers it)", provider)
	case worker.NudgeUndeliveredNoIdleBoundary:
		return " (the session never reached an idle boundary; the queued dispatcher delivers it)"
	case worker.NudgeUndeliveredPendingInteraction:
		return " (the session is waiting on a permission prompt; the queued dispatcher delivers it after the prompt is answered)"
	case worker.NudgeUndeliveredHeld, worker.NudgeQueuedHeld:
		return " (the session is held by an operator or a wait; the nudge is delivered the next time the session runs, and expires after 24h)"
	case worker.NudgeQueued:
		return " (the session cannot take it now: it is held by an operator or a wait, not running, or its agent is down; the nudge is delivered the next time the session runs, and expires after 24h)"
	case worker.NudgeUndeliveredAgentDead:
		return " (the session's agent is down; the nudge is delivered once the controller restarts it, and expires after 24h)"
	default:
		return ""
	}
}

func sendMailNotify(target nudgeTarget, sender, messageID string) (string, error) {
	store, err := openNudgeBeadStoreErr(target.cityPath)
	if err != nil {
		return "", err
	}
	if store.Store == nil {
		return "", fmt.Errorf("opening the nudge store for %q returned no store", target.agentKey())
	}
	sp, err := newSessionProvider()
	if err != nil {
		return "", err
	}
	return sendMailNotifyWithOp(target, store.Store, sp, sender, messageID)
}

// sendMailNotifyWithProvider is the store-less notify path (human sender,
// nil bead store). There is never a backing message store to address here,
// so unlike sendMailNotify it has no messageID to thread through — the
// resulting nudge carries no reference and is delivered unconditionally,
// same as before the #5321 fix.
func sendMailNotifyWithProvider(target nudgeTarget, sp runtime.Provider) error {
	_, err := sendMailNotifyWithOp(target, nil, sp, "human", "")
	return err
}

// mailNotifyOpKey is a mail notify's client key, the cause key
// mail:<message id> (DES-O0API §3.2): one op per message and recipient.
func mailNotifyOpKey(messageID string) string {
	return "mail:" + messageID
}

// mailNotifyOpPolicy is a mail notify's message policy: a notice delivered
// at the boundary (DES-O0API §3.1).
var mailNotifyOpPolicy = opjournal.Policy{Message: opjournal.MessagePolicy{When: opjournal.WhenBoundary, Wake: opjournal.WakeIfNeeded, Role: opjournal.RoleNotice}}

// sendMailNotifyWithOp tells target it has mail from sender, and returns the
// id of the op that records the notice, "" when none does. On a worker site
// it records only the recipient's wake (requestWorkerSiteNudgeWake), the one
// notice a box can leave, and no op, and returns
// errWorkerSiteNotifyNotDelivered when it recorded no wake. Elsewhere the
// notice is recorded as a message{role: notice} op keyed by its mail message
// (mailNotifyOpKey, causation the message id) before it is delivered or
// queued, and closed from that result; a notice its op already closed with
// success is not delivered again. The delivered text is the notice's,
// unchanged in every ops.content mode.
func sendMailNotifyWithOp(target nudgeTarget, store beads.Store, sp runtime.Provider, sender, messageID string) (string, error) {
	if onWorkerSite(target.cityPath) {
		outcome, err := requestWorkerSiteNudgeWake(target, store)
		if err != nil {
			return "", err
		}
		if outcome != session.WakeRecorded {
			return "", errWorkerSiteNotifyNotDelivered
		}
		if nudgeWarningWriter != nil {
			fmt.Fprintf(nudgeWarningWriter, "gc mail notify: %s\n", workerSiteNudgeNotice(outcome)) //nolint:errcheck // best-effort warning
		}
		return "", nil
	}
	msg := mailNotifyText(sender)
	op, start := startMailNotifyOp(target, store, msg, messageID)
	switch start {
	case cliOpRefused:
		return "", mailNotifyRefusedError(messageID)
	case cliOpReplayed:
		id, _, _ := op.jsonFields()
		return id, nil
	}
	defer op.end()
	queued, err := deliverMailNotifyLegacy(target, store, sp, msg, messageID)
	op.finishMessage(queued, err)
	id, _, _ := op.jsonFields()
	return id, err
}

// startMailNotifyOp starts a mail notify's op over the session store store
// backs, under the caller's principal. A notice with no store, to a target
// with no session bead, or with no message to key it by cannot be recorded:
// auto runs it unrecorded, require refuses it. Its lines go to
// nudgeWarningWriter.
func startMailNotifyOp(target nudgeTarget, store beads.Store, msg, messageID string) (*cliOp, cliOpStart) {
	req := mailNotifyOpRequest(msg, messageID)
	req.conversation = target.sessionID
	switch {
	case store == nil:
		req.unrecordable = legacyop.ReasonStoreUnavailable
	case req.unrecordable == "" && target.sessionID == "":
		req.unrecordable = legacyop.ReasonUnmaterializedTarget
	}
	return startCLIOp(target.cfg, cliSessionStore(store, target.cfg, target.cityPath), req)
}

// mailNotifyOpRequest is a mail notify's op request before its target
// resolves: a notice with no message to key it by cannot be recorded. Its
// lines go to nudgeWarningWriter.
func mailNotifyOpRequest(msg, messageID string) cliOpRequest {
	warn := nudgeWarningWriter
	if warn == nil {
		warn = io.Discard
	}
	req := cliOpRequest{
		verb: "gc mail notify", kind: opjournal.KindMessage, presetKey: mailNotifyOpKey(messageID),
		payload: msg, causation: messageID, policy: mailNotifyOpPolicy,
		stdout: io.Discard, stderr: warn,
	}
	if messageID == "" {
		req.unrecordable = legacyop.ReasonInvalidRequest
	}
	return req
}

// mailNotifyText is the notice a mail recipient receives from sender.
func mailNotifyText(sender string) string {
	return fmt.Sprintf("You have mail from %s", sender)
}

// mailNotifyRefusedError is a mail notify the op journal refused under
// require: the notice for messageID was not sent.
func mailNotifyRefusedError(messageID string) error {
	return fmt.Errorf("the op journal refused the notice for %s, so it was not sent", messageID)
}

// deliverMailNotifyLegacy delivers a mail notice msg live to a running
// target, else queues it (with a managed wake for a stopped managed
// session), and reports whether it queued it.
func deliverMailNotifyLegacy(target nudgeTarget, store beads.Store, sp runtime.Provider, msg, messageID string) (queued bool, err error) {
	now := time.Now()
	// Carry the mail message ID as the nudge's re-validation reference so
	// blockedQueuedNudgeReason can re-read the message at delivery time and
	// withdraw the nudge if it was already read or has since been archived
	// (see gastownhall/gascity#5321). messageID is empty for callers that
	// have no addressable message (e.g. sendMailNotifyWithProvider's
	// store-less human-sender path), in which case the nudge carries no
	// reference and is delivered unconditionally, same as before this fix.
	opts := queuedNudgeOptionsFromTarget(target)
	if messageID != "" {
		opts.Reference = &nudgeReference{Kind: "mail", ID: messageID}
	}
	// Session-class store for the observe/handle reads and the last-nudge stamp
	// below; the raw store keeps flowing to canRequestManagedNudgeWake,
	// enqueueManagedNudgeThenWake, and enqueueQueuedNudge (nudge class). nil store
	// -> nil (identity), preserving the store-less WithProvider caller.
	sessStore := cliSessionStore(store, target.cfg, target.cityPath)
	obs, err := workerObserveNudgeTarget(target, sessStore, sp)
	if err != nil {
		return false, err
	}
	if obs.Running {
		handle, err := workerHandleForNudgeTarget(target, sessStore, sp)
		if err == nil {
			result, nudgeErr := handle.Nudge(context.Background(), sessionActor(session.ActorAgent, target.cityPath), worker.NudgeRequest{
				Text:     msg,
				Delivery: worker.NudgeDeliveryWaitIdle,
				Source:   "mail",
				Wake:     worker.NudgeWakeLiveOnly,
			})
			delivered := nudgeErr == nil && result.Delivered
			// The submit Enter can be delivered and the composer drained with
			// only the busy-indicator OBSERVATION timing out. Delivery is
			// proven either way, so this must not fall through to the queue
			// path below and duplicate the mail notification.
			unobservedButDelivered := errors.Is(nudgeErr, tmux.ErrNudgeSubmitDeliveredUnobserved)
			if delivered || unobservedButDelivered {
				telemetry.RecordNudge(context.Background(), target.agentKey(), nil)
				var sessFront *session.Store
				if store != nil {
					sessFront = sessionFrontDoor(sessStore)
				}
				stampLastNudgeDeliveredAt(sessFront, target.sessionID, time.Now())
				return false, nil
			}
		}
	}
	skipped := ""
	if !obs.Running {
		skipped = managedNudgeWakeSkipReason(target, store != nil)
	}
	if !obs.Running && skipped == "" {
		item := newQueuedNudgeWithOptions(target.agentKey(), msg, "mail", now, opts)
		outcome, err := enqueueManagedNudgeThenWake(target, store, item)
		if err != nil {
			return false, err
		}
		if outcome == session.WakeHeld {
			// A held session keeps the nudge queued; no poke.
			warnMailNotify("queued for %s%s", target.agentKey(), queuedNudgeDowngradeNote(target, worker.NudgeQueuedHeld))
			return true, nil
		}
		if err := nudgePokeController(target.cityPath, reconcilekey.Session(target.sessionID)); err != nil {
			warnMailNotify("warning: poke failed after managed wake: %v", err)
		}
		return true, nil
	}
	if err := enqueueQueuedNudge(target.cityPath, newQueuedNudgeWithOptions(target.agentKey(), msg, "mail", now, opts)); err != nil {
		return false, err
	}
	if obs.Running {
		maybeStartNudgePoller(target)
	} else {
		warnMailNotify("queued for %s but no managed wake was requested (%s); it is delivered when the session next runs", target.agentKey(), skipped)
	}
	return true, nil
}

// warnMailNotify writes one "gc mail notify:" line to nudgeWarningWriter: what
// a city-site mail notice did when it did not simply record a wake.
func warnMailNotify(format string, args ...any) {
	if nudgeWarningWriter != nil {
		fmt.Fprintf(nudgeWarningWriter, "gc mail notify: "+format+"\n", args...) //nolint:errcheck // best-effort warning
	}
}

func resolveNudgeTarget(identifier string, warningWriter ...io.Writer) (nudgeTarget, error) {
	return resolveNudgeTargetPreferringHookIdentity(identifier, false, warningWriter...)
}

// resolveNudgeTargetPreferringHookIdentity is resolveNudgeTarget for a caller
// that may target the hook's own session. With preferHookIdentity, an open
// session bead named by GC_SESSION_ID is the target, resolved with one Get and
// without entering the named-session materialize path, and a closed one is
// refused (errHookSessionClosed) rather than replaced by whatever identifier
// resolves to now — a successor holding the alias. When GC_SESSION_ID is unset
// or stale, identifier resolves exactly as resolveNudgeTarget resolves it.
func resolveNudgeTargetPreferringHookIdentity(identifier string, preferHookIdentity bool, warningWriter ...io.Writer) (nudgeTarget, error) {
	return resolveNudgeTargetAfter(identifier, preferHookIdentity, nil, warningWriter...)
}

// resolveNudgeTargetAfter is resolveNudgeTargetPreferringHookIdentity with
// before, when set, run over the loaded config and the opened nudges store
// ahead of the resolution, which may write (it materializes a configured
// named session's row): an error from before is returned unresolved.
func resolveNudgeTargetAfter(identifier string, preferHookIdentity bool, before func(cityPath string, cfg *config.City, store beads.NudgesStore) error, warningWriter ...io.Writer) (nudgeTarget, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return nudgeTarget{}, err
	}
	cfg, err := loadCityConfig(cityPath, warningWriter...)
	if err != nil {
		return nudgeTarget{}, err
	}
	// The load ran the builtin readiness pass the routing read needs; resolve
	// the routes alongside the store open (prewarmCLIStorageRoutes).
	defer prewarmCLIStorageRoutes(cityPath)()
	store := openNudgeBeadStore(cityPath)
	if before != nil {
		if err := before(cityPath, cfg, store); err != nil {
			return nudgeTarget{}, err
		}
	}
	return resolveNudgeTargetInStore(cityPath, cfg, store, identifier, preferHookIdentity)
}

// errMessageOpRefused is a message verb's require refusal that its preflight
// (resolveMessageOpTarget) has already reported.
var errMessageOpRefused = errors.New("the op journal refused the request before resolving its target")

// resolveMessageOpTarget is resolveNudgeTarget for a message verb recorded
// as req: under require it first checks that the op journal can record req
// (cliOp.preflight), since the resolution may materialize a configured named
// session's row, and answers errMessageOpRefused, the refusal reported, when
// it cannot. With the gate off, or on a worker site, which records no op, it
// checks nothing and opens no op store.
func resolveMessageOpTarget(identifier string, req cliOpRequest, warningWriter ...io.Writer) (nudgeTarget, error) {
	return resolveNudgeTargetAfter(identifier, false, func(cityPath string, cfg *config.City, store beads.NudgesStore) error {
		return messageOpPreflight(cityPath, cfg, store, req)
	}, warningWriter...)
}

// messageOpPreflight is resolveMessageOpTarget's check over the loaded
// config and the opened nudges store: under require, whether the op journal
// can record req (cliOp.preflight), answering errMessageOpRefused, the
// refusal reported, when it cannot. A worker site checks nothing.
func messageOpPreflight(cityPath string, cfg *config.City, store beads.NudgesStore, req cliOpRequest) error {
	if onWorkerSite(cityPath) {
		// A worker site records no op (sendMailNotifyWithOp records only
		// the recipient's wake), so the journal has nothing to refuse.
		return nil
	}
	// The op the verb starts once its target resolves prints the gate's
	// warnings; this one only refuses.
	stderr := req.stderr
	req.stderr = io.Discard
	if store.Store == nil {
		req.unrecordable = legacyop.ReasonStoreUnavailable
	}
	op := newCLIOp(cfg, cliSessionStore(store.Store, cfg, cityPath), req)
	op.req.stderr = stderr
	if !op.preflight() {
		return errMessageOpRefused
	}
	return nil
}

// resolveNudgeTargetInStore is resolveNudgeTargetPreferringHookIdentity over a
// city, config and nudges store the caller already holds. A zero store resolves
// nothing and reports identifier as not found, so a caller that must tell an
// unopened store from a missing session opens the store itself and checks the
// open error first.
func resolveNudgeTargetInStore(cityPath string, cfg *config.City, store beads.NudgesStore, identifier string, preferHookIdentity bool) (nudgeTarget, error) {
	if store.Store != nil {
		// Named-session materialization is a session WRITE, and the follow-up Get
		// reads the session bead; both route through the session-class store
		// (identity today) while the raw store remains the nudge-queue currency.
		sessStore := cliSessionStore(store.Store, cfg, cityPath)
		if preferHookIdentity {
			identity, ok, err := resolveHookIdentity(sessStore)
			if err != nil {
				return nudgeTarget{}, err
			}
			if ok {
				if identity.info.Closed {
					return nudgeTarget{}, closedHookSessionError(identity.info.ID)
				}
				return resolveNudgeTargetFromSessionInfo(cityPath, cfg, identity.info), nil
			}
		}
		sessionID, err := resolveSessionIDMaterializingNamed(cityPath, cfg, sessStore, identifier)
		if err == nil {
			info, getErr := sessionFrontDoor(sessStore).Get(sessionID)
			if getErr != nil {
				return nudgeTarget{}, getErr
			}
			return resolveNudgeTargetFromSessionInfo(cityPath, cfg, info), nil
		}
		if !errors.Is(err, session.ErrSessionNotFound) {
			return nudgeTarget{}, err
		}
	}
	return nudgeTarget{}, fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
}

// nudgeTargetFields carries the pre-extracted session attributes buildNudgeTarget
// needs. resolveNudgeTargetFromSessionInfo populates it from a session.Info
// projection, so the identity-resolution tail lives in exactly one place.
type nudgeTargetFields struct {
	sessionID         string
	sessionName       string
	alias             string
	agentName         string
	template          string
	commonName        string
	aliasHistory      []string
	transport         string
	provider          string
	continuationEpoch string
}

// resolveNudgeTargetFromSessionInfo reads the session attributes buildNudgeTarget
// needs from a session.Info projection (the typed front door) rather than cracking
// the raw session bead. Note the transport source is i.TransportMetadata (the RAW
// value), not i.Transport (which normalizeTransport would make non-empty), so the
// found.Session fallback below fires identically.
func resolveNudgeTargetFromSessionInfo(cityPath string, cfg *config.City, i session.Info) nudgeTarget {
	sessionName := strings.TrimSpace(i.SessionNameMetadata)
	if sessionName == "" {
		sessionName = sessionNameFromBeadID(i.ID)
	}
	return buildNudgeTarget(cityPath, cfg, nudgeTargetFields{
		sessionID:         i.ID,
		sessionName:       sessionName,
		alias:             strings.TrimSpace(i.Alias),
		agentName:         strings.TrimSpace(i.AgentName),
		template:          strings.TrimSpace(i.Template),
		commonName:        strings.TrimSpace(i.CommonName),
		aliasHistory:      i.AliasHistory,
		transport:         strings.TrimSpace(i.TransportMetadata),
		provider:          strings.TrimSpace(i.Provider),
		continuationEpoch: strings.TrimSpace(i.ContinuationEpoch),
	})
}

func buildNudgeTarget(cityPath string, cfg *config.City, f nudgeTargetFields) nudgeTarget {
	cityName := loadedCityName(cfg, cityPath)
	identity := firstNonEmpty(f.agentName, f.template, f.commonName)
	target := nudgeTarget{
		cityPath:          cityPath,
		cityName:          cityName,
		cfg:               cfg,
		identity:          identity,
		alias:             f.alias,
		aliasHistory:      f.aliasHistory,
		transport:         f.transport,
		resolved:          &config.ResolvedProvider{Name: f.provider},
		sessionID:         f.sessionID,
		continuationEpoch: f.continuationEpoch,
		sessionName:       f.sessionName,
	}
	target.agent = parseNudgeAgentIdentity(identity)
	for _, candidate := range []string{f.agentName, f.template, f.commonName} {
		if candidate == "" {
			continue
		}
		found, ok := resolveAgentIdentity(cfg, candidate, "")
		if !ok {
			continue
		}
		target.agent = found
		target.identity = found.QualifiedName()
		if target.transport == "" {
			target.transport = found.Session
		}
		if resolved, err := config.ResolveProvider(&found, &cfg.Workspace, cfg.Providers, exec.LookPath); err == nil {
			if resolved.Name == "" {
				resolved.Name = fallbackProviderName(found.Provider, cfg)
				if resolved.Name == "" && target.resolved != nil {
					resolved.Name = target.resolved.Name
				}
			}
			target.resolved = resolved
		}
		break
	}
	if target.identity == "" {
		target.identity = target.agent.QualifiedName()
	}
	if target.identity == "" {
		target.identity = f.sessionName
	}
	return target
}

func parseNudgeAgentIdentity(identity string) config.Agent {
	dir, name := config.ParseQualifiedName(identity)
	return config.Agent{Dir: dir, Name: name}
}

func fallbackProviderName(agentProvider string, cfg *config.City) string {
	if agentProvider != "" {
		return agentProvider
	}
	if cfg != nil {
		return cfg.Workspace.Provider
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func parseNudgeDeliveryMode(raw string) (nudgeDeliveryMode, error) {
	switch nudgeDeliveryMode(raw) {
	case nudgeDeliveryImmediate, nudgeDeliveryWaitIdle, nudgeDeliveryQueue:
		return nudgeDeliveryMode(raw), nil
	default:
		return "", fmt.Errorf("unknown delivery mode %q (want immediate, wait-idle, or queue)", raw)
	}
}

func tryDeliverQueuedNudgesByPoller(target nudgeTarget, store, sessStore beads.Store, sp runtime.Provider, quiescence time.Duration, obs worker.LiveObservation) (bool, error) {
	// A pane whose agent is dead (a crash the controller has not healed)
	// takes no queued delivery: the text would be lost with the pane. The
	// incarnation the heal brings up claims it (mc-48az4). Alive is Running
	// for a provider with no process names to check.
	if !obs.Alive {
		return false, nil
	}
	matches, err := nudgeTargetLiveGenerationMatches(target, obs, sp)
	if err != nil || !matches {
		return false, err
	}
	// An operator's hold keeps queued delivery off the session too (CONTRACT
	// v5.9 D8 7(a)), as a kill fence does in the dispatcher.
	if sessStore != nil && target.sessionID != "" {
		if info, err := sessionFrontDoor(sessStore).Get(target.sessionID); err == nil && session.HoldVerdictInfo(info, obs.Running, time.Now()) {
			return false, nil
		}
	}
	if !pollerSessionIdleEnough(target, sp, quiescence, obs) {
		return false, nil
	}
	items, err := claimDueQueuedNudgeBatchForTarget(target.cityPath, target, time.Now())
	if err != nil || len(items) == 0 {
		return false, err
	}
	deliveryStore := store
	if deliveryStore == nil {
		deliveryStore = openNudgeBeadStore(target.cityPath).Store
	}
	// sessStore is the SESSION-class store the caller resolved from the WORK store
	// (the dispatcher threads cr.sessionsBeadStore().Store; the CLI poll derives it
	// from its work store), so the worker handle, the front-door stamp, and the
	// gc:wait-bead split do NOT fall back to the nudges store when nudges relocates
	// independently of sessions. deliveryStore stays the NUDGES store for the queue
	// record/dead-letter path. When a caller passes nil sessStore (the store-less
	// test corner), fall back to the pre-split derivation so the handle
	// (nil -> runtime-only) and the stamp/split (from the fallback-opened store) stay
	// byte-identical. Identity today.
	handleSessStore := sessStore
	deliverySessStore := sessStore
	if sessStore == nil {
		handleSessStore = cliSessionStore(store, target.cfg, target.cityPath)
		deliverySessStore = cliSessionStore(deliveryStore, target.cfg, target.cityPath)
	}
	var deliverySessFront *session.Store
	var deliveryMailProvider mail.Provider
	if deliveryStore != nil {
		deliverySessFront = sessionFrontDoor(deliverySessStore)
		// Same split for the mail gate: the message re-read in
		// splitQueuedNudgesForDelivery is messaging-class (cliMailStore), while the
		// provider's session-addressing half reuses the session store resolved above.
		deliveryMailProvider = newMailProviderWithSessionStore(cliMailStore(deliveryStore, target.cfg, target.cityPath).Store, deliverySessStore)
	}
	// Bookkeeping for fence-mismatched and blocked items is best-effort: a
	// failure there must not abort delivery of the remaining claimable items.
	// Items whose bookkeeping failed stay in-flight; lease expiry returns
	// them to pending and a later pass retries. They keep the batch key, so
	// the remaining items move to a new one before delivery
	// (rekeyQueuedNudgesAfterFailedBookkeeping).
	var bookkeepErr error
	items, rejected := splitQueuedNudgesForTarget(target, items)
	if len(rejected) > 0 {
		if recErr := recordQueuedNudgeFailureWithStore(target.cityPath, beads.NudgesStore{Store: deliveryStore}, queuedNudgeIDs(rejected), errNudgeSessionFenceMismatch, time.Now()); recErr != nil {
			bookkeepErr = fmt.Errorf("dead-lettering fence-mismatched nudges: %w", recErr)
		}
	}
	candidates := items
	items, blocked, err := splitQueuedNudgesForDelivery(sessionFrontDoor(deliverySessStore), deliveryMailProvider, candidates)
	if err != nil {
		relErr := releaseQueuedNudgeClaims(target.cityPath, queuedNudgeIDs(candidates))
		return false, errors.Join(bookkeepErr, err, relErr)
	}
	if len(blocked) > 0 {
		if termErr := terminalizeBlockedQueuedNudges(target.cityPath, blocked); termErr != nil {
			bookkeepErr = errors.Join(bookkeepErr, fmt.Errorf("withdrawing blocked nudges: %w", termErr))
		}
	}
	var staleMail []queuedNudge
	items, staleMail, err = splitStaleMailQueuedNudges(target, deliveryMailProvider, deliverySessStore, items)
	if err != nil && nudgeWarningWriter != nil {
		fmt.Fprintf(nudgeWarningWriter, "gc nudge: warning: checking mail staleness for %s: %v\n", target.agentKey(), err) //nolint:errcheck
	}
	if len(staleMail) > 0 {
		if termErr := terminalizeBlockedQueuedNudges(target.cityPath, map[string][]queuedNudge{"mail-already-read": staleMail}); termErr != nil {
			bookkeepErr = errors.Join(bookkeepErr, fmt.Errorf("withdrawing stale mail nudges: %w", termErr))
		}
	}
	items, bookkeepErr = rekeyQueuedNudgesAfterFailedBookkeeping(target.cityPath, items, bookkeepErr)
	if len(items) == 0 {
		return false, bookkeepErr
	}
	// One delivery per pass: a deferred user submit alone, or the reminders
	// queued ahead of the next one. The rest waits for a later pass.
	items, later := nextQueuedDeliveryGroup(items)
	if len(later) > 0 {
		if err := deferQueuedNudgesToLaterPass(target.cityPath, later); err != nil {
			relErr := releaseQueuedNudgeClaims(target.cityPath, queuedNudgeIDs(items))
			return false, errors.Join(bookkeepErr, err, relErr)
		}
	}
	var msg string
	switch {
	case isQueuedUserSubmit(items[0]):
		// The user's prompt, as the user wrote it: never a reminder.
		msg = items[0].Message
	case target.sessionTransport() == "acp":
		msg = formatNudgeRuntimeMessage(items)
	default:
		msg = formatNudgeInjectOutput(items)
	}
	handle, err := workerHandleForNudgeTarget(target, handleSessStore, sp)
	if err != nil {
		relErr := releaseQueuedNudgeClaims(target.cityPath, queuedNudgeIDs(items))
		return false, errors.Join(bookkeepErr, err, relErr)
	}
	result, err := handle.Nudge(context.Background(), sessionActor(session.ActorAgent, target.cityPath), worker.NudgeRequest{
		Text:        msg,
		Delivery:    worker.NudgeDeliveryDefault,
		Source:      "queue",
		Wake:        worker.NudgeWakeLiveOnly,
		DeliveryKey: queuedNudgeDeliveryKey(items),
	})
	if err != nil {
		telemetry.RecordNudge(context.Background(), target.agentKey(), err)
		if retryAt, released := queuedNudgeReleaseAfter(err, time.Now()); released {
			if errors.Is(err, runtime.ErrSessionBusy) && nudgeWarningWriter != nil {
				// Charged no attempt, so nothing else records that this
				// delivery keeps waiting on a turn the runtime will not interrupt.
				fmt.Fprintf(nudgeWarningWriter, "gc nudge: %s: %d queued item(s) %s deferred, retry at %s: %v\n", //nolint:errcheck // best-effort warning
					target.agentKey(), len(items), strings.Join(queuedNudgeIDs(items), ","), retryAt.UTC().Format(time.RFC3339), err)
			}
			if recErr := releaseQueuedNudgeClaimsUntil(target.cityPath, queuedNudgeIDs(items), retryAt); recErr != nil {
				return false, errors.Join(bookkeepErr, recErr)
			}
			return false, bookkeepErr
		}
		if errors.Is(err, tmux.ErrNudgeSubmitDeliveredUnobserved) {
			// The submit Enter was delivered and the composer drained; only the
			// busy-indicator OBSERVATION timed out. Delivery is proven, so this
			// must ack like a success, not run through failedQueuedNudge's
			// attempt-counting/dead-letter path — that would re-inject the same
			// reminder on the next pass.
			stampLastNudgeDeliveredAt(deliverySessFront, target.sessionID, time.Now())
			ackErr := ackQueuedNudgesWithOutcome(target.cityPath, queuedNudgeIDs(items), "injected_unobserved", "", "provider-nudge-return")
			return true, errors.Join(bookkeepErr, ackErr)
		}
		if recErr := recordQueuedNudgeFailureWithStore(target.cityPath, beads.NudgesStore{Store: deliveryStore}, queuedNudgeIDs(items), err, time.Now()); recErr != nil {
			return false, errors.Join(bookkeepErr, recErr)
		}
		return false, bookkeepErr
	}
	if !result.Delivered {
		// The runtime declined without an error (e.g. the session stopped
		// between observation and delivery). Release the claims so the next
		// pass retries promptly instead of waiting out the in-flight lease.
		relErr := releaseQueuedNudgeClaims(target.cityPath, queuedNudgeIDs(items))
		return false, errors.Join(bookkeepErr, relErr)
	}
	telemetry.RecordNudge(context.Background(), target.agentKey(), nil)
	stampLastNudgeDeliveredAt(deliverySessFront, target.sessionID, time.Now())
	return true, errors.Join(bookkeepErr, ackQueuedNudges(target.cityPath, queuedNudgeIDs(items)))
}

func stampLastNudgeDeliveredAt(sessFront *session.Store, sessionID string, t time.Time) {
	if sessFront == nil || sessionID == "" {
		return
	}
	// Best-effort stamp. Delivery already succeeded, so a metadata write
	// failure here must not bubble back to the caller and force a redelivery.
	_ = sessFront.SetMarker(sessionID, session.MetadataLastNudgeDeliveredAt, t.UTC().Format(time.RFC3339))
}

func pollerSessionIdleEnough(target nudgeTarget, sp runtime.Provider, quiescence time.Duration, obs worker.LiveObservation) bool {
	if quiescence <= 0 {
		return true
	}
	if obs.LastActivity != nil && !obs.LastActivity.IsZero() {
		return time.Since(*obs.LastActivity) >= quiescence
	}
	if pollerCanDeliverWithoutActivitySignal(target, sp) {
		return true
	}
	if target.sessionName == "" {
		return false
	}
	waiter, ok := sp.(runtime.IdleWaitProvider)
	if !ok {
		return false
	}
	// The poller may take up to the quiescence window to exit while this
	// runtime idle check is in progress.
	ctx, cancel := context.WithTimeout(context.Background(), quiescence)
	defer cancel()
	return waiter.WaitForIdle(ctx, target.sessionName, quiescence) == nil
}

func pollerCanDeliverWithoutActivitySignal(target nudgeTarget, sp runtime.Provider) bool {
	if sp == nil || target.sessionName == "" {
		return false
	}
	if sp.Capabilities().CanReportActivity {
		return false
	}
	sleeper, ok := sp.(runtime.SleepCapabilityProvider)
	if !ok {
		return false
	}
	return sleeper.SleepCapability(target.sessionName) == runtime.SessionSleepCapabilityTimedOnly
}

func maybeStartNudgePoller(target nudgeTarget) {
	if target.sessionName == "" {
		return
	}
	// Reap stale poller PID files before deciding whether to spawn. Owning
	// processes only remove their PID file via the release closure, so any
	// poller that is killed/crashes/os.Exit's leaves the .pid behind forever.
	// This low-frequency hook keeps the pollers directory from growing without
	// bound. The sibling .pid.lock is intentionally left in place (removing it
	// races concurrent acquirers — see reapStaleNudgePoller). Best-effort:
	// never block a spawn.
	_ = reapStaleNudgePollers(target.cityPath)
	startNudgePollerUnlessDispatched(target)
}

// startNudgePollerReap reaps cityPath's stale poller PID files in the
// background and returns the wait for it. `gc prime --hook` reaps this way
// while it does its own reads, and starts its poller with
// startNudgePollerUnlessDispatched: the reap lists the whole pollers
// directory, which keeps a lock file for every session that ever ran a poller
// (97k entries and 0.14-0.21 s on maintainer-city), and the spawn decision
// does not depend on it, since ensureNudgePoller checks its own PID file under
// the same per-file lock the reap takes.
func startNudgePollerReap(cityPath string) (wait func()) {
	reap := nudgePollerReaper
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = reap(cityPath)
	}()
	return func() { <-done }
}

// nudgePollerReaper is the reap startNudgePollerReap runs: a seam, so a test
// can see when it starts.
var nudgePollerReaper = reapStaleNudgePollers

// startNudgePollerUnlessDispatched is maybeStartNudgePoller without the reap,
// for a caller that reaps the pollers directory itself (startNudgePollerReap).
func startNudgePollerUnlessDispatched(target nudgeTarget) {
	if target.sessionName == "" {
		return
	}
	// Supervisor-hosted dispatcher owns delivery in supervisor mode; the
	// per-session poller would race with it and reintroduce the bd-shellout
	// load it was designed to eliminate.
	if nudgeDispatcherIsSupervisor(target.cfg) {
		return
	}
	// ACP session/prompt delivery requires the process that owns the
	// in-memory ACP connection. A sidecar `gc nudge poll` process can
	// observe the control socket but cannot safely deliver prompts. In
	// legacy mode this leaves warm-idle ACP delivery to hook-drain only;
	// configure daemon.nudge_dispatcher = "supervisor" for dispatcher-owned
	// queued delivery.
	if target.sessionTransport() == "acp" {
		return
	}
	if err := startNudgePoller(target.cityPath, target.pollerKey(), target.sessionName); err != nil {
		return
	}
}

func withNudgeTargetFence(store beads.Store, target nudgeTarget) nudgeTarget {
	if target.sessionName == "" {
		return target
	}
	if target.sessionID != "" && target.continuationEpoch != "" {
		return target
	}
	if store == nil {
		return target
	}
	open, err := loadOpenSessionInfos(store)
	if err != nil {
		return target
	}
	for _, info := range open {
		if info.SessionNameMetadata != target.sessionName {
			continue
		}
		if target.sessionID == "" {
			target.sessionID = info.ID
		}
		if target.continuationEpoch == "" {
			target.continuationEpoch = info.ContinuationEpoch
		}
		return target
	}
	return target
}

// withNudgeTargetFenceFromLiveSession is withNudgeTargetFence for a caller
// that has already read the target's live session: when that session carries
// the target's session name it is the open session the fence would find, so
// the fence is taken from it without listing the city's sessions.
func withNudgeTargetFenceFromLiveSession(store beads.Store, live *session.Info, target nudgeTarget) nudgeTarget {
	if live == nil || live.Closed || target.sessionName == "" || live.SessionNameMetadata != target.sessionName {
		return withNudgeTargetFence(store, target)
	}
	if target.sessionID == "" {
		target.sessionID = live.ID
	}
	if target.continuationEpoch == "" {
		target.continuationEpoch = live.ContinuationEpoch
	}
	return target
}

var startNudgePoller = ensureNudgePoller

// nudgeDispatcherIsSupervisor reports whether the city is configured to use
// the supervisor-hosted nudge dispatcher rather than per-session pollers.
// A nil cfg defaults to legacy mode, matching DaemonConfig.NudgeDispatcherMode.
func nudgeDispatcherIsSupervisor(cfg *config.City) bool {
	if cfg == nil {
		return false
	}
	return cfg.Daemon.NudgeDispatcherMode() == "supervisor"
}

// splitQueuedNudgesForTarget partitions claimed nudges into deliverable items
// and rejects. Items whose fence (SessionID / ContinuationEpoch) does not match
// the current target's fence are re-fenced onto it (refencedQueuedNudge) and
// returned as deliverable when the target itself is resolved to a live session
// bead. Claim already admitted the item: either the SessionID still matches
// (stale-epoch Leg A) or the fenced session is no longer a live occupant
// (session-replacement Leg B; see queuedNudgeClaimableForTarget). A stale
// fence then just marks a prior incarnation of the same seat. Delivering it
// there is what the seat's nudges were queued for; dead-lettering the whole
// queue every time a session respawns (fresh wake after a failed spawn bumps
// ContinuationEpoch; a re-materialization can mint a new SessionID) is the
// bug this coalesces around (ga-bow, thunderfartcity:gp-u63s, #5816).
//
// A target with no resolved session identity (sessionID == "") cannot be
// re-fenced against and still rejects, matching the pre-existing invariant
// that fenced items are not delivered to an unresolved target.
func splitQueuedNudgesForTarget(target nudgeTarget, items []queuedNudge) ([]queuedNudge, []queuedNudge) {
	if len(items) == 0 {
		return nil, nil
	}
	var deliverable []queuedNudge
	var rejected []queuedNudge
	for _, item := range items {
		item = refencedQueuedNudge(target, item)
		if !queuedNudgeMatchesTargetFence(target, item) {
			rejected = append(rejected, item)
			continue
		}
		deliverable = append(deliverable, item)
	}
	return deliverable, rejected
}

// refencedQueuedNudge returns item with target's fence (SessionID /
// ContinuationEpoch) when its own fence does not match target's and target is
// resolved to a session to fence it onto. Otherwise it returns item unchanged.
func refencedQueuedNudge(target nudgeTarget, item queuedNudge) queuedNudge {
	if target.sessionID == "" || queuedNudgeMatchesTargetFence(target, item) {
		return item
	}
	item.SessionID = target.sessionID
	item.ContinuationEpoch = target.continuationEpoch
	return item
}

// splitQueuedNudgesForDelivery partitions claimed nudges into deliverable items
// and reason-tagged blocked items. The sessFront param is the session
// coordination-class write front door: blockedQueuedNudgeReason reads the
// referenced gc:wait bead (coordclass.ClassSessions) to gate wait-sourced
// nudges. Callers construct it at the root over the session-class store (via
// cliSessionStore) so a [beads.classes.sessions] relocation reaches it. mp is
// the mail provider used the same way to gate mail-sourced nudges; it may be
// nil, which behaves as if no mail nudge ever carried a reference (delivered
// unconditionally, matching pre-#5321 behavior).
func splitQueuedNudgesForDelivery(sessFront *session.Store, mp mail.Provider, items []queuedNudge) ([]queuedNudge, map[string][]queuedNudge, error) {
	if len(items) == 0 {
		return nil, nil, nil
	}
	deliverable := make([]queuedNudge, 0, len(items))
	blocked := make(map[string][]queuedNudge)
	for _, item := range items {
		reason, shouldBlock, err := blockedQueuedNudgeReason(sessFront, mp, item)
		if err != nil {
			return nil, nil, err
		}
		if shouldBlock {
			blocked[reason] = append(blocked[reason], item)
			continue
		}
		deliverable = append(deliverable, item)
	}
	return deliverable, blocked, nil
}

// blockedQueuedNudgeReason re-validates a claimed nudge against the current
// state of the thing it announces, at delivery time. It's the per-source
// dispatch table: each queued-nudge source that carries a re-checkable
// reference gets its own gate below, so an item whose referent has since been
// resolved (a wait that fired, a mail message already read) is withdrawn
// instead of waking the target for stale news. See gastownhall/gascity#5321.
func blockedQueuedNudgeReason(sessFront *session.Store, mp mail.Provider, item queuedNudge) (string, bool, error) {
	switch item.Source {
	case "wait":
		return blockedQueuedWaitNudgeReason(sessFront, item)
	case "mail":
		return blockedQueuedMailNudgeReason(mp, item)
	default:
		return "", false, nil
	}
}

func blockedQueuedWaitNudgeReason(sessFront *session.Store, item queuedNudge) (string, bool, error) {
	if !sessFront.Backed() || item.Reference == nil || item.Reference.Kind != "bead" || item.Reference.ID == "" {
		return "", false, nil
	}
	wait, err := sessFront.GetWait(item.Reference.ID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return "wait-missing", true, nil
		}
		if errors.Is(err, session.ErrNotAWait) {
			return "wait-reference-invalid", true, nil
		}
		return "", false, err
	}
	switch wait.State {
	case waitStateReady:
		return "", false, nil
	case waitStateCanceled:
		return "wait-canceled", true, nil
	case waitStateClosed:
		return "wait-closed", true, nil
	case waitStateExpired:
		return "wait-expired", true, nil
	case waitStateFailed:
		return "wait-failed", true, nil
	default:
		return "wait-not-ready", true, nil
	}
}

// blockedQueuedMailNudgeReason re-reads the mail message a queued mail nudge
// announces. A message that has vanished (e.g. archived — gastownhall/gascity#4422
// deletes the underlying bead) is withdrawn as "mail-missing" rather than
// treated as an error, mirroring the wait path's not-found handling: the
// obvious predicate "is this still unread" would otherwise never fire for an
// archived message, since it's neither read nor unread. A message that has
// been read since the nudge was queued is withdrawn as "mail-already-read",
// which is #5321's primary target case.
func blockedQueuedMailNudgeReason(mp mail.Provider, item queuedNudge) (string, bool, error) {
	if mp == nil || item.Reference == nil || item.Reference.Kind != "mail" || item.Reference.ID == "" {
		return "", false, nil
	}
	msg, err := mp.Get(item.Reference.ID)
	if err != nil {
		if errors.Is(err, mail.ErrNotFound) {
			return "mail-missing", true, nil
		}
		return "", false, err
	}
	if msg.Read {
		return "mail-already-read", true, nil
	}
	return "", false, nil
}

// terminalizeBlockedQueuedNudges withdraws the blocked items by acking them
// failed with their reason. It is a seam so a test can make the withdrawal of
// chosen items fail, as an ack that cannot take the queue lock does. Tests
// that replace it must stay serial.
var terminalizeBlockedQueuedNudges = ackBlockedQueuedNudgesWithdrawn

func ackBlockedQueuedNudgesWithdrawn(cityPath string, blocked map[string][]queuedNudge) error {
	for reason, items := range blocked {
		if err := ackQueuedNudgesWithOutcome(cityPath, queuedNudgeIDs(items), "failed", reason, "delivery-withdrawn"); err != nil {
			return err
		}
	}
	return nil
}

// targetHasUnreadMail reports whether target's own mailbox still has at
// least one unread message. It re-verifies unread state at nudge-delivery
// time for a deferred "you have mail" reminder: ga-vtxkj5 found the queue
// replaying that reminder long after the mail had already been read, because
// nothing re-checked unread state between enqueue and delivery.
//
// The check is deliberately coarse: it looks at aggregate unread count for
// the target, not the specific message that produced the reminder, because
// queued mail nudges carry no message ID to check individually. Treating
// "still has unread mail" as the safe default (on ambiguity or error, report
// unread) preserves the TestSendMailNotifyQueuesIndependentRemindersForEachMail
// guarantee from PR #2968 that independent mail arrivals never get silently
// dropped -- this only withdraws a reminder when unread count is truly zero.
//
// mp must read the messaging-class store (cliMailStore), not the nudges
// store, so unread mail is visible when mail is routed separately. A nil mp
// counts as unread, so the reminder is kept.
func targetHasUnreadMail(target nudgeTarget, mp mail.Provider, sessStore beads.Store) (bool, error) {
	if mp == nil || target.sessionID == "" {
		return true, nil
	}
	resolved, err := resolveMailTargetsWithConfig(target.cityPath, target.cfg, sessStore, target.sessionID)
	if err != nil {
		return true, err
	}
	messages, err := collectMailMessages(mp.Check, resolved.recipients)
	if err != nil {
		return true, err
	}
	return len(messages) > 0, nil
}

// splitStaleMailQueuedNudges partitions items into ones that should still be
// delivered (keep) and mail-sourced ones whose target has since read all
// mail and so must be withdrawn instead of replayed as a stale reminder
// (stale). Non-mail items always pass through into keep untouched.
func splitStaleMailQueuedNudges(target nudgeTarget, mp mail.Provider, sessStore beads.Store, items []queuedNudge) (keep, stale []queuedNudge, err error) {
	hasMailItem := false
	for _, item := range items {
		if item.Source == "mail" {
			hasMailItem = true
			break
		}
	}
	if !hasMailItem {
		return items, nil, nil
	}
	unread, err := targetHasUnreadMail(target, mp, sessStore)
	if err != nil {
		return items, nil, err
	}
	if unread {
		return items, nil, nil
	}
	keep = make([]queuedNudge, 0, len(items))
	for _, item := range items {
		if item.Source == "mail" {
			stale = append(stale, item)
			continue
		}
		keep = append(keep, item)
	}
	return keep, stale, nil
}

func ensureNudgePoller(cityPath, agentName, sessionName string) error {
	pidPath := nudgePollerPIDPath(cityPath, sessionName, agentName)
	return withNudgePollerPIDLock(pidPath, func() error {
		if running, _ := existingPollerPID(pidPath, cityPath, sessionName, agentName); running {
			return nil
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := newNudgePollerCommand(exe, os.Environ(), cityPath, sessionName, agentName)
		if err := cmd.Start(); err != nil {
			return err
		}
		if err := writeNudgePollerPID(pidPath, cmd.Process.Pid); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Process.Release()
			return err
		}
		return cmd.Process.Release()
	})
}

// newNudgePollerCommand builds the detached poller child over environ, without
// the spawning session's identity (nudgepoller.ChildEnv), so the poller
// never reads as that session's leaked process.
func newNudgePollerCommand(exe string, environ []string, cityPath, sessionName, agentName string) *exec.Cmd {
	cmd := exec.Command(exe, nudgepoller.CommandArgs(cityPath, sessionName, agentName)...)
	cmd.Env = nudgepoller.ChildEnv(environ)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	disableProductMetricsForChild(cmd)
	return cmd
}

// isQueuedUserSubmit reports whether item is a user's submit the session
// layer queued (nudgequeue.SourceSubmit), not a nudge.
func isQueuedUserSubmit(item queuedNudge) bool {
	return item.Source == nudgequeue.SourceSubmit
}

// splitQueuedUserSubmits separates the deferred user submits in items from
// the nudges, keeping each side's order.
func splitQueuedUserSubmits(items []queuedNudge) (nudges, submits []queuedNudge) {
	for _, item := range items {
		if isQueuedUserSubmit(item) {
			submits = append(submits, item)
		} else {
			nudges = append(nudges, item)
		}
	}
	return nudges, submits
}

// nextQueuedDeliveryGroup splits a claimed batch, in queue order, into what
// one delivery carries and what waits. A deferred user submit goes out alone,
// as the user's prompt; nudges go out together, up to the next submit, as one
// reminder. Nothing queued after a submit is delivered ahead of it.
func nextQueuedDeliveryGroup(items []queuedNudge) (group, later []queuedNudge) {
	if len(items) == 0 || isQueuedUserSubmit(items[0]) {
		return items[:min(len(items), 1)], items[min(len(items), 1):]
	}
	n := 1
	for n < len(items) && !isQueuedUserSubmit(items[n]) {
		n++
	}
	return items[:n], items[n:]
}

// deferQueuedNudgesToLaterPass returns claimed items this pass does not
// deliver to pending, first moving them onto a new delivery key: the key they
// were claimed under belongs to this pass's delivery, and a runtime that
// deduplicates by key would answer them with its receipt (see
// rekeyQueuedNudgesAfterFailedBookkeeping).
func deferQueuedNudgesToLaterPass(cityPath string, items []queuedNudge) error {
	ids := queuedNudgeIDs(items)
	if err := repinQueuedNudgeClaims(cityPath, ids, newQueuedNudgeBatchID()); err != nil {
		return errors.Join(fmt.Errorf("moving queued items to a later delivery: %w", err), releaseQueuedNudgeClaims(cityPath, ids))
	}
	return releaseQueuedNudgeClaims(cityPath, ids)
}

// repinQueuedNudgeClaims moves the caller's in-flight claims onto delivery key
// batch, all or nothing, retrying a busy queue lock (retryNudgeBookkeeping).
func repinQueuedNudgeClaims(cityPath string, ids []string, batch string) error {
	return retryNudgeBookkeeping(func() error {
		return openNudgeQueue(cityPath, beads.NudgesStore{}).Repin.RepinClaims(ids, batch)
	})
}

func formatNudgeInjectOutput(items []queuedNudge) string {
	var sb strings.Builder
	sb.WriteString("<system-reminder>\n")
	if len(items) == 1 {
		sb.WriteString("You have a deferred reminder that was queued until a safe boundary:\n\n")
	} else {
		fmt.Fprintf(&sb, "You have %d deferred reminders that were queued until a safe boundary:\n\n", len(items))
	}
	for _, item := range items {
		// Sanitize attacker-controllable fields before interpolating into
		// the <system-reminder> block — without this, a sender can inject
		// </system-reminder> sequences and break out of the reminder.
		// See gastownhall/gascity#2195.
		source := extmsg.SanitizeForSystemReminder(item.Source)
		message := extmsg.SanitizeForSystemReminder(item.Message)
		fmt.Fprintf(&sb, "- [%s] %s\n", source, message)
	}
	sb.WriteString("\nHandle them after this turn.\n")
	sb.WriteString("</system-reminder>\n")
	return sb.String()
}

func formatNudgeRuntimeMessage(items []queuedNudge) string {
	var sb strings.Builder
	sb.WriteString("Deferred reminders:\n")
	for _, item := range items {
		fmt.Fprintf(&sb, "- [%s] %s\n", item.Source, item.Message)
	}
	sb.WriteString("\nThese were queued until the session went idle.\n")
	return sb.String()
}

func formatDueTime(ts time.Time) string {
	if ts.IsZero() {
		return "now"
	}
	d := time.Until(ts).Round(time.Second)
	switch {
	case d <= 0:
		return "now"
	case d < time.Minute:
		return d.String()
	default:
		return ts.Format(time.RFC3339)
	}
}

func deadReason(item queuedNudge) string {
	if item.LastError != "" {
		return item.LastError
	}
	if !item.ExpiresAt.IsZero() && item.ExpiresAt.Before(time.Now()) {
		return "expired"
	}
	return "dead-letter"
}

func newQueuedNudge(agentName, message string, now time.Time) queuedNudge {
	return newQueuedNudgeWithOptions(agentName, message, "session", now, queuedNudgeOptions{})
}

func newQueuedNudgeWithOptions(agentName, message, source string, now time.Time, opts queuedNudgeOptions) queuedNudge {
	id := opts.ID
	if id == "" {
		id = newQueuedNudgeID()
	}
	return queuedNudge{
		ID:                id,
		Agent:             agentName,
		SessionID:         opts.SessionID,
		ContinuationEpoch: opts.ContinuationEpoch,
		Source:            source,
		Message:           message,
		Reference:         opts.Reference,
		CreatedAt:         now.UTC(),
		DeliverAfter:      now.UTC(),
		ExpiresAt:         now.Add(defaultQueuedNudgeTTL).UTC(),
	}
}

func newQueuedNudgeID() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("nudge-%d", time.Now().UnixNano())
	}
	return "nudge-" + hex.EncodeToString(buf[:])
}

// queuedNudgeDeliveryKey is the delivery key of the claimed batch items: the
// batch id pinned on them at claim time (claimDueQueuedNudgeBatchForTarget),
// or "" when they carry no common pin and the delivery goes unkeyed.
func queuedNudgeDeliveryKey(items []queuedNudge) string {
	if len(items) == 0 {
		return ""
	}
	key := items[0].DeliveryBatch
	for _, item := range items[1:] {
		if item.DeliveryBatch != key {
			return ""
		}
	}
	return key
}

// newQueuedNudgeBatchID mints the delivery key a newly claimed batch is
// pinned to.
func newQueuedNudgeBatchID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("nudge-batch-%d", time.Now().UnixNano())
	}
	return "nudge-batch-" + hex.EncodeToString(buf[:])
}

// rekeyQueuedNudgesAfterFailedBookkeeping moves the claimed items about to be
// delivered onto a new delivery key when withdrawing a withheld sibling of
// their batch failed (bookkeepErr). That sibling stays in flight on the old
// key, and lease expiry returns it to pending still pinned there, so it later
// goes out alone under that key. Had the rest of the batch been delivered
// under the key, the runtime would answer the sibling with the receipt of a
// delivery that never contained it, acking it unseen. After a successful
// withdraw the key stays: the withdrawn items are terminal, the items left
// are the whole batch, and a redelivery after an unknown outcome must replay
// the first attempt. The new key is persisted before delivery, so a retry of
// this delivery reuses it. When it cannot be persisted, the claims are
// released and nothing is delivered.
func rekeyQueuedNudgesAfterFailedBookkeeping(cityPath string, items []queuedNudge, bookkeepErr error) ([]queuedNudge, error) {
	if bookkeepErr == nil || len(items) == 0 {
		return items, bookkeepErr
	}
	batch := newQueuedNudgeBatchID()
	if err := repinQueuedNudgeClaims(cityPath, queuedNudgeIDs(items), batch); err != nil {
		relErr := releaseQueuedNudgeClaims(cityPath, queuedNudgeIDs(items))
		return nil, errors.Join(bookkeepErr, fmt.Errorf("moving the delivery onto a new key: %w", err), relErr)
	}
	rekeyed := make([]queuedNudge, len(items))
	for i, item := range items {
		item.DeliveryBatch = batch
		rekeyed[i] = item
	}
	return rekeyed, bookkeepErr
}

func queuedNudgeIDs(items []queuedNudge) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func queuedNudgeMatchesTargetFence(target nudgeTarget, item queuedNudge) bool {
	if item.SessionID != "" && item.SessionID != target.sessionID {
		return false
	}
	if item.ContinuationEpoch != "" && item.ContinuationEpoch != target.continuationEpoch {
		return false
	}
	return true
}

// liveNudgeFenceSessionIDs is a test seam for the session-liveness census
// used by queuedNudgeClaimableForTarget. When non-nil, claim uses this
// instead of listing session beads. Tests that replace it must not use
// t.Parallel.
var liveNudgeFenceSessionIDs func(cityPath string) map[string]struct{}

func liveNudgeFenceSessionIDsForCity(cityPath string) map[string]struct{} {
	if liveNudgeFenceSessionIDs != nil {
		return liveNudgeFenceSessionIDs(cityPath)
	}
	return loadLiveNudgeFenceSessionIDsFromCity(cityPath)
}

// loadLiveNudgeFenceSessionIDsFromCity returns session IDs that still occupy
// a seat and therefore still own their fenced nudges. A nil result means the
// census was unavailable; callers must fail closed and leave mismatched
// SessionID items pending.
func loadLiveNudgeFenceSessionIDsFromCity(cityPath string) map[string]struct{} {
	store, opened := openOwnedNudgeBeadStore(cityPath)
	defer closeBeadStoreHandle(opened) //nolint:errcheck // best-effort
	if store.Store == nil {
		return nil
	}
	open, err := loadOpenSessionInfos(cliSessionStore(store.Store, nil, cityPath))
	if err != nil {
		return nil
	}
	live := make(map[string]struct{})
	for _, info := range open {
		if sessionHoldsNudgeFence(info) {
			live[info.ID] = struct{}{}
		}
	}
	return live
}

// sessionHoldsNudgeFence reports whether a session still occupies its seat
// for nudge-fence purposes. Drained, archived, failed-create, and closed
// sessions are superseded: their queued nudges may rebind to a successor.
// Asleep, draining, and quarantined sessions still own their fence so a
// concurrent sibling (or an in-flight drain) is not stolen.
func sessionHoldsNudgeFence(info session.Info) bool {
	if info.Closed || strings.TrimSpace(info.ID) == "" {
		return false
	}
	switch info.State {
	case session.StateDrained, session.StateArchived, session.StateFailedCreate:
		return false
	default:
		return true
	}
}

// nudgeQueueIsEmpty reports whether the city's persisted nudge queue holds no
// pending, in-flight, or dead item. It reads the atomically replaced state file
// without taking the queue lock; an unreadable queue is not empty.
func nudgeQueueIsEmpty(cityPath string) bool {
	empty, err := openNudgeQueue(cityPath, beads.NudgesStore{}).Queue.Empty()
	return err == nil && empty
}

// nudgeDrainExplicitCityPath returns the city resolveCity would pick when the
// explicit city environment alone decides it: no --city/--rig flag and no
// remote selector outranks GC_CITY/GC_CITY_PATH/GC_CITY_ROOT. Unlike
// resolveCity it does not load the city config to name a rig.
//
// The city's control-plane gate applies here too: a city whose descriptor
// cannot be used is not taken, so the full path's resolution refuses it.
func nudgeDrainExplicitCityPath() (string, bool) {
	if cityFlag != "" || rigFlag != "" || readRemoteSelection().hasExplicitRemote() {
		return "", false
	}
	cityPath, ok := resolveExplicitCityPathEnv()
	if !ok || gateControlPlaneAt(cityPath) != nil {
		return "", false
	}
	return cityPath, true
}

// claimDueQueuedNudgesForTarget claims every due nudge for target, pinned or
// not, and pins nothing. Its caller, the hook drain, prints them into the
// agent's own hook output with no delivery key, so taking part of a pinned
// batch is safe: no runtime records a receipt under the pin for that part, and
// the rest of the batch later goes out under its key, where the runtime either
// replays a receipt of the whole batch or commits the rest as new. A delivery
// under a key claims through claimDueQueuedNudgeBatchForTarget instead.
func claimDueQueuedNudgesForTarget(cityPath string, target nudgeTarget, now time.Time) ([]queuedNudge, error) {
	return openNudgeQueue(cityPath, beads.NudgesStore{}).Queue.ClaimDue(target.claimTarget(cityPath), now)
}

// claimDueQueuedNudgeBatchForTarget claims target's next delivery batch and
// pins it to a delivery key (queuedNudgeDeliveryKey). When a due item already
// carries a pin and its batch can be claimed whole (wholePinnedBatch), the
// batch is that pinned batch, all of its pending items and nothing else: a
// redelivery after an unknown outcome must be the same delivery. Otherwise it
// is every due unpinned item, pinned to a new key. A pinned batch is taken
// whole whatever its items' deliver_after, unlike storebinding's
// NudgeQueue.ClaimBatch, which waits until every member is due.
//
// The claim also moves each item it takes onto target's fence in the queue
// (refencedQueuedNudge), not only in the delivered copy. A batch thus carries
// the fence of the session that last tried to deliver it, and its items match
// or fail to match a target together. Were a nudge taken from a drained
// predecessor's fence to keep that fence, the predecessor waking again would
// leave the batch split for both sessions (wholePinnedBatch) until it expired.
func claimDueQueuedNudgeBatchForTarget(cityPath string, target nudgeTarget, now time.Time) ([]queuedNudge, error) {
	return openNudgeQueue(cityPath, beads.NudgesStore{}).Queue.ClaimBatch(target.claimTarget(cityPath), now)
}

// listQueuedNudgesForTargetSnapshot returns target's queue buckets from a
// lock-free read of the persisted state (ga-2kzci3 FR5, ported from the
// unmerged ga-cn8dkj fix for the sibling cmdNudgeStatus regression). It is
// the read-only peer of listQueuedNudgesForTarget: it runs no maintenance
// pass, opens no bead store, takes no flock, and never writes.
//
// A liveness check like shouldKeepNudgePollerAlive must not wait on the
// queue's writer lock. Reading without the lock is safe because WithState
// rewrites state.json atomically (temp file + rename), so a reader always
// observes one whole snapshot, never a torn one.
//
// The bucketing below mirrors the in-memory effect of the recover/prune
// passes so the result matches what a maintaining caller would report,
// while mutating nothing. Dead-letter retention pruning is deliberately NOT
// mirrored: it depends on the bead store, and treating a not-yet-swept dead
// letter as still dead is harmless for a liveness check.
func listQueuedNudgesForTargetSnapshot(cityPath string, target nudgeTarget, now time.Time) ([]queuedNudge, []queuedNudge, []queuedNudge, error) {
	state, err := openNudgeQueue(cityPath, beads.NudgesStore{}).Queue.Snapshot()
	if err != nil {
		return nil, nil, nil, err
	}
	var pending, inFlight, dead []queuedNudge
	add := func(bucket *[]queuedNudge, item queuedNudge) {
		if target.matchesQueueAgent(item.Agent) {
			*bucket = append(*bucket, item)
		}
	}
	expired := func(item queuedNudge) bool {
		return !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now)
	}
	// Mirrors recoverExpiredInFlightNudges: expired in-flight items read as
	// dead, and lease-expired ones read as pending again.
	for _, item := range state.InFlight {
		switch {
		case expired(item):
			if item.LastError == "" {
				item.LastError = "expired"
			}
			add(&dead, item)
		case item.LeaseUntil.IsZero() || !item.LeaseUntil.After(now):
			add(&pending, item)
		default:
			add(&inFlight, item)
		}
	}
	// Mirrors pruneExpiredQueuedNudges: expired pending items read as dead.
	for _, item := range state.Pending {
		if expired(item) {
			if item.LastError == "" {
				item.LastError = "expired"
			}
			add(&dead, item)
			continue
		}
		add(&pending, item)
	}
	for _, item := range state.Dead {
		add(&dead, item)
	}
	return pending, inFlight, dead, nil
}

func listQueuedNudgesForTarget(cityPath string, target nudgeTarget, now time.Time) ([]queuedNudge, []queuedNudge, []queuedNudge, error) {
	return openNudgeQueue(cityPath, beads.NudgesStore{}).Queue.ListFor(target.claimTarget(cityPath), now)
}

func enqueueQueuedNudge(cityPath string, item queuedNudge) error {
	return enqueueQueuedNudgeWithStore(cityPath, beads.NudgesStore{}, item)
}

func enqueueQueuedNudgeWithStore(cityPath string, store beads.NudgesStore, item queuedNudge) error {
	if err := openNudgeQueue(cityPath, store).Queue.Enqueue(item); err != nil {
		return err
	}
	// Best-effort wake of the supervisor's nudge dispatcher, only once the item
	// is durable. Legacy-mode cities and ad-hoc invocations (no listener) get a
	// fast dial failure and fall through to the per-session poller / patrol
	// tick.
	pingNudgeWakeSocket(cityPath)
	return nil
}

func ackQueuedNudges(cityPath string, ids []string) error {
	return ackQueuedNudgesWithOutcome(cityPath, ids, "injected", "", "provider-nudge-return")
}

// ackQueuedNudgesWithOutcome retires ids with outcome, retrying a busy queue
// lock (retryNudgeBookkeeping): the caller delivered them under its claim.
func ackQueuedNudgesWithOutcome(cityPath string, ids []string, outcome, reason, commitBoundary string) error {
	return retryNudgeBookkeeping(func() error {
		return openNudgeQueue(cityPath, beads.NudgesStore{}).Queue.Ack(ids, outcome, reason, commitBoundary)
	})
}

// queuedNudgeReleaseAfter reports whether a delivery error leaves the nudges
// undelivered rather than failed, so their claims are released without
// spending one of their bounded attempts, and when a later pass may claim them
// again. A vanished session and a seat holding a pending interaction are
// refusals, not delivery failures: nothing was typed, and charging them would
// dead-letter a good nudge for arriving at the wrong moment. A session that
// went away may be back at once. The ACP provider refuses a nudge while it
// holds a permission request, and the next pass tries again. A permission
// prompt the runtime refused to type into (#2892) stays up until someone
// answers it, so claiming the nudges again on the very next pass would only
// be refused again. A session still running a turn the runtime would not
// interrupt (runtime.ErrSessionBusy) is deferred the same way: nothing was
// typed, and the turn needs time to end.
func queuedNudgeReleaseAfter(err error, now time.Time) (time.Time, bool) {
	switch {
	case errors.Is(err, runtime.ErrSessionBusy):
		return now.Add(defaultQueuedNudgeRetryDelay), true
	// Before ErrPendingInteraction: the session layer wraps this refusal in
	// session.ErrPendingInteraction, which is that sentinel.
	case errors.Is(err, runtime.ErrNudgeRefusedPendingInteraction):
		return time.Time{}, true
	case errors.Is(err, runtime.ErrPendingInteraction):
		return now.Add(defaultQueuedNudgeRetryDelay), true
	// The runtime could not be observed, so nothing was delivered: back off
	// without spending an attempt, so an outage cannot dead-letter the queue.
	case errors.Is(err, runtime.ErrRuntimeUnavailable):
		return now.Add(defaultQueuedNudgeRetryDelay), true
	case errors.Is(err, runtime.ErrSessionNotFound):
		return time.Time{}, true
	default:
		return time.Time{}, false
	}
}

func releaseQueuedNudgeClaims(cityPath string, ids []string) error {
	return releaseQueuedNudgeClaimsUntil(cityPath, ids, time.Time{})
}

// releaseQueuedNudgeClaimsUntil returns claimed nudges to pending. A non-zero
// deliverAfter keeps them from being claimed again before then. A busy queue
// lock is retried (retryNudgeBookkeeping).
func releaseQueuedNudgeClaimsUntil(cityPath string, ids []string, deliverAfter time.Time) error {
	return retryNudgeBookkeeping(func() error {
		return openNudgeQueue(cityPath, beads.NudgesStore{}).Queue.ReleaseClaims(ids, deliverAfter)
	})
}

func recordQueuedNudgeFailure(cityPath string, ids []string, cause error, now time.Time) error {
	return recordQueuedNudgeFailureWithStore(cityPath, beads.NudgesStore{}, ids, cause, now)
}

func recordQueuedNudgeFailureWithStore(cityPath string, store beads.NudgesStore, ids []string, cause error, now time.Time) error {
	_, err := recordQueuedNudgeFailureDetailed(cityPath, store, ids, cause, now)
	return err
}

// recordQueuedNudgeFailureDetailed charges one failed delivery attempt to ids
// and returns the ones it dead-lettered, which is not the set the caller asked
// for: an item acked since the caller last read the queue is gone by the time
// the write takes the lock. doNudgeDrop reports per-ID outcomes from it for
// exactly that reason. A busy queue lock is retried (retryNudgeBookkeeping).
func recordQueuedNudgeFailureDetailed(cityPath string, store beads.NudgesStore, ids []string, cause error, now time.Time) ([]queuedNudge, error) {
	var deadLettered []queuedNudge
	err := retryNudgeBookkeeping(func() error {
		var err error
		deadLettered, err = openNudgeQueue(cityPath, store).Queue.RecordFailure(ids, cause, now)
		return err
	})
	return deadLettered, err
}

// rollbackQueuedNudge dead-letters item after the action that queued it (a
// managed wake) failed, and closes its shadow in store as failed with reason.
func rollbackQueuedNudge(cityPath string, store beads.NudgesStore, item queuedNudge, reason string) error {
	return openNudgeQueue(cityPath, store).Queue.Rollback(item, reason)
}

func nudgePollerPIDPath(cityPath, sessionName, agentName string) string {
	return citylayout.RuntimePath(cityPath, "nudges", "pollers", nudgepoller.PollerFileStem(sessionName, agentName)+".pid")
}

// reapStaleNudgePollers removes orphaned nudge poller PID files left behind by
// pollers that were killed, crashed, or os.Exit'd without running their release
// closure. A *.pid file is stale when its contents are unparseable or its PID
// is no longer alive. The sibling .pid.lock file is deliberately NOT removed —
// it is the stable per-key flock mutex inode and removing it under the lock
// would race concurrent acquirers (see reapStaleNudgePoller). The work is done
// under the same per-file lock the lease path uses so a concurrently starting
// poller is never raced. It is best-effort: a missing pollers directory is a
// no-op and per-file errors are accumulated and returned without aborting the
// sweep.
func reapStaleNudgePollers(cityPath string) error {
	pollersDir := citylayout.RuntimePath(cityPath, "nudges", "pollers")
	entries, err := os.ReadDir(pollersDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading nudge pollers dir: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pid") {
			continue
		}
		pidPath := filepath.Join(pollersDir, entry.Name())
		if err := reapStaleNudgePoller(pidPath); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reapStaleNudgePoller removes pidPath when the PID it names is unparseable or
// no longer alive. It takes the per-file lock so the liveness check and removal
// cannot race a poller acquiring the same lease, mirroring exactly what the
// lease release closure does (which is safe).
func reapStaleNudgePoller(pidPath string) error {
	return withNudgePollerPIDLock(pidPath, func() error {
		data, err := os.ReadFile(pidPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read nudge poller pid %q: %w", pidPath, err)
		}
		pidText := strings.TrimSpace(string(data))
		var pid int
		if n, parseErr := fmt.Sscanf(pidText, "%d", &pid); parseErr == nil && n == 1 && pidutil.Alive(pid) {
			return nil
		}
		// Remove ONLY the stale .pid. The sibling .pid.lock is intentionally
		// left in place: it is the stable per-key flock mutex inode. Calling
		// os.Remove on it while we hold LOCK_EX would let a third concurrent
		// acquirer create a brand-new lock inode and run its critical section
		// alongside a blocked acquirer that inherited the now-orphaned inode,
		// breaking mutual exclusion and double-spawning pollers — exactly what
		// the lease exists to prevent. Leave the lock inode permanently stable.
		if err := os.Remove(pidPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale nudge poller pid %q: %w", pidPath, err)
		}
		return nil
	})
}

var errNudgePollerRunning = errors.New("nudge poller already running")

func acquireNudgePollerLease(cityPath, sessionName, agentName string) (func(), error) {
	pidPath := nudgePollerPIDPath(cityPath, sessionName, agentName)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		return nil, fmt.Errorf("creating nudge poller dir: %w", err)
	}
	pid := []byte(fmt.Sprintf("%d\n", os.Getpid()))
	release := func() {
		current, err := os.ReadFile(pidPath)
		if err != nil {
			return
		}
		if strings.TrimSpace(string(current)) == strings.TrimSpace(string(pid)) {
			_ = os.Remove(pidPath)
		}
	}
	err := withNudgePollerPIDLock(pidPath, func() error {
		current, err := os.ReadFile(pidPath)
		switch {
		case err == nil && strings.TrimSpace(string(current)) == strings.TrimSpace(string(pid)):
			return nil
		case err == nil:
			if running, _ := existingPollerPID(pidPath, cityPath, sessionName, agentName); running {
				return errNudgePollerRunning
			}
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("read nudge poller pid: %w", err)
		}
		return writeNudgePollerPID(pidPath, os.Getpid())
	})
	if err != nil {
		return nil, err
	}
	return release, nil
}

func existingPollerPID(pidPath, cityPath, sessionName, agentName string) (bool, error) {
	data, err := os.ReadFile(pidPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	pidText := strings.TrimSpace(string(data))
	if pidText == "" {
		return false, nil
	}
	var pid int
	if _, err := fmt.Sscanf(pidText, "%d", &pid); err != nil || pid <= 0 {
		return false, nil
	}
	if cityPath == "" || sessionName == "" || agentName == "" {
		return false, nil
	}
	if pidutil.AliveWithCmdline(pid, nudgepoller.CmdlineMatcher(cityPath, sessionName, agentName)) {
		return true, nil
	}
	return false, nil
}

func writeNudgePollerPID(pidPath string, pid int) error {
	data := []byte(fmt.Sprintf("%d\n", pid))
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, pidPath, data, 0o644); err != nil {
		return fmt.Errorf("write nudge poller pid: %w", err)
	}
	return nil
}

func withNudgePollerPIDLock(pidPath string, fn func() error) error {
	lockPath := pidPath + ".lock"
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		return fmt.Errorf("creating nudge poller dir: %w", err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening nudge poller lock: %w", err)
	}
	defer lockFile.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking nudge poller: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}
