package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func runPackRuntimesCheck(t *testing.T, cfg *config.City) *doctor.CheckResult {
	t.Helper()
	check := newPackRuntimesDoctorCheck(cfg)
	if check.Name() != "pack-runtimes" {
		t.Fatalf("check name = %q, want pack-runtimes", check.Name())
	}
	if check.CanFix() {
		t.Fatal("pack-runtimes is diagnostic-only")
	}
	return check.Run(&doctor.CheckContext{CityPath: t.TempDir()})
}

func TestPackRuntimesDoctorCheck_NoneDeclaredIsOK(t *testing.T) {
	res := runPackRuntimesCheck(t, &config.City{})
	if res.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK: %s", res.Status, res.Message)
	}
}

func TestPackRuntimesDoctorCheck_HandshakingRuntimeIsOK(t *testing.T) {
	script := writeRPPScript(t, `
case "$1" in
  protocol) echo '{"version":0,"capabilities":[]}' ;;
  *) exit 2 ;;
esac
`)
	cfg := &config.City{Runtimes: map[string]config.DiscoveredRuntime{
		"cloud": {Name: "cloud", Command: script, PackName: "p"},
	}}
	res := runPackRuntimesCheck(t, cfg)
	if res.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK: %s %v", res.Status, res.Message, res.Details)
	}
}

func TestPackRuntimesDoctorCheck_NoProtocolOpIsV0FloorOK(t *testing.T) {
	// RUNTIME-RPP-008: a missing protocol op (exit 2) means version 0
	// with no optional capabilities — every pre-handshake script is valid.
	script := writeRPPScript(t, "exit 2\n")
	cfg := &config.City{Runtimes: map[string]config.DiscoveredRuntime{
		"plain": {Name: "plain", Command: script, PackName: "p"},
	}}
	res := runPackRuntimesCheck(t, cfg)
	if res.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK (v0 floor): %s %v", res.Status, res.Message, res.Details)
	}
}

func TestPackRuntimesDoctorCheck_MissingExecutableErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-installed")
	cfg := &config.City{Runtimes: map[string]config.DiscoveredRuntime{
		"ghost": {Name: "ghost", Command: missing, PackName: "ghostpack"},
	}}
	res := runPackRuntimesCheck(t, cfg)
	if res.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error", res.Status)
	}
	joined := strings.Join(res.Details, "\n")
	for _, want := range []string{"ghost", "ghostpack"} {
		if !strings.Contains(joined, want) {
			t.Errorf("details %q should mention %q", joined, want)
		}
	}
}

func TestPackRuntimesDoctorCheck_BareNameNotOnPathErrors(t *testing.T) {
	cfg := &config.City{Runtimes: map[string]config.DiscoveredRuntime{
		"lost": {Name: "lost", Command: "gc-runtime-definitely-not-installed", PackName: "p"},
	}}
	res := runPackRuntimesCheck(t, cfg)
	if res.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error", res.Status)
	}
	if !strings.Contains(strings.Join(res.Details, "\n"), "PATH") {
		t.Errorf("details %v should explain the PATH lookup failure", res.Details)
	}
}

func TestPackRuntimesDoctorCheck_BrokenHandshakeErrors(t *testing.T) {
	script := writeRPPScript(t, `
case "$1" in
  protocol) echo 'not json at all' ;;
  *) exit 2 ;;
esac
`)
	cfg := &config.City{Runtimes: map[string]config.DiscoveredRuntime{
		"garbled": {Name: "garbled", Command: script, PackName: "p"},
	}}
	res := runPackRuntimesCheck(t, cfg)
	if res.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error", res.Status)
	}
	if !strings.Contains(strings.Join(res.Details, "\n"), "handshake") {
		t.Errorf("details %v should name the handshake failure", res.Details)
	}
}

func TestPackRuntimesDoctorCheck_NonExecutableCommandErrors(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "provider.sh")
	if err := os.WriteFile(plain, []byte("#!/bin/sh\nexit 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(dir, "provider-dir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, command := range map[string]string{"plainfile": plain, "directory": subdir} {
		cfg := &config.City{Runtimes: map[string]config.DiscoveredRuntime{
			name: {Name: name, Command: command, PackName: "p"},
		}}
		res := runPackRuntimesCheck(t, cfg)
		if res.Status != doctor.StatusError {
			t.Fatalf("%s: status = %v, want Error", name, res.Status)
		}
		if joined := strings.Join(res.Details, "\n"); !strings.Contains(joined, "not an executable file") {
			t.Errorf("%s: details %q should carry the non-executable diagnostic, not a handshake error", name, joined)
		}
	}
}

func TestPackRuntimesDoctorCheck_BarePathNameHandshakes(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "gc-runtime-probe"), []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := &config.City{Runtimes: map[string]config.DiscoveredRuntime{
		"probe": {Name: "probe", Command: "gc-runtime-probe", PackName: "p"},
	}}
	res := runPackRuntimesCheck(t, cfg)
	if res.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK (bare PATH name resolving to a v0-floor executable): %s %v",
			res.Status, res.Message, res.Details)
	}
}

func TestPackRuntimesDoctorCheck_MixedReportsOnlyBroken(t *testing.T) {
	good := writeRPPScript(t, "exit 2\n")
	cfg := &config.City{Runtimes: map[string]config.DiscoveredRuntime{
		"good": {Name: "good", Command: good, PackName: "p"},
		"bad":  {Name: "bad", Command: filepath.Join(t.TempDir(), "missing"), PackName: "p"},
	}}
	res := runPackRuntimesCheck(t, cfg)
	if res.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error", res.Status)
	}
	joined := strings.Join(res.Details, "\n")
	if !strings.Contains(joined, "bad") {
		t.Errorf("details %q should mention the broken runtime", joined)
	}
	if strings.Contains(joined, "good") {
		t.Errorf("details %q should not flag the healthy runtime", joined)
	}
}
