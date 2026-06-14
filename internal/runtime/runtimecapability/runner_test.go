package runtimecapability

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// refConfig parameterizes the golden reference runtime: which capabilities it
// declares, and which of the three it actually satisfies. The golden sets all
// true; a mutant flips one off to prove negative gating.
type refConfig struct {
	caps                 []string
	materializeWorkspace bool
	installTooling       bool
	injectIdentity       bool
}

func goldenConfig() refConfig {
	return refConfig{
		caps:                 []string{"env.workspace", "env.tooling", "env.identity"},
		materializeWorkspace: true,
		installTooling:       true,
		injectIdentity:       true,
	}
}

// writeRef generates a stateful RPP shell runtime backed by a state dir. It
// implements protocol/start/exec/stop/is-running. start materializes the
// work_dir, installs a fake toolchain, and records the identity env — each
// gated by the config so a mutant can break exactly one.
func writeRef(t *testing.T, cfg refConfig) string {
	t.Helper()
	state := t.TempDir()
	capsJSON := "["
	for i, c := range cfg.caps {
		if i > 0 {
			capsJSON += ","
		}
		capsJSON += fmt.Sprintf("%q", c)
	}
	capsJSON += "]"

	materialize := `mkdir -p "$D/workdir"; [ -n "$wd" ] && cp -a "$wd/." "$D/workdir/" 2>/dev/null || true`
	if !cfg.materializeWorkspace {
		materialize = `mkdir -p "$D/workdir"` // create the dir but DON'T copy the work_dir
	}
	install := `mkdir -p "$D/bin"; for tprog in gc bd git; do printf '#!/bin/sh\necho "%s version (ref)\n"' "$tprog" > "$D/bin/$tprog"; chmod +x "$D/bin/$tprog"; done`
	if !cfg.installTooling {
		install = `mkdir -p "$D/bin"` // no gc/bd/git installed
	}
	inject := `printf 'export GC_SESSION=%s\n' "$sess" > "$D/env"`
	if !cfg.injectIdentity {
		inject = `: > "$D/env"` // empty env
	}

	body := fmt.Sprintf(`#!/bin/sh
S=%q
op="$1"; name="$2"
D="$S/$name"
case "$op" in
  protocol) printf '{"version":0,"capabilities":%s}\n' ;;
  start)
    cfg=$(cat)
    wd=$(printf '%%s' "$cfg" | sed -n 's/.*"work_dir":"\([^"]*\)".*/\1/p')
    sess=$(printf '%%s' "$cfg" | sed -n 's/.*"GC_SESSION":"\([^"]*\)".*/\1/p')
    mkdir -p "$D"
    %s
    %s
    %s
    ;;
  exec)
    cmd=$(cat)
    [ -d "$D/workdir" ] || exit 1
    . "$D/env" 2>/dev/null || true
    cd "$D/workdir" || exit 1
    # Controlled PATH so the "session" models an isolated sandbox: only the
    # reference-installed tools ($D/bin) + base utils — host gc/bd do not leak.
    PATH="$D/bin:/usr/bin:/bin" sh -c "$cmd"
    exit $?
    ;;
  stop) rm -rf "$D" ;;
  is-running) [ -d "$D" ] && echo true || echo false ;;
  *) exit 2 ;;
esac
`, state, capsJSON, materialize, install, inject)

	path := filepath.Join(t.TempDir(), "gc-runtime-ref")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func run(t *testing.T, path string) Report {
	t.Helper()
	rep, err := Run(context.Background(), path, Options{Command: "true"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rep
}

func TestProbesCoverCatalog(t *testing.T) {
	for _, cap := range catalog {
		if _, ok := probes[cap.Code]; !ok {
			t.Errorf("catalog capability %s has no probe", cap.Code)
		}
	}
	if len(probes) != len(catalog) {
		t.Errorf("probes (%d) and catalog (%d) differ", len(probes), len(catalog))
	}
}

func TestGoldenReferenceSatisfiesAllDeclared(t *testing.T) {
	rep := run(t, writeRef(t, goldenConfig()))
	if rep.Failed() {
		for _, res := range rep.Results {
			if res.Status == StatusFail {
				t.Errorf("golden failed %s: %s", res.Code, res.Detail)
			}
		}
		t.Fatalf("golden must pass every declared capability; summary=%+v", rep.Summary)
	}
	if rep.Summary.Passed != len(catalog) {
		t.Errorf("golden passed %d/%d", rep.Summary.Passed, len(catalog))
	}
}

func TestUndeclaredCapabilitiesSkip(t *testing.T) {
	// A runtime that declares no env.* caps: every capability SKIPs, run is
	// not failed (a tmux/local runtime that needs no materialization).
	cfg := goldenConfig()
	cfg.caps = nil
	rep := run(t, writeRef(t, cfg))
	if rep.Failed() {
		t.Errorf("a runtime declaring no env.* caps must not fail; got %+v", rep.Summary)
	}
	if rep.Summary.Skipped != len(catalog) {
		t.Errorf("skipped %d, want %d", rep.Summary.Skipped, len(catalog))
	}
}

// TestEveryCapabilityIsGated is the negative-gating proof: a runtime that
// DECLARES a capability but does not satisfy it must FAIL exactly that
// capability's probe. "Declared" is not "guaranteed".
func TestEveryCapabilityIsGated(t *testing.T) {
	mutants := []struct {
		target Code
		mutate func(*refConfig)
	}{
		{CapWorkspace, func(c *refConfig) { c.materializeWorkspace = false }},
		{CapTooling, func(c *refConfig) { c.installTooling = false }},
		{CapIdentity, func(c *refConfig) { c.injectIdentity = false }},
	}
	covered := map[Code]bool{}
	for _, m := range mutants {
		covered[m.target] = true
	}
	for _, cap := range catalog {
		if !covered[cap.Code] {
			t.Errorf("capability %s has no negative-gating mutant", cap.Code)
		}
	}

	for _, m := range mutants {
		t.Run(string(m.target), func(t *testing.T) {
			cfg := goldenConfig()
			m.mutate(&cfg)
			rep := run(t, writeRef(t, cfg))
			var got Result
			for _, res := range rep.Results {
				if res.Code == m.target {
					got = res
				}
			}
			if got.Status != StatusFail {
				t.Errorf("a runtime that declares %s but doesn't provide it must FAIL it; got %s (%s)",
					m.target, got.Status, got.Detail)
			}
		})
	}
}
