// Package session manages persistent, resumable chat sessions.
//
// A chat session is a conversation between a human and an agent template
// that can be started, suspended (freeing runtime resources), and resumed
// later. Sessions are backed by beads (type "session") for persistence
// and use runtime.Provider for runtime management.
package session

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// State represents the runtime state of a chat session.
type State string

const (
	// StateActive means the conversation has a live runtime session.
	StateActive State = "active"
	// StateAsleep means the session is dormant with no live runtime.
	StateAsleep State = "asleep"
	// StateSuspended means the conversation is paused with no runtime resources.
	StateSuspended State = "suspended"
	// StateCreating means the session bead has been written but the runtime
	// process has not yet been confirmed alive. Counts against pool occupancy.
	StateCreating State = "creating"
	// StateDraining means the session is being gracefully stopped (in-flight
	// work completing). The pool routing label has been removed so no new
	// work is routed to this session.
	StateDraining State = "draining"
	// StateDrained marks an acknowledged drain that should remain dormant
	// until an explicit compatible wake reason appears.
	StateDrained State = "drained"
	// StateAwake is equivalent to StateActive. Written by the reconciler's
	// healState when a session transitions from asleep to running.
	StateAwake State = "awake"
	// StateArchived means the session completed its drain and is retained
	// for history. Does NOT count against pool occupancy.
	StateArchived State = "archived"
	// StateQuarantined means the session hit the crash-loop threshold and
	// is temporarily blocked from waking. Counts against pool occupancy.
	StateQuarantined State = "quarantined"
)

// BeadType is the bead type for chat sessions.
const BeadType = "session"

// LabelSession is the label applied to all session beads for filtering.
const LabelSession = "gc:session"

// Info holds the user-facing details of a chat session.
type Info struct {
	ID            string
	Template      string
	State         State
	Closed        bool
	Title         string
	Alias         string
	Provider      string
	Command       string // resolved command stored at creation
	WorkDir       string
	SessionName   string // tmux session name
	SessionKey    string // provider-specific resume handle (UUID)
	ResumeFlag    string // stored provider resume flag (e.g., "--resume")
	ResumeStyle   string // "flag" or "subcommand"
	ResumeCommand string // explicit resume command template ({{.SessionKey}})
	CreatedAt     time.Time
	LastActive    time.Time
	Attached      bool
}

// RuntimeObservation reports the provider-backed live runtime state for a
// persisted session.
type RuntimeObservation struct {
	Running     bool
	Alive       bool
	Attached    bool
	LastActive  time.Time
	SessionName string
}

func normalizeInfoState(state State) State {
	switch state {
	case "awake":
		return StateActive
	case "drained":
		return StateAsleep
	}
	return state
}

// ProviderResume describes a provider's session resume capabilities.
// Populated from config.ResolvedProvider's resume fields.
type ProviderResume struct {
	// ResumeFlag is the CLI flag for resuming (e.g., "--resume").
	// Empty means the provider doesn't support resume.
	ResumeFlag string
	// ResumeStyle is "flag" (--resume <key>) or "subcommand" (command resume <key>).
	ResumeStyle string
	// ResumeCommand is the full shell command template for resuming.
	// Supports {{.SessionKey}}. When set, takes precedence over ResumeFlag/ResumeStyle.
	ResumeCommand string
	// SessionIDFlag is the CLI flag for creating with a specific ID (e.g., "--session-id").
	// Enables Generate & Pass strategy.
	SessionIDFlag string
}

// Manager orchestrates chat session lifecycle using beads for persistence
// and runtime.Provider for runtime.
type Manager struct {
	store             beads.Store
	sp                runtime.Provider
	cityPath          string
	transportResolver func(template string) string
}

// PruneResult reports which sessions were pruned and which queued wait nudges
// should be eagerly withdrawn afterward.
type PruneResult struct {
	Count        int
	SessionIDs   []string
	WaitNudgeIDs []string
}

type acpRouteRegistrar interface {
	RouteACP(name string)
	Unroute(name string)
}

type transportDetector interface {
	DetectTransport(name string) string
}

func normalizeTransport(provider, transport string) string {
	if transport != "" {
		return transport
	}
	if provider == "acp" {
		return "acp"
	}
	return ""
}

func transportFromMetadata(b beads.Bead) string {
	return normalizeTransport(b.Metadata["provider"], b.Metadata["transport"])
}

func (m *Manager) transportForBead(b beads.Bead, sessName string) (string, bool) {
	transport := transportFromMetadata(b)
	if transport != "" {
		return transport, false
	}
	if detector, ok := m.sp.(transportDetector); ok {
		transport = normalizeTransport(b.Metadata["provider"], detector.DetectTransport(sessName))
		if transport != "" {
			return transport, true
		}
	}
	return "", false
}

func (m *Manager) persistTransport(id, provider, transport string) {
	transport = normalizeTransport(provider, transport)
	if transport == "" {
		return
	}
	_ = m.store.SetMetadata(id, "transport", transport)
}

func (m *Manager) routeACPIfNeeded(provider, transport, sessName string) func() {
	if normalizeTransport(provider, transport) != "acp" {
		return nil
	}
	router, ok := m.sp.(acpRouteRegistrar)
	if !ok {
		return nil
	}
	router.RouteACP(sessName)
	return func() { router.Unroute(sessName) }
}

// NewManager creates a Manager backed by the given bead store and session provider.
func NewManager(store beads.Store, sp runtime.Provider) *Manager {
	return &Manager{store: store, sp: sp}
}

