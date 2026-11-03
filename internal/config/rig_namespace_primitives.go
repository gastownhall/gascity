package config

import (
	"errors"
	"regexp"
	"strings"
)

// ErrRigNamespace is the one sentinel every refusal of the rig namespace
// guard wraps (federation = "rig"): the guarded rig store's, the non-store
// claim and routing paths', and gc bd's. errors.Is(err, ErrRigNamespace)
// tells a guard refusal from an operational failure on every path.
var ErrRigNamespace = errors.New(`outside a guarded rig's namespace (federation = "rig")`)

// RigRouteQualified reports whether a routing value names a rig-scoped
// identity: its fold (FoldIdentity) contains "/" and no segment of it is
// empty ("/x", "--x", "x/" and "a//b" are not qualified). A bare value is
// city-scoped, and in a store several cities share every city that defines it
// serves it, so a guarded rig never accepts one.
func RigRouteQualified(v string) bool {
	f := FoldIdentity(v)
	if !strings.Contains(f, "/") {
		return false
	}
	for _, seg := range strings.Split(f, "/") {
		if seg == "" {
			return false
		}
	}
	return true
}

// PoolSessionBeadIDPattern is the shape of a pool session bead id a city pins
// itself: "<store prefix>-session-<instance token>", the token being 16
// random bytes in lower-case hex. Ids of that shape cannot collide across
// cities; every other session id is minted by the store and repeats across
// cities. A prefix segment may hold '_' and '.'.
var PoolSessionBeadIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.]+(?:-[A-Za-z0-9_.]+)*-session-[0-9a-f]{32}$`)

// EmbeddedPoolSessionIDPattern matches any value that ends in a pool session
// bead id of any city's prefix: the id itself, or a pool session's runtime
// name "<template base>-<bead id>". Such a value names a session of whichever
// city minted the id, so it is never admitted, or accepted as a route, by its
// spelling alone.
var EmbeddedPoolSessionIDPattern = regexp.MustCompile(`-session-[0-9a-f]{32}$`)

// MaxPoolSessionIdentityLen bounds the identities treated as pool session ids
// or names; anything longer is no city's pool session.
const MaxPoolSessionIdentityLen = 512

// IsPoolSessionBeadID reports whether id has the unguessable pool shape.
func IsPoolSessionBeadID(id string) bool {
	return len(id) <= MaxPoolSessionIdentityLen && PoolSessionBeadIDPattern.MatchString(id)
}

// EmbedsPoolSessionID reports whether v, trimmed, is a pool session bead id
// or ends in one.
func EmbedsPoolSessionID(v string) bool {
	return EmbeddedPoolSessionIDPattern.MatchString(strings.TrimSpace(v))
}
