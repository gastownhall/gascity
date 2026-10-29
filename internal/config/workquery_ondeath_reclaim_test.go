package config

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runOnDeathScript runs the generated hook with a fake bd first on PATH and
// returns the hook's stdout and the log of every bd invocation. Unlike
// runLifecycleHookCommand it keeps stdout apart from stderr, because the
// controller call sites parse the hook's stdout for gc-recovery diagnostics.
func runOnDeathScript(t *testing.T, command, bdScript string) (stdout, bdLog string) {
	t.Helper()
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "bd"), []byte(bdScript), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	logPath := filepath.Join(tmp, "bd.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create bd log: %v", err)
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = []string{"PATH=" + tmp + ":" + os.Getenv("PATH"), "BD_LOG=" + logPath}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("run on_death hook: %v\nstdout:\n%s", err, out.String())
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	return out.String(), string(data)
}

func poolInstanceAgent() *Agent {
	return &Agent{
		Name:              "dog-1",
		Dir:               "hello-world",
		MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(5),
		PoolName: "hello-world/dog",
	}
}

// A named (non-pool) agent's session is not a leased pool seat: on_death never
// fires for it in production (computePoolDeathHandlers only covers pool
// instances), so the default hook must carry no release logic at all.
func TestEffectiveOnDeathNamedAgentHasNoReleaseLogic(t *testing.T) {
	for _, a := range []*Agent{
		{Name: "mayor"},
		{Name: "refinery", Dir: "hello-world"},
		{Name: "worker", MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(5)},
	} {
		if got := a.EffectiveOnDeath(); got != "" {
			t.Errorf("%s: EffectiveOnDeath() = %q, want no default release script", a.QualifiedName(), got)
		}
		if got := a.EffectiveOnDeathFor(QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}}); got != "" {
			t.Errorf("%s: EffectiveOnDeathFor() = %q, want no default release script", a.QualifiedName(), got)
		}
	}
}

// A user-supplied on_death override still wins for a named agent.
func TestEffectiveOnDeathNamedAgentOverrideStillReturnedVerbatim(t *testing.T) {
	a := &Agent{Name: "mayor", OnDeath: "custom-death-cmd"}
	if got := a.EffectiveOnDeath(); got != "custom-death-cmd" {
		t.Errorf("EffectiveOnDeath() = %q, want the override verbatim", got)
	}
}

// A pool instance's release goes through bd reclaim, scoped to the one bead id
// with no grace override, no force and no assignee: reclaim only reverts a
// bead whose lease has expired, so a bead a live session just re-claimed is
// never stolen back.
func TestEffectiveOnDeathPoolAgentUsesBdReclaim(t *testing.T) {
	got := poolInstanceAgent().EffectiveOnDeath()
	if !strings.Contains(got, `bd reclaim --id "$id" --json`) {
		t.Errorf("pool on_death must release through `bd reclaim --id \"$id\" --json`:\n%s", got)
	}
	for _, banned := range []string{
		`bd update "$id" --assignee`,
		`--status open`,
		`--older-than`,
		`--force`,
	} {
		if strings.Contains(got, banned) {
			t.Errorf("pool on_death must not contain %q:\n%s", banned, got)
		}
	}
	if !strings.Contains(got, RecoveryHookMarker) {
		t.Errorf("pool on_death must keep emitting the %q marker:\n%s", RecoveryHookMarker, got)
	}
}