// NewManagerWithTransportResolver creates a Manager that can infer session
// transport from template config when older beads do not have transport metadata.
func NewManagerWithTransportResolver(store beads.Store, sp runtime.Provider, resolver func(template string) string) *Manager {
	return &Manager{store: store, sp: sp, transportResolver: resolver}
}

// NewManagerWithCityPath creates a Manager that can persist deferred submits
// into the city's nudge queue.
func NewManagerWithCityPath(store beads.Store, sp runtime.Provider, cityPath string) *Manager {
	return &Manager{store: store, sp: sp, cityPath: cityPath}
}

// NewManagerWithTransportResolverAndCityPath creates a Manager that can infer
// session transport from template config and persist deferred submits into the
// city's nudge queue.
func NewManagerWithTransportResolverAndCityPath(store beads.Store, sp runtime.Provider, cityPath string, resolver func(template string) string) *Manager {
	return &Manager{store: store, sp: sp, cityPath: cityPath, transportResolver: resolver}
}

// Create creates a new chat session bead and starts the runtime session.
// The command is the full provider command to execute (e.g., "claude --dangerously-skip-permissions").
// The resume parameter carries provider resume capabilities; if the provider
// supports SessionIDFlag, a UUID session key is generated and injected.
// The caller is responsible for attaching after Create returns.
func (m *Manager) Create(ctx context.Context, template, title, command, workDir, provider string, env map[string]string, resume ProviderResume, hints runtime.Config) (Info, error) {
	return m.CreateAliasedNamedWithTransportAndMetadata(ctx, "", "", template, title, command, workDir, provider, "", env, resume, hints, map[string]string{
		"session_origin": "manual",
	})
}

// CreateWithTransport creates a new chat session bead and starts the runtime
// session, preserving the transport override separately from the provider name
// so ACP-routed sessions can be resumed correctly.
func (m *Manager) CreateWithTransport(ctx context.Context, template, title, command, workDir, provider, transport string, env map[string]string, resume ProviderResume, hints runtime.Config) (Info, error) {
	return m.CreateAliasedNamedWithTransportAndMetadata(ctx, "", "", template, title, command, workDir, provider, transport, env, resume, hints, map[string]string{
		"session_origin": "manual",
	})
}

// CreateAliasedNamedWithTransport creates a new chat session bead with an
// optional public alias and optional explicit runtime session_name.
func (m *Manager) CreateAliasedNamedWithTransport(ctx context.Context, alias, explicitName, template, title, command, workDir, provider, transport string, env map[string]string, resume ProviderResume, hints runtime.Config) (Info, error) {
	return m.createAliasedNamedWithTransport(ctx, alias, explicitName, template, title, command, workDir, provider, transport, env, resume, hints, map[string]string{
		"session_origin": "manual",
	})
}

// CreateAliasedNamedWithTransportAndMetadata creates a new chat session bead
// with additional metadata published atomically at bead creation time.
func (m *Manager) CreateAliasedNamedWithTransportAndMetadata(ctx context.Context, alias, explicitName, template, title, command, workDir, provider, transport string, env map[string]string, resume ProviderResume, hints runtime.Config, extraMeta map[string]string) (Info, error) {
	return m.createAliasedNamedWithTransport(ctx, alias, explicitName, template, title, command, workDir, provider, transport, env, resume, hints, extraMeta)
}

