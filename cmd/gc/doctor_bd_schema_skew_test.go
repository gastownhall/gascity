package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
)

func TestBdSchemaSkewCheckWarnsOnSkewSignature(t *testing.T) {
	check := newBdSchemaSkewCheck("/city")
	check.lookPath = func(string) (string, error) { return "/home/jim-claude/.local/bin/bd", nil }
	check.probe = func(bdPath, cityPath string) string {
		if bdPath != "/home/jim-claude/.local/bin/bd" || cityPath != "/city" {
			t.Fatalf("probe called with (%q, %q), want resolved bd path and city path", bdPath, cityPath)
		}
		return "schema version mismatch: database is at v51, binary knows up to v49 (2 migrations ahead)\n   Database: Unable to open database\n"
	}

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning on schema-skew signature: %#v", res.Status, res)
	}
	if res.Severity != doctor.SeverityAdvisory {
		t.Fatalf("Severity = %v, want Advisory (never gates gc start/dispatch)", res.Severity)
	}
	if !strings.Contains(res.Message, "/home/jim-claude/.local/bin/bd") {
		t.Errorf("Message %q must name the resolved bd path", res.Message)
	}
	if check.CanFix() {
		t.Errorf("CanFix = true, want false (read-only observability check)")
	}
}

func TestBdSchemaSkewCheckOKWhenHealthy(t *testing.T) {
	check := newBdSchemaSkewCheck("/city")
	check.lookPath = func(string) (string, error) { return "/usr/local/bin/bd", nil }
	check.probe = func(string, string) string {
		return "Database: OK\nAll checks passed.\n"
	}

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK when bd doctor reports no schema-skew signature: %#v", res.Status, res)
	}
	if res.Severity != doctor.SeverityAdvisory {
		t.Fatalf("Severity = %v, want Advisory", res.Severity)
	}
	if !strings.Contains(res.Message, "/usr/local/bin/bd") {
		t.Errorf("Message %q must name the resolved bd path", res.Message)
	}
}

func TestBdSchemaSkewCheckOKWhenBdNotOnPath(t *testing.T) {
	check := newBdSchemaSkewCheck("/city")
	check.lookPath = func(string) (string, error) {
		return "", fmt.Errorf("exec: \"bd\": executable file not found in $PATH")
	}
	check.probe = func(string, string) string {
		t.Fatal("probe should not run when bd is not resolvable")
		return ""
	}

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK (advisory skip) when bd is absent from PATH: %#v", res.Status, res)
	}
}

func TestBdSchemaSkewCheckOtherSkewSignature(t *testing.T) {
	check := newBdSchemaSkewCheck("/city")
	check.lookPath = func(string) (string, error) { return "/bin/bd", nil }
	check.probe = func(string, string) string {
		return "fatal: Unable to open database: permission denied\n"
	}

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning for the 'Unable to open database' signature too: %#v", res.Status, res)
	}
	// "Unable to open database" is ambiguous — it covers both a skewed binary
	// and a simply-unreachable store — so the remedy must mention checking
	// store reachability, not only rebuilding bd (ga-7xzmtd review note).
	if !strings.Contains(strings.ToLower(res.Message), "unreachable") {
		t.Errorf("Message %q should acknowledge the store may be unreachable, not only schema-skewed", res.Message)
	}
	if !strings.Contains(res.FixHint, "reachable") {
		t.Errorf("FixHint %q should tell the operator to confirm the store is reachable first", res.FixHint)
	}
}
