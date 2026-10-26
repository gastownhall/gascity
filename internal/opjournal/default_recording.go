package opjournal

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// NotRecordedByDefaultError reports an op store left unopened because the
// rollout gate asking for it is auto only by its builtin default and the
// sessions store's conditional writes do not land in a SQLite store. The
// builtin default records only on SQLite: on a Dolt-served store each op's
// writes cost the request path far more, so recording there is opt-in
// through an explicit [ops] gate or GC_OPS_* override. Backend describes the
// store the writes would have landed in.
type NotRecordedByDefaultError struct {
	Backend beads.ConditionalWritesBackend
}

// Error names the backend and the opt-in. The opt-in is conditional: no
// capability probe ran, so whether this store can record ops at all (a
// released bd cannot) is not known here.
func (e *NotRecordedByDefaultError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("opening op store: the builtin auto default records ops only on a SQLite sessions store, not on %s; "+
		"an [ops] gate (or GC_OPS_*) set to auto or require records here if the store can record ops", backendPhrase(e.Backend))
}

// backendPhrase is the store a NotRecordedByDefaultError names.
func backendPhrase(b beads.ConditionalWritesBackend) string {
	if b.DoltServed {
		return "a Dolt-served one (" + b.Kind + ")"
	}
	return b.Kind
}

// CheckRecordsByDefault reports whether the builtin auto default records ops
// over store: nil when its conditional writes land in a SQLite store, else a
// *NotRecordedByDefaultError. It is an allowlist, so a store that gains
// conditional writes (a bd with --if-revision) does not start recording by
// default. It does no I/O; a caller under an explicit gate skips it and
// opens the store with NewStore.
func CheckRecordsByDefault(store beads.SessionStore) error {
	backend := beads.ResolveConditionalWritesBackend(store)
	if backend.SQLite {
		return nil
	}
	return &NotRecordedByDefaultError{Backend: backend}
}
