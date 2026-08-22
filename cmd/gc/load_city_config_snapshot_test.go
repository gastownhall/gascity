package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode"
)

// TestCityConfigLoadersDeclineTheRevisionSnapshot pins a file-level invariant:
// every city-config loader in cmd_agent.go discards the Provenance, so every one
// of them must decline the load-time revision snapshot.
//
// The snapshot content-hashes every pack directory so a later config.Revision()
// can compare against the tree as it was loaded. These loaders return only
// *config.City — they use the Provenance to emit warnings and then drop it — so
// nothing they load can observe the snapshot, and building it is pure cost on a
// one-shot command.
//
// This is a source-level guard rather than a behavioral one because the cost is
// invisible by construction: a loader that reverts to the default returns
// exactly the same config and passes every functional test, it just re-reads
// every pack file. Nothing fails; it only gets slower, which is precisely the
// regression a test suite does not otherwise catch.
//
// It follows the file-scanning idiom of TestGCNonTestFilesStayOnWorkerBoundary
// and TestGCNonTestFilesStayOnRigProvisionBoundary: read the guarded file from a
// runtime.Caller-derived directory rather than the process working directory,
// and match with substring needles.
//
// The loaders no longer each spell the option out at their own call site: they
// share a body that takes the options as a parameter, since which options to
// pass is what separates a blocking load from an advisory one. So the guard
// checks the two halves that together still forbid a default-options load —
// every config.LoadOptions value this file declares declines the snapshot, and
// every load call is handed one of those values rather than something else.
//
// That second half deliberately requires a *named* value: an inline
// config.LoadOptions{SkipRevisionSnapshot: true} at a call site would be
// correct today but leaves the reasoning above attached to nothing, and the
// next option added beside it has no comment to inherit. Declare a var with a
// rationale and pass that.
//
// Scope is deliberately narrow. Only cmd_agent.go's loaders are known to discard
// the Provenance; roughly a dozen other call sites elsewhere in the tree do the
// same thing and are out of scope for this change and this guard.
//
// If a loader here is ever changed to RETURN the Provenance it should keep the
// default instead, and this test should be updated to exempt it by name rather
// than deleted.
func TestCityConfigLoadersDeclineTheRevisionSnapshot(t *testing.T) {
	const guarded = "cmd_agent.go"
	const capturingCall = "config.LoadWithIncludes("
	// optionsType, not "config.LoadOptions{": a zero value declared without a
	// literal (`var opts config.LoadOptions`) captures the snapshot just as
	// surely, and matching on the brace would not see it.
	const optionsType = "config.LoadOptions"
	const option = "SkipRevisionSnapshot: true"
	// sharedBodyParam is the parameter the shared loader body forwards. It is
	// safe only because every call that supplies it is checked below.
	const sharedBodyParam = "opts"

	// Calls that must be handed a declining options value.
	loadCalls := []string{"config.LoadWithIncludesOptions(", "loadPrematerializedCityConfig("}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(currentFile), guarded))
	if err != nil {
		t.Fatalf("reading %s: %v", guarded, err)
	}
	text := string(src)

	// capturingCall ends in '(' so it does not also match
	// config.LoadWithIncludesOptions(, whose next character is 'O'.
	if strings.Contains(text, capturingCall) {
		t.Errorf("%s calls %s — that form always captures the revision snapshot; use %sOptions(..., <declining options>)",
			guarded, capturingCall, capturingCall)
	}

	// Scan line by line rather than with a bracket-matching regex: a pattern
	// like `\([^)]*\)` truncates at the first ')', so a future call containing a
	// nested call expression would fail spuriously with a misleading message.
	lines := strings.Split(text, "\n")

	// First half: every options value this file declares must decline the
	// snapshot, and record its name so the call scan below can recognize it.
	declining := map[string]bool{sharedBodyParam: true}
	optionsFound := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(line, optionsType) {
			continue
		}
		// A comment naming the type declares nothing.
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		// The shared body's signature declares the parameter itself. It is
		// checked indirectly instead: every call that supplies it is required
		// below to pass one of the values verified here.
		if strings.HasPrefix(trimmed, "func ") {
			continue
		}
		optionsFound++
		if !strings.Contains(line, option) {
			t.Errorf("%s:%d declares options that capture the revision snapshot: %s",
				guarded, i+1, trimmed)
			continue
		}
		if name, ok := declaredVarName(line); ok {
			declining[name] = true
		}
	}
	if optionsFound == 0 {
		t.Fatalf("no %s declaration in %s; this guard is no longer watching anything", optionsType, guarded)
	}

	// Second half: every load call must be handed one of those values. Without
	// this, the first half could pass while a loader was quietly switched to a
	// default config.LoadOptions{} declared elsewhere.
	callsFound := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// The shared body's own signature names the parameter; it is a
		// declaration, not a call.
		if strings.HasPrefix(trimmed, "func ") {
			continue
		}
		if !containsAny(line, loadCalls) {
			continue
		}
		callsFound++
		if !hasIdentifierIn(line, declining) {
			t.Errorf("%s:%d loads config without passing one of this file's named declining options values %s: %s",
				guarded, i+1, sortedKeys(declining), trimmed)
		}
	}
	if callsFound == 0 {
		t.Fatalf("no config load call in %s; this guard is no longer watching anything", guarded)
	}
}

// declaredVarName returns the identifier bound by a `var NAME = ...` line.
func declaredVarName(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	rest, ok := strings.CutPrefix(trimmed, "var ")
	if !ok {
		return "", false
	}
	name, _, ok := strings.Cut(rest, " ")
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

func containsAny(line string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(line, n) {
			return true
		}
	}
	return false
}

// hasIdentifierIn reports whether the line uses any of the named values as a
// whole identifier.
//
// Whole identifier, not substring: a substring test for the parameter name
// "opts" also accepts "sneakyopts", so declaring `var sneakyopts
// config.LoadOptions` and passing it would satisfy this half of the guard
// while capturing the snapshot — which is precisely the evasion the two halves
// exist to close.
func hasIdentifierIn(line string, names map[string]bool) bool {
	for _, ident := range identifiers(line) {
		if names[ident] {
			return true
		}
	}
	return false
}

// identifiers splits a source line into its Go identifier tokens. It does not
// distinguish identifiers from keywords or from the digits of a numeric
// literal; the guard only ever looks up names it declared itself, so the extra
// tokens cannot produce a false match.
func identifiers(line string) []string {
	var out []string
	start := -1
	for i, r := range line {
		if r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			out = append(out, line[start:i])
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, line[start:])
	}
	return out
}
