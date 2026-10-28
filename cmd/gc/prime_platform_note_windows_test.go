//go:build windows

package main

import (
	"bytes"
	"strings"
	"testing"
)

// The claim nudge gc falls back to names gc.exe, which PowerShell runs as gc;
// a bare gc there is Get-Content.
func TestDefaultPoolClaimNudgeNamesGCExeOnWindows(t *testing.T) {
	if !strings.HasPrefix(defaultPoolClaimNudge, "Run gc.exe hook --claim --drain-ack --json now;") {
		t.Fatalf("default pool claim nudge does not name gc.exe: %q", defaultPoolClaimNudge)
	}
}

// Context a hook injects ends with the note telling the agent to type gc.exe,
// whether or not the rendered prompt itself is suppressed. Plain `gc prime`
// output, which the agent asked for by running gc.exe, has no note.
func TestPrimeHookContextEndsWithTheGCExeNoteOnWindows(t *testing.T) {
	const note = "run Gas City commands as `gc.exe`: in PowerShell, `gc` is the Get-Content alias."
	for _, suppress := range []bool{false, true} {
		var out bytes.Buffer
		writePrimePromptWithFormat(&out, "city", "worker", "the prompt", true, "", suppress, "", nil)
		if !strings.HasSuffix(out.String(), note) {
			t.Errorf("hook context (suppressed=%v) does not end with the gc.exe note:\n%s", suppress, out.String())
		}
	}
	var plain bytes.Buffer
	writePrimePromptWithFormat(&plain, "city", "worker", "the prompt", false, "", false, "", nil)
	if strings.Contains(plain.String(), "gc.exe") {
		t.Errorf("plain prime output carries the hook note:\n%s", plain.String())
	}
	var silent bytes.Buffer
	writePrimePromptWithFormat(&silent, "", "", "", true, "", false, "", nil)
	if silent.Len() != 0 {
		t.Errorf("a hook with no context to inject wrote the note alone: %q", silent.String())
	}
}
