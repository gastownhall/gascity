// session_circuit_breaker.go implements a respawn circuit breaker for named
// sessions. The supervisor reconciler will otherwise restart a named session
// indefinitely with zero awareness of loop conditions. When a named session
// is stuck in a respawn loop with no observable progress, this breaker trips
// and blocks further respawn attempts until an operator intervenes (or the
// automatic cooldown reset fires). The breaker here is the minimal
// infrastructure to interrupt repeated no-progress respawn loops. See also the
// instructions logged in the ERROR path below for the manual reset knob.
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// sessionCircuitBreakerConfig controls the breaker thresholds. Zero values
// fall back to package defaults so callers can construct with only the
// fields they want to override.
type sessionCircuitBreakerConfig struct {
	// Window is the rolling window over which restart timestamps are
	// counted. Default: 30 minutes.
	Window time.Duration
	// MaxRestarts is the number of restarts allowed within Window before
	// the breaker considers tripping. Default: 5.
	MaxRestarts int
	// ResetAfter is the cooldown interval after which an OPEN breaker
	// automatically resets back to CLOSED. Default: 2 * Window.
	ResetAfter time.Duration
}

const (
	defaultCircuitBreakerWindow      = 30 * time.Minute
	defaultCircuitBreakerMaxRestarts = 5
)

const (
	sessionCircuitStateMetadata             = "session_circuit_state"
	sessionCircuitRestartsMetadata          = "session_circuit_restarts"
	sessionCircuitLastRestartMetadata       = "session_circuit_last_restart"
	sessionCircuitLastProgressMetadata      = "session_circuit_last_progress"
	sessionCircuitLastObservedMetadata      = "session_circuit_last_observed"
	sessionCircuitProgressSignatureMetadata = "session_circuit_progress_signature"
	sessionCircuitOpenedAtMetadata          = "session_circuit_opened_at"
	sessionCircuitOpenRestartCountMetadata  = "session_circuit_open_restart_count"
	sessionCircuitResetGenerationMetadata   = "session_circuit_reset_generation"
)

var sessionCircuitMetadataKeys = []string{
	sessionCircuitStateMetadata,
	sessionCircuitRestartsMetadata,
	sessionCircuitLastRestartMetadata,
	sessionCircuitLastProgressMetadata,
	sessionCircuitLastObservedMetadata,
	sessionCircuitProgressSignatureMetadata,
	sessionCircuitOpenedAtMetadata,
	sessionCircuitOpenRestartCountMetadata,
	sessionCircuitResetGenerationMetadata,
}

func (c sessionCircuitBreakerConfig) withDefaults() sessionCircuitBreakerConfig {
	if c.Window <= 0 {
		c.Window = defaultCircuitBreakerWindow
	}
	if c.MaxRestarts <= 0 {
		c.MaxRestarts = defaultCircuitBreakerMaxRestarts
	}
	if c.ResetAfter <= 0 {
		c.ResetAfter = 2 * c.Window
	}
	return c
}

func sessionCircuitBreakerConfigFromCity(cfg *config.City) (sessionCircuitBreakerConfig, bool) {
	if cfg == nil || !cfg.Daemon.SessionCircuitBreaker {
		return sessionCircuitBreakerConfig{}, false
	}
	maxRestarts := cfg.Daemon.SessionCircuitBreakerMaxRestartsOrDefault()
	if maxRestarts <= 0 {
		return sessionCircuitBreakerConfig{}, false
	}
	cbCfg := sessionCircuitBreakerConfig{
		Window:      cfg.Daemon.SessionCircuitBreakerWindowDuration(),
		MaxRestarts: maxRestarts,
		ResetAfter:  cfg.Daemon.SessionCircuitBreakerResetAfterDuration(),
	}
	return cbCfg.withDefaults(), true
}

// circuitBreakerStateKind is the logical state of a single identity's
// breaker entry. CLOSED is the normal case (respawns allowed). OPEN means
// the supervisor MUST NOT materialize or spawn this session.
type circuitBreakerStateKind int