func (m *Manager) createAliasedNamedWithTransport(ctx context.Context, alias, explicitName, template, title, command, workDir, provider, transport string, env map[string]string, resume ProviderResume, hints runtime.Config, extraMeta map[string]string) (Info, error) {
	alias, err := ValidateAlias(alias)
	if err != nil {
		return Info{}, err
	}
	explicitName, err = ValidateExplicitName(explicitName)
	if err != nil {
		return Info{}, err
	}
	if title == "" {
		title = template
	}
	aliasOwner := ""
	if extraMeta["configured_named_session"] == "true" && extraMeta["configured_named_identity"] == alias {
		aliasOwner = alias
	}
	var info Info
	err = withSessionIdentifierReservationLocks([]string{alias, explicitName}, func() error {
		if err := ensureSessionAliasAvailable(m.store, nil, alias, "", aliasOwner); err != nil {
			return err
		}
		if err := ensureSessionNameAvailableForSelfAndOwner(m.store, explicitName, "", aliasOwner); err != nil {
			return err
		}

		// Generate session key only when the provider supports Generate & Pass
		// (has SessionIDFlag). Otherwise the key would never be passed to the
		// provider and BuildResumeCommand would produce invalid resume commands.
		var sessionKey string
		if resume.SessionIDFlag != "" {
			generatedKey, genErr := GenerateSessionKey()
			if genErr != nil {
				return fmt.Errorf("generating session key: %w", genErr)
			}
			sessionKey = generatedKey
		}

		// Create the bead first to get the ID.
		meta := map[string]string{
			"template":           template,
			"state":              string(StateActive),
			"provider":           provider,
			"work_dir":           workDir,
			"command":            command,
			"resume_flag":        resume.ResumeFlag,
			"resume_style":       resume.ResumeStyle,
			"resume_command":     resume.ResumeCommand,
			"generation":         fmt.Sprintf("%d", DefaultGeneration),
			"continuation_epoch": fmt.Sprintf("%d", DefaultContinuationEpoch),
			"instance_token":     NewInstanceToken(),
		}
		// provider_kind may be injected via extraMeta when the caller has
		// resolved the canonical builtin kind for a custom provider alias.
		if alias != "" {
			meta["alias"] = alias
		}
		if normalizedTransport := normalizeTransport(provider, transport); normalizedTransport != "" {
			meta["transport"] = normalizedTransport
		}
		if sessionKey != "" {
			meta["session_key"] = sessionKey
		}
		if explicitName != "" {
			meta["session_name"] = explicitName
			meta["session_name_explicit"] = "true"
		}
		for k, v := range extraMeta {
			meta[k] = v
		}
		if meta["session_origin"] == "" {
			meta["session_origin"] = "manual"
		}
		createdBead, createErr := m.store.Create(beads.Bead{
			Title: title,
			Type:  BeadType,
			Labels: []string{
				LabelSession,
				"template:" + template,
			},
			Metadata: meta,
		})
		if createErr != nil {
			return fmt.Errorf("creating session bead: %w", createErr)
		}
		b := createdBead

		sessName := explicitName
		if sessName == "" {
			sessName = sessionNameFor(b.ID)
			if err := m.store.SetMetadata(b.ID, "session_name", sessName); err != nil {
				_ = m.store.Close(b.ID)
				return fmt.Errorf("storing session name: %w", err)
			}
		}
		if b.Metadata == nil {
			b.Metadata = make(map[string]string)
		}
		b.Metadata["session_name"] = sessName
		if explicitName != "" {
			b.Metadata["session_name_explicit"] = "true"
		}

		unroute := m.routeACPIfNeeded(provider, transport, sessName)
		rollbackFailedCreate := func() error {
			if unroute != nil {
				unroute()
			}
			if explicitName != "" {
				if err := m.store.SetMetadata(b.ID, "session_name", ""); err != nil {
					return fmt.Errorf("clearing session name during rollback: %w", err)
				}
				if err := m.store.SetMetadata(b.ID, "session_name_explicit", ""); err != nil {
					return fmt.Errorf("clearing explicit session name flag during rollback: %w", err)
				}
				b.Metadata["session_name"] = ""
				b.Metadata["session_name_explicit"] = ""
			}
			if err := m.store.Close(b.ID); err != nil {
				return fmt.Errorf("closing rolled-back session bead: %w", err)
			}
			return nil
		}

		// If the provider supports Generate & Pass, inject --session-id into command.
		startCommand := command
		if resume.SessionIDFlag != "" && sessionKey != "" {
			startCommand = command + " " + resume.SessionIDFlag + " " + sessionKey
		}

		// Build the session config from the hints, overriding command/workdir/env.
		cfg := hints
		cfg.Command = startCommand
		cfg.WorkDir = workDir
		runtimeAlias := alias
		if runtimeAlias == "" {
			runtimeAlias = strings.TrimSpace(extraMeta["agent_name"])
		}
		cfg.Env = mergeEnv(mergeEnv(cfg.Env, env), RuntimeEnvWithSessionContext(
			b.ID,
			sessName,
			runtimeAlias,
			template,
			meta["session_origin"],
			DefaultGeneration,
			DefaultContinuationEpoch,
			meta["instance_token"],
		))
		if gcProvider := providerKindFromMetadata(meta, provider); gcProvider != "" {
			cfg.Env = mergeEnv(cfg.Env, map[string]string{"GC_PROVIDER": gcProvider})
		}
		cfg = runtime.SyncWorkDirEnv(cfg)

		// Start the runtime session.
		if err := m.sp.Start(ctx, sessName, cfg); err != nil {
			if runtimeSessionMatchesBead(m.sp, sessName, b.ID, meta["instance_token"]) {
				info = m.infoFromBead(b)
				return nil
			}
			if errors.Is(err, runtime.ErrSessionExists) && m.sp.IsRunning(sessName) {
				if rbErr := rollbackFailedCreate(); rbErr != nil {
					return errors.Join(fmt.Errorf("%w: %q already active in runtime", ErrSessionNameExists, sessName), rbErr)
				}
				return fmt.Errorf("%w: %q already active in runtime", ErrSessionNameExists, sessName)
			}
			if rbErr := rollbackFailedCreate(); rbErr != nil {
				return errors.Join(fmt.Errorf("starting session: %w", err), rbErr)
			}
			return fmt.Errorf("starting session: %w", err)
		}

		info = m.infoFromBead(b)
		return nil
	})
	if err != nil {
		return Info{}, err
	}
	return info, nil
}

// CreateNamedWithTransport creates a new chat session bead with an optional
// explicit session_name and starts the runtime session.
//
// WARNING: withSessionNameReservationLock only serializes callers inside this
// process. Callers MUST also hold WithCitySessionNameLock(cityPath, explicitName)
// when explicitName is non-empty so duplicate names cannot race across processes.
func (m *Manager) CreateNamedWithTransport(ctx context.Context, explicitName, template, title, command, workDir, provider, transport string, env map[string]string, resume ProviderResume, hints runtime.Config) (Info, error) {
	return m.CreateAliasedNamedWithTransportAndMetadata(ctx, "", explicitName, template, title, command, workDir, provider, transport, env, resume, hints, map[string]string{
		"session_origin": "manual",
	})
}

func runtimeSessionMatchesBead(sp runtime.Provider, sessionName, beadID, instanceToken string) bool {
	if sp == nil {
		return false
	}
	if liveID, err := sp.GetMeta(sessionName, "GC_SESSION_ID"); err == nil {
		liveID = strings.TrimSpace(liveID)
		if liveID != "" {
			return liveID == beadID
		}
	}
	instanceToken = strings.TrimSpace(instanceToken)
	if instanceToken == "" {
		return false
	}
	liveToken, err := sp.GetMeta(sessionName, "GC_INSTANCE_TOKEN")
	if err != nil {
		return false
	}
	return strings.TrimSpace(liveToken) == instanceToken
}

