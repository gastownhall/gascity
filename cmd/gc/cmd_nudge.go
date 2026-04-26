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
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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
	defaultNudgePollInterval        = 2 * time.Second
	defaultNudgePollQuiescence      = 3 * time.Second
	defaultNudgePollStartGrace      = 15 * time.Second
	defaultNudgeWaitIdleTimeout     = 30 * time.Second
)

var errNudgeSessionFenceMismatch = errors.New("queued nudge session fence mismatch")

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

func (t nudgeTarget) queueKeys() []string {
	var keys []string
	seen := map[string]bool{}
	for _, key := range []string{t.alias, t.sessionID, t.agent.QualifiedName(), t.identity, t.sessionName} {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	for _, key := range t.aliasHistory {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
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
	)
	return cmd
}

func newNudgeStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "status [session]",
		Short: "Show queued and dead-letter nudges for a session",
		Long: `Show queued and dead-letter nudges for a session.

Defaults to $GC_ALIAS or $GC_SESSION_ID when run inside a session.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdNudgeStatus(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
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
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdNudgePoll(args, sessionName, interval, quiescence, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sessionName, "session", "", "runtime session name (defaults to $GC_SESSION_NAME)")
	cmd.Flags().DurationVar(&interval, "interval", defaultNudgePollInterval, "poll interval")
	cmd.Flags().DurationVar(&quiescence, "quiescence", defaultNudgePollQuiescence, "minimum inactivity before injecting")
	return cmd
}

func cmdNudgeStatus(args []string, stdout, stderr io.Writer) int {
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
	return 0
}

func cmdNudgeDrainWithFormat(args []string, inject bool, hookFormat string, stdout, stderr io.Writer) int {
	targetID := os.Getenv("GC_ALIAS")
	if targetID == "" {
		targetID = os.Getenv("GC_SESSION_ID")
	}
	if len(args) > 0 {
		targetID = args[0]
	}
	if targetID == "" {
		if inject {
			return 0
		}
		fmt.Fprintln(stderr, "gc nudge drain: session not specified (set $GC_ALIAS/$GC_SESSION_ID or pass an alias/id)") //nolint:errcheck
		return 1
	}

	target, err := resolveNudgeTarget(targetID, stderr)
	if err != nil {
		if inject {
			return 0
		}
		fmt.Fprintf(stderr, "gc nudge drain: %v\n", err) //nolint:errcheck
		return 1
	}

	now := time.Now()
	items, err := claimDueQueuedNudgesForTarget(target.cityPath, target, now)
	if err != nil {
		if inject {
			return 0
		}
		fmt.Fprintf(stderr, "gc nudge drain: %v\n", err) //nolint:errcheck
		return 1
	}
	if len(items) == 0 {
		if inject {
			return 0
		}
		return 1
	}
	items, rejected := splitQueuedNudgesForTarget(target, items)
	if len(rejected) > 0 {
		_ = recordQueuedNudgeFailure(target.cityPath, queuedNudgeIDs(rejected), errNudgeSessionFenceMismatch, time.Now())
	}
	items, blocked, err := splitQueuedNudgesForDelivery(openNudgeBeadStore(target.cityPath), items)
	if err != nil {
		if inject {
			fmt.Fprintf(stderr, "gc nudge drain: validating claimed nudges: %v\n", err) //nolint:errcheck
			return 0
		}
		fmt.Fprintf(stderr, "gc nudge drain: validating claimed nudges: %v\n", err) //nolint:errcheck
		return 1
	}
	if len(blocked) > 0 {
		if err := terminalizeBlockedQueuedNudges(target.cityPath, blocked); err != nil {
			if inject {
				fmt.Fprintf(stderr, "gc nudge drain: withdrawing blocked nudges: %v\n", err) //nolint:errcheck
				return 0
			}
			fmt.Fprintf(stderr, "gc nudge drain: withdrawing blocked nudges: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	if len(items) == 0 {
		if inject {
			return 0
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
		writeErr = writeProviderHookContextForEvent(stdout, hookFormat, "UserPromptSubmit", out)
	} else {
		_, writeErr = io.WriteString(stdout, out)
	}
	if writeErr != nil {
		_ = recordQueuedNudgeFailure(target.cityPath, queuedNudgeIDs(items), writeErr, time.Now())
		if inject {
			return 0
		}
		fmt.Fprintf(stderr, "gc nudge drain: writing output: %v\n", writeErr) //nolint:errcheck
		return 1
	}
	if inject {
		if err := ackQueuedNudgesWithOutcome(target.cityPath, queuedNudgeIDs(items), "accepted_for_injection", "", "hook-transport-accepted"); err != nil {
			fmt.Fprintf(stderr, "gc nudge drain: recording injection ack: %v\n", err) //nolint:errcheck
			return 0
		}
		return 0
	}
	if err := ackQueuedNudges(target.cityPath, queuedNudgeIDs(items)); err != nil {
		fmt.Fprintf(stderr, "gc nudge drain: %v\n", err) //nolint:errcheck
		return 1
	}
	return 0
}

func queuedNudgeOptionsFromTarget(target nudgeTarget) queuedNudgeOptions {
	return queuedNudgeOptions{
		SessionID:         target.sessionID,
		ContinuationEpoch: target.continuationEpoch,
	}
}

func cmdNudgePoll(args []string, sessionName string, interval, quiescence time.Duration, _ io.Writer, stderr io.Writer) int {
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

	release, err := acquireNudgePollerLease(target.cityPath, target.sessionName)
	if err != nil {
		if errors.Is(err, errNudgePollerRunning) {
			return 0
		}
		fmt.Fprintf(stderr, "gc nudge poll: %v\n", err) //nolint:errcheck
		return 1
	}
	defer release()

	sp := newSessionProvider()
	store := openNudgeBeadStore(target.cityPath)
	if store == nil {
		fmt.Fprintf(stderr, "gc nudge poll: opening city store for %q\n", target.agentKey()) //nolint:errcheck
		return 1
	}
	var missingSince time.Time
	for {
		obs, err := workerObserveNudgeTarget(target, store, sp)
		if err != nil {
			fmt.Fprintf(stderr, "gc nudge poll: %v\n", err) //nolint:errcheck
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
		delivered, pollErr := tryDeliverQueuedNudgesByPoller(target, store, sp, quiescence)
		if pollErr != nil {
			fmt.Fprintf(stderr, "gc nudge poll: %v\n", pollErr) //nolint:errcheck
		}
		if delivered {
			continue
		}
		time.Sleep(interval)
	}
}

func shouldKeepNudgePollerAlive(target nudgeTarget, missingSince, now time.Time) bool {
	pending, inFlight, _, err := listQueuedNudgesForTarget(target.cityPath, target, now)
	if err != nil || (len(pending) == 0 && len(inFlight) == 0) {
		return false
	}
	if missingSince.IsZero() {
		return true
	}
	return now.Sub(missingSince) < defaultNudgePollStartGrace
}

func deliverSessionNudge(target nudgeTarget, message string, mode nudgeDeliveryMode, stdout, stderr io.Writer) int {
	store := openNudgeBeadStore(target.cityPath)
	if store == nil {
		fmt.Fprintf(stderr, "gc session nudge: opening city store for %q\n", target.agentKey()) //nolint:errcheck
		return 1
	}
	return deliverSessionNudgeWithWorker(target, store, newSessionProvider(), message, mode, stdout, stderr)
}

func deliverSessionNudgeWithWorker(target nudgeTarget, store beads.Store, sp runtime.Provider, message string, mode nudgeDeliveryMode, stdout, stderr io.Writer) int {
	if mode == nudgeDeliveryQueue {
		return queueSessionNudgeWithWorker(target, store, sp, message, stdout, stderr)
	}
	delivery, ok := workerNudgeDeliveryForMode(mode)
	if !ok {
		fmt.Fprintf(stderr, "gc session nudge: unknown delivery mode %q\n", mode) //nolint:errcheck
		return 1
	}
	handle, err := workerHandleForNudgeTarget(target, store, sp)
	if err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck
		return 1
	}
	result, err := handle.Nudge(context.Background(), worker.NudgeRequest{
		Text:     message,
		Delivery: delivery,
		Source:   "session",
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck
		return 1
	}
	if mode == nudgeDeliveryWaitIdle && !result.Delivered {
		return queueSessionNudgeWithWorker(target, store, sp, message, stdout, stderr)
	}
	fmt.Fprintf(stdout, "Nudged %s\n", target.agentKey()) //nolint:errcheck
	return 0
}

func workerHandleForNudgeTarget(target nudgeTarget, store beads.Store, sp runtime.Provider) (worker.Handle, error) {
	if target.sessionID != "" {
		return workerHandleForSessionWithConfig(target.cityPath, store, sp, target.cfg, target.sessionID)
	}
	return runtimeWorkerHandleWithConfig(
		target.cityPath,
		store,
		sp,
		target.cfg,
		target.sessionName,
		strings.TrimSpace(target.providerName()),
		strings.TrimSpace(target.sessionTransport()),
		nil,
	)
}

func workerObserveNudgeTarget(target nudgeTarget, store beads.Store, sp runtime.Provider) (worker.LiveObservation, error) {
	if target.sessionID != "" {
		return workerObserveSessionTargetWithConfig(target.cityPath, store, sp, target.cfg, target.sessionID)
	}
	return workerObserveSessionTargetWithConfig(target.cityPath, store, sp, target.cfg, target.sessionName)
}

func deliverSessionNudgeWithProvider(target nudgeTarget, sp runtime.Provider, mode nudgeDeliveryMode, stdout, stderr io.Writer) int {
	return deliverSessionNudgeWithWorker(target, nil, sp, "check deploy status", mode, stdout, stderr)
}

func queueSessionNudgeWithWorker(target nudgeTarget, store beads.Store, sp runtime.Provider, message string, stdout, stderr io.Writer) int {
	if err := enqueueQueuedNudge(target.cityPath, newQueuedNudgeWithOptions(target.agentKey(), message, "session", time.Now(), queuedNudgeOptionsFromTarget(target))); err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck
		return 1
	}
	if obs, err := workerObserveNudgeTarget(target, store, sp); err == nil && obs.Running {
		maybeStartNudgePoller(target)
	}
	fmt.Fprintf(stdout, "Queued nudge for %s\n", target.agentKey()) //nolint:errcheck
	return 0
}

func sendMailNotify(target nudgeTarget, sender string) error {
	store := openNudgeBeadStore(target.cityPath)
	if store == nil {
		return fmt.Errorf("opening city store for %q", target.agentKey())
	}
	return sendMailNotifyWithWorker(target, store, newSessionProvider(), sender)
}

func sendMailNotifyWithProvider(target nudgeTarget, sp runtime.Provider) error {
	return sendMailNotifyWithWorker(target, nil, sp, "human")
}

func sendMailNotifyWithWorker(target nudgeTarget, store beads.Store, sp runtime.Provider, sender string) error {
	msg := fmt.Sprintf("You have mail from %s", sender)
	now := time.Now()
	obs, err := workerObserveNudgeTarget(target, store, sp)
	if err != nil {
		return err
	}
	if obs.Running {
		handle, err := workerHandleForNudgeTarget(target, store, sp)
		if err != nil {
			return err
		}
		result, err := handle.Nudge(context.Background(), worker.NudgeRequest{
			Text:     msg,
			Delivery: worker.NudgeDeliveryWaitIdle,
			Source:   "mail",
			Wake:     worker.NudgeWakeLiveOnly,
		})
		if err != nil {
			return err
		}
		if result.Delivered {
			telemetry.RecordNudge(context.Background(), target.agentKey(), nil)
			return nil
		}
	}
	if err := enqueueQueuedNudge(target.cityPath, newQueuedNudgeWithOptions(target.agentKey(), msg, "mail", now, queuedNudgeOptionsFromTarget(target))); err != nil {
		return err
	}
	if obs.Running {
		maybeStartNudgePoller(target)
	}
	return nil
}

func resolveNudgeTarget(identifier string, warningWriter ...io.Writer) (nudgeTarget, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return nudgeTarget{}, err
	}
	cfg, err := loadCityConfig(cityPath, warningWriter...)
	if err != nil {
		return nudgeTarget{}, err
	}
	store := openNudgeBeadStore(cityPath)
	if store != nil {
		sessionID, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, identifier)
		if err == nil {
			b, getErr := store.Get(sessionID)
			if getErr != nil {
				return nudgeTarget{}, getErr
			}
			return resolveNudgeTargetFromSessionBead(cityPath, cfg, b), nil
		}
		if !errors.Is(err, session.ErrSessionNotFound) {
			return nudgeTarget{}, err
		}
	}
	return nudgeTarget{}, fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
}

func resolveNudgeTargetFromSessionBead(cityPath string, cfg *config.City, b beads.Bead) nudgeTarget {
	cityName := loadedCityName(cfg, cityPath)
	sessionName := strings.TrimSpace(b.Metadata["session_name"])
	if sessionName == "" {
		sessionName = sessionNameFromBeadID(b.ID)
	}
	alias := strings.TrimSpace(b.Metadata["alias"])
	identity := firstNonEmpty(
		strings.TrimSpace(b.Metadata["agent_name"]),
		strings.TrimSpace(b.Metadata["template"]),
		strings.TrimSpace(b.Metadata["common_name"]),
	)
	target := nudgeTarget{
		cityPath:          cityPath,
		cityName:          cityName,
		cfg:               cfg,
		identity:          identity,
		alias:             alias,
		aliasHistory:      session.AliasHistory(b.Metadata),
		transport:         strings.TrimSpace(b.Metadata["transport"]),
		resolved:          &config.ResolvedProvider{Name: strings.TrimSpace(b.Metadata["provider"])},
		sessionID:         b.ID,
		continuationEpoch: strings.TrimSpace(b.Metadata["continuation_epoch"]),
		sessionName:       sessionName,
	}
	target.agent = parseNudgeAgentIdentity(identity)
	for _, candidate := range []string{
		strings.TrimSpace(b.Metadata["agent_name"]),
		strings.TrimSpace(b.Metadata["template"]),
		strings.TrimSpace(b.Metadata["common_name"]),
	} {
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
		target.identity = sessionName
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

func tryDeliverQueuedNudgesByPoller(target nudgeTarget, store beads.Store, sp runtime.Provider, quiescence time.Duration) (bool, error) {
	if !pollerSessionIdleEnough(target, store, sp, quiescence) {
		return false, nil
	}
	items, err := claimDueQueuedNudgesForTarget(target.cityPath, target, time.Now())
	if err != nil || len(items) == 0 {
		return false, err
	}
	items, rejected := splitQueuedNudgesForTarget(target, items)
	if len(rejected) > 0 {
		if recErr := recordQueuedNudgeFailure(target.cityPath, queuedNudgeIDs(rejected), errNudgeSessionFenceMismatch, time.Now()); recErr != nil {
			return false, recErr
		}
	}
	deliveryStore := store
	if deliveryStore == nil {
		deliveryStore = openNudgeBeadStore(target.cityPath)
	}
	items, blocked, err := splitQueuedNudgesForDelivery(deliveryStore, items)
	if err != nil {
		return false, err
	}
	if len(blocked) > 0 {
		if err := terminalizeBlockedQueuedNudges(target.cityPath, blocked); err != nil {
			return false, err
		}
	}
	if len(items) == 0 {
		return false, nil
	}
	var msg string
	if target.sessionTransport() == "acp" {
		msg = formatNudgeRuntimeMessage(items)
	} else {
		msg = formatNudgeInjectOutput(items)
	}
	handle, err := workerHandleForNudgeTarget(target, store, sp)
	if err != nil {
		return false, err
	}
	result, err := handle.Nudge(context.Background(), worker.NudgeRequest{
		Text:     msg,
		Delivery: worker.NudgeDeliveryDefault,
		Source:   "queue",
		Wake:     worker.NudgeWakeLiveOnly,
	})
	if err != nil {
		telemetry.RecordNudge(context.Background(), target.agentKey(), err)
		if recErr := recordQueuedNudgeFailure(target.cityPath, queuedNudgeIDs(items), err, time.Now()); recErr != nil {
			return false, recErr
		}
		return false, nil
	}
	if !result.Delivered {
		return false, nil
	}
	telemetry.RecordNudge(context.Background(), target.agentKey(), nil)
	return true, ackQueuedNudges(target.cityPath, queuedNudgeIDs(items))
}

func pollerSessionIdleEnough(target nudgeTarget, store beads.Store, sp runtime.Provider, quiescence time.Duration) bool {
	obs, err := workerObserveNudgeTarget(target, store, sp)
	if err != nil || obs.LastActivity == nil || obs.LastActivity.IsZero() {
		return false
	}
	return time.Since(*obs.LastActivity) >= quiescence
}

func maybeStartNudgePoller(target nudgeTarget) {
	if target.sessionName == "" {
		return
	}
	if target.sessionTransport() == "acp" {
		return
	}
	if err := startNudgePoller(target.cityPath, target.agentKey(), target.sessionName); err != nil {
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
	open, err := loadSessionBeads(store)
	if err != nil {
		return target
	}
	for _, b := range open {
		if b.Metadata["session_name"] != target.sessionName {
			continue
		}
		if target.sessionID == "" {
			target.sessionID = b.ID
		}
		if target.continuationEpoch == "" {
			target.continuationEpoch = b.Metadata["continuation_epoch"]
		}
		return target
	}
	return target
}

var startNudgePoller = ensureNudgePoller

func splitQueuedNudgesForTarget(target nudgeTarget, items []queuedNudge) ([]queuedNudge, []queuedNudge) {
	if len(items) == 0 {
		return nil, nil
	}
	var deliverable []queuedNudge
	var rejected []queuedNudge
	for _, item := range items {
		if !queuedNudgeMatchesTargetFence(target, item) {
			rejected = append(rejected, item)
			continue
		}
		deliverable = append(deliverable, item)
	}
	return deliverable, rejected
}

func splitQueuedNudgesForDelivery(store beads.Store, items []queuedNudge) ([]queuedNudge, map[string][]queuedNudge, error) {
	if len(items) == 0 {
		return nil, nil, nil
	}
	deliverable := make([]queuedNudge, 0, len(items))
	blocked := make(map[string][]queuedNudge)
	for _, item := range items {
		reason, shouldBlock, err := blockedQueuedNudgeReason(store, item)
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

func blockedQueuedNudgeReason(store beads.Store, item queuedNudge) (string, bool, error) {
	if store == nil || item.Source != "wait" || item.Reference == nil || item.Reference.Kind != "bead" || item.Reference.ID == "" {
		return "", false, nil
	}
	wait, err := store.Get(item.Reference.ID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return "wait-missing", true, nil
		}
		return "", false, err
	}
	if !session.IsWaitBead(wait) {
		return "wait-reference-invalid", true, nil
	}
	switch wait.Metadata["state"] {
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

func terminalizeBlockedQueuedNudges(cityPath string, blocked map[string][]queuedNudge) error {
	for reason, items := range blocked {
		if err := ackQueuedNudgesWithOutcome(cityPath, queuedNudgeIDs(items), "failed", reason, "delivery-withdrawn"); err != nil {
			return err
		}
	}
	return nil
}

func ensureNudgePoller(cityPath, agentName, sessionName string) error {
	pidPath := nudgePollerPIDPath(cityPath, sessionName)
	return withNudgePollerPIDLock(pidPath, func() error {
		if running, _ := existingPollerPID(pidPath); running {
			return nil
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.Command(exe, "nudge", "poll", "--city", cityPath, "--session", sessionName, agentName)
		cmd.Env = os.Environ()
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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

func formatNudgeInjectOutput(items []queuedNudge) string {
	var sb strings.Builder
	sb.WriteString("<system-reminder>\n")
	if len(items) == 1 {
		sb.WriteString("You have a deferred reminder that was queued until a safe boundary:\n\n")
	} else {
		fmt.Fprintf(&sb, "You have %d deferred reminders that were queued until a safe boundary:\n\n", len(items))
	}
	for _, item := range items {
		fmt.Fprintf(&sb, "- [%s] %s\n", item.Source, item.Message)
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

func newQueuedNudge(agentName, message, source string, now time.Time) queuedNudge {
	return newQueuedNudgeWithOptions(agentName, message, source, now, queuedNudgeOptions{})
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

func queuedNudgeClaimableForTarget(target nudgeTarget, item queuedNudge) bool {
	if !target.matchesQueueAgent(item.Agent) {
		return false
	}
	if item.SessionID != "" {
		if target.sessionID == "" {
			return false
		}
		return item.SessionID == target.sessionID
	}
	if item.ContinuationEpoch != "" && target.continuationEpoch == "" {
		return false
	}
	return true
}

func claimDueQueuedNudges(cityPath, agentName string, now time.Time) ([]queuedNudge, error) {
	return claimDueQueuedNudgesMatching(cityPath, now, func(item queuedNudge) bool {
		return item.Agent == agentName
	})
}

func claimDueQueuedNudgesForTarget(cityPath string, target nudgeTarget, now time.Time) ([]queuedNudge, error) {
	return claimDueQueuedNudgesMatching(cityPath, now, func(item queuedNudge) bool {
		return queuedNudgeClaimableForTarget(target, item)
	})
}

func claimDueQueuedNudgesMatching(cityPath string, now time.Time, match func(queuedNudge) bool) ([]queuedNudge, error) {
	store := openNudgeBeadStore(cityPath)
	var claimed []queuedNudge
	err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		if err := recoverExpiredInFlightNudges(state, store, now); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudges(state, store, now); err != nil {
			return err
		}
		if err := pruneDeadQueuedNudges(state, store, now); err != nil {
			return err
		}
		pending := state.Pending[:0]
		for _, item := range state.Pending {
			if !match(item) {
				pending = append(pending, item)
				continue
			}
			if !item.DeliverAfter.IsZero() && item.DeliverAfter.After(now) {
				pending = append(pending, item)
				continue
			}
			item.ClaimedAt = now.UTC()
			item.LeaseUntil = now.Add(defaultQueuedNudgeClaimTTL).UTC()
			state.InFlight = append(state.InFlight, item)
			claimed = append(claimed, item)
		}
		state.Pending = pending
		sortQueuedNudges(state)
		return nil
	})
	return claimed, err
}

func listQueuedNudges(cityPath, agentName string, now time.Time) ([]queuedNudge, []queuedNudge, []queuedNudge, error) {
	store := openNudgeBeadStore(cityPath)
	var pending []queuedNudge
	var inFlight []queuedNudge
	var dead []queuedNudge
	err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		if err := recoverExpiredInFlightNudges(state, store, now); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudges(state, store, now); err != nil {
			return err
		}
		if err := pruneDeadQueuedNudges(state, store, now); err != nil {
			return err
		}
		for _, item := range state.Pending {
			if item.Agent == agentName {
				pending = append(pending, item)
			}
		}
		for _, item := range state.InFlight {
			if item.Agent == agentName {
				inFlight = append(inFlight, item)
			}
		}
		for _, item := range state.Dead {
			if item.Agent == agentName {
				dead = append(dead, item)
			}
		}
		return nil
	})
	return pending, inFlight, dead, err
}

func listQueuedNudgesForTarget(cityPath string, target nudgeTarget, now time.Time) ([]queuedNudge, []queuedNudge, []queuedNudge, error) {
	store := openNudgeBeadStore(cityPath)
	var pending []queuedNudge
	var inFlight []queuedNudge
	var dead []queuedNudge
	err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		if err := recoverExpiredInFlightNudges(state, store, now); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudges(state, store, now); err != nil {
			return err
		}
		if err := pruneDeadQueuedNudges(state, store, now); err != nil {
			return err
		}
		for _, item := range state.Pending {
			if target.matchesQueueAgent(item.Agent) {
				pending = append(pending, item)
			}
		}
		for _, item := range state.InFlight {
			if target.matchesQueueAgent(item.Agent) {
				inFlight = append(inFlight, item)
			}
		}
		for _, item := range state.Dead {
			if target.matchesQueueAgent(item.Agent) {
				dead = append(dead, item)
			}
		}
		return nil
	})
	return pending, inFlight, dead, err
}

func enqueueQueuedNudge(cityPath string, item queuedNudge) error {
	return enqueueQueuedNudgeWithStore(cityPath, nil, item)
}

func enqueueQueuedNudgeWithStore(cityPath string, store beads.Store, item queuedNudge) error {
	if store == nil {
		store = openNudgeBeadStore(cityPath)
	}
	beadID, created, err := ensureQueuedNudgeBead(store, item)
	if err != nil {
		return err
	}
	if beadID != "" {
		item.BeadID = beadID
	}
	err = withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		now := time.Now()
		if err := recoverExpiredInFlightNudges(state, store, now); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudges(state, store, now); err != nil {
			return err
		}
		if err := pruneDeadQueuedNudges(state, store, now); err != nil {
			return err
		}
		if queuedNudgeExists(state, item.ID) {
			return nil
		}
		// Supersede pending and in-flight nudges for the same (agent, source, reference).
		if item.Reference != nil && item.Reference.ID != "" {
			matchesSupersession := func(existing queuedNudge) bool {
				return existing.Agent == item.Agent && existing.Source == item.Source &&
					existing.Reference != nil && existing.Reference.Kind == item.Reference.Kind &&
					existing.Reference.ID == item.Reference.ID
			}
			filtered := state.Pending[:0]
			for _, existing := range state.Pending {
				if matchesSupersession(existing) {
					existing.DeadAt = now.UTC()
					existing.LastError = "superseded"
					state.Dead = append(state.Dead, existing)
					if err := markQueuedNudgeTerminal(store, existing, "superseded", "superseded", "", now); err != nil {
						return err
					}
					continue
				}
				filtered = append(filtered, existing)
			}
			state.Pending = filtered
			// Also supersede in-flight nudges. Note: an active delivery may
			// already be running for a superseded item. When it completes, its
			// ack/failure won't find the item in InFlight and will no-op.
			// This causes at most one redundant delivery, not data corruption.
			inFlight := state.InFlight[:0]
			for _, existing := range state.InFlight {
				if matchesSupersession(existing) {
					existing.DeadAt = now.UTC()
					existing.LastError = "superseded"
					state.Dead = append(state.Dead, existing)
					if err := markQueuedNudgeTerminal(store, existing, "superseded", "superseded", "", now); err != nil {
						return err
					}
					continue
				}
				inFlight = append(inFlight, existing)
			}
			state.InFlight = inFlight
		}
		state.Pending = append(state.Pending, item)
		sortQueuedNudges(state)
		return nil
	})
	if err != nil && created && store != nil && beadID != "" {
		_ = store.Close(beadID)
	}
	return err
}

func ackQueuedNudges(cityPath string, ids []string) error {
	return ackQueuedNudgesWithOutcome(cityPath, ids, "injected", "", "provider-nudge-return")
}

func ackQueuedNudgesWithOutcome(cityPath string, ids []string, outcome, reason, commitBoundary string) error {
	if len(ids) == 0 {
		return nil
	}
	store := openNudgeBeadStore(cityPath)
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	return withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		now := time.Now()
		if err := recoverExpiredInFlightNudges(state, store, now); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudges(state, store, now); err != nil {
			return err
		}
		if err := pruneDeadQueuedNudges(state, store, now); err != nil {
			return err
		}
		var terminal []queuedNudge
		filtered := state.Pending[:0]
		for _, item := range state.Pending {
			if want[item.ID] {
				terminal = append(terminal, item)
				continue
			}
			filtered = append(filtered, item)
		}
		state.Pending = filtered
		inFlight := state.InFlight[:0]
		for _, item := range state.InFlight {
			if want[item.ID] {
				terminal = append(terminal, item)
				continue
			}
			inFlight = append(inFlight, item)
		}
		state.InFlight = inFlight
		for _, item := range terminal {
			if err := markQueuedNudgeTerminal(store, item, outcome, reason, commitBoundary, now); err != nil {
				return err
			}
		}
		return nil
	})
}

func recordQueuedNudgeFailure(cityPath string, ids []string, cause error, now time.Time) error {
	_, err := recordQueuedNudgeFailureDetailed(cityPath, ids, cause, now)
	return err
}

func recordQueuedNudgeFailureDetailed(cityPath string, ids []string, cause error, now time.Time) ([]queuedNudge, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	store := openNudgeBeadStore(cityPath)
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var deadLettered []queuedNudge
	err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		if err := recoverExpiredInFlightNudges(state, store, now); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudges(state, store, now); err != nil {
			return err
		}
		if err := pruneDeadQueuedNudges(state, store, now); err != nil {
			return err
		}
		var requeued []queuedNudge
		var dead []queuedNudge
		pending := state.Pending[:0]
		for _, item := range state.Pending {
			if !want[item.ID] {
				pending = append(pending, item)
				continue
			}
			updated, deadLetter := failedQueuedNudge(item, cause, now)
			if deadLetter {
				dead = append(dead, updated)
				deadLettered = append(deadLettered, updated)
				continue
			}
			requeued = append(requeued, updated)
		}
		state.Pending = pending
		inFlight := state.InFlight[:0]
		for _, item := range state.InFlight {
			if !want[item.ID] {
				inFlight = append(inFlight, item)
				continue
			}
			updated, deadLetter := failedQueuedNudge(item, cause, now)
			if deadLetter {
				dead = append(dead, updated)
				deadLettered = append(deadLettered, updated)
				continue
			}
			requeued = append(requeued, updated)
		}
		state.InFlight = inFlight
		state.Pending = append(state.Pending, requeued...)
		state.Dead = append(state.Dead, dead...)
		for _, item := range dead {
			if err := markQueuedNudgeTerminal(store, item, "failed", item.LastError, "", now); err != nil {
				return err
			}
		}
		sortQueuedNudges(state)
		return nil
	})
	return deadLettered, err
}

func failedQueuedNudge(item queuedNudge, cause error, now time.Time) (queuedNudge, bool) {
	item.Attempts++
	item.LastAttemptAt = now.UTC()
	item.LastError = cause.Error()
	item.ClaimedAt = time.Time{}
	item.LeaseUntil = time.Time{}
	if errors.Is(cause, errNudgeSessionFenceMismatch) {
		item.DeadAt = now.UTC()
		return item, true
	}
	if item.Attempts >= defaultQueuedNudgeMaxAttempts || (!item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now)) {
		item.DeadAt = now.UTC()
		return item, true
	}
	item.DeliverAfter = now.Add(defaultQueuedNudgeRetryDelay).UTC()
	return item, false
}

func pruneExpiredQueuedNudges(state *nudgeQueueState, store beads.Store, now time.Time) error {
	filtered := state.Pending[:0]
	for _, item := range state.Pending {
		if !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now) {
			item.DeadAt = now.UTC()
			if item.LastError == "" {
				item.LastError = "expired"
			}
			state.Dead = append(state.Dead, item)
			if err := markQueuedNudgeTerminal(store, item, "expired", item.LastError, "", now); err != nil {
				return err
			}
			continue
		}
		filtered = append(filtered, item)
	}
	state.Pending = filtered
	sortQueuedNudges(state)
	return nil
}

func recoverExpiredInFlightNudges(state *nudgeQueueState, store beads.Store, now time.Time) error {
	filtered := state.InFlight[:0]
	for _, item := range state.InFlight {
		if !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now) {
			item.DeadAt = now.UTC()
			if item.LastError == "" {
				item.LastError = "expired"
			}
			state.Dead = append(state.Dead, item)
			if err := markQueuedNudgeTerminal(store, item, "expired", item.LastError, "", now); err != nil {
				return err
			}
			continue
		}
		if item.LeaseUntil.IsZero() || !item.LeaseUntil.After(now) {
			item.ClaimedAt = time.Time{}
			item.LeaseUntil = time.Time{}
			item.DeliverAfter = now.UTC()
			state.Pending = append(state.Pending, item)
			continue
		}
		filtered = append(filtered, item)
	}
	state.InFlight = filtered
	sortQueuedNudges(state)
	return nil
}

// pruneDeadQueuedNudges removes dead-letter items older than defaultQueuedNudgeDeadRetention
// when a durable terminal bead record exists in the store. Items without a confirmed terminal
// bead are retained so terminal history is not lost if the bead store write failed.
func pruneDeadQueuedNudges(state *nudgeQueueState, store beads.Store, now time.Time) error {
	cutoff := now.Add(-defaultQueuedNudgeDeadRetention)
	filtered := state.Dead[:0]
	for _, item := range state.Dead {
		if !item.DeadAt.IsZero() && item.DeadAt.Before(cutoff) && item.BeadID != "" {
			if store == nil {
				// No store available — retain the item to avoid data loss.
				filtered = append(filtered, item)
				continue
			}
			b, ok, err := findAnyQueuedNudgeBead(store, item.ID)
			if err != nil {
				// Fail open: store lookup errors retain the item rather than
				// blocking the entire queue operation. Pruning is best-effort.
				filtered = append(filtered, item)
				continue
			}
			if !ok || !isTerminalNudgeState(b.Metadata["state"]) {
				// Terminal bead not confirmed — retain the queue entry.
				filtered = append(filtered, item)
				continue
			}
			// Terminal bead confirmed in store — safe to prune.
			continue
		}
		filtered = append(filtered, item)
	}
	state.Dead = filtered
	return nil
}

func queuedNudgeExists(state *nudgeQueueState, id string) bool {
	for _, item := range state.Pending {
		if item.ID == id {
			return true
		}
	}
	for _, item := range state.InFlight {
		if item.ID == id {
			return true
		}
	}
	for _, item := range state.Dead {
		if item.ID == id {
			return true
		}
	}
	return false
}

func sortQueuedNudges(state *nudgeQueueState) {
	nudgequeue.SortState(state)
}

func withNudgeQueueState(cityPath string, fn func(*nudgeQueueState) error) error {
	return nudgequeue.WithState(cityPath, fn)
}

func nudgePollerPIDPath(cityPath, sessionName string) string {
	return citylayout.RuntimePath(cityPath, "nudges", "pollers", sessionName+".pid")
}

var errNudgePollerRunning = errors.New("nudge poller already running")

func acquireNudgePollerLease(cityPath, sessionName string) (func(), error) {
	pidPath := nudgePollerPIDPath(cityPath, sessionName)
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
			if running, _ := existingPollerPID(pidPath); running {
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

func existingPollerPID(pidPath string) (bool, error) {
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
	if err := syscall.Kill(pid, 0); err == nil || errors.Is(err, syscall.EPERM) {
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