const (
	circuitClosed circuitBreakerStateKind = iota
	circuitOpen
)

func (k circuitBreakerStateKind) String() string {
	switch k {
	case circuitOpen:
		return "CIRCUIT_OPEN"
	default:
		return "CIRCUIT_CLOSED"
	}
}

// circuitBreakerEntry is the in-memory state tracked for a single named
// session identity. All fields are owned by the parent breaker and are only
// read/written with the breaker's mutex held.
type circuitBreakerEntry struct {
	restarts       []time.Time // timestamps within the rolling window
	lastRestart    time.Time
	lastProgress   time.Time
	lastObserved   time.Time
	progressSig    string // last observed assigned-bead status signature
	observedSig    bool
	state          circuitBreakerStateKind
	openedAt       time.Time
	openRestartCnt int // snapshot of restart count at the moment the breaker opened
	loggedOpenOnce bool
}

// CircuitBreakerSnapshot is a point-in-time view of a single identity's
// breaker state. Exposed to the status hook so operators can see who is
// tripped without reaching into breaker internals.
type CircuitBreakerSnapshot struct {
	Identity         string    `json:"identity"`
	State            string    `json:"state"`
	RestartCount     int       `json:"restart_count"`
	OpenRestartCount int       `json:"open_restart_count,omitempty"`
	WindowStart      time.Time `json:"window_start,omitempty"`
	LastRestart      time.Time `json:"last_restart,omitempty"`
	LastProgress     time.Time `json:"last_progress,omitempty"`
	OpenedAt         time.Time `json:"opened_at,omitempty"`
	ResetAfter       time.Time `json:"reset_after,omitempty"`
}

// sessionCircuitBreaker tracks restart attempts for named sessions and
// enforces a rolling-window circuit-breaker policy. It is safe for
// concurrent use by multiple reconciler ticks.
type sessionCircuitBreaker struct {
	cfg              sessionCircuitBreakerConfig
	mu               sync.Mutex
	entries          map[string]*circuitBreakerEntry
	resetGenerations map[string]uint64
}

type sessionCircuitBreakerIdentitySnapshot struct {
	entry         *circuitBreakerEntry
	hadEntry      bool
	generation    uint64
	hadGeneration bool
}

// newSessionCircuitBreaker constructs a breaker with the given config.
// Zero-valued config fields fall back to defaults.
func newSessionCircuitBreaker(cfg sessionCircuitBreakerConfig) *sessionCircuitBreaker {
	return &sessionCircuitBreaker{
		cfg:              cfg.withDefaults(),
		entries:          make(map[string]*circuitBreakerEntry),
		resetGenerations: make(map[string]uint64),
	}
}

func (b *sessionCircuitBreaker) configure(cfg sessionCircuitBreakerConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg = cfg.withDefaults()
}

// trimLocked discards restart timestamps older than the rolling window. The
// caller must hold b.mu.
func (b *sessionCircuitBreaker) trimLocked(e *circuitBreakerEntry, now time.Time) {
	cutoff := now.Add(-b.cfg.Window)
	i := 0
	for ; i < len(e.restarts); i++ {
		if !e.restarts[i].Before(cutoff) {
			break
		}
	}
	if i > 0 {
		e.restarts = append(e.restarts[:0], e.restarts[i:]...)
	}
}

// maybeAutoResetLocked resets an OPEN entry to CLOSED after its wall-clock
// cooldown expires. While OPEN, the supervisor may keep ticking, but respawn
// attempts are blocked before RecordRestart, so this is not a silence detector.
// The caller must hold b.mu.
func (b *sessionCircuitBreaker) maybeAutoResetLocked(e *circuitBreakerEntry, now time.Time) bool {
	if e.state != circuitOpen {
		return false
	}
	if e.lastRestart.IsZero() {
		return false
	}
	if now.Sub(e.lastRestart) >= b.cfg.ResetAfter {
		e.state = circuitClosed
		e.restarts = nil
		e.lastRestart = time.Time{}
		e.lastProgress = time.Time{}
		e.openedAt = time.Time{}
		e.openRestartCnt = 0
		e.loggedOpenOnce = false
		e.lastObserved = time.Time{}
		e.progressSig = ""
		e.observedSig = false
		return true
	}
	return false
}

