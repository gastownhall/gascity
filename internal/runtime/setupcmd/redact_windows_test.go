//go:build windows

package setupcmd

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestRunSetupCommandFailureKeepsWindowsPathsLegible proves that a failure
// naming Windows environment paths keeps them legible while scrubbing a
// credential in the same output.
func TestRunSetupCommandFailureKeepsWindowsPathsLegible(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", sentinel)

	var command strings.Builder
	var want []string
	for _, name := range []string{"USERPROFILE", "TEMP", "APPDATA", "LOCALAPPDATA", "SystemRoot"} {
		value := os.Getenv(name)
		if runtime.RedactSecrets(value, []string{value}) == value {
			t.Fatalf("%s = %q is not substitutable, so this test would prove nothing about it", name, value)
		}
		command.WriteString("[Console]::Error.WriteLine(\"cannot open $env:" + name + "\\gc-city\\missing.toml\")\n")
		want = append(want, value+`\gc-city\missing.toml`)
	}
	command.WriteString("[Console]::Error.WriteLine(\"token=$env:ANTHROPIC_AUTH_TOKEN\")\nexit 2")

	err := Run(context.Background(), command.String(), nil, fixed(60*time.Second))
	if err == nil {
		t.Fatal("a setup command exiting 2 must fail")
	}
	for _, path := range want {
		if !strings.Contains(err.Error(), path) {
			t.Errorf("the failure lost %s: %v", path, err)
		}
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("the credential reached the failure: %v", err)
	}
	if !strings.Contains(err.Error(), "token="+runtime.RedactedValue) {
		t.Fatalf("nothing in the failure was scrubbed, so the paths above prove nothing: %v", err)
	}
}
