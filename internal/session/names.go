package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
)

var (
	// ErrInvalidSessionName reports a malformed explicit session name.
	ErrInvalidSessionName = errors.New("invalid session name")
	// ErrSessionNameExists reports that a session name is already reserved by
	// another session bead and therefore cannot be reused.
	ErrSessionNameExists = errors.New("session name already exists")
)

var sessionNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

const (
	explicitSessionNameMaxLen = 64
	autoSessionNamePrefix     = "s-"
)

type sessionNameReservationLockEntry struct {
	mu   sync.Mutex
	refs int
}

var (
	sessionNameReservationLocksMu sync.Mutex
	sessionNameReservationLocks   = map[string]*sessionNameReservationLockEntry{}
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

func withSessionNameReservationLock(name string, fn func() error) error {
	if name == "" {
		return fn()
	}
	lock := acquireSessionNameReservationLock(name)
	defer releaseSessionNameReservationLock(name, lock)
	return fn()
}

func acquireSessionNameReservationLock(name string) *sessionNameReservationLockEntry {
	sessionNameReservationLocksMu.Lock()
	lock := sessionNameReservationLocks[name]
	if lock == nil {
		lock = &sessionNameReservationLockEntry{}
		sessionNameReservationLocks[name] = lock
	}
	lock.refs++
	sessionNameReservationLocksMu.Unlock()

	lock.mu.Lock()
	return lock
}

func releaseSessionNameReservationLock(name string, lock *sessionNameReservationLockEntry) {
	lock.mu.Unlock()

	sessionNameReservationLocksMu.Lock()
	lock.refs--
	if lock.refs == 0 {
		delete(sessionNameReservationLocks, name)
	}
	sessionNameReservationLocksMu.Unlock()
}

// WithCitySessionNameLock serializes explicit-name creation across processes
// within one city. It uses a per-name flock under the canonical session-name
// lock directory so all callers share the same cross-process guard.
func WithCitySessionNameLock(cityPath, name string, fn func() error) error {
	if name == "" || strings.TrimSpace(cityPath) == "" {
		return fn()
	}
	lockPath := filepath.Join(citylayout.SessionNameLocksDir(cityPath), name+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("creating session name lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening session name lock: %w", err)
	}
	defer f.Close() //nolint:errcheck // best-effort cleanup
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking session name %q: %w", name, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // best-effort unlock
	return fn()
}

func ensureSessionNameAvailable(store beads.Store, name string) error {
	if name == "" {
		return nil
	}
	all, err := store.ListByLabel(LabelSession, 0)
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}
	for _, b := range all {
		if b.Type != BeadType {
			continue
		}
		// Explicit session names are permanent identities; once claimed by any
		// session bead, including a closed one, they are never reused.
		if strings.TrimSpace(b.Metadata["session_name"]) == name {
			return fmt.Errorf("%w: %q already belongs to %s", ErrSessionNameExists, name, b.ID)
		}
		if b.Status == "closed" {
			continue
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