// RecordRestart records a restart attempt for the given identity at time
// `now`. If the rolling-window restart count exceeds the configured max AND
// there is no progress signal inside the window, the entry transitions to
// CIRCUIT_OPEN. Returns the post-record state kind.
func (b *sessionCircuitBreaker) RecordRestart(identity string, now time.Time) circuitBreakerStateKind {
	if identity == "" {
		return circuitClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.recordRestartLocked(identity, now)
}

func (b *sessionCircuitBreaker) recordRestartLocked(identity string, now time.Time) circuitBreakerStateKind {
	e := b.entries[identity]
	if e == nil {
		e = &circuitBreakerEntry{}
		b.entries[identity] = e
	}
	b.maybeAutoResetLocked(e, now)
	if e.state == circuitOpen {
		return e.state
	}
	e.restarts = append(e.restarts, now)
	e.lastRestart = now
	b.trimLocked(e, now)

	if len(e.restarts) > b.cfg.MaxRestarts {
		// No progress signal inside the window = trip the breaker. A
		// progress event that landed inside the window keeps us CLOSED.
		if !progressWithinWindow(e, now, b.cfg.Window) {
			e.state = circuitOpen
			e.openedAt = now
			e.openRestartCnt = len(e.restarts)
		}
	}
	return e.state
}

// RecordProgress records an observable progress signal (a bead state
// transition attributable to the identity) at time `now`. Progress events
// do NOT clear an already-OPEN breaker — only automatic reset or the manual
// reset knob can do that — but they do keep a CLOSED breaker from tripping
// even if restarts accumulate.
func (b *sessionCircuitBreaker) RecordProgress(identity string, now time.Time) {
	if identity == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[identity]
	if e == nil {
		e = &circuitBreakerEntry{}
		b.entries[identity] = e
	}
	e.lastProgress = now
}

// ObserveProgressSignature records an arbitrary opaque signature
// describing what the reconciler sees for `identity` (typically a digest of
// its assigned beads' statuses). If the signature has changed since the
// last observation, that counts as a progress event. The first observation
// is NOT counted as progress (there is nothing to compare against yet);
// the reconciler's very first tick after process start should not magically
// reset a breaker that is already OPEN.
func (b *sessionCircuitBreaker) ObserveProgressSignature(identity, sig string, now time.Time) bool {
	if identity == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[identity]
	if e == nil {
		if sig == "" {
			return false
		}
		e = &circuitBreakerEntry{progressSig: sig, observedSig: true, lastObserved: now}
		b.entries[identity] = e
		return true
	}
	e.lastObserved = now
	if !e.observedSig {
		e.progressSig = sig
		e.observedSig = true
		return true
	}
	if e.progressSig != sig {
		e.progressSig = sig
		e.lastProgress = now
		return true
	}
	return false
}

func (b *sessionCircuitBreaker) restoreFromMetadata(identity string, meta map[string]string, now time.Time) (bool, error) {
	if identity == "" || len(meta) == 0 {
		return false, nil
	}
	if !hasSessionCircuitMetadata(meta) {
		return false, nil
	}
	resetGeneration, err := parseCircuitResetGeneration(meta[sessionCircuitResetGenerationMetadata])
	if err != nil {
		return false, err
	}

	e := &circuitBreakerEntry{
		progressSig: meta[sessionCircuitProgressSignatureMetadata],
	}
	if e.restarts, err = parseCircuitTimeList(meta[sessionCircuitRestartsMetadata]); err != nil {
		return false, fmt.Errorf("parsing %s: %w", sessionCircuitRestartsMetadata, err)
	}
	if e.lastRestart, err = parseCircuitTime(meta[sessionCircuitLastRestartMetadata]); err != nil {
		return false, fmt.Errorf("parsing %s: %w", sessionCircuitLastRestartMetadata, err)
	}
	if e.lastProgress, err = parseCircuitTime(meta[sessionCircuitLastProgressMetadata]); err != nil {
		return false, fmt.Errorf("parsing %s: %w", sessionCircuitLastProgressMetadata, err)
	}
	if e.lastObserved, err = parseCircuitTime(meta[sessionCircuitLastObservedMetadata]); err != nil {
		return false, fmt.Errorf("parsing %s: %w", sessionCircuitLastObservedMetadata, err)
	}
	if e.openedAt, err = parseCircuitTime(meta[sessionCircuitOpenedAtMetadata]); err != nil {
		return false, fmt.Errorf("parsing %s: %w", sessionCircuitOpenedAtMetadata, err)
	}
	if s := strings.TrimSpace(meta[sessionCircuitOpenRestartCountMetadata]); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return false, fmt.Errorf("parsing %s: %w", sessionCircuitOpenRestartCountMetadata, err)
		}
		e.openRestartCnt = n
	}
	e.observedSig = !e.lastObserved.IsZero() || strings.TrimSpace(e.progressSig) != ""
	switch meta[sessionCircuitStateMetadata] {
	case circuitOpen.String():
		e.state = circuitOpen
	case "", circuitClosed.String():
		e.state = circuitClosed
	default:
		return false, fmt.Errorf("parsing %s: unknown state %q", sessionCircuitStateMetadata, meta[sessionCircuitStateMetadata])
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	currentGeneration := b.resetGenerationLocked(identity)
	if resetGeneration < currentGeneration {
		return false, nil
	}
	if b.entries[identity] != nil {
		return false, nil
	}
	reset := b.maybeAutoResetLocked(e, now)
	b.trimLocked(e, now)
	b.entries[identity] = e
	return reset, nil
}

