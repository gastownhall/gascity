package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// refusedScopeEnv is the scope a managed Dolt start must be refused in, plus
// the fakes the lifecycle script runs against. The scope is real: its
// refusal comes from the real gc command tree (the test binary re-exec'd as
// gc), not from a stub, so the exit code the script sees is the one gc emits.
type refusedScopeEnv struct {
	cityPath string
	script   string
	env      []string
	marker   string
}

// newRefusedScopeEnv builds a scope of the named shape and the fake gc and
// dolt the lifecycle script runs against. The fake dolt appends one line to
// the marker file when asked to run `sql-server`, which is the only thing a
// launched managed server would do here; it never binds a port. The fake gc
// hands the helper ops whose answer decides the outcome to the real command
// tree and answers the rest as "helper unavailable" (exit 1, nothing on
// stdout), except preflight-clean, which is stubbed because the real one
// removes dolt sockets under the host's temp dir.
func newRefusedScopeEnv(t *testing.T, shape string) refusedScopeEnv {
	t.Helper()
	root := t.TempDir()
	cityPath := filepath.Join(root, "scope")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	switch shape {
	case "redirecting":
		elsewhere := filepath.Join(root, "elsewhere", ".beads")
		if err := os.MkdirAll(elsewhere, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cityPath, ".beads", "redirect"), []byte(elsewhere+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	case "stray":
		// A directory that leaked a .gc and nothing else: no city.toml.
	default:
		t.Fatalf("unknown scope shape %q", shape)
	}
	materializeBuiltinPacksForTest(t, cityPath)

	binDir := filepath.Join(root, "bin")
	homeDir := filepath.Join(root, "home")
	tmpDir := filepath.Join(root, "tmp")
	for _, dir := range []string{binDir, homeDir, tmpDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(root, "spawn.log")
	fakeDolt := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  sql-server) echo SPAWN >> " + shellQuote(marker) + "; exit 1 ;;\n" +
		"  version) echo 'dolt version 1.0.0' ;;\n" +
		"  config) echo fake ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(fakeDolt), 0o755); err != nil {
		t.Fatal(err)
	}
	realGC := reexecGCTestBinaryForTests(t)
	// write-provider and write-managed die in the script when the helper fails,
	// so the fake writes the file they were asked for (recording --running); every other helper op is
	// "unavailable" and the script falls back to its native code.
	fakeGC := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"  'dolt-state admit-start') [ -z \"$FAKE_GC_ADMIT\" ] || exit 0; exec " + shellQuote(realGC) + " \"$@\" ;;\n" +
		"  'dolt-state start-managed'|'dolt-state recover-managed'|'dolt-state runtime-layout') exec " + shellQuote(realGC) + " \"$@\" ;;\n" +
		"  'dolt-state preflight-clean') exit 0 ;;\n" +
		"  'dolt-state write-provider'|'dolt-config write-managed')\n" +
		"    file=; running=false\n" +
		"    while [ \"$#\" -gt 0 ]; do\n" +
		"      case \"$1\" in --file) file=$2 ;; --running) running=$2 ;; esac\n" +
		"      shift\n" +
		"    done\n" +
		"    [ -n \"$file\" ] || exit 1\n" +
		"    mkdir -p \"$(dirname \"$file\")\" && printf '{\"running\":%s}\\n' \"$running\" > \"$file\"\n" +
		"    exit 0 ;;\n" +
		"esac\n" +
		"exit 1\n"
	fakeGCPath := filepath.Join(binDir, "gc-fake")
	if err := os.WriteFile(fakeGCPath, []byte(fakeGC), 0o755); err != nil {
		t.Fatal(err)
	}

	return refusedScopeEnv{
		cityPath: cityPath,
		script:   gcBeadsBdScriptPath(cityPath),
		marker:   marker,
		env: sanitizedBaseEnv(
			"HOME="+homeDir,
			"TMPDIR="+tmpDir,
			"GC_CITY_PATH="+cityPath,
			"GC_BIN="+fakeGCPath,
			"PATH="+binDir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin",
		),
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runRefused runs name with args against the scope and returns its exit code
// and combined output.
func (e refusedScopeEnv) runRefused(t *testing.T, name string, extraEnv []string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(append([]string{}, e.env...), extraEnv...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		return 0, out.String()
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out.String())
	}
	return exitErr.ExitCode(), out.String()
}

// requireNoSpawn fails when a dolt sql-server was launched.
func (e refusedScopeEnv) requireNoSpawn(t *testing.T) {
	t.Helper()
	if data, err := os.ReadFile(e.marker); err == nil {
		t.Errorf("a dolt sql-server was launched in the refused scope (%d launch(es))", strings.Count(string(data), "SPAWN"))
	}
}

// requireNothingLaunched fails when the refused scope shows any sign that a
// managed Dolt was started in it: a sql-server launch, or a config, lock, log
// or provider-state file a launch writes. With stoppedStateOK a provider-state
// file that records the server as not running is allowed: restart runs the
// stop operation first, which is admitted in every scope so that a server
// started where it should not have been can still be stopped, and it records
// "not running" even when nothing ran.
func (e refusedScopeEnv) requireNothingLaunched(t *testing.T, stoppedStateOK bool) {
	t.Helper()
	e.requireNoSpawn(t)
	launchFiles := map[string]bool{
		"dolt-config.yaml":         true,
		"dolt.lock":                true,
		"dolt.log":                 true,
		"dolt-provider-state.json": true,
		"dolt-state.json":          true,
	}
	walkErr := filepath.WalkDir(e.cityPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && launchFiles[d.Name()] {
			if stoppedStateOK && d.Name() == "dolt-provider-state.json" {
				if data, readErr := os.ReadFile(path); readErr == nil && strings.Contains(string(data), `"running":false`) {
					return nil
				}
			}
			t.Errorf("refused scope holds %s, a file only a launched managed Dolt writes", path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
}

// TestGcBeadsBdRefusesManagedStartInRefusedScopes drives the real
// gc-beads-bd.sh against a scope gc refuses to start a managed Dolt in. The
// refusal is the helper's to give; the script has to treat it as fatal and not
// as "helper unavailable", or it launches `dolt sql-server` itself.
func TestGcBeadsBdRefusesManagedStartInRefusedScopes(t *testing.T) {
	for _, shape := range []string{"redirecting", "stray"} {
		t.Run(shape+"/start", func(t *testing.T) {
			env := newRefusedScopeEnv(t, shape)
			code, out := env.runRefused(t, env.script, nil, "start")
			if code == 0 {
				t.Errorf("gc-beads-bd.sh start exited 0 in a %s scope, want a refusal\n%s", shape, out)
			}
			env.requireNothingLaunched(t, false)
		})
		t.Run(shape+"/restart", func(t *testing.T) {
			env := newRefusedScopeEnv(t, shape)
			restart := filepath.Join(repoRootForLint(t), "examples", "bd", "dolt", "commands", "restart", "run.sh")
			code, out := env.runRefused(t, restart, []string{"GC_BEADS_BD_SCRIPT=" + env.script})
			if code == 0 {
				t.Errorf("gc dolt restart exited 0 in a %s scope, want a refusal\n%s", shape, out)
			}
			env.requireNothingLaunched(t, true)
		})
		// The start-managed refusal is fatal on its own too: admit-start can pass
		// and start-managed still refuse, because it checks again under the
		// lifecycle lock.
		t.Run(shape+"/start-managed-refusal-alone", func(t *testing.T) {
			env := newRefusedScopeEnv(t, shape)
			code, out := env.runRefused(t, env.script, []string{"FAKE_GC_ADMIT=1"}, "start")
			if code == 0 {
				t.Errorf("gc-beads-bd.sh start exited 0 after start-managed refused in a %s scope\n%s", shape, out)
			}
			if !strings.Contains(out, "refused") {
				t.Errorf("gc-beads-bd.sh start output = %q, want it to say gc refused", out)
			}
			// The start lock is taken before the helper is asked, so only the
			// launch itself is checked here; the admit step above it is what keeps
			// the scope untouched.
			env.requireNoSpawn(t)
		})
		// recover is closed today only because recover-managed prints its zero
		// report before it returns the refusal, so the script reads a report and
		// dies. This pins the outcome, whatever the refusal looks like on the wire.
		t.Run(shape+"/recover", func(t *testing.T) {
			env := newRefusedScopeEnv(t, shape)
			code, out := env.runRefused(t, env.script, nil, "recover")
			if code == 0 {
				t.Errorf("gc-beads-bd.sh recover exited 0 in a %s scope, want a refusal\n%s", shape, out)
			}
			env.requireNothingLaunched(t, false)
		})
	}
}

// TestDoltStateStartAndRecoverRefusalHasItsOwnExitCode pins the process
// boundary of the refusal: admit-start, start-managed and recover-managed exit 3
// when the scope cannot own a managed Dolt, so a caller can tell a refusal from
// an absent or failing helper (both exit 1).
func TestDoltStateStartAndRecoverRefusalHasItsOwnExitCode(t *testing.T) {
	for _, shape := range []string{"redirecting", "stray"} {
		for _, op := range []string{"admit-start", "start-managed", "recover-managed"} {
			t.Run(shape+"/"+op, func(t *testing.T) {
				env := newRefusedScopeEnv(t, shape)
				args := []string{"dolt-state", op, "--city", env.cityPath}
				if op != "admit-start" {
					args = append(args, "--port", "3307")
				}
				var stdout, stderr bytes.Buffer
				code := run(args, &stdout, &stderr)
				if code != 3 {
					t.Errorf("gc dolt-state %s exit = %d, want 3 (refused)\nstdout: %s\nstderr: %s", op, code, stdout.String(), stderr.String())
				}
				if !strings.Contains(stderr.String(), env.cityPath) {
					t.Errorf("gc dolt-state %s stderr = %q, want it to name the scope %s", op, stderr.String(), env.cityPath)
				}
			})
		}
	}
}
