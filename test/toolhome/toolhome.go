// Package toolhome keeps the bd and dolt a test forks away from the invoking
// user's home.
//
// bd resolves user-level state straight from HOME and the XDG base
// directories: the config layers ~/.beads/config.yaml,
// ~/.config/bd/config.yaml and $XDG_CONFIG_HOME/bd/config.yaml; in
// shared-server mode (`dolt.shared-server: true` in any of them) the host-wide
// Dolt server under ~/.beads/shared-server, which `bd init` STARTS with
// whatever `dolt` is on PATH even when handed an explicit
// --server-host/--server-port; and machine-id, metrics and event state under
// ~/.beads and ~/.config/bd.
//
// On 2026-09-29 an acceptance run's `bd init --server` inherited the operator's
// HOME, read their `dolt.shared-server: true`, and started the operator's own
// shared Dolt server from the run's temp dolt binary, rewriting their
// ~/.beads/config.yaml on the way.
//
// Test harnesses cannot simply give every child a temp HOME: gc's platform
// supervisor refuses an overridden HOME (platformSupervisorHomeOverrideError in
// cmd/gc) and isolates through GC_HOME instead. So gc keeps the real HOME and
// every bd and dolt child is re-homed on its own, with Environ for a child the
// test forks directly and WrapperScript for a bd that gc (or a shim) finds on
// PATH or through BD_BIN.
package toolhome

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SharedServerConfigEnv is bd's viper env binding for dolt.shared-server. It
// outranks every config-file layer, so "false" keeps bd out of shared-server
// mode even if some layer a harness did not isolate says otherwise. A test that
// exercises the user-level layer on purpose passes it explicitly as "" (viper
// reads an empty value as unset).
const SharedServerConfigEnv = "BD_DOLT_SHARED_SERVER"

// DisableMetricsEnv turns off bd's usage metrics. A re-homed bd has never shown
// its one-time metrics notice, so without this every fresh tool home prints it
// into the output a test parses (and a test run would report usage upstream).
const DisableMetricsEnv = "BD_DISABLE_METRICS"

// pinnedBeadsVars are the bd switches Environ and WrapperScript set when the
// caller did not, in a fixed order.
var pinnedBeadsVars = [][2]string{
	{SharedServerConfigEnv, "false"},
	{DisableMetricsEnv, "1"},
}

// Vars are the variables bd and dolt resolve user-level state through. HOME is
// always replaced; the XDG directories are replaced unless a caller keeps them.
var Vars = []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"}

// defaults returns the value each of Vars takes under home.
func defaults(home string) map[string]string {
	return map[string]string{
		"HOME":            home,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
	}
}

// IsHomeVar reports whether name is one of Vars.
func IsHomeVar(name string) bool {
	for _, v := range Vars {
		if v == name {
			return true
		}
	}
	return false
}

// IsBeadsVar reports whether name is one of bd's own configuration variables.
func IsBeadsVar(name string) bool {
	return strings.HasPrefix(name, "BEADS_") || strings.HasPrefix(name, "BD_")
}

// Environ returns base rewritten for a bd or dolt child that must not resolve
// anything from the invoking user's home.
//
// HOME and the XDG base directories are pointed under home, every BEADS_* and
// BD_* variable is dropped, BD_DOLT_SHARED_SERVER is pinned to "false" and
// BD_DISABLE_METRICS to "1".
// Names in keep survive from base untouched — that is how a caller passes a
// variable deliberately (a BEADS_DIR for the workspace it drives, an
// XDG_CONFIG_HOME it seeded on purpose) — except HOME, which is never kept.
//
// home must exist before the child runs: bd fails on a HOME it cannot stat.
func Environ(base []string, home string, keep ...string) []string {
	kept := make(map[string]bool, len(keep))
	for _, name := range keep {
		if name != "HOME" {
			kept[name] = true
		}
	}
	vals := defaults(home)
	out := make([]string, 0, len(base)+len(vals)+1)
	present := make(map[string]bool)
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if (IsHomeVar(name) || IsBeadsVar(name)) && !kept[name] {
			continue
		}
		present[name] = true
		out = append(out, kv)
	}
	for _, name := range Vars {
		if !present[name] {
			out = append(out, name+"="+vals[name])
		}
	}
	for _, pin := range pinnedBeadsVars {
		if !present[pin[0]] {
			out = append(out, pin[0]+"="+pin[1])
		}
	}
	return out
}

