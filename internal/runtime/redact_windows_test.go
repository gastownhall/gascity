//go:build windows

package runtime

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// windowsInertNames is the inventory envRedactionInertWindows must hold,
// written out apart from it: a check that walked the set itself would lose a
// name's check along with the name.
var windowsInertNames = []string{
	"APPDATA", "HOMEDRIVE", "HOMEPATH", "LOCALAPPDATA", "OneDrive", "OneDriveCommercial", "OneDriveConsumer",
	"TEMP", "TMP", "USERPROFILE",
	"ALLUSERSPROFILE", "CommonProgramFiles", "CommonProgramFiles(x86)", "CommonProgramW6432", "ComSpec",
	"DriverData", "PATHEXT", "ProgramData", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "PSModulePath",
	"PUBLIC", "SystemDrive", "SystemRoot", "windir",
	"COMPUTERNAME", "LOGONSERVER", "SESSIONNAME", "USERDNSDOMAIN", "USERDOMAIN", "USERDOMAIN_ROAMINGPROFILE",
	"USERNAME",
	"NUMBER_OF_PROCESSORS", "OS", "PROCESSOR_ARCHITECTURE", "PROCESSOR_IDENTIFIER", "PROCESSOR_LEVEL",
	"PROCESSOR_REVISION",
}

// TestWindowsRedactionInertSetIsTheInventory pins the set to the inventory in
// both directions. A dropped name is a silent diagnostics regression; an added
// one decides that a variable's value may appear in error messages, which is a
// credential decision, so it has to be made here as well.
func TestWindowsRedactionInertSetIsTheInventory(t *testing.T) {
	want := map[string]bool{}
	for _, name := range windowsInertNames {
		want[name] = true
		if !envRedactionInertWindows[name] {
			t.Errorf("%s is missing from the Windows redaction-inert set", name)
		}
	}
	for name := range envRedactionInertWindows {
		if !want[name] {
			t.Errorf("%s is in the Windows redaction-inert set but not in the inventory; add it there if its value may appear in errors", name)
		}
	}
}

// TestProcessSecretEnvValuesSkipsWindowsNames is the Windows half of
// TestProcessSecretEnvValuesSkipsPOSIXNames. Every one of these is set on a
// Windows controller, and scrubbing their values rewrites every path a failing
// command prints. Every name in the inventory is pinned, not a sample, for the
// reason the POSIX test gives: dropping one is a silent diagnostics regression.
func TestProcessSecretEnvValuesSkipsWindowsNames(t *testing.T) {
	inert := map[string]string{}
	for _, name := range windowsInertNames {
		// Distinct, above the substitution floor, and shaped like the paths
		// most of these hold.
		value := `C:\inert\` + strings.ToLower(name) + `\value`
		t.Setenv(name, value)
		inert[value] = name
	}
	if len(inert) != len(windowsInertNames) {
		t.Fatalf("built %d probe values for %d names", len(inert), len(windowsInertNames))
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", sentinel)

	got := processSecretEnvValues()
	found := false
	for _, v := range got {
		if name, ok := inert[v]; ok {
			t.Errorf("%s was treated as a credential; its value %q would be cut out of every message", name, v)
		}
		if v == sentinel {
			found = true
		}
	}
	if !found {
		t.Fatal("the process credential was not found, so the negatives prove nothing")
	}
}

// TestRedactionInertNamesIgnoreCaseOnWindows pins the case fold. The same
// variable reaches gc as SystemRoot or SYSTEMROOT depending on the process that
// started it, so an exact lookup would decide whether a path is scrubbed by who
// launched gc. It also pins, as the POSIX test does, that redaction-inert is
// not argv-safe.
func TestRedactionInertNamesIgnoreCaseOnWindows(t *testing.T) {
	names := append([]string(nil), windowsInertNames...)
	for name := range envRedactionInert {
		names = append(names, name)
	}
	for _, name := range names {
		for _, spelling := range []string{name, strings.ToUpper(name), strings.ToLower(name)} {
			if redactableEnvValue(spelling, `C:\some\path`) {
				t.Errorf("%s is being scrubbed out of rendered text", spelling)
			}
			if !ArgvSecretEnvValue(spelling, `C:\some\path`) {
				t.Errorf("%s stopped being routed through the private-file path", spelling)
			}
		}
	}
	// The control: an unrecognized name is still a credential in any case.
	for _, spelling := range []string{"SOME_NEW_TOKEN", "some_new_token"} {
		if !redactableEnvValue(spelling, "value-long-enough") {
			t.Fatalf("%s was not treated as a credential", spelling)
		}
	}
}

// TestRunSetupCommandFailureKeepsWindowsPathsLegible is the outcome the names
// exist for, end to end and with this machine's real values: a setup command
// that fails while naming paths under the user's profile, temp, app-data and
// system directories still names them in the error, while a credential in the
// same output is scrubbed.
func TestRunSetupCommandFailureKeepsWindowsPathsLegible(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", sentinel)

	var command strings.Builder
	var want []string
	for _, name := range []string{"USERPROFILE", "TEMP", "APPDATA", "LOCALAPPDATA", "SystemRoot"} {
		value := os.Getenv(name)
		if len(value) < substitutionFloor {
			t.Fatalf("%s = %q is shorter than the substitution floor, so this test would prove nothing about it", name, value)
		}
		command.WriteString("[Console]::Error.WriteLine(\"cannot open $env:" + name + "\\gc-city\\missing.toml\")\n")
		want = append(want, value+`\gc-city\missing.toml`)
	}
	command.WriteString("[Console]::Error.WriteLine(\"token=$env:ANTHROPIC_AUTH_TOKEN\")\nexit 2")

	err := RunSetupCommand(context.Background(), command.String(), nil, 60*time.Second)
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
	if !strings.Contains(err.Error(), "token="+RedactedValue) {
		t.Fatalf("nothing in the failure was scrubbed, so the paths above prove nothing: %v", err)
	}
}
