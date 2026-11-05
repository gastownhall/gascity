package extmsg

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/session"
)

// resolveLiveSession maps a stable session name to the current live session
// session record, returning session.ErrSessionNotFound when no open session
// owns the name. It is a package-level var so tests can substitute a
// deterministic resolver without standing up real session records (mirrors
// timeNow).
var resolveLiveSession = func(directory session.AddressDirectory, name string) (session.Info, error) {
	return directory.ResolveAddress(name, false)
}

// overlayLiveSessionID re-points *target at the current live bead for the
// given stable session name. It is a no-op when the stored bead is still the
// active owner of the name (fast path avoiding name-resolution round-trips).
// ErrSessionNotFound is the only compatibility miss: it leaves *target
// unchanged. Backend, ambiguity, and other indeterminate directory failures
// are returned so callers never route using a stale identity.
//
// "Not closed" alone does not prove the stored bead still owns the name: a
// retired named session is archived without being closed
// (session.RetireNamedSessionPatch clears its identifiers but leaves the bead
// open so historical references can be reassigned). An open-but-identity-released
// bead no longer owns the name, so it must fall through to name resolution;
// otherwise routing keeps targeting the retired bead instead of its respawned
// replacement.
func overlayLiveSessionID(directory session.AddressDirectory, name, currentID string, target *string) error {
	if name == "" {
		return nil
	}
	if nilAddressDirectory(directory) {
		return errors.New("session address directory is required")
	}
	if currentID != "" {
		current, err := directory.ResolveAddress(currentID, false)
		switch {
		case err == nil && strings.TrimSpace(current.ID) == "":
			return fmt.Errorf("%w: session directory returned an empty current session ID", ErrInvariantViolation)
		case err == nil && !current.Closed && !session.LifecycleIdentityReleasedInfo(current):
			return nil
		case err == nil:
			// A closed or identity-released record no longer owns the stable
			// name. Fall through to resolve its replacement.
		case errors.Is(err, session.ErrSessionNotFound):
			// A retired or closed current ID is the expected respawn shape.
			// Fall through to the stable-name lookup.
		default:
			return newSafeOperationError("resolve current session address", err)
		}
	}
	live, err := resolveLiveSession(directory, name)
	switch {
	case errors.Is(err, session.ErrSessionNotFound):
		return nil
	case err != nil:
		return newSafeOperationError("resolve replacement session address", err)
	}
	liveID, err := resolvedLiveSessionID(live)
	if err != nil {
		return err
	}
	*target = liveID
	return nil
}

// sessionNameForSelector resolves a bind selector (a session bead ID, alias,
// or session name) to the stable session name recorded on the target bead.
// Bindings store this name so they can follow the session across respawn.
//
// ErrSessionNotFound intentionally falls back to the legacy pure-session-ID
// shape. Any other lookup failure is returned because persisting an empty
// stable name during an indeterminate directory failure would strand the
// binding or participant across respawn. A non-empty result is always the
// record's recorded session_name, never the raw selector.
func sessionNameForSelector(directory session.AddressDirectory, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", nil
	}
	if nilAddressDirectory(directory) {
		return "", errors.New("session address directory is required")
	}
	info, err := directory.ResolveAddress(selector, true)
	switch {
	case errors.Is(err, session.ErrSessionNotFound):
		return "", nil
	case err != nil:
		return "", err
	}
	return strings.TrimSpace(info.SessionNameMetadata), nil
}

func resolvedLiveSessionID(info session.Info) (string, error) {
	id := strings.TrimSpace(info.ID)
	if id == "" {
		return "", fmt.Errorf("%w: session directory returned an empty live session ID", ErrInvariantViolation)
	}
	if info.Closed || session.LifecycleIdentityReleasedInfo(info) {
		return "", fmt.Errorf("%w: session directory returned a non-live session", ErrInvariantViolation)
	}
	return id, nil
}