func hasSessionCircuitMetadata(meta map[string]string) bool {
	for _, key := range sessionCircuitMetadataKeys {
		if key == sessionCircuitResetGenerationMetadata {
			continue
		}
		if strings.TrimSpace(meta[key]) != "" {
			return true
		}
	}
	return false
}

func parseCircuitResetGeneration(value string) (uint64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	generation, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", sessionCircuitResetGenerationMetadata, err)
	}
	return generation, nil
}

func parseCircuitTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}

func parseCircuitTimeList(value string) ([]time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	var raw []string
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, err
	}
	out := make([]time.Time, 0, len(raw))
	for _, s := range raw {
		tm, err := parseCircuitTime(s)
		if err != nil {
			return nil, err
		}
		if !tm.IsZero() {
			out = append(out, tm)
		}
	}
	return out, nil
}

// pruneIdle removes stale entries that were created only to remember progress
// signatures for configured sessions that never restarted. It bounds map
// growth when named-session configuration changes over a long-running
// supervisor process.
func (b *sessionCircuitBreaker) pruneIdle(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, e := range b.entries {
		if e.state != circuitClosed || !e.lastRestart.IsZero() || e.lastObserved.IsZero() {
			continue
		}
		if now.Sub(e.lastObserved) >= b.cfg.ResetAfter {
			delete(b.entries, id)
			// Keep resetGenerations: it is the stale-snapshot rejection floor
			// for this identity if the named session is later configured again.
		}
	}
}

// IsOpen returns true if the breaker for `identity` is currently OPEN and
// the reconciler MUST NOT materialize or spawn the session. The call may
// transition the entry to CLOSED if the cooldown has elapsed.
func (b *sessionCircuitBreaker) IsOpen(identity string, now time.Time) bool {
	if identity == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[identity]
	if e == nil {
		return false
	}
	b.maybeAutoResetLocked(e, now)
	return e.state == circuitOpen
}

