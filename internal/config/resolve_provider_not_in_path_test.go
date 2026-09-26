package config

import (
	"errors"
	"testing"
)

func notFoundLookPath(string) (string, error) {
	return "", errors.New("executable file not found in $PATH")
}

// TestResolveProvider_NotInPATH_IsStructured proves the failure carries the
// provider reference and the command as data. Before ob-woag the only place
// those two identifiers survived was the formatted message, which is why the
// controller could drop every session for a missing binary and report nothing
// but a smaller number.
func TestResolveProvider_NotInPATH_IsStructured(t *testing.T) {
	providers := map[string]ProviderSpec{"codex": {Command: "codex"}}
	_, err := ResolveProvider(&Agent{Provider: "codex"}, &Workspace{}, providers, notFoundLookPath)
	if err == nil {
		t.Fatal("ResolveProvider() = nil error, want a PATH failure")
	}
	var notInPath *ProviderNotInPATHError
	if !errors.As(err, &notInPath) {
		t.Fatalf("errors.As(%v, *ProviderNotInPATHError) = false", err)
	}
	if notInPath.Provider != "codex" {
		t.Errorf("Provider = %q, want %q", notInPath.Provider, "codex")
	}
	if notInPath.Command != "codex" {
		t.Errorf("Command = %q, want %q", notInPath.Command, "codex")
	}
}

// TestResolveProvider_NotInPATH_KeepsSentinelAndMessage pins the compatibility
// contract: every caller that predates the typed form used errors.Is or
// matched on the text, including `gc session new`, which was the one command
// that named the cause correctly.
func TestResolveProvider_NotInPATH_KeepsSentinelAndMessage(t *testing.T) {
	providers := map[string]ProviderSpec{"codex": {Command: "codex"}}
	_, err := ResolveProvider(&Agent{Provider: "codex"}, &Workspace{}, providers, notFoundLookPath)
	if !errors.Is(err, ErrProviderNotInPATH) {
		t.Fatalf("errors.Is(%v, ErrProviderNotInPATH) = false", err)
	}
	const want = `provider not found in PATH: provider "codex" command "codex"`
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

// TestResolveProvider_NotInPATH_ReportsCheckedBinary covers a multi-word
// command: pathCheckBinary probes the first word, so that is the name the
// operator has to install and therefore the one the error must report.
func TestResolveProvider_NotInPATH_ReportsCheckedBinary(t *testing.T) {
	providers := map[string]ProviderSpec{"wrapped": {Command: "aimux run codex"}}
	_, err := ResolveProvider(&Agent{Provider: "wrapped"}, &Workspace{}, providers, notFoundLookPath)
	var notInPath *ProviderNotInPATHError
	if !errors.As(err, &notInPath) {
		t.Fatalf("errors.As(%v, *ProviderNotInPATHError) = false", err)
	}
	if notInPath.Command != "aimux" {
		t.Errorf("Command = %q, want the probed binary %q", notInPath.Command, "aimux")
	}
}