// Explicit returns every XDG base directory and BEADS_*/BD_* name set in env.
// Use it as Environ's keep list for an env that is already an explicit
// projection (built from an allowlist, or already scrubbed of host values), so
// the variables the harness set on purpose survive while HOME is replaced.
func Explicit(env []string) []string {
	var names []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if name != "HOME" && (IsHomeVar(name) || IsBeadsVar(name)) {
			names = append(names, name)
		}
	}
	return names
}

// WrapperScript is the text of a POSIX shell bd that re-homes itself under home
// and then execs realBD.
//
// HOME is always replaced. The XDG directories and the pinned bd switches take
// Environ's defaults only when the caller left them unset, so a variable a test
// passes on purpose still reaches bd. It execs, so the process table, bd's
// os.Executable() and everything bd launches name the real binary.
func WrapperScript(home, realBD string) (string, error) {
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("bd tool-home wrapper: no home")
	}
	if strings.TrimSpace(realBD) == "" {
		return "", fmt.Errorf("bd tool-home wrapper: no bd to exec")
	}
	vals := defaults(home)
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# test harness: bd never resolves user-level state from the invoking user's HOME.\n")
	fmt.Fprintf(&b, "HOME=%s\n", shellQuote(vals["HOME"]))
	for _, name := range Vars[1:] {
		fmt.Fprintf(&b, "[ -n \"${%s:-}\" ] || %s=%s\n", name, name, shellQuote(vals[name]))
	}
	exported := append([]string{}, Vars...)
	for _, pin := range pinnedBeadsVars {
		fmt.Fprintf(&b, "[ -n \"${%s+set}\" ] || %s=%s\n", pin[0], pin[0], pin[1])
		exported = append(exported, pin[0])
	}
	fmt.Fprintf(&b, "export %s\n", strings.Join(exported, " "))
	fmt.Fprintf(&b, "exec %s \"$@\"\n", shellQuote(realBD))
	return b.String(), nil
}

// WriteWrapper writes WrapperScript(home, realBD) to path as an executable,
// creating path's directory and home (bd refuses a HOME that does not exist).
// It refuses a path that is realBD itself (directly or through a symlink):
// that would replace the real binary with a script that execs itself.
func WriteWrapper(path, home, realBD string) error {
	script, err := WrapperScript(home, realBD)
	if err != nil {
		return err
	}
	if sameFile(path, realBD) {
		return fmt.Errorf("bd tool-home wrapper: refusing to write over the real bd %s", realBD)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return fmt.Errorf("create bd tool home: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create bd tool-home wrapper dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // the wrapper must be executable
		return fmt.Errorf("write bd tool-home wrapper %s: %w", path, err)
	}
	return nil
}

// ScrubProcessEnv removes the invoking shell's XDG base directories and every
// BEADS_*/BD_* variable from the current process, then sets the pinned bd
// switches (BD_DOLT_SHARED_SERVER=false, BD_DISABLE_METRICS=1), so every child a harness builds from
// os.Environ() starts from explicit values only. HOME is left alone: gc needs
// the real one (see the package doc); bd gets re-homed by Environ or a wrapper.
//
// Call it from TestMain, before any goroutine reads the environment.
func ScrubProcessEnv() error {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if (IsHomeVar(name) && name != "HOME") || IsBeadsVar(name) {
			if err := os.Unsetenv(name); err != nil {
				return fmt.Errorf("unset %s: %w", name, err)
			}
		}
	}
	for _, pin := range pinnedBeadsVars {
		if err := os.Setenv(pin[0], pin[1]); err != nil {
			return fmt.Errorf("set %s: %w", pin[0], err)
		}
	}
	return nil
}

// sameFile reports whether a and b name the same file: the same cleaned
// absolute path, or two existing paths that resolve to one inode.
func sameFile(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil && errB == nil && absA == absB {
		return true
	}
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