// LogOpenOnce writes a loud ERROR-level message the first time a given
// OPEN breaker is observed during respawn suppression. The message tells
// operators exactly how to clear the state. Subsequent calls for the same
// OPEN incident are suppressed to avoid log floods (the supervisor may
// re-check the breaker on every tick).
func (b *sessionCircuitBreaker) LogOpenOnce(identity string, w io.Writer) {
	if identity == "" || w == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[identity]
	if e == nil || e.state != circuitOpen || e.loggedOpenOnce {
		return
	}
	e.loggedOpenOnce = true
	fmt.Fprintf(w, //nolint:errcheck // best-effort stderr
		"ERROR session-circuit-breaker: CIRCUIT_OPEN for named session %q (restarts=%d in last %s, no progress). "+
			"Supervisor will NOT respawn. Run `gc session reset %s` to clear.\n",
		identity, e.openRestartCnt, b.cfg.Window, identity)
}

// Reset forces the entry for `identity` back to CLOSED, discards any
// accumulated restart history, and advances the reset generation used to
// reject stale reconciler metadata snapshots.
func (b *sessionCircuitBreaker) Reset(identity string) uint64 {
	if identity == "" {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, identity)
	b.resetGenerations[identity] = b.resetGenerationLocked(identity) + 1
	return b.resetGenerations[identity]
}

func (b *sessionCircuitBreaker) observeResetGenerationFromMetadata(identity string, meta map[string]string) error {
	if b == nil || identity == "" || len(meta) == 0 {
		return nil
	}
	generation, err := parseCircuitResetGeneration(meta[sessionCircuitResetGenerationMetadata])
	if err != nil {
		return err
	}
	b.observeResetGeneration(identity, generation)
	return nil
}

func (b *sessionCircuitBreaker) observeResetGeneration(identity string, generation uint64) {
	if b == nil || identity == "" || generation == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if generation > b.resetGenerationLocked(identity) {
		b.resetGenerations[identity] = generation
	}
}

func (b *sessionCircuitBreaker) resetGenerationLocked(identity string) uint64 {
	if b.resetGenerations == nil {
		b.resetGenerations = make(map[string]uint64)
	}
	return b.resetGenerations[identity]
}

func (b *sessionCircuitBreaker) metadata(identity string, now time.Time) (map[string]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.metadataLocked(identity, now)
}

func (b *sessionCircuitBreaker) metadataLocked(identity string, now time.Time) (map[string]string, error) {
	out := emptySessionCircuitMetadata()
	if identity == "" {
		return out, nil
	}

	e := b.entries[identity]
	if e == nil {
		return out, nil
	}
	b.maybeAutoResetLocked(e, now)
	b.trimLocked(e, now)
	restarts := make([]string, 0, len(e.restarts))
	for _, tm := range e.restarts {
		restarts = append(restarts, tm.UTC().Format(time.RFC3339Nano))
	}
	if len(restarts) > 0 {
		data, err := json.Marshal(restarts)
		if err != nil {
			return nil, fmt.Errorf("encoding restart history: %w", err)
		}
		out[sessionCircuitRestartsMetadata] = string(data)
	}
	out[sessionCircuitStateMetadata] = e.state.String()
	out[sessionCircuitLastRestartMetadata] = formatCircuitTime(e.lastRestart)
	out[sessionCircuitLastProgressMetadata] = formatCircuitTime(e.lastProgress)
	out[sessionCircuitLastObservedMetadata] = formatCircuitTime(e.lastObserved)
	out[sessionCircuitProgressSignatureMetadata] = e.progressSig
	if e.state == circuitOpen {
		out[sessionCircuitOpenedAtMetadata] = formatCircuitTime(e.openedAt)
		out[sessionCircuitOpenRestartCountMetadata] = strconv.Itoa(e.openRestartCnt)
	}
	if generation := b.resetGenerationLocked(identity); generation > 0 {
		out[sessionCircuitResetGenerationMetadata] = strconv.FormatUint(generation, 10)
	}
	return out, nil
}