// The route backfill only follows a bead the reclaim actually reverted.
func TestEffectiveOnDeathPoolAgentBackfillsRouteOnlyForReclaimedBeads(t *testing.T) {
	fakeBD := func(reclaimOut string) string {
		return `#!/bin/sh
set -eu
case "$1" in
  list)
    printf '%s\n' "$*" >> "$BD_LOG"
    printf '[{"id":"ga-1","type":"wisp","metadata":{}}]'
    ;;
  reclaim)
    printf '%s\n' "$*" >> "$BD_LOG"
    printf '%s' '` + reclaimOut + `'
    ;;
  update)
    printf '%s\n' "$*" >> "$BD_LOG"
    ;;
  *)
    exit 1
    ;;
esac
`
	}

	t.Run("reclaimed", func(t *testing.T) {
		stdout, log := runOnDeathScript(t, poolInstanceAgent().EffectiveOnDeath(),
			fakeBD(`{"reclaimed":[{"id":"ga-1","previous_owner":"hello-world/dog-1"}],"count":1,"schema_version":1}`))
		if !strings.Contains(log, "reclaim --id ga-1 --json") {
			t.Fatalf("bd log = %q, want reclaim of ga-1", log)
		}
		if !strings.Contains(log, "update ga-1 --set-metadata gc.run_target=hello-world/dog") {
			t.Fatalf("bd log = %q, want run_target backfill for the reclaimed bead", log)
		}
		for _, line := range strings.Split(log, "\n") {
			// The initial `bd list --assignee=... --status=...` read is expected;
			// only the writes (reclaim/update) must stay off status and assignee.
			if strings.HasPrefix(line, "list ") {
				continue
			}
			if strings.Contains(line, "--status") || strings.Contains(line, "--assignee") {
				t.Fatalf("bd log = %q, the backfill must not touch status or assignee (line %q)", log, line)
			}
		}
		if strings.Contains(stdout, RecoveryHookMarker) {
			t.Fatalf("stdout = %q, a clean release prints no recovery diagnostic", stdout)
		}
	})

	t.Run("nothing reclaimed (null)", func(t *testing.T) {
		stdout, log := runOnDeathScript(t, poolInstanceAgent().EffectiveOnDeath(),
			fakeBD(`{"count":0,"reclaimed":null,"schema_version":1,"scoped":true}`))
		if !strings.Contains(log, "reclaim --id ga-1 --json") {
			t.Fatalf("bd log = %q, want reclaim of ga-1", log)
		}
		if strings.Contains(log, "update ") {
			t.Fatalf("bd log = %q, a bead the reclaim left alone must not be written", log)
		}
		if strings.Contains(stdout, RecoveryHookMarker) {
			t.Fatalf("stdout = %q, count 0 is not a failure", stdout)
		}
	})

	t.Run("some other bead reclaimed", func(t *testing.T) {
		_, log := runOnDeathScript(t, poolInstanceAgent().EffectiveOnDeath(),
			fakeBD(`{"reclaimed":[{"id":"ga-other","previous_owner":"x"}],"count":1}`))
		if strings.Contains(log, "update ") {
			t.Fatalf("bd log = %q, the backfill is scoped to ids that were actually reclaimed", log)
		}
	})
}

// A bead that already carries a route is reclaimed and left otherwise alone.
func TestEffectiveOnDeathPoolAgentKeepsExistingRouteAfterReclaim(t *testing.T) {
	_, log := runOnDeathScript(t, poolInstanceAgent().EffectiveOnDeath(), `#!/bin/sh
set -eu
case "$1" in
  list)
    printf '%s\n' "$*" >> "$BD_LOG"
    printf '[{"id":"ga-routed","type":"wisp","metadata":{"gc.routed_to":"already/routed"}}]'
    ;;
  reclaim)
    printf '%s\n' "$*" >> "$BD_LOG"
    printf '{"reclaimed":[{"id":"ga-routed","previous_owner":"x"}],"count":1}'
    ;;
  *)
    exit 1
    ;;
esac
`)
	if !strings.Contains(log, "reclaim --id ga-routed --json") {
		t.Fatalf("bd log = %q, want reclaim of ga-routed", log)
	}
	if strings.Contains(log, "update ") {
		t.Fatalf("bd log = %q, an existing route is preserved without any write", log)
	}
}

// A failed reclaim prints the marker line carrying the bead id and bd's error
// text, writes nothing else, and does not stop the loop on the next bead.
func TestEffectiveOnDeathPoolAgentSurfacesReclaimFailure(t *testing.T) {
	stdout, log := runOnDeathScript(t, poolInstanceAgent().EffectiveOnDeath(), `#!/bin/sh
set -eu
case "$1" in
  list)
    printf '%s\n' "$*" >> "$BD_LOG"
    printf '[{"id":"ga-bad","type":"wisp","metadata":{}},{"id":"ga-good","type":"wisp","metadata":{"gc.routed_to":"r"}}]'
    ;;
  reclaim)
    printf '%s\n' "$*" >> "$BD_LOG"
    if [ "$3" = "ga-bad" ]; then
      echo "dolt: connection refused" >&2
      exit 1
    fi
    printf '{"reclaimed":[{"id":"ga-good","previous_owner":"x"}],"count":1}'
    ;;
  *)
    exit 1
    ;;
esac
`)
	if !strings.Contains(log, "reclaim --id ga-good --json") {
		t.Fatalf("bd log = %q, a failure on one bead must not stop the loop", log)
	}
	if strings.Contains(log, "update ") {
		t.Fatalf("bd log = %q, a bead whose reclaim failed must not be backfilled", log)
	}
	failures := ParseRecoveryFailures(stdout)
	if len(failures) != 1 {
		t.Fatalf("ParseRecoveryFailures(%q) = %+v, want exactly the ga-bad failure", stdout, failures)
	}
	f := failures[0]
	if f.Hook != "on_death" || f.BeadID != "ga-bad" || !strings.Contains(f.Error, "dolt: connection refused") {
		t.Fatalf("failure = %+v, want hook=on_death bead=ga-bad error containing bd's stderr", f)
	}
}

