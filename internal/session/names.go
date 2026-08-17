package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
)

var (
	// ErrInvalidSessionName reports a malformed explicit session name.
	ErrInvalidSessionName = errors.New("invalid session name")
	// ErrSessionNameExists reports that a session name is already reserved by
	// another session bead and therefore cannot be reused.
	ErrSessionNameExists = errors.New("session name already exists")
	// ErrInvalidSessionAlias reports a malformed human-chosen session alias.
	ErrInvalidSessionAlias = errors.New("invalid session alias")
	// ErrSessionAliasExists reports that a live session already owns the alias.
	ErrSessionAliasExists = errors.New("session alias already exists")
)

var (
	sessionNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
	// sessionAliasPattern allows dots so that V2 import-bound identities
	// (e.g. "gastown.mayor") are legal as user-facing session aliases.
	// Session names themselves stay tmux-safe via SanitizeQualifiedNameForSession.
	sessionAliasPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*(/[a-zA-Z0-9][a-zA-Z0-9_.-]*)*$`)
	sessionIDPattern    = regexp.MustCompile(`^gc-[0-9]+$`)
)

const (
	explicitSessionNameMaxLen = 64
	autoSessionNamePrefix     = "s-"
)

type sessionIdentifierReservationLockEntry struct {
	mu   sync.Mutex
	refs int
}

type sessionAttachLeaseEntry struct {
	mu   sync.RWMutex
	refs int
}

var (
	sessionIdentifierReservationLocksMu sync.Mutex
	sessionIdentifierReservationLocks   = map[string]*sessionIdentifierReservationLockEntry{}
	sessionAttachLeasesMu               sync.Mutex
	sessionAttachLeases                 = map[string]*sessionAttachLeaseEntry{}
)

// IsSessionNameSyntaxValid reports whether a persisted session_name uses the
// allowed character set. It intentionally does not enforce explicit-name-only
// business rules like reserved prefixes.
func IsSessionNameSyntaxValid(name string) bool {
	return sessionNamePattern.MatchString(name)
}

// ValidateExplicitName validates a human-chosen session name. Empty means
// "let the system derive one".
func ValidateExplicitName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if len(name) > explicitSessionNameMaxLen {
		return "", fmt.Errorf("%w: %q exceeds max length %d", ErrInvalidSessionName, name, explicitSessionNameMaxLen)
	}
	if strings.HasPrefix(name, autoSessionNamePrefix) {
		return "", fmt.Errorf("%w: %q uses reserved prefix %q", ErrInvalidSessionName, name, autoSessionNamePrefix)
	}
	if !IsSessionNameSyntaxValid(name) {
		return "", fmt.Errorf("%w: %q", ErrInvalidSessionName, name)
	}
	return name, nil
}

// GenerateAdhocExplicitName produces a tmux-safe explicit session name for
// multi-session templates that are materialized without a user alias.
func GenerateAdhocExplicitName(base string) (string, error) {
	token, err := GenerateSessionKey()
	if err != nil {
		return "", fmt.Errorf("generate pooled session identity: %w", err)
	}
	compact := strings.ReplaceAll(token, "-", "")
	if len(compact) > 10 {
		compact = compact[:10]
	}
	base = strings.TrimSpace(base)
	if base == "" {
		base = "session"
	}
	suffix := "-adhoc-" + compact
	maxBaseLen := explicitSessionNameMaxLen - len(suffix)
	if maxBaseLen < 1 {
		maxBaseLen = 1
	}
	if len(base) > maxBaseLen {
		base = base[:maxBaseLen]
	}
	return ValidateExplicitName(base + suffix)
}

// GenerateAdhocIdentity produces a stable, MCP-safe per-session identity for
// aliasless sessions that still need a concrete unique name for templating.
func GenerateAdhocIdentity(base string) (string, error) {
	token, err := GenerateSessionKey()
	if err != nil {
		return "", fmt.Errorf("generate adhoc identity: %w", err)
	}
	compact := strings.ReplaceAll(token, "-", "")
	if len(compact) > 10 {
		compact = compact[:10]
	}
	base = agent.SanitizeQualifiedNameForSession(strings.TrimSpace(base))
	if base == "" {
		base = "session"
	}
	return base + "-adhoc-" + compact, nil
}

// ValidateAlias validates a human-chosen session alias. Empty means
// "no alias".
func ValidateAlias(alias string) (string, error) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return "", nil
	}
	if len(alias) > explicitSessionNameMaxLen {
		return "", fmt.Errorf("%w: %q exceeds max length %d", ErrInvalidSessionAlias, alias, explicitSessionNameMaxLen)
	}
	if strings.HasPrefix(alias, autoSessionNamePrefix) {
		return "", fmt.Errorf("%w: %q uses reserved prefix %q", ErrInvalidSessionAlias, alias, autoSessionNamePrefix)
	}
	if alias == "human" {
		return "", fmt.Errorf("%w: %q is reserved", ErrInvalidSessionAlias, alias)
	}
	if sessionIDPattern.MatchString(alias) {
		return "", fmt.Errorf("%w: %q conflicts with session ID syntax", ErrInvalidSessionAlias, alias)
	}
	if !sessionAliasPattern.MatchString(alias) {
		return "", fmt.Errorf("%w: %q", ErrInvalidSessionAlias, alias)
	}
	return alias, nil
}

// EnsureAliasAvailable reports whether alias can be assigned to a live
// session without colliding with another alias or runtime session name.
func EnsureAliasAvailable(store beads.Store, alias, selfID string) error {
	return ensureSessionAliasAvailable(store, nil, alias, selfID, "")
}

// EnsureAliasAvailableWithConfig extends alias reservation checks with
// configured named-session aliases so public targets cannot be squatted
// before their managed session bead exists.
func EnsureAliasAvailableWithConfig(store beads.Store, cfg *config.City, alias, selfID string) error {
	return ensureSessionAliasAvailable(store, cfg, alias, selfID, "")
}

// EnsureAliasAvailableWithConfigForOwner extends alias reservation checks
// with an explicit configured owner identity so callers creating a new
// managed session bead can reserve that alias before a bead ID exists.
func EnsureAliasAvailableWithConfigForOwner(store beads.Store, cfg *config.City, alias, selfID, selfOwner string) error {
	return ensureSessionAliasAvailable(store, cfg, alias, selfID, selfOwner)
}

// EnsureSessionNameAvailableWithConfig extends session-name reservation checks
// with configured named-session runtime names.
func EnsureSessionNameAvailableWithConfig(store beads.Store, cfg *config.City, name, selfID string) error {
	return ensureConfiguredSessionNameAvailable(store, cfg, name, selfID, "")
}

// EnsureSessionNameAvailableWithConfigForOwner extends session-name
// reservation checks with an explicit configured named-session owner.
func EnsureSessionNameAvailableWithConfigForOwner(store beads.Store, cfg *config.City, name, selfID, selfOwner string) error {
	return ensureConfiguredSessionNameAvailable(store, cfg, name, selfID, selfOwner)
}

func withSessionAliasReservationLock(alias string, fn func() error) error {
	return withSessionIdentifierReservationLock(alias, fn)
}

func withSessionIdentifierReservationLock(identifier string, fn func() error) error {
	if identifier == "" {
		return fn()
	}
	lock := acquireSessionIdentifierReservationLock(identifier)
	defer releaseSessionIdentifierReservationLock(identifier, lock)
	return fn()
}

func withSessionIdentifierReservationLocks(identifiers []string, fn func() error) error {
	identifiers = normalizeSessionIdentifiers(identifiers...)
	if len(identifiers) == 0 {
		return fn()
	}
	locks := make([]*sessionIdentifierReservationLockEntry, 0, len(identifiers))
	for _, identifier := range identifiers {
		locks = append(locks, acquireSessionIdentifierReservationLock(identifier))
	}
	defer func() {
		for i := len(identifiers) - 1; i >= 0; i-- {
			releaseSessionIdentifierReservationLock(identifiers[i], locks[i])
		}
	}()
	return fn()
}

func normalizeSessionIdentifiers(values ...string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func acquireSessionIdentifierReservationLock(identifier string) *sessionIdentifierReservationLockEntry {
	sessionIdentifierReservationLocksMu.Lock()
	lock := sessionIdentifierReservationLocks[identifier]
	if lock == nil {
		lock = &sessionIdentifierReservationLockEntry{}
		sessionIdentifierReservationLocks[identifier] = lock
	}
	lock.refs++
	sessionIdentifierReservationLocksMu.Unlock()

	lock.mu.Lock()
	return lock
}

func releaseSessionIdentifierReservationLock(identifier string, lock *sessionIdentifierReservationLockEntry) {
	lock.mu.Unlock()

	sessionIdentifierReservationLocksMu.Lock()
	lock.refs--
	if lock.refs == 0 {
		delete(sessionIdentifierReservationLocks, identifier)
	}
	sessionIdentifierReservationLocksMu.Unlock()
}

// WithCitySessionNameLock serializes operations that reserve a session name
// within a city, preventing concurrent callers from claiming the same name.
func WithCitySessionNameLock(cityPath, name string, fn func() error) error {
	return withCitySessionIdentifierLock(cityPath, name, fn)
}

// WithCitySessionAliasLock serializes operations that reserve a session alias
// within a city, preventing concurrent callers from claiming the same alias.
func WithCitySessionAliasLock(cityPath, alias string, fn func() error) error {
	return withCitySessionIdentifierLock(cityPath, alias, fn)
}

// WithCitySessionIdentifierLocks serializes operations that reserve multiple
// identifiers within a city, acquiring deterministic lock order to prevent
// deadlocks across concurrent creators.
func WithCitySessionIdentifierLocks(cityPath string, identifiers []string, fn func() error) error {
	identifiers = normalizeSessionIdentifiers(identifiers...)
	if len(identifiers) == 0 {
		return fn()
	}
	var lockRecursive func(idx int) error
	lockRecursive = func(idx int) error {
		if idx >= len(identifiers) {
			return fn()
		}
		return withCitySessionIdentifierLock(cityPath, identifiers[idx], func() error {
			return lockRecursive(idx + 1)
		})
	}
	return lockRecursive(0)
}

// WithCitySessionLifecycleLock serializes destructive lifecycle finalization
// with explicit operator mutations for one durable session bead. With a city
// path it uses the same cross-process flock substrate as identifier reservation;
// without one it falls back to the process-local keyed mutex used by unit tests
// and unmanaged callers. Every production wake/suspend path that can race the
// managed reconciler must participate in this lock before reading or writing
// lifecycle state.
//
// The session bead ID, rather than its mutable alias/runtime name, is the lock
// identity. It remains stable across wake, suspend, provider restart, and alias
// changes, so all lifecycle participants converge on one lock file.
func WithCitySessionLifecycleLock(cityPath, sessionID string, fn func() error) error {
	return WithCitySessionIdentifierLocks(cityPath, []string{sessionID}, fn)
}

type citySessionLifecycleLockContextKey struct{}

type citySessionLifecycleLockContextValue struct {
	cityPath  string
	sessionID string
	active    atomic.Bool
}

// WithCitySessionLifecycleLockContext is the context-aware form of
// [WithCitySessionLifecycleLock]. The callback context proves ownership of the
// exact city/session lock to lower session and worker layers, allowing a
// multi-layer lifecycle transaction to call Manager operations without trying
// to acquire the non-reentrant flock a second time.
//
// The proof is intentionally unexported and exact: callers cannot manufacture
// one, and a context for another city or session never suppresses acquisition.
func WithCitySessionLifecycleLockContext(ctx context.Context, cityPath, sessionID string, fn func(context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		acquired, err := TryWithCitySessionLifecycleLock(cityPath, sessionID, func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			proof := &citySessionLifecycleLockContextValue{
				cityPath:  strings.TrimSpace(cityPath),
				sessionID: strings.TrimSpace(sessionID),
			}
			proof.active.Store(true)
			defer proof.active.Store(false)
			owned := context.WithValue(ctx, citySessionLifecycleLockContextKey{}, proof)
			return fn(owned)
		})
		if err != nil {
			return err
		}
		if acquired {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func citySessionLifecycleLockHeld(ctx context.Context, cityPath, sessionID string) bool {
	if ctx == nil {
		return false
	}
	owned, ok := ctx.Value(citySessionLifecycleLockContextKey{}).(*citySessionLifecycleLockContextValue)
	return ok && owned != nil && owned.active.Load() && owned.cityPath == strings.TrimSpace(cityPath) && owned.sessionID == strings.TrimSpace(sessionID)
}

// TryWithCitySessionLifecycleLock is the non-blocking finalizer form of
// WithCitySessionLifecycleLock. A false acquired result is not an error: a
// wake/start/suspend currently owns the session, so the reconciler should fail
// closed and retry on a later tick instead of stalling the whole city loop.
func TryWithCitySessionLifecycleLock(cityPath, sessionID string, fn func() error) (acquired bool, err error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return true, fn()
	}
	if strings.TrimSpace(cityPath) == "" {
		lock, ok := tryAcquireSessionIdentifierReservationLock(sessionID)
		if !ok {
			return false, nil
		}
		defer releaseSessionIdentifierReservationLock(sessionID, lock)
		return true, fn()
	}
	lockPath := filepath.Join(citylayout.SessionNameLocksDir(cityPath), sessionIdentifierLockFileName(sessionID)+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return false, fmt.Errorf("creating session lifecycle lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, fmt.Errorf("opening session lifecycle lock: %w", err)
	}
	defer f.Close() //nolint:errcheck // best-effort cleanup
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil
		}
		return false, fmt.Errorf("locking session lifecycle %q: %w", sessionID, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // best-effort unlock
	return true, fn()
}

// TryAcquireCitySessionAttachLease acquires the crash-recoverable lease that
// spans a blocking provider Attach call. Attach takes this lease while it still
// owns the short lifecycle lock, then releases the lifecycle lock before
// entering the terminal. Destructive/latch paths take the lifecycle lock first
// and try this same lease; failure means an attach already won and they defer.
//
// The production lease is an OS flock, so process death releases it without a
// durable owner record or timeout guess. The empty-city test/unmanaged fallback
// uses the existing process-local keyed mutex under a disjoint key.
func TryAcquireCitySessionAttachLease(cityPath, sessionID string) (release func(), acquired bool, err error) {
	return tryAcquireCitySessionAttachLease(cityPath, sessionID, false)
}

// TryAcquireCitySessionAttachExclusiveLease is the destructive-side probe.
// It conflicts with every shared Attach lease and is held only for the short
// quiet/identity check plus stop/close mutation.
func TryAcquireCitySessionAttachExclusiveLease(cityPath, sessionID string) (release func(), acquired bool, err error) {
	return tryAcquireCitySessionAttachLease(cityPath, sessionID, true)
}

func tryAcquireCitySessionAttachLease(cityPath, sessionID string, exclusive bool) (release func(), acquired bool, err error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return func() {}, true, nil
	}
	leaseID := "attach-lease:" + sessionID
	if strings.TrimSpace(cityPath) == "" {
		sessionAttachLeasesMu.Lock()
		lock := sessionAttachLeases[leaseID]
		if lock == nil {
			lock = &sessionAttachLeaseEntry{}
			sessionAttachLeases[leaseID] = lock
		}
		lock.refs++
		sessionAttachLeasesMu.Unlock()
		ok := false
		if exclusive {
			ok = lock.mu.TryLock()
		} else {
			ok = lock.mu.TryRLock()
		}
		if !ok {
			sessionAttachLeasesMu.Lock()
			lock.refs--
			if lock.refs == 0 {
				delete(sessionAttachLeases, leaseID)
			}
			sessionAttachLeasesMu.Unlock()
			return nil, false, nil
		}
		var once sync.Once
		return func() {
			once.Do(func() {
				if exclusive {
					lock.mu.Unlock()
				} else {
					lock.mu.RUnlock()
				}
				sessionAttachLeasesMu.Lock()
				lock.refs--
				if lock.refs == 0 {
					delete(sessionAttachLeases, leaseID)
				}
				sessionAttachLeasesMu.Unlock()
			})
		}, true, nil
	}
	lockPath := filepath.Join(citylayout.SessionNameLocksDir(cityPath), sessionIdentifierLockFileName(leaseID)+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, false, fmt.Errorf("creating session attach lease dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("opening session attach lease: %w", err)
	}
	mode := syscall.LOCK_SH | syscall.LOCK_NB
	if exclusive {
		mode = syscall.LOCK_EX | syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), mode); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("locking session attach lease %q: %w", sessionID, err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}, true, nil
}

// TryWithCitySessionDestructiveLock is the background stop/close/latch
// ordering boundary: lifecycle exclusive first, then a nonblocking exclusive
// attach lease. attachBusy reports that an interactive Attach owns a shared
// lease; callers must defer without provider or durable lifecycle mutation.
func TryWithCitySessionDestructiveLock(cityPath, sessionID string, fn func() error) (acquired, attachBusy bool, err error) {
	acquired, err = TryWithCitySessionLifecycleLock(cityPath, sessionID, func() error {
		release, leaseAcquired, leaseErr := TryAcquireCitySessionAttachExclusiveLease(cityPath, sessionID)
		if leaseErr != nil {
			return leaseErr
		}
		if !leaseAcquired {
			attachBusy = true
			return nil
		}
		defer release()
		if fn == nil {
			return nil
		}
		return fn()
	})
	return acquired, attachBusy, err
}

func tryAcquireSessionIdentifierReservationLock(identifier string) (*sessionIdentifierReservationLockEntry, bool) {
	sessionIdentifierReservationLocksMu.Lock()
	lock := sessionIdentifierReservationLocks[identifier]
	if lock == nil {
		lock = &sessionIdentifierReservationLockEntry{}
		sessionIdentifierReservationLocks[identifier] = lock
	}
	lock.refs++
	sessionIdentifierReservationLocksMu.Unlock()

	if lock.mu.TryLock() {
		return lock, true
	}
	sessionIdentifierReservationLocksMu.Lock()
	lock.refs--
	if lock.refs == 0 {
		delete(sessionIdentifierReservationLocks, identifier)
	}
	sessionIdentifierReservationLocksMu.Unlock()
	return nil, false
}

func withCitySessionIdentifierLock(cityPath, identifier string, fn func() error) error {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return fn()
	}
	if strings.TrimSpace(cityPath) == "" {
		return withSessionIdentifierReservationLock(identifier, fn)
	}
	lockPath := filepath.Join(citylayout.SessionNameLocksDir(cityPath), sessionIdentifierLockFileName(identifier)+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("creating session identifier lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening session identifier lock: %w", err)
	}
	defer f.Close() //nolint:errcheck // best-effort cleanup
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking session identifier %q: %w", identifier, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // best-effort unlock
	return fn()
}

func sessionIdentifierLockFileName(identifier string) string {
	sum := sha256.Sum256([]byte(identifier))
	return hex.EncodeToString(sum[:])
}

func ensureSessionNameAvailable(store beads.Store, name string) error {
	return ensureSessionNameAvailableForSelf(store, name, "")
}

func ensureSessionNameAvailableForSelf(store beads.Store, name, selfID string) error {
	return ensureSessionNameAvailableForSelfAndOwner(store, name, selfID, "")
}

func ensureSessionNameAvailableForSelfAndOwner(store beads.Store, name, selfID, selfOwner string) error {
	if name == "" {
		return nil
	}
	all, err := ExactMetadataSessionCandidates(store, true,
		map[string]string{"session_name": name},
		map[string]string{"alias": name},
		map[string]string{"agent_name": name},
		map[string]string{"template": name},
		map[string]string{"common_name": name},
	)
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}
	for _, b := range all {
		if !IsSessionBeadOrRepairable(b) {
			continue
		}
		if b.ID == selfID {
			continue
		}
		if failedCreateIdentityReleased(b) {
			continue
		}
		// Explicit session names are permanent identities; once claimed by any
		// session bead, including a closed one, they are never reused.
		//
		// Exception: closed beads that belong to a configured named session
		// release their session_name so the reconciler can re-materialize a
		// fresh canonical bead for the same identity. The design doc specifies:
		// "Closed historical beads do not poison future canonical
		// materialization of the reserved identity." Recognition is by the
		// boolean flag OR the recorded identity (wasConfiguredNamedSession) so a
		// stale/legacy bead missing the flag still releases its name (ga-841) —
		// independent of whether the caller passed a matching selfOwner, since
		// the configured-named reservation is re-enforced by the cfg-aware check
		// in ensureConfiguredSessionNameAvailable.
		if strings.TrimSpace(b.Metadata["session_name"]) == name {
			if continuityIneligibleConfiguredOwner(b, selfOwner) {
				continue
			}
			if b.Status == "closed" && wasConfiguredNamedSession(b) {
				continue
			}
			// A retired ephemeral pool slot must not permanently reserve the
			// name either. The reconciler closes the slot bead without
			// clearing session_name, and a configured named session that was
			// materialized through the pool path lands here with neither the
			// configured_named_session marker nor configured_named_identity,
			// so the release above never fires for it. A dead pool slot must
			// not poison future materialization of the identity it happened
			// to be running as.
			if b.Status == "closed" &&
				strings.TrimSpace(b.Metadata["pool_managed"]) == "true" &&
				strings.TrimSpace(b.Metadata["session_origin"]) == "ephemeral" {
				continue
			}
			return fmt.Errorf("%w: %q already belongs to %s", ErrSessionNameExists, name, b.ID)
		}
		if b.Status == "closed" {
			continue
		}
		if strings.TrimSpace(b.Metadata["alias"]) == name {
			if continuityIneligibleConfiguredOwner(b, selfOwner) {
				continue
			}
			return fmt.Errorf("%w: %q conflicts with live alias on %s", ErrSessionNameExists, name, b.ID)
		}
		// Historical aliases are compatibility-only input and do not reserve
		// namespace for new session-name claims.
		// Identifier collisions are exact-match only: a bare name like
		// "control-dispatcher" does not collide with a qualified sibling like
		// "<rig>/control-dispatcher", since configured multi-rig dispatchers
		// occupy distinct namespaces by design.
		if sessionNameConflictsWithExistingIdentifier(b, name) {
			// Configured named sessions reserve their exact runtime name in
			// config, so a pool-managed backing-template bead must not squat it.
			if configuredOwnerCanReusePoolIdentifier(b, name, selfOwner) {
				continue
			}
			if continuityIneligibleConfiguredOwner(b, selfOwner) {
				continue
			}
			return fmt.Errorf("%w: %q conflicts with existing identifier on %s", ErrSessionNameExists, name, b.ID)
		}
	}
	return nil
}

func failedCreateIdentityReleased(b beads.Bead) bool {
	return strings.TrimSpace(b.Metadata["state"]) == string(StateFailedCreate)
}

func continuityIneligibleConfiguredOwner(b beads.Bead, selfOwner string) bool {
	if failedCreateIdentityReleased(b) {
		return false
	}
	if selfOwner == "" || strings.TrimSpace(b.Metadata["configured_named_identity"]) != selfOwner {
		return false
	}
	return !NamedSessionContinuityEligible(b)
}

func sessionNameConflictsWithExistingIdentifier(b beads.Bead, name string) bool {
	for _, field := range []string{
		b.Metadata["agent_name"],
		b.Metadata["template"],
		b.Metadata["common_name"],
	} {
		if field == "" {
			continue
		}
		if field == name {
			return true
		}
	}
	return false
}

func configuredOwnerCanReusePoolIdentifier(b beads.Bead, name, selfOwner string) bool {
	name = strings.TrimSpace(name)
	selfOwner = strings.TrimSpace(selfOwner)
	if name == "" || selfOwner == "" {
		return false
	}
	if name != selfOwner && !strings.HasSuffix(selfOwner, "/"+name) {
		return false
	}
	if strings.TrimSpace(b.Metadata["pool_managed"]) == "true" {
		return true
	}
	return strings.TrimSpace(b.Metadata["pool_slot"]) != ""
}

func configuredNamedSessionOwnerForBead(b beads.Bead, reserved string) string {
	reserved = strings.TrimSpace(reserved)
	if reserved == "" {
		return ""
	}
	if strings.TrimSpace(b.Metadata["configured_named_session"]) == "true" &&
		strings.TrimSpace(b.Metadata["configured_named_identity"]) == reserved {
		return reserved
	}
	return ""
}

func configuredNamedSessionOwnerForSessionName(cfg *config.City, b beads.Bead, reservedName string) string {
	if cfg == nil {
		return ""
	}
	identity := strings.TrimSpace(b.Metadata["configured_named_identity"])
	if identity == "" || strings.TrimSpace(b.Metadata["configured_named_session"]) != "true" {
		return ""
	}
	if config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, identity) != reservedName {
		return ""
	}
	return identity
}

func ensureConfiguredSessionNameAvailable(store beads.Store, cfg *config.City, name, selfID, selfOwner string) error {
	if err := ensureSessionNameAvailableForSelfAndOwner(store, name, selfID, selfOwner); err != nil {
		// When a closed bead blocks the name and the caller is materializing
		// a configured named session that owns this name, allow it. This
		// handles legacy beads that predate the configured_named_session flag
		// and were closed with a terminal reason (orphaned, reconfigured, etc.)
		// but still hold the session_name. Without this, cold-boot recovery
		// is permanently blocked by stale closed beads.
		if !errors.Is(err, ErrSessionNameExists) || cfg == nil || selfOwner == "" {
			return err
		}
		if !isConfiguredNamedSessionRuntimeName(cfg, name, selfOwner) {
			return err
		}
		if !noLiveSessionNameCollisions(store, name, selfID, selfOwner) {
			return err
		}
		// All holders are closed and the name belongs to a configured named
		// session owned by selfOwner — allow reuse.
	}
	if cfg == nil || name == "" {
		return nil
	}
	if selfOwner == "" && selfID != "" {
		if self, getErr := store.Get(selfID); getErr == nil && IsSessionBeadOrRepairable(self) {
			selfOwner = configuredNamedSessionOwnerForSessionName(cfg, self, name)
		}
	}
	for _, named := range cfg.NamedSessions {
		reserved := strings.TrimSpace(named.QualifiedName())
		if reserved == "" {
			continue
		}
		if config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, reserved) != name {
			continue
		}
		if selfOwner != "" && selfOwner == reserved {
			return nil
		}
		return fmt.Errorf("%w: %q reserved for configured named session %s", ErrSessionNameExists, name, reserved)
	}
	return nil
}

// isConfiguredNamedSessionRuntimeName reports whether name is the runtime
// session name for a configured named session with the given owner identity.
func isConfiguredNamedSessionRuntimeName(cfg *config.City, name, owner string) bool {
	for _, named := range cfg.NamedSessions {
		reserved := strings.TrimSpace(named.QualifiedName())
		if reserved == "" || reserved != owner {
			continue
		}
		if config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, reserved) == name {
			return true
		}
	}
	return false
}

// noLiveSessionNameCollisions reports whether no live bead conflicts with
// the given name via session_name, alias, alias_history, or identifier
// fields. This mirrors the full collision check in
// ensureSessionNameAvailableForSelf so the legacy-bypass path cannot
// suppress rejections from live alias or identifier collisions.
func noLiveSessionNameCollisions(store beads.Store, name, selfID, selfOwner string) bool {
	all, err := ExactMetadataSessionCandidates(store, true,
		map[string]string{"session_name": name},
		map[string]string{"alias": name},
		map[string]string{"agent_name": name},
		map[string]string{"template": name},
		map[string]string{"common_name": name},
	)
	if err != nil {
		return false
	}
	for _, b := range all {
		if !IsSessionBeadOrRepairable(b) || b.ID == selfID {
			continue
		}
		if failedCreateIdentityReleased(b) {
			continue
		}
		// A live bead holding the name as session_name blocks.
		if strings.TrimSpace(b.Metadata["session_name"]) == name && b.Status != "closed" {
			return false
		}
		if b.Status == "closed" {
			continue
		}
		// Live alias collision blocks.
		if strings.TrimSpace(b.Metadata["alias"]) == name {
			return false
		}
		// Historical aliases are compatibility-only input and do not reserve
		// namespace for new session-name claims.
		// Live identifier collision blocks.
		if sessionNameConflictsWithExistingIdentifier(b, name) {
			if configuredOwnerCanReusePoolIdentifier(b, name, selfOwner) {
				continue
			}
			return false
		}
	}
	return true
}

func ensureSessionAliasAvailable(store beads.Store, cfg *config.City, alias, selfID, selfOwner string) error {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return nil
	}
	var (
		selfBead    beads.Bead
		hasSelfBead bool
	)
	if cfg != nil && selfID != "" {
		if self, getErr := store.Get(selfID); getErr == nil && IsSessionBeadOrRepairable(self) {
			selfBead = self
			hasSelfBead = true
		}
	}
	all, err := ExactMetadataSessionCandidates(store, false,
		map[string]string{"session_name": alias},
		map[string]string{"alias": alias},
		map[string]string{"agent_name": alias},
	)
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}
	for _, b := range all {
		if !IsSessionBeadOrRepairable(b) || b.ID == selfID {
			continue
		}
		if failedCreateIdentityReleased(b) {
			continue
		}
		if b.Status == "closed" {
			continue
		}
		if strings.TrimSpace(b.Metadata["session_name"]) == alias {
			// A superseded, non-running (asleep) configured-named-session
			// predecessor for the SAME identity must not block that
			// identity's own live holder from claiming its canonical alias
			// (#2885, Fix Candidate A). Scoped narrowly: only when (1) the
			// claimant asserts the exact owner identity the holder was
			// minted for, (2) the holder is asleep rather than genuinely
			// running, and (3) the holder is recognizably a
			// configured-named-session bead FOR THAT SAME IDENTITY. This
			// mirrors the self-owner exception the agent_name branch below
			// already has, and does not resurrect or steal an alias from an
			// unrelated, live, or ambiguous session.
			//
			// Condition (3) is two-part on purpose.
			// wasConfiguredNamedSession(b) establishes the holder is a
			// configured-named-session bead at all, but it is owner-AGNOSTIC
			// — a bead minted for a DIFFERENT configured identity that merely
			// persisted this identity's runtime session_name would satisfy it
			// and hand the alias over on the claimant's assertion alone.
			// configuredNamedIdentitySignalsMatch is the owner-scoped
			// recognizer introduced for the identical trap in
			// name_claim_sweep.go (review #3373); it matches the recorded
			// identity, alias, agent_name, or template/role signal against
			// THIS identity.
			if selfOwner != "" && selfOwner == alias &&
				strings.TrimSpace(b.Metadata["state"]) == string(StateAsleep) &&
				wasConfiguredNamedSession(b) &&
				configuredNamedIdentitySignalsMatch(b, selfOwner) {
				continue
			}
			return fmt.Errorf("%w: %q conflicts with session name on %s", ErrSessionAliasExists, alias, b.ID)
		}
		if strings.TrimSpace(b.Metadata["alias"]) == alias {
			return fmt.Errorf("%w: %q already belongs to %s", ErrSessionAliasExists, alias, b.ID)
		}
		if strings.TrimSpace(b.Metadata["agent_name"]) == alias {
			if selfOwner != "" && selfOwner == alias {
				continue
			}
			return fmt.Errorf("%w: %q conflicts with concrete session identity on %s", ErrSessionAliasExists, alias, b.ID)
		}
		// Historical aliases are compatibility-only input and do not reserve
		// namespace for new alias claims.
	}
	if cfg != nil {
		for _, named := range cfg.NamedSessions {
			reserved := strings.TrimSpace(named.QualifiedName())
			if reserved == "" || reserved != alias {
				continue
			}
			if selfOwner == "" && hasSelfBead {
				selfOwner = configuredNamedSessionOwnerForBead(selfBead, reserved)
			}
			if selfOwner != "" && selfOwner == reserved {
				return nil
			}
			return fmt.Errorf("%w: %q reserved for configured named session %s", ErrSessionAliasExists, alias, reserved)
		}
	}
	return nil
}