func emptySessionCircuitMetadata() map[string]string {
	out := make(map[string]string, len(sessionCircuitMetadataKeys))
	for _, key := range sessionCircuitMetadataKeys {
		out[key] = ""
	}
	return out
}

func formatCircuitTime(tm time.Time) string {
	if tm.IsZero() {
		return ""
	}
	return tm.UTC().Format(time.RFC3339Nano)
}

func persistSessionCircuitBreakerMetadata(
	store beads.Store,
	session *beads.Bead,
	cb *sessionCircuitBreaker,
	identity string,
	now time.Time,
) error {
	if store == nil || session == nil || cb == nil {
		return nil
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	metadata, err := cb.metadataLocked(identity, now)
	if err != nil {
		return err
	}
	if sessionCircuitMetadataEqual(session.Metadata, metadata) {
		return nil
	}
	if err := store.SetMetadataBatch(session.ID, metadata); err != nil {
		return fmt.Errorf("persisting session circuit breaker metadata for %s: %w", session.ID, err)
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, len(metadata))
	}
	for key, value := range metadata {
		session.Metadata[key] = value
	}
	return nil
}

func recordSessionCircuitBreakerRestart(
	store beads.Store,
	session *beads.Bead,
	cb *sessionCircuitBreaker,
	identity string,
	now time.Time,
) (circuitBreakerStateKind, error) {
	if store == nil || session == nil {
		return circuitClosed, nil
	}
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return circuitClosed, nil
	}
	if cb == nil {
		cb = defaultSessionCircuitBreaker()
	}

	cb.mu.Lock()
	defer cb.mu.Unlock()
	previous, hadPrevious := cloneCircuitBreakerEntry(cb.entries[identity]), cb.entries[identity] != nil
	state := cb.recordRestartLocked(identity, now)
	metadata, err := cb.metadataLocked(identity, now)
	if err != nil {
		cb.restoreEntryLocked(identity, previous, hadPrevious)
		return state, err
	}
	if sessionCircuitMetadataEqual(session.Metadata, metadata) {
		return state, nil
	}
	if err := store.SetMetadataBatch(session.ID, metadata); err != nil {
		cb.restoreEntryLocked(identity, previous, hadPrevious)
		return state, fmt.Errorf("persisting session circuit breaker metadata for %s: %w", session.ID, err)
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, len(metadata))
	}
	for key, value := range metadata {
		session.Metadata[key] = value
	}
	return state, nil
}

func cloneCircuitBreakerEntry(e *circuitBreakerEntry) *circuitBreakerEntry {
	if e == nil {
		return nil
	}
	clone := *e
	if e.restarts != nil {
		clone.restarts = append([]time.Time(nil), e.restarts...)
	}
	return &clone
}

func (b *sessionCircuitBreaker) restoreEntryLocked(identity string, entry *circuitBreakerEntry, existed bool) {
	if existed {
		b.entries[identity] = entry
		return
	}
	delete(b.entries, identity)
}

func (b *sessionCircuitBreaker) snapshotIdentity(identity string) sessionCircuitBreakerIdentitySnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, hadEntry := b.entries[identity]
	generation, hadGeneration := b.resetGenerations[identity]
	return sessionCircuitBreakerIdentitySnapshot{
		entry:         cloneCircuitBreakerEntry(entry),
		hadEntry:      hadEntry,
		generation:    generation,
		hadGeneration: hadGeneration,
	}
}

func (b *sessionCircuitBreaker) restoreIdentity(identity string, snapshot sessionCircuitBreakerIdentitySnapshot) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.restoreEntryLocked(identity, snapshot.entry, snapshot.hadEntry)
	if snapshot.hadGeneration {
		b.resetGenerations[identity] = snapshot.generation
		return
	}
	delete(b.resetGenerations, identity)
}

