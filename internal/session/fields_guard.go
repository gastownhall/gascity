package session

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
)

// keyViolations records every unregistered key the guard saw, so a test whose
// recover() swallows the guard's panic still fails the run (FailOnKeyViolations).
var keyViolations struct {
	sync.Mutex
	msgs []string
}

// GuardSessionKeys makes every store write of a session-row key the registry
// does not have call fail with the key and its stack, whoever made the write:
// production code, a fixture, a test helper. A fixture that needs a key no
// production code writes spells it test_* (the registry's fixture-only
// family). Every test that writes a session row is then a census of the keys
// written, computed ones included. Tests install it (cmd/gc, internal/api,
// internal/session, internal/worker); production never does. A CASOnly key
// written through no conditional verb fails the same way, unless it is a
// CheckedWrite key written under Store.UpdateMetadataChecked, whose unfenced
// write is a legacy city's, made only on an explicit answer that the city
// does not record by CAS.
func GuardSessionKeys(fail func(msg string)) {
	beads.SetSessionKeyHook(func(key string) {
		f, ok := LookupField(key)
		var msg string
		switch {
		case !ok:
			msg = fmt.Sprintf("session row key %q is not in the session field registry (internal/session/fields.go)\n%s", key, debug.Stack())
		case f.CASOnly && !conditionalWriteOnStack(f.CheckedWrite):
			msg = fmt.Sprintf("session row key %q is CASOnly but was written blind: write it by a CAS on the row (UpdateRowFenced, or UpdateMetadataChecked for a CheckedWrite key)\n%s", key, debug.Stack())
		default:
			return
		}
		keyViolations.Lock()
		keyViolations.msgs = append(keyViolations.msgs, msg)
		keyViolations.Unlock()
		fail(msg)
	})
}

// conditionalVerbs are the store verbs that write only at a revision or value
// the caller read: a write made under one of them is not blind.
var conditionalVerbs = []string{".UpdateIfMatch", ".CompareAndSetMetadataKey", ".CloseWithMetadataIfMatch"}

// checkedVerb is the session verb whose unfenced write of a CheckedWrite key
// is not blind: it writes unfenced only where the store has no conditional
// writer and the caller answered that the city does not record by CAS.
const checkedVerb = "internal/session.(*Store).UpdateMetadataChecked"

// conditionalWriteOnStack reports whether the store write the hook is seeing
// runs under a conditional verb, of any backend or wrapper, or, when checked,
// under the checked verb.
func conditionalWriteOnStack(checked bool) bool {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		fr, more := frames.Next()
		if checked && strings.HasSuffix(fr.Function, checkedVerb) {
			return true
		}
		for _, verb := range conditionalVerbs {
			if strings.HasSuffix(fr.Function, verb) {
				return true
			}
		}
		if !more {
			return false
		}
	}
}

// FailOnKeyViolations is a guarded package's TestMain epilogue: given m.Run's
// code, it reports every violation the guard recorded to w and returns a
// failing code if there was any.
func FailOnKeyViolations(code int, w io.Writer) int {
	keyViolations.Lock()
	defer keyViolations.Unlock()
	for _, msg := range keyViolations.msgs {
		fmt.Fprintf(w, "session field guard: %s\n", msg) //nolint:errcheck // best-effort report before exit
	}
	if len(keyViolations.msgs) > 0 && code == 0 {
		return 1
	}
	return code
}
