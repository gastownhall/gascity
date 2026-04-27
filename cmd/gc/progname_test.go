package main

import (
	"bytes"
	"errors"
	"testing"
)

func TestProg_ReturnsDefaultInTestContext(t *testing.T) {
	// In test context, os.Args[0] ends with ".test", so prog() should
	// fall back to the binaryName default.
	got := prog()
	if got != binaryName {
		t.Errorf("prog() = %q, want %q (binaryName default in test context)", got, binaryName)
	}
}

func TestCmdName_WithSubcommand(t *testing.T) {
	got := cmdName("start")
	want := prog() + " start"
	if got != want {
		t.Errorf("cmdName(%q) = %q, want %q", "start", got, want)
	}
}

func TestCmdName_Empty(t *testing.T) {
	got := cmdName("")
	want := prog()
	if got != want {
		t.Errorf("cmdName(%q) = %q, want %q", "", got, want)
	}
}

func TestCmdErr_WritesExpectedFormat(t *testing.T) {
	var buf bytes.Buffer
	cmdErr(&buf, "start", errors.New("something broke"))
	got := buf.String()
	want := prog() + " start: something broke\n"
	if got != want {
		t.Errorf("cmdErr output = %q, want %q", got, want)
	}
}

func TestCmdErr_EmptySubcommand(t *testing.T) {
	var buf bytes.Buffer
	cmdErr(&buf, "", errors.New("bad input"))
	got := buf.String()
	want := prog() + ": bad input\n"
	if got != want {
		t.Errorf("cmdErr output = %q, want %q", got, want)
	}
}