// CreateBeadOnly creates a session bead without starting the runtime process.
// The bead is created with state "creating" — the controller's reconciler
// will detect it in buildDesiredState and start the process on its next tick.
//
// This is the Phase 2 path: CLI creates intent (bead), reconciler executes.
func (m *Manager) CreateBeadOnly(template, title, command, workDir, provider, transport string, env map[string]string, resume ProviderResume) (Info, error) {
	return m.CreateBeadOnlyNamed("", template, title, command, workDir, provider, transport, env, resume)
}

// CreateAliasedBeadOnlyNamed creates a session bead without starting the
// runtime process, preserving an optional public alias and explicit runtime
// session_name for the reconciler.
func (m *Manager) CreateAliasedBeadOnlyNamed(alias, explicitName, template, title, command, workDir, provider, transport string, _ map[string]string, resume ProviderResume) (Info, error) {
	return m.createAliasedBeadOnlyNamed(alias, explicitName, template, title, command, workDir, provider, transport, resume, nil)
}

// CreateAliasedBeadOnlyNamedWithMetadata creates a session bead without
// starting the runtime process, publishing extra metadata atomically.
func (m *Manager) CreateAliasedBeadOnlyNamedWithMetadata(alias, explicitName, template, title, command, workDir, provider, transport string, resume ProviderResume, extraMeta map[string]string) (Info, error) {
	return m.createAliasedBeadOnlyNamed(alias, explicitName, template, title, command, workDir, provider, transport, resume, extraMeta)
}

func (m *Manager) createAliasedBeadOnlyNamed(alias, explicitName, template, title, command, workDir, provider, transport string, resume ProviderResume, extraMeta map[string]string) (Info, error) {
	alias, err := ValidateAlias(alias)
	if err != nil {
		return Info{}, err
	}
	explicitName, err = ValidateExplicitName(explicitName)
	if err != nil {
		return Info{}, err
	}
	if title == "" {
		title = template
	}
	aliasOwner := ""
	if extraMeta["configured_named_session"] == "true" && extraMeta["configured_named_identity"] == alias {
		aliasOwner = alias
	}
	var info Info
	err = withSessionIdentifierReservationLocks([]string{alias, explicitName}, func() error {
		if err := ensureSessionAliasAvailable(m.store, nil, alias, "", aliasOwner); err != nil {
			return err
		}
		if err := ensureSessionNameAvailableForSelfAndOwner(m.store, explicitName, "", aliasOwner); err != nil {
			return err
		}

		var sessionKey string
		if resume.SessionIDFlag != "" {
			generatedKey, genErr := GenerateSessionKey()
			if genErr != nil {
				return fmt.Errorf("generating session key: %w", genErr)
			}
			sessionKey = generatedKey
		}

		meta := map[string]string{
			"template":           template,
			"state":              "creating",
			"provider":           provider,
			"work_dir":           workDir,
			"command":            command,
			"resume_flag":        resume.ResumeFlag,
			"resume_style":       resume.ResumeStyle,
			"resume_command":     resume.ResumeCommand,
			"generation":         fmt.Sprintf("%d", DefaultGeneration),
			"continuation_epoch": fmt.Sprintf("%d", DefaultContinuationEpoch),
			"instance_token":     NewInstanceToken(),
		}
		if alias != "" {
			meta["alias"] = alias
		}
		if normalizedTransport := normalizeTransport(provider, transport); normalizedTransport != "" {
			meta["transport"] = normalizedTransport
		}
		if sessionKey != "" {
			meta["session_key"] = sessionKey
		}
		meta["pending_create_claim"] = "true"
		if explicitName != "" {
			meta["session_name"] = explicitName
			meta["session_name_explicit"] = "true"
		}
		for k, v := range extraMeta {
			meta[k] = v
		}
		if meta["session_origin"] == "" {
			meta["session_origin"] = "ephemeral"
		}
		createdBead, createErr := m.store.Create(beads.Bead{
			Title: title,
			Type:  BeadType,
			Labels: []string{
				LabelSession,
				"template:" + template,
			},
			Metadata: meta,
		})
		if createErr != nil {
			return fmt.Errorf("creating session bead: %w", createErr)
		}
		b := createdBead

		sessName := explicitName
		if sessName == "" {
			sessName = sessionNameFor(b.ID)
			if err := m.store.SetMetadata(b.ID, "session_name", sessName); err != nil {
				_ = m.store.Close(b.ID)
				return fmt.Errorf("storing session name: %w", err)
			}
		}
		if b.Metadata == nil {
			b.Metadata = make(map[string]string)
		}
		b.Metadata["session_name"] = sessName

		info = m.infoFromBead(b)
		return nil
	})
	if err != nil {
		return Info{}, err
	}
	return info, nil
}

// CreateBeadOnlyNamed creates a session bead without starting the runtime
// process, preserving an optional explicit session_name for the reconciler.
//
// WARNING: withSessionNameReservationLock only serializes callers inside this
// process. Callers MUST also hold WithCitySessionNameLock(cityPath, explicitName)
// when explicitName is non-empty so duplicate names cannot race across processes.
func (m *Manager) CreateBeadOnlyNamed(explicitName, template, title, command, workDir, provider, transport string, _ map[string]string, resume ProviderResume) (Info, error) {
	return m.CreateAliasedBeadOnlyNamed("", explicitName, template, title, command, workDir, provider, transport, nil, resume)
}

// Attach attaches the user's terminal to the session. If the session is
// suspended, it is resumed first using resumeCommand. If the tmux session
// died (active bead but no process), it is restarted.
func (m *Manager) Attach(ctx context.Context, id string, resumeCommand string, hints runtime.Config) error {
	return withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		if err := m.ensureRunning(ctx, id, b, sessName, resumeCommand, hints); err != nil {
			return err
		}

		return m.sp.Attach(sessName)
	})
}

