//go:build windows

package runtime

import (
	"strings"
	"testing"
	"time"
)

// A beacon that tells an agent to fetch its context names gc.exe: in the
// agent's PowerShell, `gc prime` would run Get-Content.
func TestFormatBeaconPrimeInstructionNamesGCExeOnWindows(t *testing.T) {
	got := FormatBeaconAt("city", "worker", true, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if !strings.Contains(got, "Run `gc.exe prime` to initialize your context.") {
		t.Fatalf("beacon prime instruction does not name gc.exe: %q", got)
	}
}
