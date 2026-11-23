package main

import (
	"bytes"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func TestInstallSupervisorSystemdBinaryMismatchGuard(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}

	for _, tc := range []struct {
		name           string
		existingBinary string
		force          bool
		wantCode       int
		wantReplaced   bool
	}{
		{
			name:           "refuses different existing binary without force",
			existingBinary: "OTHER",
			wantCode:       1,
		},
		{
			name:           "allows matching existing binary",
			existingBinary: "CURRENT",
			wantCode:       0,
		},
		{
			name:           "allows different existing binary with force",
			existingBinary: "OTHER",
			force:          true,
			wantCode:       0,
		},
		{
			name:           "replaces missing existing binary without force",
			existingBinary: "MISSING",
			wantCode:       0,
			wantReplaced:   true,
		},
		{
			name:     "allows fresh install",
			wantCode: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homeDir := t.TempDir()
			gcHome := filepath.Join(homeDir, ".gc")
			currentBinary := filepath.Join(homeDir, "bin", "gc")
			t.Setenv("HOME", homeDir)
			t.Setenv("GC_HOME", gcHome)
			setSupervisorInstallForceForTest(t, tc.force)

			data := supervisorInstallGuardServiceData(gcHome, currentBinary)
			unitPath := supervisorSystemdServicePath()
			var original []byte
			var existingBinary string
			if tc.existingBinary != "" {
				existingBinary = supervisorInstallGuardExistingBinary(t, homeDir, currentBinary, tc.existingBinary)
				original = []byte("[Unit]\nDescription=test\n\n[Service]\nExecStart=" + existingBinary + " supervisor run\n")
				if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(unitPath, original, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			oldRun := supervisorSystemctlRun
			oldActive := supervisorSystemctlActive
			var calls []string
			supervisorSystemctlRun = func(args ...string) error {
				calls = append(calls, strings.Join(args, " "))
				return nil
			}
			supervisorSystemctlActive = func(_ string) bool {
				return false
			}
			t.Cleanup(func() {
				supervisorSystemctlRun = oldRun
				supervisorSystemctlActive = oldActive
			})

			var stdout, stderr bytes.Buffer
			if code := installSupervisorSystemd(data, &stdout, &stderr); code != tc.wantCode {
				t.Fatalf("installSupervisorSystemd code = %d, want %d; stderr=%q", code, tc.wantCode, stderr.String())
			}

			if tc.wantCode == 1 {
				if len(calls) != 0 {
					t.Fatalf("systemctl calls = %v, want none when existing unit is refused", calls)
				}
				got, err := os.ReadFile(unitPath)
				if err != nil {
					t.Fatalf("ReadFile(%q): %v", unitPath, err)
				}
				if !bytes.Equal(got, original) {
					t.Fatalf("refused install rewrote unit:\n got: %q\nwant: %q", got, original)
				}
				for _, want := range []string{"existing unit", existingBinary, currentBinary, "--force"} {
					if !strings.Contains(stderr.String(), want) {
						t.Fatalf("stderr = %q, want %q", stderr.String(), want)
					}
				}
				return
			}

			assertSupervisorInstallReplacedMissingBinary(t, stdout.String(), existingBinary, tc.wantReplaced)
			joined := strings.Join(calls, "\n")
			if !strings.Contains(joined, "--user start "+supervisorSystemdServiceName()) {
				t.Fatalf("systemctl calls = %v, want service start", calls)
			}
			got, err := os.ReadFile(unitPath)
			if err != nil {
				t.Fatalf("ReadFile(%q): %v", unitPath, err)
			}
			if !strings.Contains(string(got), "ExecStart="+currentBinary+" supervisor run") {
				t.Fatalf("installed unit = %q, want current gc binary %q", got, currentBinary)
			}
		})
	}
}

func TestInstallSupervisorLaunchdBinaryMismatchGuard(t *testing.T) {
	for _, tc := range []struct {
		name           string
		existingBinary string
		force          bool
		wantCode       int
		wantReplaced   bool
	}{
		{
			name:           "refuses different existing binary without force",
			existingBinary: "OTHER",
			wantCode:       1,
		},
		{
			name:           "allows matching existing binary",
			existingBinary: "CURRENT",
			wantCode:       0,
		},
		{
			name:           "allows different existing binary with force",
			existingBinary: "OTHER",
			force:          true,
			wantCode:       0,
		},
		{
			name:           "replaces missing existing binary without force",
			existingBinary: "MISSING",
			wantCode:       0,
			wantReplaced:   true,
		},
		{
			name:     "allows fresh install",
			wantCode: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homeDir := t.TempDir()
			gcHome := filepath.Join(homeDir, ".gc")
			currentBinary := filepath.Join(homeDir, "bin", "gc")
			t.Setenv("HOME", homeDir)
			t.Setenv("GC_HOME", gcHome)
			setSupervisorInstallForceForTest(t, tc.force)

			data := supervisorInstallGuardServiceData(gcHome, currentBinary)
			plistPath := supervisorLaunchdPlistPath()
			var original []byte
			var existingBinary string
			if tc.existingBinary != "" {
				existingBinary = supervisorInstallGuardExistingBinary(t, homeDir, currentBinary, tc.existingBinary)
				original = supervisorInstallGuardLaunchdPlist(existingBinary)
				if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(plistPath, original, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			oldRun := supervisorLaunchctlRun
			var calls []string
			supervisorLaunchctlRun = func(args ...string) error {
				calls = append(calls, strings.Join(args, " "))
				return nil
			}
			t.Cleanup(func() {
				supervisorLaunchctlRun = oldRun
			})

			var stdout, stderr bytes.Buffer
			if code := installSupervisorLaunchd(data, &stdout, &stderr); code != tc.wantCode {
				t.Fatalf("installSupervisorLaunchd code = %d, want %d; stderr=%q", code, tc.wantCode, stderr.String())
			}

			if tc.wantCode == 1 {
				if len(calls) != 0 {
					t.Fatalf("launchctl calls = %v, want none when existing plist is refused", calls)
				}
				got, err := os.ReadFile(plistPath)
				if err != nil {
					t.Fatalf("ReadFile(%q): %v", plistPath, err)
				}
				if !bytes.Equal(got, original) {
					t.Fatalf("refused install rewrote plist:\n got: %q\nwant: %q", got, original)
				}
				for _, want := range []string{"existing plist", existingBinary, currentBinary, "--force"} {
					if !strings.Contains(stderr.String(), want) {
						t.Fatalf("stderr = %q, want %q", stderr.String(), want)
					}
				}
				return
			}

			assertSupervisorInstallReplacedMissingBinary(t, stdout.String(), existingBinary, tc.wantReplaced)
			joined := strings.Join(calls, "\n")
			if !strings.Contains(joined, "load "+plistPath) {
				t.Fatalf("launchctl calls = %v, want plist load", calls)
			}
			got, err := os.ReadFile(plistPath)
			if err != nil {
				t.Fatalf("ReadFile(%q): %v", plistPath, err)
			}
			if !strings.Contains(string(got), "<string>"+currentBinary+"</string>") {
				t.Fatalf("installed plist = %q, want current gc binary %q", got, currentBinary)
			}
		})
	}
}

func TestSupervisorSystemdExecStartBinaryParsesQuotedAndUnquotedPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		unit string
		want string
	}{
		{
			name: "unquoted",
			unit: "[Service]\nExecStart=/usr/local/bin/gc supervisor run\n",
			want: "/usr/local/bin/gc",
		},
		{
			name: "quoted",
			unit: "[Service]\nExecStart=\"/Applications/Gas City/gc\" supervisor run\n",
			want: "/Applications/Gas City/gc",
		},
		{
			name: "quoted escaped",
			unit: "[Service]\nExecStart=\"/opt/Gas \\\"City\\\"/gc\" supervisor run\n",
			want: "/opt/Gas \"City\"/gc",
		},
		{
			name: "missing",
			unit: "[Service]\nRestart=always\n",
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := supervisorSystemdExecStartBinary(tc.unit); got != tc.want {
				t.Fatalf("supervisorSystemdExecStartBinary() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSupervisorLaunchdPlistGCPathExtractsProgramArgument(t *testing.T) {
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.gascity.supervisor</string>
    <key>ProgramArguments</key>
    <array>
        <string>/Applications/Gas &amp; City/gc</string>
        <string>supervisor</string>
        <string>run</string>
    </array>
</dict>
</plist>`
	if got := supervisorLaunchdPlistGCPath(plist); got != "/Applications/Gas & City/gc" {
		t.Fatalf("supervisorLaunchdPlistGCPath() = %q, want escaped path decoded", got)
	}
	if got := supervisorLaunchdPlistGCPath("<plist><dict></dict></plist>"); got != "" {
		t.Fatalf("supervisorLaunchdPlistGCPath(missing ProgramArguments) = %q, want empty", got)
	}
}

func TestSupervisorSameBinaryComparesCleanPathAndInode(t *testing.T) {
	missingA := filepath.Join(t.TempDir(), "bin", "..", "bin", "gc")
	missingB := filepath.Clean(missingA)
	if !supervisorSameBinary(missingA, missingB) {
		t.Fatalf("supervisorSameBinary() = false, want true for equivalent cleaned paths")
	}

	dir := t.TempDir()
	gcPath := filepath.Join(dir, "gc")
	if err := os.WriteFile(gcPath, []byte("gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "gc-hardlink")
	if err := os.Link(gcPath, linkPath); err != nil {
		t.Skipf("hardlink unavailable on this filesystem: %v", err)
	}
	if !supervisorSameBinary(gcPath, linkPath) {
		t.Fatalf("supervisorSameBinary() = false, want true for hardline binaries")
	}

	otherPath := filepath.Join(dir, "other-gc")
	if err := os.WriteFile(otherPath, []byte("other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if supervisorSameBinary(gcPath, otherPath) {
		t.Fatalf("supervisorSameBinary() = true, want false for different files")
	}
	if supervisorSameBinary(filepath.Join(dir, "missing-a"), filepath.Join(dir, "missing-b")) {
		t.Fatalf("supervisorSameBinary() = true, want false for different missing files")
	}
}

func TestSupervisorInstallCommandRegistersForceFlag(t *testing.T) {
	setSupervisorInstallForceForTest(t, false)

	var stdout, stderr bytes.Buffer
	cmd := newSupervisorInstallCmd(&stdout, &stderr)
	if cmd.Flags().Lookup("force") == nil {
		t.Fatal("supervisor install command missing --force flag")
	}
	if err := cmd.Flags().Set("force", "true"); err != nil {
		t.Fatalf("setting --force flag: %v", err)
	}
	if !supervisorInstallForce {
		t.Fatal("setting --force did not enable supervisorInstallForce")
	}
}

func TestSupervisorInstallBinaryGuard(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "current", "gc")
	other := filepath.Join(dir, "other", "gc")
	for _, p := range []string{current, other} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("gc"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dangling := filepath.Join(dir, "dangling-gc")
	if err := os.Symlink(filepath.Join(dir, "removed", "gc"), dangling); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		existing string
		want     supervisorInstallBinaryGuardResult
	}{
		{name: "no existing binary", existing: "", want: supervisorInstallBinaryAllowed},
		{name: "same binary", existing: current, want: supervisorInstallBinaryAllowed},
		{name: "different present binary", existing: other, want: supervisorInstallBinaryDifferent},
		{name: "missing binary", existing: filepath.Join(dir, "removed", "gc"), want: supervisorInstallBinaryMissing},
		{name: "dangling symlink", existing: dangling, want: supervisorInstallBinaryMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := supervisorInstallBinaryGuard(tc.existing, current); got != tc.want {
				t.Fatalf("supervisorInstallBinaryGuard(%q, %q) = %v, want %v", tc.existing, current, got, tc.want)
			}
		})
	}
}

// supervisorInstallGuardExistingBinary resolves a table sentinel to the binary
// path an existing service file should reference: the current binary, a
// different binary present on disk, or a path that no longer exists.
func supervisorInstallGuardExistingBinary(t *testing.T, homeDir, currentBinary, sentinel string) string {
	t.Helper()
	switch sentinel {
	case "CURRENT":
		return currentBinary
	case "MISSING":
		return filepath.Join(homeDir, "removed", "bin", "gc")
	case "OTHER":
		other := filepath.Join(homeDir, "other", "bin", "gc")
		if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(other, []byte("other gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		return other
	default:
		t.Fatalf("unknown existing-binary sentinel %q", sentinel)
		return ""
	}
}

func assertSupervisorInstallReplacedMissingBinary(t *testing.T, stdout, existingBinary string, want bool) {
	t.Helper()
	got := strings.Contains(stdout, "no longer exists")
	if got != want {
		t.Fatalf("stdout = %q, want missing-binary replacement notice = %v", stdout, want)
	}
	if want && !strings.Contains(stdout, existingBinary) {
		t.Fatalf("stdout = %q, want it to name the missing binary %q", stdout, existingBinary)
	}
}

func setSupervisorInstallForceForTest(t *testing.T, force bool) {
	t.Helper()
	oldForce := supervisorInstallForce
	supervisorInstallForce = force
	t.Cleanup(func() {
		supervisorInstallForce = oldForce
	})
}

func supervisorInstallGuardServiceData(gcHome, gcPath string) *supervisorServiceData {
	return &supervisorServiceData{
		GCPath:        gcPath,
		LogPath:       filepath.Join(gcHome, "supervisor.log"),
		GCHome:        gcHome,
		XDGRuntimeDir: "",
		LaunchdLabel:  supervisorLaunchdLabel(),
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}
}

func supervisorInstallGuardLaunchdPlist(gcPath string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
    <key>ProgramArguments</key>
    <array>
        <string>` + xmlEscape(gcPath) + `</string>
        <string>supervisor</string>
        <string>run</string>
    </array>
</dict>
</plist>
`)
}
