//go:build windows

package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRunSetupCommandRefusesPOSIXShell pins that a setup command naming a
// POSIX shell is refused rather than handed to whatever sh PATH offers.
func TestRunSetupCommandRefusesPOSIXShell(t *testing.T) {
	err := RunSetupCommand(context.Background(), "bash ./setup.sh", nil, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("RunSetupCommand = %v, want a refusal", err)
	}
}
