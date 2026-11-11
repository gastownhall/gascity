package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// The ready-query lint is advisory preflight for `gc config show`: it reads
// each agent's resolved work_query and scale_check as text and reports shapes
// that read the wrong ledger or lose a reader failure on a split city. It never
// executes, rewrites or rejects a query and never changes an exit status.
// docs/runbooks/split-storage-classes.md explains each finding and the
// route-keyed reader that replaces it.
//
// The scan is textual, so it has a ceiling: a reader inside a double-quoted
// "$(...)" is not seen, and a reader assembled at run time is not either.

var (
	// readyLintGCBd matches one `gc bd ...` command up to the end of its
	// shell segment.
	readyLintGCBd = regexp.MustCompile(`\bgc\s+bd\b[^|;&)\n]*`)
	// readyLintGCReady matches one `gc [global flags] ready ...` command up to
	// the end of its shell segment.
	readyLintGCReady = regexp.MustCompile(`\bgc(?:\s+--?[\w-]+(?:=\S+)?(?:\s+[^\s-]\S*)?)*\s+ready\b[^|;&)\n]*`)
	// readyLintRedirect matches fd redirections whose "&" would end a segment.
	readyLintRedirect = regexp.MustCompile(`\d*[<>]&\d*-?|&>>?`)
	// readyLintGuard matches the `|| ...` that follows a captured reader.
	readyLintGuard = regexp.MustCompile(`^\s*\|\|\s*(\{[^}]*\}|[^\n]*)`)
	// readyLintGroupPipe matches a brace group whose status is a pipeline's.
	readyLintGroupPipe = regexp.MustCompile(`\}\s*\|(?:[^|]|$)`)
	readyLintExit      = regexp.MustCompile(`\bexit\b`)
)

// readyReader is one `gc ready` command found in a query.
type readyReader struct {
	start, end int
	tokens     []string
}

// readyQueryLintWarnings returns one advisory per unsafe reader shape in the
// resolved work_query and scale_check of every agent.
func readyQueryLintWarnings(cfg *config.City) []string {
	if cfg == nil {
		return nil
	}
	split := len(relocatedBeadClasses(cfg)) > 0
	var warnings []string
	for i := range cfg.Agents {
		agentCfg := &cfg.Agents[i]
		for _, field := range []struct{ name, query string }{
			{"work_query", agentCfg.WorkQuery},
			{"scale_check", agentCfg.ScaleCheck},
		} {
			for _, advisory := range readyQueryAdvisories(field.query, split) {
				warnings = append(warnings, fmt.Sprintf("agent %q: %s: %s", agentCfg.QualifiedName(), field.name, advisory))
			}
		}
	}
	return warnings
}

// readyQueryAdvisories checks one query. A comma exclusion list is unsafe on
// any city; the rest of the shapes are split-city findings.
func readyQueryAdvisories(query string, split bool) []string {
	if strings.TrimSpace(query) == "" {
		return nil
	}
	masked := maskShellQuery(query)
	readers := findReadyReaders(query, masked)
	var out []string
	if split {
		out = append(out, splitBlindReaderAdvisory(masked)...)
	}
	out = append(out, commaExclusionAdvisories(readers)...)
	if split {
		out = append(out, convertedReaderAdvisories(masked, readers)...)
	}
	return out
}

// maskShellQuery blanks quoted text and fd redirections so segment ends and
// operators are found only where the shell would see them. The result has the
// same length as the query, so offsets in it index the original.
func maskShellQuery(query string) string {
	b := []byte(query)
	var quote byte
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == '\\' && quote != '\'' && i+1 < len(b):
			b[i], b[i+1] = '_', '_'
			i++
		case quote == 0 && (c == '\'' || c == '"'):
			quote, b[i] = c, '_'
		case quote != 0 && c == quote:
			quote, b[i] = 0, '_'
		case quote != 0 && c != '\n':
			b[i] = '_'
		}
	}
	return readyLintRedirect.ReplaceAllStringFunc(string(b), func(m string) string {
		return strings.Repeat("_", len(m))
	})
}

