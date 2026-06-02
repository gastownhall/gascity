//go:build acceptance_b

package tierb_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestGastownIdleOpenBeadCountsStayBounded(t *testing.T) {
	probe := idleBeadStabilityProbeConfig(t)
	env := newIsolatedTierBEnv(t, "fake")
	c := helpers.NewCity(t, env)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	// gc init --from starts a controller for the copied city. Restart under the
	// isolated supervisor after installing the fast idle orders below.
	if out, err := c.GC("stop", c.Dir); err != nil {
		t.Logf("gc stop after init-from: %v\n%s", err, out)
	}
	if out, err := c.GC("unregister", c.Dir); err != nil {
		t.Logf("gc unregister after init-from: %v\n%s", err, out)
	}

	rewriteGastownPatrolInterval(t, c.Dir, "1s")
	writeGastownIdleProbeOrders(t, c.Dir)

	c.StartWithSupervisor()

	var activity beadCountSnapshot
	if ok := c.WaitForCondition(func() bool {
		activity = readOpenBeadSnapshot(t, c.Dir)
		return activity.OpenWisps > 0
	}, 30*time.Second); !ok {
		t.Fatalf("idle probe order did not create a wisp within 30s; last snapshot: %s", activity)
	}

	time.Sleep(probe.Warmup)
	samples := sampleOpenBeadCounts(t, c.Dir, probe)
	assertOpenBeadCountsStayBounded(t, samples)
}

func TestPlainIdleOpenBeadCountsStayBounded(t *testing.T) {
	probe := idleBeadStabilityProbeConfig(t)
	env := newIsolatedTierBEnv(t, "fake")
	c := helpers.NewCity(t, env)
	c.Init("claude")
	writePlainIdleProbeOrders(t, c.Dir)

	c.StartWithSupervisor()

	time.Sleep(probe.Warmup)
	samples := sampleOpenBeadCounts(t, c.Dir, probe)
	assertOpenBeadCountsStayBounded(t, samples)
}

func sampleOpenBeadCounts(t *testing.T, cityDir string, probe idleBeadStabilityProbe) []beadCountSnapshot {
	t.Helper()
	samples := make([]beadCountSnapshot, 0, probe.Samples)
	for i := 0; i < probe.Samples; i++ {
		samples = append(samples, readOpenBeadSnapshot(t, cityDir))
		time.Sleep(probe.Interval)
	}
	return samples
}

func assertOpenBeadCountsStayBounded(t *testing.T, samples []beadCountSnapshot) {
	t.Helper()
	first := samples[0]
	last := samples[len(samples)-1]
	maxIssues, maxWisps := first.OpenIssues, first.OpenWisps
	for _, sample := range samples[1:] {
		if sample.OpenIssues > maxIssues {
			maxIssues = sample.OpenIssues
		}
		if sample.OpenWisps > maxWisps {
			maxWisps = sample.OpenWisps
		}
	}

	const toleratedOpenJitter = 3
	if maxIssues > first.OpenIssues+toleratedOpenJitter || last.OpenIssues > first.OpenIssues+toleratedOpenJitter {
		t.Fatalf("open issue count grew during idle cycles:\n%s", formatBeadCountSnapshots(samples))
	}
	if maxWisps > first.OpenWisps+toleratedOpenJitter || last.OpenWisps > first.OpenWisps+toleratedOpenJitter {
		t.Fatalf("open wisp count grew during idle cycles:\n%s", formatBeadCountSnapshots(samples))
	}
}

func TestIdleBeadStabilityProbeConfigReadsNightlyOverrides(t *testing.T) {
	t.Setenv("GC_IDLE_BEAD_STABILITY_WARMUP", "10s")
	t.Setenv("GC_IDLE_BEAD_STABILITY_SAMPLES", "36")
	t.Setenv("GC_IDLE_BEAD_STABILITY_INTERVAL", "5s")

	got := idleBeadStabilityProbeConfig(t)
	if got.Warmup != 10*time.Second {
		t.Fatalf("Warmup = %s, want 10s", got.Warmup)
	}
	if got.Samples != 36 {
		t.Fatalf("Samples = %d, want 36", got.Samples)
	}
	if got.Interval != 5*time.Second {
		t.Fatalf("Interval = %s, want 5s", got.Interval)
	}
}

type idleBeadStabilityProbe struct {
	Warmup   time.Duration
	Samples  int
	Interval time.Duration
}

func idleBeadStabilityProbeConfig(t *testing.T) idleBeadStabilityProbe {
	t.Helper()
	return idleBeadStabilityProbe{
		Warmup:   idleProbeDurationEnv(t, "GC_IDLE_BEAD_STABILITY_WARMUP", 3*time.Second),
		Samples:  idleProbeSamplesEnv(t, "GC_IDLE_BEAD_STABILITY_SAMPLES", 8),
		Interval: idleProbeDurationEnv(t, "GC_IDLE_BEAD_STABILITY_INTERVAL", 2*time.Second),
	}
}

func idleProbeDurationEnv(t *testing.T, key string, fallback time.Duration) time.Duration {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		t.Fatalf("%s must be a positive Go duration, got %q", key, raw)
	}
	return d
}

func idleProbeSamplesEnv(t *testing.T, key string, fallback int) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 2 {
		t.Fatalf("%s must be an integer >= 2, got %q", key, raw)
	}
	return n
}

