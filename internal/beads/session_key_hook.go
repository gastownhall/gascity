package beads

import "sync/atomic"

// The session field registry's write chokepoint (ARCH-RESTRUCTURE R1). Every
// backend's write verbs (MemStore's, SQLiteStore's, NativeDoltStore's,
// BdStore's and its doltlite reader's; the wrappers write through them)
// report each key they write to a session row to the installed hook. internal/session
// installs a hook that fails a test on a key the registry does not have; no
// hook is installed in production, so production behavior is unchanged.

var sessionKeyHook atomic.Pointer[func(key string)]

// sessionWriteHook sees each write's whole metadata to a session row, an
// update's patch or a create's seed, once per write, with the row's ID, for
// internal/session's disposition write guard.
var sessionWriteHook atomic.Pointer[func(id string, kvs map[string]string)]

// SetSessionKeyHook installs fn to see every metadata key a MemStore writes to
// a session bead; nil removes it.
func SetSessionKeyHook(fn func(key string)) {
	if fn == nil {
		sessionKeyHook.Store(nil)
		return
	}
	sessionKeyHook.Store(&fn)
}

// SetSessionWriteHook installs fn to see every metadata write to a session
// bead, an update's patch or a create's seed; nil removes it.
func SetSessionWriteHook(fn func(id string, kvs map[string]string)) {
	if fn == nil {
		sessionWriteHook.Store(nil)
		return
	}
	sessionWriteHook.Store(&fn)
}

// sessionHooked reports whether either hook is installed, so a store reads a
// row to learn its type only in a guarded test.
func sessionHooked() bool { return sessionKeyHook.Load() != nil || sessionWriteHook.Load() != nil }

// noteSessionCreate reports a created session row's seed to both hooks.
func noteSessionCreate(b Bead) { noteSessionKeys(b, b.Metadata) }

// isSessionBead is type "session", or untyped with the "gc:session" label
// (internal/session's BeadType and LabelSession, which this package cannot
// import).
func isSessionBead(b Bead) bool {
	return b.Type == "session" || (b.Type == "" && hasLabel(b.Labels, "gc:session"))
}

// noteSessionKeys reports kvs's keys to the hook when b is a session bead:
// type "session", or untyped with the "gc:session" label (internal/session's
// BeadType and LabelSession, which this package cannot import).
func noteSessionKeys(b Bead, kvs map[string]string) {
	if len(kvs) == 0 || !isSessionBead(b) {
		return
	}
	if fn := sessionKeyHook.Load(); fn != nil {
		for k := range kvs {
			(*fn)(k)
		}
	}
	if fn := sessionWriteHook.Load(); fn != nil {
		(*fn)(b.ID, kvs)
	}
}

// noteSessionKeysByID reports kvs when id's row, read by get, is a session
// bead. A store that must read a row to learn its type reads it only while a
// hook is installed, so production does no extra read.
func noteSessionKeysByID(get func(string) (Bead, error), id string, kvs map[string]string) {
	if !sessionHooked() || len(kvs) == 0 {
		return
	}
	if b, err := get(id); err == nil {
		noteSessionKeys(b, kvs)
	}
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