// A failed backfill is surfaced the same way.
func TestEffectiveOnDeathPoolAgentSurfacesBackfillFailure(t *testing.T) {
	stdout, _ := runOnDeathScript(t, poolInstanceAgent().EffectiveOnDeath(), `#!/bin/sh
set -eu
case "$1" in
  list)
    printf '[{"id":"ga-1","type":"wisp","metadata":{}}]'
    ;;
  reclaim)
    printf '{"reclaimed":[{"id":"ga-1","previous_owner":"x"}],"count":1}'
    ;;
  update)
    echo "metadata write rejected" >&2
    exit 1
    ;;
  *)
    exit 1
    ;;
esac
`)
	failures := ParseRecoveryFailures(stdout)
	if len(failures) != 1 || failures[0].BeadID != "ga-1" || !strings.Contains(failures[0].Error, "metadata write rejected") {
		t.Fatalf("ParseRecoveryFailures(%q) = %+v, want the ga-1 backfill failure", stdout, failures)
	}
}

func TestParseRecoveryFailures(t *testing.T) {
	t.Run("on_death and on_boot lines", func(t *testing.T) {
		out := "gc-recovery: on_death release failed for ga-1: boom\n" +
			"gc-recovery: on_boot reopen failed for ga-2: bad things\n"
		got := ParseRecoveryFailures(out)
		want := []RecoveryFailure{
			{Hook: "on_death", BeadID: "ga-1", Error: "boom"},
			{Hook: "on_boot", BeadID: "ga-2", Error: "bad things"},
		}
		if len(got) != len(want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("failure[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("multi-line error text stays with its bead", func(t *testing.T) {
		out := "gc-recovery: on_death release failed for ga-1: first line\nsecond line\n" +
			"gc-recovery: on_death release failed for ga-2: other\n"
		got := ParseRecoveryFailures(out)
		if len(got) != 2 {
			t.Fatalf("got %+v, want 2 failures", got)
		}
		if got[0].Error != "first line\nsecond line" {
			t.Errorf("failure[0].Error = %q, want both lines", got[0].Error)
		}
		if got[1].Error != "other" {
			t.Errorf("failure[1].Error = %q, want %q", got[1].Error, "other")
		}
	})

	t.Run("lines that are not failures are skipped", func(t *testing.T) {
		for _, out := range []string{
			"",
			"unrelated output\n",
			"gc-recovery: could not release for /rig/dog\n",
			"released 1 of 1\n",
		} {
			if got := ParseRecoveryFailures(out); len(got) != 0 {
				t.Errorf("ParseRecoveryFailures(%q) = %+v, want none", out, got)
			}
		}
	})

	t.Run("empty error text", func(t *testing.T) {
		got := ParseRecoveryFailures("gc-recovery: on_boot reopen failed for ga-9: \n")
		if len(got) != 1 || got[0].BeadID != "ga-9" || got[0].Error != "" {
			t.Fatalf("got %+v, want ga-9 with empty error", got)
		}
	})
}

// The on_boot default emits lines the parser reads back, so the emitter and the
// parser cannot drift apart.
func TestOnBootDefaultFailureLineRoundTripsThroughParser(t *testing.T) {
	a := poolInstanceAgent()
	stdout, _ := runOnDeathScript(t, a.EffectiveOnBoot(), `#!/bin/sh
set -eu
case "$1" in
  list)
    printf '[{"id":"ga-boot"}]'
    ;;
  update)
    echo "update refused" >&2
    exit 1
    ;;
  *)
    exit 1
    ;;
esac
`)
	got := ParseRecoveryFailures(stdout)
	if len(got) != 1 || got[0].Hook != "on_boot" || got[0].BeadID != "ga-boot" || !strings.Contains(got[0].Error, "update refused") {
		t.Fatalf("ParseRecoveryFailures(%q) = %+v, want the on_boot ga-boot failure", stdout, got)
	}
}