func newIsolatedTierBEnv(t *testing.T, sessionProvider string) *helpers.Env {
	t.Helper()
	gcBin, err := helpers.ResolveGCPath(testEnvB)
	if err != nil {
		t.Fatalf("resolve gc binary: %v", err)
	}

	root := helpers.TempDir(t)
	gcHome := filepath.Join(root, "gc-home")
	runtimeDir := filepath.Join(root, "runtime")
	for _, dir := range []string{gcHome, runtimeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	if err := helpers.WriteSupervisorConfig(gcHome); err != nil {
		t.Fatalf("write supervisor config: %v", err)
	}

	return helpers.NewEnv(gcBin, gcHome, runtimeDir).
		With("GC_BEADS", "file").
		With("GC_DOLT", "skip").
		With("GC_SESSION", sessionProvider)
}

func rewriteGastownPatrolInterval(t *testing.T, cityDir, interval string) {
	t.Helper()
	path := filepath.Join(cityDir, "city.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read city.toml: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, `patrol_interval = "30s"`) {
		t.Fatalf("city.toml missing expected gastown patrol interval")
	}
	body = strings.Replace(body, `patrol_interval = "30s"`, fmt.Sprintf("patrol_interval = %q", interval), 1)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
}

func writeGastownIdleProbeOrders(t *testing.T, cityDir string) {
	t.Helper()
	orderDir := filepath.Join(cityDir, "orders")
	if err := os.MkdirAll(orderDir, 0o755); err != nil {
		t.Fatalf("create orders dir: %v", err)
	}
	formulaOrder, err := os.ReadFile(filepath.Join(cityDir, "packs", "gastown", "orders", "digest-generate.toml"))
	if err != nil {
		t.Fatalf("read copied digest order: %v", err)
	}
	formulaOrderBody := string(formulaOrder)
	if !strings.Contains(formulaOrderBody, `interval = "24h"`) {
		t.Fatalf("copied digest order missing expected interval")
	}
	formulaOrderBody = strings.Replace(formulaOrderBody, `interval = "24h"`, `interval = "1s"`, 1)
	orders := map[string]string{
		"idle-wisp.toml": formulaOrderBody,
		"idle-exec.toml": `[order]
description = "Nightly idle-city exec tracking leak probe"
exec = "true"
trigger = "cooldown"
interval = "1s"
`,
	}
	for name, body := range orders {
		if err := os.WriteFile(filepath.Join(orderDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write order %s: %v", name, err)
		}
	}
}

func writePlainIdleProbeOrders(t *testing.T, cityDir string) {
	t.Helper()
	orderDir := filepath.Join(cityDir, "orders")
	if err := os.MkdirAll(orderDir, 0o755); err != nil {
		t.Fatalf("create orders dir: %v", err)
	}
	body := `[order]
description = "Nightly plain idle-city exec tracking leak probe"
exec = "true"
trigger = "cooldown"
interval = "1s"
`
	if err := os.WriteFile(filepath.Join(orderDir, "idle-exec.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write plain idle order: %v", err)
	}
}

type beadCountSnapshot struct {
	At         time.Time
	OpenIssues int
	OpenWisps  int
	IssueIDs   []string
	WispIDs    []string
}

func readOpenBeadSnapshot(t *testing.T, cityDir string) beadCountSnapshot {
	t.Helper()
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cityDir, ".gc", "beads.json"))
	if err != nil {
		t.Fatalf("open file bead store: %v", err)
	}

	issues, err := store.List(beads.ListQuery{Status: "open", AllowScan: true, TierMode: beads.TierIssues})
	if err != nil {
		t.Fatalf("list open issue beads: %v", err)
	}
	wisps, err := store.List(beads.ListQuery{Status: "open", AllowScan: true, TierMode: beads.TierWisps})
	if err != nil {
		t.Fatalf("list open wisp beads: %v", err)
	}

	return beadCountSnapshot{
		At:         time.Now(),
		OpenIssues: len(issues),
		OpenWisps:  len(wisps),
		IssueIDs:   describeOpenBeads(issues),
		WispIDs:    describeOpenBeads(wisps),
	}
}

func describeOpenBeads(rows []beads.Bead) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		kind := row.Metadata["gc.kind"]
		if kind == "" {
			kind = row.Type
		}
		out = append(out, fmt.Sprintf("%s:%s:%s", row.ID, row.Status, kind))
	}
	sort.Strings(out)
	if len(out) > 12 {
		out = append(out[:12], fmt.Sprintf("...(+%d)", len(out)-12))
	}
	return out
}

func formatBeadCountSnapshots(samples []beadCountSnapshot) string {
	var b strings.Builder
	for i, sample := range samples {
		fmt.Fprintf(&b, "%02d at=%s open_issues=%d open_wisps=%d issue_ids=%v wisp_ids=%v\n",
			i,
			sample.At.Format(time.RFC3339),
			sample.OpenIssues,
			sample.OpenWisps,
			sample.IssueIDs,
			sample.WispIDs,
		)
	}
	return b.String()
}

func (s beadCountSnapshot) String() string {
	return fmt.Sprintf("open_issues=%d open_wisps=%d issue_ids=%v wisp_ids=%v",
		s.OpenIssues, s.OpenWisps, s.IssueIDs, s.WispIDs)
}
