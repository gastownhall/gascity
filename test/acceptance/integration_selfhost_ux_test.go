//go:build acceptance_a

package acceptance_test

import (
	"os"
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestSelfhostUX_PackV1V2Collision(t *testing.T) {
	t.Skip("pending ga-cwdaqv: supervised gc start does not validate config pre-registration")

	c := helpers.NewCity(t, testEnv)
	c.Init("claude")
	c.WriteV1AgentBlock("worker")
	c.WriteV2AgentDir("worker")

	start := time.Now()
	out := c.StartExpectingFatal(t)
	if elapsed := time.Since(start); elapsed > durationFromEnv(t, "GC_TEST_GC_START_BUDGET", 30*time.Second) {
		t.Fatalf("gc start fatal path took %s; output:\n%s", elapsed, out)
	}
	for _, want := range []string{
		"pack v1/v2 layout collision",
		"pack.toml ([[agent]] worker)",
		"agents/worker/agent.toml",
		"docs/packv2/migration.mdx",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("gc start fatal output missing %q:\n%s", want, out)
		}
	}
	// TODO(ga-q0bf.1): assert structured trailing line once the gc-start summary ships.
}

func TestSelfhostUX_GcStopBypassValidation(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")
	c.WriteV1AgentBlock("worker")
	c.WriteV2AgentDir("worker")

	out, err := c.GC("stop", c.Dir)
	if err != nil {
		t.Fatalf("gc stop should bypass broken config validation: %v\n%s", err, out)
	}
}

func TestSelfhostUX_BinaryDriftRestart(t *testing.T) {
	t.Skip("pending merge of builder/ga-xxqx-1 (PR#2219): drift auto-restart not on origin/main")

	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	initialBinary, err := helpers.ResolveGCPath(testEnv)
	if err != nil {
		t.Fatalf("resolve initial gc binary: %v", err)
	}
	initialOut, err := c.GC("start", c.Dir)
	if err != nil {
		t.Fatalf("initial gc start: %v\n%s", err, initialOut)
	}

	rebuiltBinary := helpers.BuildGC(t.TempDir())
	if rebuiltBinary == initialBinary {
		t.Fatalf("rebuilt binary path should differ from initial path %q", initialBinary)
	}
	driftEnv := helpers.NewEnv(rebuiltBinary, testEnv.Get("GC_HOME"), testEnv.Get("XDG_RUNTIME_DIR"))
	out, err := helpers.RunGC(driftEnv, c.Dir, "start", c.Dir)
	if err != nil {
		t.Fatalf("gc start after binary drift: %v\n%s", err, out)
	}
	for _, want := range []string{"binary drift", rebuiltBinary, "Supervisor:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("binary drift restart output missing %q:\n%s", want, out)
		}
	}
}

func TestSelfhostUX_OpInitFastPath(t *testing.T) {
	budget := durationFromEnv(t, "GC_TEST_GC_INIT_FAST_BUDGET", 5*time.Second)
	c := helpers.NewCity(t, testEnv)

	start := time.Now()
	c.Init("claude")
	if elapsed := time.Since(start); elapsed > budget {
		t.Fatalf("gc init took %s, want <= %s", elapsed, budget)
	}
}

func TestSelfhostUX_Composed(t *testing.T) {
	t.Skip("placeholder for ga-q0bf.1, ga-r8iz.1, ga-6wrr.1, and post-PR#2219 assertions")
}

func TestSelfhostUX_StructuredStartSummaryPlaceholder(t *testing.T) {
	t.Skip("placeholder for ga-q0bf.1")
}

func TestSelfhostUX_StopBypassDetailsPlaceholder(t *testing.T) {
	t.Skip("placeholder for ga-r8iz.1")
}

func TestSelfhostUX_MigrationGuideURLPlaceholder(t *testing.T) {
	t.Skip("placeholder for ga-6wrr.1")
}

func TestSelfhostUX_BinaryDriftPostMergePlaceholder(t *testing.T) {
	t.Skip("placeholder for ga-xxqx")
}

func durationFromEnv(t *testing.T, name string, fallback time.Duration) time.Duration {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("parse %s=%q: %v", name, raw, err)
	}
	if d <= 0 {
		t.Fatalf("%s must be positive, got %s", name, d)
	}
	return d
}
