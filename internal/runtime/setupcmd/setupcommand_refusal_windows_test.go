//go:build windows

package setupcmd

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRunSetupCommandRefusesPOSIXShell pins that a setup command naming a
// POSIX shell is refused rather than handed to whatever sh PATH offers.
func TestRunSetupCommandRefusesPOSIXShell(t *testing.T) {
	err := Run(context.Background(), "bash ./setup.sh", nil, fixed(10*time.Second))
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("Run = %v, want a refusal", err)
	}
}
