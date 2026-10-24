package acceptancehelpers

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/test/toolhome"
)

// gc rig add removes the rig's new .beads when bd init fails, so the dolt logs
// that explain a failed init are gone by the time the test sees the error. The
// bd that InstallBeadsTooling puts on PATH therefore prints them itself, after
// the real bd has failed and before control returns to gc.

// BDInitDiagnosticsHeadBytes and BDInitDiagnosticsTailBytes bound how much of
// each bd log the init wrapper copies to stderr. A log longer than both is cut
// to its head and tail, and the lines that name a port conflict are kept
// between them, so the startup conflict line survives wherever it sits.
const (
	BDInitDiagnosticsHeadBytes = 4096
	BDInitDiagnosticsTailBytes = 4096
)

const (
	bdInitDiagnosticsOpen  = "--- bd init diagnostics: "
	bdInitDiagnosticsClose = "--- end bd init diagnostics: "
)

// bdInitLogs are the logs under the bd scope's dolt directory that the
// wrapper reports, in report order.
var bdInitLogs = []string{"server.log", "proxy.log"}

// BDInitDiagnostic is one log section the init wrapper printed.
type BDInitDiagnostic struct {
	Path string // the log's path
	Note string // the header's parenthetical: a size, or why the log is unavailable
	Body string // the log's bounded content; empty for an unavailable log
}

var bdInitDiagnosticsHeader = regexp.MustCompile(`^` + regexp.QuoteMeta(bdInitDiagnosticsOpen) + `(.+) \((.*)\) ---$`)

// ParseBDInitDiagnostics returns the sections the init wrapper printed in a
// failed bd init's output, in order. Text around the sections, such as gc's own
// error prefix, is ignored. Output with no sections parses to nil.
func ParseBDInitDiagnostics(out string) []BDInitDiagnostic {
	var sections []BDInitDiagnostic
	var cur *BDInitDiagnostic
	var body strings.Builder
	for _, line := range strings.SplitAfter(out, "\n") {
		trimmed := strings.TrimSuffix(line, "\n")
		if cur == nil {
			if m := bdInitDiagnosticsHeader.FindStringSubmatch(trimmed); m != nil {
				cur = &BDInitDiagnostic{Path: m[1], Note: m[2]}
				body.Reset()
			}
			continue
		}
		if trimmed == bdInitDiagnosticsClose+cur.Path+" ---" {
			cur.Body = body.String()
			sections = append(sections, *cur)
			cur = nil
			continue
		}
		body.WriteString(line)
	}
	return sections
}

// bdInitWrapperScript is the bd that gc finds on PATH: it runs the tool-home
// wrapper at inner, passes its streams and exit status through untouched, and
// after a failed `init` appends the scope's dolt logs to stderr. The scope is
// BEADS_DIR, or .beads in the working directory when that is unset.
func bdInitWrapperScript(inner string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# test harness: a failed bd init reports its dolt logs before gc rig add removes them.\n")
	fmt.Fprintf(&b, "inner=%s\n", shellQuote(inner))
	fmt.Fprintf(&b, "open=%s\nclose=%s\n", shellQuote(bdInitDiagnosticsOpen), shellQuote(bdInitDiagnosticsClose))
	b.WriteString(`"$inner" "$@"
status=$?
if [ "$status" -ne 0 ] && [ "${1:-}" = init ]; then
`)
	fmt.Fprintf(&b, "\thead=%d\n\ttail=%d\n", BDInitDiagnosticsHeadBytes, BDInitDiagnosticsTailBytes)
	b.WriteString("\tscope=${BEADS_DIR:-$PWD/.beads}\n")
	fmt.Fprintf(&b, "\tfor name in %s; do\n", strings.Join(bdInitLogs, " "))
	b.WriteString(`		f=$scope/dolt/$name
		if [ ! -f "$f" ] || [ ! -r "$f" ]; then
			printf '%s%s (unavailable: not a readable file) ---\n%s%s ---\n' "$open" "$f" "$close" "$f" >&2
			continue
		fi
		size=$(wc -c < "$f" | tr -d ' ')
		if [ "$size" -le $((head + tail)) ]; then
			printf '%s%s (%s bytes) ---\n' "$open" "$f" "$size" >&2
			cat "$f" >&2
		else
			printf '%s%s (%s bytes, cut to head and tail) ---\n' "$open" "$f" "$size" >&2
			head -c "$head" "$f" >&2
			printf '\n[... middle omitted; port conflict lines kept ...]\n' >&2
			grep -E '^Port [0-9]+ already in use\.$' "$f" | head -n 8 | head -c 1024 >&2
			printf '[... tail ...]\n' >&2
			tail -c "$tail" "$f" >&2
		fi
		[ -z "$(tail -c 1 "$f")" ] || printf '\n' >&2
		printf '%s%s ---\n' "$close" "$f" >&2
	done
fi
exit "$status"
`)
	return b.String()
}

// writeBDInitWrapper writes the tool-home wrapper around realBD next to path
// and the diagnostics wrapper at path.
func writeBDInitWrapper(path, home, realBD string) error {
	if a, errA := os.Stat(path); errA == nil {
		if b, errB := os.Stat(realBD); errB == nil && os.SameFile(a, b) {
			return fmt.Errorf("bd init diagnostics wrapper: refusing to write over the real bd %s", realBD)
		}
	}
	inner := filepath.Join(filepath.Dir(path), "bd-tool-home")
	if err := toolhome.WriteWrapper(inner, home, realBD); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(bdInitWrapperScript(inner)), 0o755); err != nil { //nolint:gosec // the wrapper must be executable
		return fmt.Errorf("write bd init diagnostics wrapper %s: %w", path, err)
	}
	return nil
}
