package logutil

import (
	"strings"
	"testing"
)

func TestFatalFormattingUsesMarkerAndCauseURL(t *testing.T) {
	line := FormatFatalLine(`agent "worker": pack v1/v2 layout collision`)
	want := `gc-fatal: agent "worker": pack v1/v2 layout collision see: ` + WalkthroughURL["duplicate_name_v1v2"]
	if got := line; got != want {
		t.Fatalf("FormatFatalLine() = %q, want %q", got, want)
	}

	message, ok := ParseFatalLine(line)
	if !ok {
		t.Fatal("ParseFatalLine did not recognize fatal marker")
	}
	if got, want := FatalCause(message), "pack-v1-v2-collision"; got != want {
		t.Fatalf("FatalCause() = %q, want %q", got, want)
	}
}

func TestFatalFormattingRoutesV1V2DuplicateNamesToMigration(t *testing.T) {
	line := FormatFatalLine(`pack v1/v2 duplicate name "worker"`)
	want := `gc-fatal: pack v1/v2 duplicate name "worker" see: ` + WalkthroughURL["duplicate_name_v1v2"]
	if got := line; got != want {
		t.Fatalf("FormatFatalLine() = %q, want %q", got, want)
	}
}

func TestRenderFatalLineTTY(t *testing.T) {
	got := RenderFatalLine("broken", true)
	const want = "\x1b[1;31mFATAL: broken\x1b[0m"
	if got != want {
		t.Fatalf("RenderFatalLine() = %q, want %q", got, want)
	}
}

func TestFatalFormattingCompactsMultilineMessages(t *testing.T) {
	got := FormatFatalLine("first line\n  second line")
	if strings.Contains(got, "\n") {
		t.Fatalf("FormatFatalLine() should be one line, got %q", got)
	}
}

func TestFatalCauseRoutesBeadsInitKilledToOpInitTimeout(t *testing.T) {
	got := FatalCause("exec beads init: signal: killed")
	if got != "op-init-timeout" {
		t.Fatalf("FatalCause() = %q, want %q", got, "op-init-timeout")
	}
	line := FormatFatalLine("exec beads init: signal: killed")
	want := "gc-fatal: exec beads init: signal: killed see: " + WalkthroughURL["bd_op_init_timeout"]
	if line != want {
		t.Fatalf("FormatFatalLine() = %q, want %q", line, want)
	}
}