// Suspend saves session state and kills the runtime session.
func (m *Manager) Suspend(id string) error {
	return withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		// Closed beads are terminal; mutating lifecycle metadata after
		// close produces impossible status=closed + live-state rows.
		if b.Status == "closed" {
			return &IllegalTransitionError{From: StateClosed, Command: CmdSuspend}
		}
		current := State(b.Metadata["state"])
		if current == StateSuspended {
			return nil // idempotent: already suspended
		}
		// Legacy bead normalization: pre-metadata cities may have empty
		// state fields. Treat empty as StateActive so the state-machine
		// transition works during upgrade. Matches what Close and
		// checkTransition already do for the other lifecycle methods.
		if current == StateNone {
			current = StateActive
		}
		// StateAwake is the reconciler's alias for StateActive.
		if current == StateAwake {
			current = StateActive
		}
		if _, err := Transition(current, CmdSuspend); err != nil {
			return err
		}

		// Kill the runtime session (skip if already dead).
		if m.sp.IsRunning(sessName) {
			if err := m.sp.Stop(sessName); err != nil {
				return fmt.Errorf("stopping runtime session: %w", err)
			}
		}

		// Update state and record suspension timestamp.
		if err := m.store.SetMetadata(id, "state", string(StateSuspended)); err != nil {
			return fmt.Errorf("updating session state: %w", err)
		}
		if err := m.store.SetMetadata(id, "suspended_at", time.Now().UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("storing suspension timestamp: %w", err)
		}

		return nil
	})
}

// RequestFreshRestart marks a session for a controller-owned fresh restart
// without closing its bead or clearing resume metadata immediately.
func (m *Manager) RequestFreshRestart(id string) error {
	return withSessionMutationLock(id, func() error {
		if _, _, err := m.sessionBead(id); err != nil {
			return err
		}
		return m.store.SetMetadataBatch(id, map[string]string{
			"restart_requested":          "true",
			"continuation_reset_pending": "true",
		})
	})
}

// Close ends a conversation permanently.
func (m *Manager) Close(id string) error {
	return withSessionMutationLock(id, func() error {
		b, sessName, err := m.loadSessionBead(id, true)
		if err != nil {
			return err
		}
		if b.Status == "closed" {
			return nil // idempotent: already closed
		}
		// CmdClose is legal from any non-none state; this is effectively a
		// documentation check that will catch future table changes. Treat
		// empty metadata state as StateActive for bootstrap beads, and
		// treat the reconciler's StateAwake alias as StateActive so
		// already-awake beads can close cleanly.
		current := State(b.Metadata["state"])
		if current == StateNone {
			current = StateActive
		}
		if current == StateAwake {
			current = StateActive
		}
		if _, err := Transition(current, CmdClose); err != nil {
			return err
		}

		// Best-effort stop cleans up any live runtime and allows auto.Provider
		// to discard stale ACP route entries for suspended sessions as well.
		_ = m.sp.Stop(sessName)
		_ = CancelWaits(m.store, id, time.Now().UTC())
		if err := m.clearWakeAndHoldOverrides(id); err != nil {
			return err
		}
		if err := m.retireConfiguredNamedSessionIdentifiers(id, b); err != nil {
			return err
		}

		return m.store.Close(id)
	})
}

func (m *Manager) clearWakeAndHoldOverrides(id string) error {
	update := map[string]string{
		"pin_awake":    "",
		"held_until":   "",
		"sleep_intent": "",
	}
	if err := m.store.SetMetadataBatch(id, update); err != nil {
		return fmt.Errorf("clearing wake and hold overrides: %w", err)
	}
	return nil
}

func (m *Manager) retireConfiguredNamedSessionIdentifiers(id string, b beads.Bead) error {
	if strings.TrimSpace(b.Metadata["configured_named_session"]) != "true" {
		return nil
	}
	update := beads.UpdateOpts{
		Metadata: UpdatedAliasMetadata(b.Metadata, ""),
	}
	update.Metadata["session_name"] = ""
	update.Metadata["session_name_explicit"] = ""
	update.Metadata["pending_create_claim"] = ""
	if err := m.store.Update(id, update); err != nil {
		return fmt.Errorf("retiring configured named session identifiers: %w", err)
	}
	return nil
}

// Kill force-kills the runtime process for a session without changing bead
// state. This is intended for manual intervention; the reconciler will detect
// the dead process and restart it according to the session's lifecycle rules.
func (m *Manager) Kill(id string) error {
	b, sessName, err := m.sessionBead(id)
	if err != nil {
		return err
	}
	// Accept any state where a runtime process could plausibly exist.
	// The reconciler uses "awake" as equivalent to "active", and metadata
	// state can lag behind reality, so also check provider liveness.
	state := State(b.Metadata["state"])
	switch state {
	case StateActive, StateCreating, StateDraining, StateAwake:
		// Known live states — proceed.
	default:
		if !m.sp.IsRunning(sessName) {
			return fmt.Errorf("session %s is not active", id)
		}
	}
	return m.sp.Stop(sessName)
}

// BeginDrain transitions a session to the draining state. The caller is
// responsible for signaling the runtime process to finish its work.
// Idempotent: returns nil if the session is already draining.
func (m *Manager) BeginDrain(id, reason string) error {
	return withSessionMutationLock(id, func() error {
		cmdLegal, err := m.checkTransition(id, CmdDrain, StateDraining)
		if err != nil {
			return err
		}
		if !cmdLegal {
			return nil // idempotent: already draining
		}
		return m.store.SetMetadataBatch(id, BeginDrainPatch(time.Now().UTC(), reason))
	})
}

