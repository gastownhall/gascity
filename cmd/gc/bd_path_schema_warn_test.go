package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestPATHBdSchemaSkewWarningNamesBothPinsAndInstallTarget(t *testing.T) {
	c := pathBdSchemaSkewCheck{
		lookPath: func(string) (string, error) { return "/tmp/stale-bd", nil },
		gcPin:    func() string { return "v1.1.1-0.20260805093327-bf97b73749ac" },
		binPin:   func(string) (string, error) { return "v1.0.5", nil },
	}

	got := c.warning()
	if got == "" {
		t.Fatal("warning() = empty, want a loud skew warning")
	}
	for _, want := range []string{
		"WARNING",
		"/tmp/stale-bd",
		"v1.0.5",
		"v1.1.1-0.20260805093327-bf97b73749ac",
		"schema version mismatch",
		"make install-bd",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warning missing %q:\n%s", want, got)
		}
	}
}

func TestPATHBdSchemaSkewWarningSilentWhenPinsMatch(t *testing.T) {
	pin := "v1.1.1-0.20260805093327-bf97b73749ac"
	c := pathBdSchemaSkewCheck{
		lookPath: func(string) (string, error) { return "/usr/local/bin/bd", nil },
		gcPin:    func() string { return pin },
		binPin:   func(string) (string, error) { return pin, nil },
	}
	if got := c.warning(); got != "" {
		t.Fatalf("warning() = %q, want empty when PATH bd matches gc", got)
	}
}

func TestPATHBdSchemaSkewWarningSilentWhenBdMissing(t *testing.T) {
	c := pathBdSchemaSkewCheck{
		lookPath: func(string) (string, error) { return "", errors.New("not found") },
		gcPin:    func() string { return "v1.1.1-0.20260805093327-bf97b73749ac" },
		binPin:   func(string) (string, error) { t.Fatal("binPin should not run"); return "", nil },
	}
	if got := c.warning(); got != "" {
		t.Fatalf("warning() = %q, want empty when bd is not on PATH", got)
	}
}

func TestPATHBdSchemaSkewWarningSilentWhenBinaryPinUnreadable(t *testing.T) {
	c := pathBdSchemaSkewCheck{
		lookPath: func(string) (string, error) { return "/usr/bin/bd", nil },
		gcPin:    func() string { return "v1.1.1-0.20260805093327-bf97b73749ac" },
		binPin:   func(string) (string, error) { return "", errors.New("not a Go binary") },
	}
	if got := c.warning(); got != "" {
		t.Fatalf("warning() = %q, want empty when PATH bd has no beads pin", got)
	}
}

func TestPATHBdSchemaSkewWarningSilentWhenGcPinUnknown(t *testing.T) {
	c := pathBdSchemaSkewCheck{
		lookPath: func(string) (string, error) { return "/usr/bin/bd", nil },
		gcPin:    func() string { return "" },
		binPin:   func(string) (string, error) { return "v1.0.5", nil },
	}
	if got := c.warning(); got != "" {
		t.Fatalf("warning() = %q, want empty when gc's beads pin is unknown", got)
	}
}

func TestPATHBdSchemaSkewWarnWritesOnceAndDoesNotFail(t *testing.T) {
	c := pathBdSchemaSkewCheck{
		lookPath: func(string) (string, error) { return "/tmp/stale-bd", nil },
		gcPin:    func() string { return "gc-pin" },
		binPin:   func(string) (string, error) { return "bd-pin", nil },
	}
	var buf bytes.Buffer
	c.warn(&buf)
	c.warn(&buf)
	got := buf.String()
	if got == "" {
		t.Fatal("warn wrote nothing")
	}
	if strings.Count(got, "WARNING") != 1 {
		t.Fatalf("warn should print once; got:\n%s", got)
	}
}

func TestBeadsModuleVersionFromThisBinaryMatchesLinkedPin(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	fromFile, err := beadsModuleVersionFromBinary(exe)
	if err != nil {
		t.Fatalf("beadsModuleVersionFromBinary(%s): %v", exe, err)
	}
	linked := linkedBeadsModuleVersion()
	if linked == "" {
		t.Fatal("linkedBeadsModuleVersion is empty; this test binary should link beads")
	}
	if fromFile != linked {
		t.Fatalf("binary pin %q != linked pin %q", fromFile, linked)
	}
}
