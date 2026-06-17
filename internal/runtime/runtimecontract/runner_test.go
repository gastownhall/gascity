package runtimecontract

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// goldenBodies returns the op bodies of a fully-conformant, stateful RPP
// executable. The bodies reference the shell state dir ($S) that writeScript
// injects, so they need no Go-side path. It passes every catalog
// requirement; mutants override one body to violate exactly one behavior.
func goldenBodies() map[string]string {
	return map[string]string{
		"protocol":   `echo '{"version":0,"capabilities":[]}'`,
		"start":      `cat >/dev/null; if [ -e "$S/$name" ]; then exit 1; fi; : > "$S/$name"`,
		"stop":       `rm -f "$S/$name"`,
		"is-running": `if [ -e "$S/$name" ]; then echo true; else echo false; fi`,
		"exec":       `sh -c "$(cat)"`,
	}
}

// writeScript writes an executable RPP shell script with the given op
// bodies and returns its path. Unlisted ops exit 2.
func writeScript(t *testing.T, stateDir string, bodies map[string]string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "#!/bin/sh\nS='%s'\nop=\"$1\"\nname=\"$2\"\ncase \"$op\" in\n", stateDir)
	// Stable order for readable scripts.
	ops := make([]string, 0, len(bodies))
	for op := range bodies {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		fmt.Fprintf(&b, "  %s) %s ;;\n", op, bodies[op])
	}
	b.WriteString("  *) exit 2 ;;\nesac\n")

	path := filepath.Join(t.TempDir(), "gc-runtime-golden")
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeGolden(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	return writeScript(t, state, goldenBodies())
}

func runGolden(t *testing.T, path string) Report {
	t.Helper()
	report, err := Run(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("Run(%s): %v", path, err)
	}
	return report
}

func TestProbesCoverCatalogExactly(t *testing.T) {
	catalogCodes := map[Code]bool{}
	for _, req := range catalog {
		catalogCodes[req.Code] = true
		if _, ok := probes[req.Code]; !ok {
			t.Errorf("catalog requirement %s has no probe", req.Code)
		}
	}
	for code := range probes {
		if !catalogCodes[code] {
			t.Errorf("probe %s has no catalog requirement", code)
		}
	}
}

func TestCatalogCodesAreUnique(t *testing.T) {
	seen := map[Code]bool{}
	for _, req := range catalog {
		if seen[req.Code] {
			t.Errorf("duplicate catalog code %s", req.Code)
		}
		seen[req.Code] = true
		if req.Title == "" {
			t.Errorf("%s has no Title", req.Code)
		}
	}
}

func TestRunCoversEveryCatalogRequirementInOrder(t *testing.T) {
	report := runGolden(t, writeGolden(t))
	if len(report.Results) != len(catalog) {
		t.Fatalf("got %d results, want one per catalog requirement (%d)", len(report.Results), len(catalog))
	}
	for i, req := range catalog {
		if report.Results[i].Code != req.Code {
			t.Errorf("result[%d].Code = %s, want %s (catalog order)", i, report.Results[i].Code, req.Code)
		}
	}
}

func TestGoldenReferenceIsFullyConformant(t *testing.T) {
	report := runGolden(t, writeGolden(t))
	if report.Failed() {
		for _, res := range report.Results {
			if res.Status == StatusFail {
				t.Errorf("golden failed %s: %s", res.Code, res.Detail)
			}
		}
		t.Fatalf("golden reference must pass every requirement; summary=%+v", report.Summary)
	}
	if report.Summary.Passed != len(catalog) {
		t.Errorf("golden passed %d/%d (skipped %d) — the golden must exercise every required behavior",
			report.Summary.Passed, len(catalog), report.Summary.Skipped)
	}
}

// TestEveryRequirementIsGated is the negative-gating proof: for each
// requirement, a reference that violates exactly that behavior must FAIL
// exactly that requirement's check. This is what upgrades "probed" to
// "guaranteed" — a vacuous probe would not catch its mutant.
func TestEveryRequirementIsGated(t *testing.T) {
	mutants := []struct {
		target       Code
		op           string
		body         string
		whatItBreaks string
	}{
		{ReqProtocolHandshake, "protocol", `echo 'not json at all'`, "malformed handshake"},
		{ReqLifecycleStartRunning, "is-running", `echo false`, "session never reports running"},
		{ReqLifecycleDuplicateErr, "start", `cat >/dev/null; : > "$S/$name"`, "duplicate start silently succeeds"},
		{ReqLifecycleStopNotRunning, "stop", `:`, "stop does not actually stop"},
		{ReqLifecycleStopIdempotent, "stop", `if [ -e "$S/$name" ]; then rm -f "$S/$name"; else exit 1; fi`, "stop errors on a missing session"},
		{ReqLifecycleUnknownNotRunning, "is-running", `echo true`, "unknown session reports running"},
		{ReqConnectionExec, "exec", `cat >/dev/null; echo wrong`, "exec ignores the command and emits fixed output"},
	}
	// Every catalog requirement must have a mutant — otherwise a new
	// requirement could ship ungated.
	covered := map[Code]bool{}
	for _, m := range mutants {
		covered[m.target] = true
	}
	for _, req := range catalog {
		if !covered[req.Code] {
			t.Errorf("requirement %s has no negative-gating mutant", req.Code)
		}
	}

	for _, m := range mutants {
		t.Run(string(m.target), func(t *testing.T) {
			state := t.TempDir()
			bodies := goldenBodies()
			bodies[m.op] = m.body
			path := writeScript(t, state, bodies)

			report, err := Run(context.Background(), path, Options{})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			res := resultFor(t, report, m.target)
			if res.Status != StatusFail {
				t.Errorf("mutant (%s) did not fail %s: status=%s detail=%q — the check is vacuous",
					m.whatItBreaks, m.target, res.Status, res.Detail)
			}
		})
	}
}

func resultFor(t *testing.T, report Report, code Code) Result {
	t.Helper()
	for _, res := range report.Results {
		if res.Code == code {
			return res
		}
	}
	t.Fatalf("no result for %s", code)
	return Result{}
}

func TestRunErrorsOnMissingExecutable(t *testing.T) {
	if _, err := Run(context.Background(), filepath.Join(t.TempDir(), "nope"), Options{}); err == nil {
		t.Fatal("Run must error when the executable cannot be resolved")
	}
}