// Archive transitions a session from draining to archived. Idempotent:
// returns nil if the session is already archived.
func (m *Manager) Archive(id, reason string) error {
	return withSessionMutationLock(id, func() error {
		cmdLegal, err := m.checkTransition(id, CmdArchive, StateArchived)
		if err != nil {
			return err
		}
		if !cmdLegal {
			return nil // idempotent: already archived
		}
		return m.store.SetMetadataBatch(id, ArchivePatch(time.Now().UTC(), reason, false))
	})
}

// Quarantine marks a session as crash-quarantined until the given time.
// Idempotent: returns nil if the session is already quarantined.
func (m *Manager) Quarantine(id string, until time.Time, cycle int) error {
	return withSessionMutationLock(id, func() error {
		cmdLegal, err := m.checkTransition(id, CmdQuarantine, StateQuarantined)
		if err != nil {
			return err
		}
		if !cmdLegal {
			return nil // idempotent: already quarantined
		}
		return m.store.SetMetadataBatch(id, QuarantinePatch(until, cycle))
	})
}

// Reactivate clears archive/quarantine blockers and returns a session to
// asleep so normal wake machinery owns the next runtime start. Idempotent:
// returns nil if the session is already in an awake-eligible state.
func (m *Manager) Reactivate(id string) error {
	return withSessionMutationLock(id, func() error {
		cmdLegal, err := m.checkTransition(id, CmdWake, StateAsleep)
		if err != nil {
			return err
		}
		if !cmdLegal {
			return nil // idempotent: already in target state
		}
		b, err := m.store.Get(id)
		if err != nil {
			return err
		}
		view := ProjectLifecycle(LifecycleInput{
			Status:   b.Status,
			Metadata: b.Metadata,
		})
		// Note: quarantine_cycle is intentionally preserved across reactivations.
		// It tracks how many quarantine rounds the session has been through,
		// enabling eviction after quarantine_max_attempts.
		return m.store.SetMetadataBatch(id, ReactivatePatch(view.ContinuityEligible))
	})
}

// ConfirmCreation transitions a session from creating to active after the
// runtime process has been confirmed alive. Idempotent: returns nil if the
// session is already active.
func (m *Manager) ConfirmCreation(id string) error {
	return withSessionMutationLock(id, func() error {
		cmdLegal, err := m.checkTransition(id, CmdReady, StateActive)
		if err != nil {
			return err
		}
		if !cmdLegal {
			return nil // idempotent: already active
		}
		return m.store.SetMetadataBatch(id, ConfirmStartedPatch(time.Now()))
	})
}

// checkTransition reads the current state of session id and reports whether
// cmd is legal. Empty state metadata is treated as StateActive for legacy
// bootstrap beads (pre-metadata upgrades). Closed beads are terminal and
// reject any lifecycle mutation (callers should use the dedicated Close
// idempotency branch, not a lifecycle transition). Returns:
//   - cmdLegal: true if the command produces a real transition, false if
//     the session is already in targetState (idempotent no-op)
//   - err: *IllegalTransitionError wrapping ErrIllegalTransition when the
//     command is neither legal nor a no-op
//
// MUST be called while holding withSessionMutationLock(id).
func (m *Manager) checkTransition(id string, cmd TransitionCommand, targetState State) (bool, error) {
	b, _, err := m.sessionBead(id)
	if err != nil {
		return false, err
	}
	// Closed beads are terminal. Mutating lifecycle metadata after close
	// would produce impossible status=closed + live-state combinations
	// that the reconciler misreads. Surface a clear illegal-transition
	// error instead of silently mutating.
	if b.Status == "closed" {
		return false, &IllegalTransitionError{From: StateClosed, Command: cmd}
	}
	current := State(b.Metadata["state"])
	if current == StateNone {
		// Legacy bead: pre-metadata cities may have empty state fields.
		// Treat as active so transitions work during upgrade.
		current = StateActive
	}
	// StateAwake is the reconciler's alias for StateActive. The state
	// machine table only knows StateActive, so normalize before calling
	// Transition to keep already-awake beads accepting Suspend/Drain/
	// Archive/Quarantine.
	if current == StateAwake {
		current = StateActive
	}
	if current == targetState {
		return false, nil
	}
	if _, err := Transition(current, cmd); err != nil {
		return false, err
	}
	return true, nil
}

// Rename updates the title of a chat session.
func (m *Manager) Rename(id, title string) error {
	return m.UpdatePresentation(id, &title, nil)
}

// UpdatePresentation updates user-facing session attributes.
func (m *Manager) UpdatePresentation(id string, title *string, alias *string) error {
	return withSessionMutationLock(id, func() error {
		b, sessName, err := m.loadSessionBead(id, true)
		if err != nil {
			return err
		}
		currentAlias := strings.TrimSpace(b.Metadata["alias"])
		var nextAlias string
		if alias != nil {
			validated, err := ValidateAlias(*alias)
			if err != nil {
				return err
			}
			nextAlias = validated
			if strings.TrimSpace(b.Metadata["configured_named_session"]) == "true" && nextAlias != currentAlias {
				return fmt.Errorf("configured named session alias is immutable while config-managed")
			}
		}
		update := beads.UpdateOpts{}
		if title != nil {
			update.Title = title
		}
		if alias != nil {
			return withSessionAliasReservationLock(nextAlias, func() error {
				if nextAlias != currentAlias {
					if err := ensureSessionAliasAvailable(m.store, nil, nextAlias, id, ""); err != nil {
						return err
					}
				}
				update.Metadata = UpdatedAliasMetadata(b.Metadata, nextAlias)
				runtimeRunning := sessName != "" && m.sp != nil && m.sp.IsRunning(sessName)
				if runtimeRunning {
					if err := SyncRuntimeAlias(m.sp, sessName, nextAlias); err != nil {
						return fmt.Errorf("updating runtime alias: %w", err)
					}
				}
				if err := m.store.Update(id, update); err != nil {
					if runtimeRunning {
						if rollbackErr := SyncRuntimeAlias(m.sp, sessName, currentAlias); rollbackErr != nil {
							log.Printf("session %s: restoring runtime alias %q on %s failed: %v", id, currentAlias, sessName, rollbackErr)
						}
					}
					return err
				}
				return nil
			})
		}
		return m.store.Update(id, update)
	})
}

