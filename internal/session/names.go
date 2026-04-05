package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"

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
	sessionNamePattern  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
	sessionAliasPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*(/[a-zA-Z0-9][a-zA-Z0-9_-]*)*$`)
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

var (
	sessionIdentifierReservationLocksMu sync.Mutex
	sessionIdentifierReservationLocks   = map[string]*sessionIdentifierReservationLockEntry{}
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
// configured singleton aliases so public targets cannot be squatted before
// their managed session bead exists.
func EnsureAliasAvailableWithConfig(store beads.Store, cfg *config.City, alias, selfID string) error {
	return ensureSessionAliasAvailable(store, cfg, alias, selfID, "")
}

// EnsureAliasAvailableWithConfigForOwner extends alias reservation checks
// with an explicit configured-singleton owner identity so callers creating a
// new managed session bead can reserve that alias before a bead ID exists.
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
	return strings.ReplaceAll(identifier, "/", "%2F")
}

func ensureSessionNameAvailable(store beads.Store, name string) error {
	return ensureSessionNameAvailableForSelf(store, name, "")
}

func ensureSessionNameAvailableForSelf(store beads.Store, name, selfID string) error {
	if name == "" {
		return nil
	}
	all, err := store.List(beads.ListQuery{
		Label:         LabelSession,
		Type:          BeadType,
		IncludeClosed: true,
	})
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}
	for _, b := range all {
		if b.Type != BeadType {
			continue
		}
		if b.ID == selfID {
			continue
		}
		// Explicit session names are permanent identities; once claimed by any
		// session bead, including a closed one, they are never reused.
		//
		// Exception: closed beads that belong to a configured named session
		// (configured_named_session=true) release their session_name so the
		// reconciler can re-materialize a fresh canonical bead for the same
		// identity. The design doc specifies: "Closed historical beads do not
		// poison future canonical materialization of the reserved identity."
		if strings.TrimSpace(b.Metadata["session_name"]) == name {
			if b.Status == "closed" && strings.TrimSpace(b.Metadata["configured_named_session"]) == "true" {
				continue
			}
			return fmt.Errorf("%w: %q already belongs to %s", ErrSessionNameExists, name, b.ID)
		}
		if b.Status == "closed" {
			continue
		}
		if strings.TrimSpace(b.Metadata["alias"]) == name {
			return fmt.Errorf("%w: %q conflicts with live alias on %s", ErrSessionNameExists, name, b.ID)
		}
		for _, historicalAlias := range AliasHistory(b.Metadata) {
			if historicalAlias == name {
				return fmt.Errorf("%w: %q conflicts with live alias history on %s", ErrSessionNameExists, name, b.ID)
			}
		}
		// This collision check is intentionally one-way. Explicit names cannot
		// reuse a live short identifier, but later template/common-name sessions
		// may still coexist and are resolved second to the exact session_name.
		if sessionNameConflictsWithExistingIdentifier(b, name) {
			return fmt.Errorf("%w: %q conflicts with existing identifier on %s", ErrSessionNameExists, name, b.ID)
		}
	}
	return nil
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
		if !strings.Contains(name, "/") && strings.HasSuffix(field, "/"+name) {
			return true
		}
	}
	return false
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
	if err := ensureSessionNameAvailableForSelf(store, name, selfID); err != nil {
		return err
	}
	if cfg == nil || name == "" {
		return nil
	}
	if selfOwner == "" && selfID != "" {
		if self, getErr := store.Get(selfID); getErr == nil && self.Type == BeadType {
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
		if self, getErr := store.Get(selfID); getErr == nil && self.Type == BeadType {
			selfBead = self
			hasSelfBead = true
		}
	}
	all, err := store.List(beads.ListQuery{
		Label: LabelSession,
		Type:  BeadType,
	})
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}
	for _, b := range all {
		if b.Type != BeadType || b.ID == selfID {
			continue
		}
		if b.Status == "closed" {
			continue
		}
		if strings.TrimSpace(b.Metadata["session_name"]) == alias {
			return fmt.Errorf("%w: %q conflicts with session name on %s", ErrSessionAliasExists, alias, b.ID)
		}
		if strings.TrimSpace(b.Metadata["alias"]) == alias {
			return fmt.Errorf("%w: %q already belongs to %s", ErrSessionAliasExists, alias, b.ID)
		}
		for _, historicalAlias := range AliasHistory(b.Metadata) {
			if historicalAlias == alias {
				return fmt.Errorf("%w: %q reserved in live alias history on %s", ErrSessionAliasExists, alias, b.ID)
			}
		}
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
