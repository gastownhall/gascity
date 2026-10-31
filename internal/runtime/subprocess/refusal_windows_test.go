//go:build windows

package subprocess

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestStartRefusesPOSIXShellCommand pins that a session command naming a POSIX
// shell is refused before anything starts, rather than handed to whatever sh
// PATH offers.
func TestStartRefusesPOSIXShellCommand(t *testing.T) {
	p := newTestProvider(t)
	const name = "posix-shell-refused"
	err := p.Start(context.Background(), name, runtime.Config{Command: "bash -c 'sleep 60'"})
	if err == nil {
		_ = p.Stop(name)
		t.Fatal("Start = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("Start = %v, want a refusal", err)
	}
	if p.IsRunning(name) {
		t.Fatal("IsRunning = true after a refused Start")
	}
}