// Prune closes suspended sessions whose suspension time is before the given
// cutoff. Active and already-closed sessions are never pruned.
// Returns the number of sessions pruned.
func (m *Manager) Prune(before time.Time) (int, error) {
	result, err := m.PruneDetailed(before)
	return result.Count, err
}

// PruneDetailed closes suspended sessions whose suspension time is before the
// given cutoff and reports the affected session IDs and queued wait nudges.
func (m *Manager) PruneDetailed(before time.Time) (PruneResult, error) {
	all, err := m.store.List(beads.ListQuery{
		Label: LabelSession,
	})
	if err != nil {
		return PruneResult{}, fmt.Errorf("listing sessions: %w", err)
	}
	result := PruneResult{}
	for _, b := range all {
		if !IsSessionBeadOrRepairable(b) {
			continue
		}
		if b.Status == "closed" {
			continue // already closed
		}
		state := State(b.Metadata["state"])
		if state != StateSuspended {
			continue // only prune suspended sessions
		}
		// Use suspended_at timestamp if available, fall back to CreatedAt
		// for beads created before suspended_at was introduced.
		ts := b.CreatedAt
		if raw := b.Metadata["suspended_at"]; raw != "" {
			if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
				ts = parsed
			}
		}
		if !ts.Before(before) {
			continue
		}
		nudgeIDs, err := WaitNudgeIDs(m.store, b.ID)
		if err != nil {
			return result, fmt.Errorf("listing wait nudges for session %s: %w", b.ID, err)
		}
		result.WaitNudgeIDs = append(result.WaitNudgeIDs, nudgeIDs...)
		_ = CancelWaits(m.store, b.ID, time.Now().UTC())
		if err := m.store.Close(b.ID); err != nil {
			return result, fmt.Errorf("closing session %s: %w", b.ID, err)
		}
		result.Count++
		result.SessionIDs = append(result.SessionIDs, b.ID)
	}
	return result, nil
}

// Get returns info about a single session.
func (m *Manager) Get(id string) (Info, error) {
	b, _, err := m.loadSessionBead(id, true)
	if err != nil {
		return Info{}, err
	}
	return m.infoFromBead(b), nil
}

// ObserveRuntime reports live provider state for the current session runtime.
func (m *Manager) ObserveRuntime(id string, processNames []string) (RuntimeObservation, error) {
	info, err := m.Get(id)
	if err != nil {
		return RuntimeObservation{}, err
	}
	obs := RuntimeObservation{SessionName: info.SessionName}
	if strings.TrimSpace(info.SessionName) == "" || m.sp == nil {
		return obs, nil
	}
	obs.Running = m.sp.IsRunning(info.SessionName)
	if !obs.Running {
		return obs, nil
	}
	obs.Alive = m.sp.ProcessAlive(info.SessionName, processNames)
	obs.Attached = m.sp.IsAttached(info.SessionName)
	if lastActive, err := m.sp.GetLastActivity(info.SessionName); err == nil {
		obs.LastActive = lastActive
	}
	return obs, nil
}

// ListResult holds the results of a ListFull call, including the raw beads
// to avoid redundant store queries.
type ListResult struct {
	Sessions []Info
	Beads    []beads.Bead // All session beads (unfiltered by state/template)
}

// List returns all chat sessions, optionally filtered by state and template.
func (m *Manager) List(stateFilter string, templateFilter string) ([]Info, error) {
	r, err := m.ListFull(stateFilter, templateFilter)
	if err != nil {
		return nil, err
	}
	return r.Sessions, nil
}

// ListFull is like List but also returns the raw session beads to avoid
// redundant store queries by the caller (e.g., for building a bead index).
func (m *Manager) ListFull(stateFilter string, templateFilter string) (*ListResult, error) {
	all, err := m.store.List(beads.ListQuery{
		Label: LabelSession,
		Sort:  beads.SortCreatedDesc,
	})
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	return m.ListFullFromBeads(all, stateFilter, templateFilter), nil
}

// ListFullFromBeads is like ListFull but reuses a caller-supplied slice of
// session-labeled beads. Callers that already loaded session beads can avoid
// a second store scan by passing the same slice here.
func (m *Manager) ListFullFromBeads(all []beads.Bead, stateFilter string, templateFilter string) *ListResult {
	result := make([]Info, 0, len(all))
	for _, b := range all {
		if !IsSessionBeadOrRepairable(b) {
			continue
		}
		state := normalizeInfoState(State(b.Metadata["state"]))

		// Filter by state.
		if stateFilter != "" && stateFilter != "all" {
			match := false
			for _, s := range strings.Split(stateFilter, ",") {
				switch {
				case s == "closed" && b.Status == "closed":
					match = true
				case s == "open" && b.Status == "open":
					match = true
				case b.Status != "closed" && s == string(state):
					// Only match metadata state for non-closed beads.
					match = true
				}
				if match {
					break
				}
			}
			if !match {
				continue
			}
		} else if stateFilter == "" {
			// Default: exclude closed sessions.
			if b.Status == "closed" {
				continue
			}
		}

		// Filter by template.
		if templateFilter != "" && b.Metadata["template"] != templateFilter {
			continue
		}

		result = append(result, m.infoFromBead(b))
	}
	return &ListResult{Sessions: result, Beads: all}
}