func sessionCircuitMetadataEqual(existing map[string]string, next map[string]string) bool {
	for _, key := range sessionCircuitMetadataKeys {
		if existing[key] != next[key] {
			return false
		}
	}
	return true
}

func loadPersistedSessionCircuitResetGeneration(store beads.Store, sessionID, identity string, cb *sessionCircuitBreaker) error {
	if store == nil || cb == nil || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(identity) == "" {
		return nil
	}
	session, err := store.Get(sessionID)
	if err != nil {
		return fmt.Errorf("loading session circuit breaker metadata for %s: %w", sessionID, err)
	}
	if err := cb.observeResetGenerationFromMetadata(identity, session.Metadata); err != nil {
		return fmt.Errorf("loading session circuit breaker reset generation for %s: %w", sessionID, err)
	}
	return nil
}

func clearPersistedSessionCircuitBreakerMetadata(store beads.Store, sessionID string, resetGeneration uint64) error {
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	metadata := make(map[string]string, len(sessionCircuitMetadataKeys))
	for _, key := range sessionCircuitMetadataKeys {
		metadata[key] = ""
	}
	if resetGeneration > 0 {
		metadata[sessionCircuitResetGenerationMetadata] = strconv.FormatUint(resetGeneration, 10)
	}
	if err := store.SetMetadataBatch(sessionID, metadata); err != nil {
		return fmt.Errorf("clearing session circuit breaker metadata for %s: %w", sessionID, err)
	}
	return nil
}

// Snapshot returns a stable-ordered point-in-time view of all tracked
// identities. Used by status output and by tests.
func (b *sessionCircuitBreaker) Snapshot(now time.Time) []CircuitBreakerSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]CircuitBreakerSnapshot, 0, len(b.entries))
	for id, e := range b.entries {
		b.maybeAutoResetLocked(e, now)
		b.trimLocked(e, now)
		snap := CircuitBreakerSnapshot{
			Identity:     id,
			State:        e.state.String(),
			RestartCount: len(e.restarts),
			LastRestart:  e.lastRestart,
			LastProgress: e.lastProgress,
		}
		if len(e.restarts) > 0 {
			snap.WindowStart = e.restarts[0]
		}
		if e.state == circuitOpen {
			snap.OpenedAt = e.openedAt
			snap.OpenRestartCount = e.openRestartCnt
			if !e.lastRestart.IsZero() {
				snap.ResetAfter = e.lastRestart.Add(b.cfg.ResetAfter)
			}
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity < out[j].Identity })
	return out
}

// progressWithinWindow reports whether a progress event is recent enough
// to keep the breaker CLOSED. "Recent enough" means "no earlier than the
// start of the current restart rolling window", which is `now - window`.
func progressWithinWindow(e *circuitBreakerEntry, now time.Time, window time.Duration) bool {
	if e.lastProgress.IsZero() {
		return false
	}
	return !e.lastProgress.Before(now.Add(-window))
}

// -----------------------------------------------------------------------------
// Package-level singleton used by the reconciler. Kept as an indirection so
// tests can swap it out without threading a new parameter through every
// reconcileSessionBeads call site.
// -----------------------------------------------------------------------------

var (
	sessionCircuitBreakerMu        sync.Mutex
	sessionCircuitBreakerSingleton *sessionCircuitBreaker
)

// defaultSessionCircuitBreaker returns the process-wide breaker, lazily
// constructing it with defaults on first use.
func defaultSessionCircuitBreaker() *sessionCircuitBreaker {
	sessionCircuitBreakerMu.Lock()
	defer sessionCircuitBreakerMu.Unlock()
	if sessionCircuitBreakerSingleton == nil {
		sessionCircuitBreakerSingleton = newSessionCircuitBreaker(sessionCircuitBreakerConfig{})
	}
	return sessionCircuitBreakerSingleton
}

