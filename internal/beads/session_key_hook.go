package beads

import "sync/atomic"

// The session field registry's write chokepoint (ARCH-RESTRUCTURE R1). Every
// backend's write verbs (MemStore's, SQLiteStore's, NativeDoltStore's,
// BdStore's and its doltlite reader's; the wrappers write through them)
// report each key they write to a session row to the installed hook. internal/session
// installs a hook that fails a test on a key the registry does not have; no
// hook is installed in production, so production behavior is unchanged.

var sessionKeyHook atomic.Pointer[func(key string)]

// SetSessionKeyHook installs fn to see every metadata key a MemStore writes to
// a session bead; nil removes it.
func SetSessionKeyHook(fn func(key string)) {
	if fn == nil {
		sessionKeyHook.Store(nil)
		return
	}
	sessionKeyHook.Store(&fn)
}

// noteSessionKeys reports kvs's keys to the hook when b is a session bead:
// type "session", or untyped with the "gc:session" label (internal/session's
// BeadType and LabelSession, which this package cannot import).
func noteSessionKeys(b Bead, kvs map[string]string) {
	fn := sessionKeyHook.Load()
	if fn == nil || len(kvs) == 0 {
		return
	}
	if b.Type != "session" && (b.Type != "" || !hasLabel(b.Labels, "gc:session")) {
		return
	}
	for k := range kvs {
		(*fn)(k)
	}
}

// noteSessionKeysByID reports kvs when id's row, read by get, is a session
// bead. A store that must read a row to learn its type reads it only while a
// hook is installed, so production does no extra read.
func noteSessionKeysByID(get func(string) (Bead, error), id string, kvs map[string]string) {
	if sessionKeyHook.Load() == nil || len(kvs) == 0 {
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