// Peek captures the last N lines of output from the session.
func (m *Manager) Peek(id string, lines int) (string, error) {
	b, sessName, err := m.loadSessionBead(id, true)
	if err != nil {
		return "", err
	}
	if b.Status == "closed" || State(b.Metadata["state"]) == StateSuspended {
		return "", fmt.Errorf("%w: %s", ErrSessionInactive, id)
	}
	return m.sp.Peek(sessName, lines)
}

// infoFromBead converts a bead to an Info struct, enriching with runtime state.
func (m *Manager) infoFromBead(b beads.Bead) Info {
	sessName := b.Metadata["session_name"]
	if sessName == "" {
		sessName = sessionNameFor(b.ID)
	}
	closed := b.Status == "closed"
	if !closed {
		transport, _ := m.transportForBead(b, sessName)
		_ = m.routeACPIfNeeded(b.Metadata["provider"], transport, sessName)
	}

	state := normalizeInfoState(State(b.Metadata["state"]))
	if closed {
		state = "" // closed beads have no runtime state
	} else if m.sp != nil && state == StateActive && !m.sp.IsRunning(sessName) {
		// Surface stale "awake" / "active" beads as dormant immediately.
		// The controller also heals metadata on the next tick.
		state = StateAsleep
	}

	info := Info{
		ID:            b.ID,
		Template:      b.Metadata["template"],
		State:         state,
		Closed:        closed,
		Title:         b.Title,
		Alias:         b.Metadata["alias"],
		Provider:      b.Metadata["provider"],
		Command:       b.Metadata["command"],
		WorkDir:       b.Metadata["work_dir"],
		SessionName:   sessName,
		SessionKey:    b.Metadata["session_key"],
		ResumeFlag:    b.Metadata["resume_flag"],
		ResumeStyle:   b.Metadata["resume_style"],
		ResumeCommand: b.Metadata["resume_command"],
		CreatedAt:     b.CreatedAt,
	}

	// Enrich with live runtime state if active.
	if state == StateActive && m.sp != nil {
		info.Attached = m.sp.IsAttached(sessName)
		if t, err := m.sp.GetLastActivity(sessName); err == nil && !t.IsZero() {
			info.LastActive = t
		}
	}

	return info
}

// PersistSessionKey stores a provider resume key on an existing session when
// the key is learned after creation (for example from transcript evidence).
// Existing non-empty keys are preserved.
func (m *Manager) PersistSessionKey(id, sessionKey string) error {
	sessionKey = strings.TrimSpace(sessionKey)
	if id == "" || sessionKey == "" {
		return nil
	}
	return withSessionMutationLock(id, func() error {
		b, _, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		if strings.TrimSpace(b.Metadata["session_key"]) != "" {
			return nil
		}
		if err := m.store.SetMetadata(id, "session_key", sessionKey); err != nil {
			return fmt.Errorf("storing session key: %w", err)
		}
		return nil
	})
}

// sessionNameFor derives the tmux session name from a bead ID.
// Uses the "s-" prefix to avoid collision with agent sessions.
func sessionNameFor(beadID string) string {
	return "s-" + strings.ReplaceAll(beadID, "/", "--")
}

// BuildResumeCommand constructs the resume command from stored session info.
// Priority: explicit ResumeCommand (with {{.SessionKey}} expansion) >
// ResumeFlag/ResumeStyle auto-construction > stored command as-is.
func BuildResumeCommand(info Info) string {
	// Explicit resume_command takes precedence.
	if info.ResumeCommand != "" && info.SessionKey != "" {
		return strings.ReplaceAll(info.ResumeCommand, "{{.SessionKey}}", info.SessionKey)
	}

	if info.ResumeFlag == "" || info.SessionKey == "" {
		// Provider doesn't support resume or no key — use stored command.
		cmd := info.Command
		if cmd == "" {
			cmd = info.Provider
		}
		return cmd
	}

	// Build resume command based on style.
	cmd := info.Command
	if cmd == "" {
		cmd = info.Provider
	}
	switch info.ResumeStyle {
	case "subcommand":
		// Insert subcommand after the binary name:
		//   "codex --model o3" → "codex resume <key> --model o3"
		parts := strings.SplitN(cmd, " ", 2)
		if len(parts) == 2 {
			return parts[0] + " " + info.ResumeFlag + " " + info.SessionKey + " " + parts[1]
		}
		return cmd + " " + info.ResumeFlag + " " + info.SessionKey
	default: // "flag"
		// command --resume <key> (e.g., claude --resume <uuid>)
		return cmd + " " + info.ResumeFlag + " " + info.SessionKey
	}
}

// mergeEnv merges two env maps, with override taking precedence.
func mergeEnv(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	merged := make(map[string]string)
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range override {
		merged[k] = v
	}
	return merged
}

// GenerateSessionKey creates a random UUID v4 for session identification.
func GenerateSessionKey() (string, error) {
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	uuid[6] = (uuid[6] & 0x0f) | 0x40 // version 4
	uuid[8] = (uuid[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16]), nil
}