func findReadyReaders(query, masked string) []readyReader {
	var readers []readyReader
	for _, loc := range readyLintGCReady.FindAllStringIndex(masked, -1) {
		readers = append(readers, readyReader{start: loc[0], end: loc[1], tokens: strings.Fields(query[loc[0]:loc[1]])})
	}
	return readers
}

// splitBlindReaderAdvisory flags `gc bd ready` and `gc bd list --ready`: bd
// reads the work ledger only, so on a split city it cannot see the classes
// served from another binding. Raw `bd ready` is not matched.
func splitBlindReaderAdvisory(masked string) []string {
	for _, segment := range readyLintGCBd.FindAllString(masked, -1) {
		fields := strings.Fields(segment)
		for i := 2; i < len(fields); i++ {
			if fields[i] == "ready" && !strings.HasPrefix(fields[i-1], "-") {
				return []string{splitBlindMessage("gc bd ... ready")}
			}
			if fields[i] == "--ready" || strings.HasPrefix(fields[i], "--ready=") {
				return []string{splitBlindMessage("gc bd ... list --ready")}
			}
		}
	}
	return nil
}

func splitBlindMessage(shape string) string {
	return shape + " reads the work ledger only and cannot see the coordination classes this city serves from a relocated binding; " +
		"read by route with gc ready (GC_BD_ALLOW_RELOCATED_CLASS_READ=1 is a gate-blind one-ledger scan, never a dispatch reader)"
}

// commaExclusionAdvisories flags a comma list given to a gc ready --exclude-*
// flag: the value is one literal, so nothing is excluded.
func commaExclusionAdvisories(readers []readyReader) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range readers {
		for i, tok := range r.tokens {
			name, value, ok := strings.Cut(tok, "=")
			if !strings.HasPrefix(name, "--exclude-") {
				continue
			}
			if !ok && i+1 < len(r.tokens) {
				value = r.tokens[i+1]
			}
			if strings.Contains(value, ",") && !seen[name] {
				seen[name] = true
				out = append(out, fmt.Sprintf("gc ready %s takes one value per flag, so a comma list is one literal and excludes nothing; repeat %s once per value", name, name))
			}
		}
	}
	return out
}

// convertedReaderAdvisories checks the gc ready readers: an ignored --rig, a
// missing --limit, and a reader failure that never reaches the exit status.
func convertedReaderAdvisories(masked string, readers []readyReader) []string {
	var out []string
	rig, limit, swallowed := false, false, false
	for _, r := range readers {
		rig = rig || readerHasFlag(r, "--rig")
		limit = limit || readerHasFlag(r, "--limit")
		swallowed = swallowed || readerExitSwallowed(masked, r)
	}
	if rig {
		out = append(out, "gc ready --rig is ignored (it selects by route, not by rig); remove it and select with --metadata-field gc.routed_to=<route>")
	}
	if len(readers) > 0 && !limit {
		out = append(out, "gc ready reader has no --limit; give the converted reader an explicit --limit")
	}
	if swallowed {
		out = append(out, "gc ready reader exit status is swallowed (pipeline stage, unguarded capture or fallback), so a failed read looks like an empty queue; capture it and exit with its status")
	}
	return out
}

func readerHasFlag(r readyReader, name string) bool {
	for _, tok := range r.tokens {
		if tok == name || strings.HasPrefix(tok, name+"=") {
			return true
		}
	}
	return false
}

// readerExitSwallowed reports whether a failed reader leaves the query's exit
// status unchanged: the reader feeds a pipeline, is followed by commands, falls
// back without exiting, or is captured without a guard that exits.
func readerExitSwallowed(masked string, r readyReader) bool {
	rest := masked[r.end:]
	switch {
	case strings.TrimSpace(rest) == "":
		return false
	case rest[0] == ')':
		guard := readyLintGuard.FindStringSubmatch(rest[1:])
		return guard == nil || !readyLintExit.MatchString(guard[1]) || readyLintGroupPipe.MatchString(masked)
	case strings.HasPrefix(rest, "||"):
		return !readyLintExit.MatchString(readyLintGuard.FindString(rest))
	default:
		return true
	}
}
