package herdr

import (
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

// redactedValue replaces a credential in a rendered argv. It keeps the "=" so
// the result still reads as an assignment.
const redactedValue = "<redacted>"

// redactArgs returns a copy of a herdr argv safe to put in an error message,
// with the value of every secret-carrying `--env KEY=VALUE` pair replaced.
//
// Errors from this client reach logs, the event bus and bead notes, so a
// credential rendered into one outlives the process that leaked it — a durable
// exposure, unlike argv, which dies with the process. Redacting here is
// independent of whether the value should have been in argv at all.
//
// The key survives: "which variable did herdr choke on" is the entire
// diagnostic value of printing the argv, and the key is not the secret. Values
// of argv-safe variables survive too — they are already legible in
// /proc/<pid>/cmdline, so hiding them costs diagnostics and buys nothing.
//
// Only the operand of an `--env` flag is treated as an assignment. A bare
// KEY=VALUE elsewhere is a positional argument (`herdr pane run -- make
// BUILD_TAG=release`) and is left alone.
//
// Callers pass the argv on every error path rather than redacting once up
// front, so the allocation lands only when something has already failed.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i < len(out)-1; i++ {
		if out[i] != "--env" {
			continue
		}
		key, value, ok := strings.Cut(out[i+1], "=")
		if ok && runtime.ArgvSecretEnvValue(key, value) {
			out[i+1] = key + "=" + redactedValue
		}
		i++ // the operand is consumed either way; it cannot also be a flag
	}
	return out
}
