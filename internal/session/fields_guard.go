package session

import (
	"fmt"
	"io"
	"runtime/debug"
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
// internal/session, internal/worker); production never does.
func GuardSessionKeys(fail func(msg string)) {
	beads.SetSessionKeyHook(func(key string) {
		if _, ok := LookupField(key); ok {
			return
		}
		msg := fmt.Sprintf("session row key %q is not in the session field registry (internal/session/fields.go)\n%s", key, debug.Stack())
		keyViolations.Lock()
		keyViolations.msgs = append(keyViolations.msgs, msg)
		keyViolations.Unlock()
		fail(msg)
	})
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