// setSessionCircuitBreakerForTest swaps the singleton, returning a cleanup
// function that restores the previous value. Tests call this to inject a
// fake-clocked breaker without touching production wiring.
func setSessionCircuitBreakerForTest(b *sessionCircuitBreaker) func() {
	sessionCircuitBreakerMu.Lock()
	prev := sessionCircuitBreakerSingleton
	sessionCircuitBreakerSingleton = b
	sessionCircuitBreakerMu.Unlock()
	return func() {
		sessionCircuitBreakerMu.Lock()
		sessionCircuitBreakerSingleton = prev
		sessionCircuitBreakerMu.Unlock()
	}
}

// computeNamedSessionProgressSignatures returns a signature per named
// session identity derived from the identities of its assigned work beads
// and their statuses. A signature change between reconciler ticks means a
// bead changed status (open -> in_progress, in_progress -> closed, a new
// bead was routed, an old one dropped, etc.), which is treated as a
// progress signal by the circuit breaker.
//
// Assignee on a work bead may be a bead ID, a session name, or an alias;
// we resolve to the named-session identity via session bead metadata the
// same way the rest of the reconciler does.
func computeNamedSessionProgressSignatures(
	sessionBeads []beads.Bead,
	assignedWorkBeads []beads.Bead,
) map[string]string {
	if len(sessionBeads) == 0 {
		return nil
	}
	// Build: resolver key -> identity. Bare session names and aliases are
	// ignored when more than one configured identity claims the same key.
	resolve := make(map[string]string, len(sessionBeads)*3)
	bareResolve := make(map[string]string, len(sessionBeads)*2)
	ambiguous := make(map[string]bool)
	knownIdentities := make(map[string]bool)
	for _, sb := range sessionBeads {
		identity := strings.TrimSpace(sb.Metadata[namedSessionIdentityMetadata])
		if identity == "" {
			continue
		}
		knownIdentities[identity] = true
		resolve[identity] = identity
		if id := strings.TrimSpace(sb.ID); id != "" {
			resolve[id] = identity
		}
		if sn := strings.TrimSpace(sb.Metadata["session_name"]); sn != "" {
			addSessionCircuitResolverKey(bareResolve, ambiguous, sn, identity)
		}
		if alias := strings.TrimSpace(sb.Metadata["alias"]); alias != "" {
			addSessionCircuitResolverKey(bareResolve, ambiguous, alias, identity)
		}
	}
	if len(knownIdentities) == 0 {
		return nil
	}
	for key, identity := range bareResolve {
		if ambiguous[key] {
			continue
		}
		if _, exact := resolve[key]; exact {
			continue
		}
		resolve[key] = identity
	}

	// Gather per-identity (beadID, status) pairs.
	perIdentity := make(map[string][]string, len(knownIdentities))
	for _, wb := range assignedWorkBeads {
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" {
			continue
		}
		identity, ok := resolve[assignee]
		if !ok {
			continue
		}
		perIdentity[identity] = append(perIdentity[identity],
			wb.ID+"="+wb.Status)
	}

	out := make(map[string]string, len(knownIdentities))
	for identity := range knownIdentities {
		pairs := perIdentity[identity]
		if len(pairs) == 0 {
			out[identity] = ""
			continue
		}
		sort.Strings(pairs)
		h := sha1.Sum([]byte(strings.Join(pairs, "|")))
		out[identity] = hex.EncodeToString(h[:])
	}
	return out
}

func addSessionCircuitResolverKey(resolve map[string]string, ambiguous map[string]bool, key, identity string) {
	if existing, ok := resolve[key]; ok && existing != identity {
		delete(resolve, key)
		ambiguous[key] = true
		return
	}
	if ambiguous[key] {
		return
	}
	resolve[key] = identity
}

func sessionCircuitBreakerSnapshot(now time.Time) []CircuitBreakerSnapshot {
	return defaultSessionCircuitBreaker().Snapshot(now)
}
