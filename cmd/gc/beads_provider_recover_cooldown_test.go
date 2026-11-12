package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const providerCooldownHelperCityEnv = "GC_TEST_PROVIDER_COOLDOWN_HELPER_CITY"

// TestHealthBeadsProviderCooldownHelperProcess runs one provider health check
// in its own process for TestHealthBeadsProviderCooldownSpansProcesses.
func TestHealthBeadsProviderCooldownHelperProcess(_ *testing.T) {
	cityPath := os.Getenv(providerCooldownHelperCityEnv)
	if cityPath == "" {
		return
	}
	_ = healthBeadsProvider(cityPath)
}

func readProviderRecoveryHistory(t *testing.T, cityPath string) managedDoltRecoveryState {
	t.Helper()
	layout, err := resolveManagedDoltRuntimeLayoutStrict(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayoutStrict: %v", err)
	}
	state, diagnostic, err := readManagedDoltRecoveryState(managedDoltRecoveryStatePath(layout))
	if err != nil || diagnostic != "" {
		t.Fatalf("readManagedDoltRecoveryState: diagnostic=%q err=%v", diagnostic, err)
	}
	return state
}

func readCooldownProviderOps(t *testing.T, opsFile string) []string {
	t.Helper()
	ops, err := os.ReadFile(opsFile)
	if err != nil {
		t.Fatalf("read provider ops: %v", err)
	}
	return strings.Fields(strings.TrimSpace(string(ops)))
}

func TestHealthBeadsProviderCooldownSpansProcesses(t *testing.T) {
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	opsFile := writeBreakerAwarePreflightFakes(t, cityPath, "unhealthy")

	const processes = 3
	type helper struct {
		cmd *exec.Cmd
		out *bytes.Buffer
	}
	helpers := make([]helper, 0, processes)
	for range processes {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHealthBeadsProviderCooldownHelperProcess$")
		cmd.Env = sanitizedBaseEnv(providerCooldownHelperCityEnv + "=" + cityPath)
		out := &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatalf("start provider health process: %v", err)
		}
		helpers = append(helpers, helper{cmd: cmd, out: out})
	}
	for _, h := range helpers {
		if err := h.cmd.Wait(); err != nil {
			t.Fatalf("provider health process: %v\n%s", err, h.out.String())
		}
	}

	if h, r := countOps(readCooldownProviderOps(t, opsFile), "health", "recover"); h != processes || r != 1 {
		t.Fatalf("provider ops health=%d recover=%d; want health=%d and one recover across processes within the cooldown", h, r, processes)
	}
	reserved := readProviderRecoveryHistory(t, cityPath).LastProviderAttemptAt
	if reserved == nil {
		t.Fatal("recovery history has no provider attempt reservation")
	}

	prevNow := providerRecoverNow
	providerRecoverNow = func() time.Time { return reserved.Add(providerRecoverCooldown() + time.Second) }
	t.Cleanup(func() { providerRecoverNow = prevNow })
	_ = healthBeadsProvider(cityPath)
	if _, r := countOps(readCooldownProviderOps(t, opsFile), "health", "recover"); r != 2 {
		t.Fatalf("recover count after the cooldown expired = %d, want 2", r)
	}
}

func TestHealthBeadsProviderCooldownResetsAReservationAheadOfNow(t *testing.T) {
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	opsFile := writeBreakerAwarePreflightFakes(t, cityPath, "unhealthy")
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	layout, err := resolveManagedDoltRuntimeLayoutStrict(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayoutStrict: %v", err)
	}
	future := now.Add(time.Hour)
	if err := os.MkdirAll(layout.PackStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedDoltRecoveryState(managedDoltRecoveryStatePath(layout), managedDoltRecoveryState{LastProviderAttemptAt: &future}); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	prevNow := providerRecoverNow
	providerRecoverNow = func() time.Time { return now }
	t.Cleanup(func() { providerRecoverNow = prevNow })

	_ = healthBeadsProvider(cityPath)

	if _, r := countOps(readCooldownProviderOps(t, opsFile), "health", "recover"); r != 1 {
		t.Fatalf("recover count = %d, want 1 (a reservation ahead of now cannot block recovery)", r)
	}
	if got := readProviderRecoveryHistory(t, cityPath).LastProviderAttemptAt; got == nil || !got.Equal(now) {
		t.Fatalf("last_provider_attempt_at = %v, want reset to %v", got, now)
	}
	journal, err := os.ReadFile(layout.LogFile)
	if err != nil {
		t.Fatalf("read recovery journal: %v", err)
	}
	if !strings.Contains(string(journal), "action=history-diagnostic") || !strings.Contains(string(journal), "clock moved backwards") {
		t.Fatalf("journal = %q, want the clock-reversal reset journaled", journal)
	}
}

func TestHealthBeadsProviderCooldownUnavailableDoesNotRecover(t *testing.T) {
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	opsFile := writeBreakerAwarePreflightFakes(t, cityPath, "unhealthy")
	layout, err := resolveManagedDoltRuntimeLayoutStrict(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayoutStrict: %v", err)
	}
	if err := os.MkdirAll(managedDoltRecoveryStatePath(layout), 0o755); err != nil {
		t.Fatalf("make history unreadable: %v", err)
	}

	err = healthBeadsProvider(cityPath)
	if err == nil || !strings.Contains(err.Error(), "unhealthy") || !strings.Contains(err.Error(), "cooldown") {
		t.Fatalf("healthBeadsProvider() error = %v, want the unhealthy error plus the unavailable cooldown", err)
	}
	if _, r := countOps(readCooldownProviderOps(t, opsFile), "health", "recover"); r != 0 {
		t.Fatalf("recover count = %d, want 0 when the cooldown cannot be checked", r)
	}
}
