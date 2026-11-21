package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	t.Setenv(managedDoltRecoverCallerEnv, "")
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
	assertProviderHealthJournaled(t, layout, "action=history-diagnostic", "clock moved backwards")
}

// TestHealthBeadsProviderCooldownHoldsAConcurrentReservationAheadOfNow seeds
// a reservation a moment ahead of the check's clock. That is what another
// process leaves when it reserves after this one read the time. The
// reservation must hold the cooldown rather than be reset as a clock
// reversal, or concurrent checks each recover.
func TestHealthBeadsProviderCooldownHoldsAConcurrentReservationAheadOfNow(t *testing.T) {
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	opsFile := writeBreakerAwarePreflightFakes(t, cityPath, "unhealthy")
	layout := strictProviderLayout(t, cityPath)
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	concurrent := now.Add(time.Millisecond)
	if err := os.MkdirAll(layout.PackStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedDoltRecoveryState(managedDoltRecoveryStatePath(layout), managedDoltRecoveryState{LastProviderAttemptAt: &concurrent}); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	prevNow := providerRecoverNow
	providerRecoverNow = func() time.Time { return now }
	t.Cleanup(func() { providerRecoverNow = prevNow })

	err := healthBeadsProvider(cityPath)

	if _, r := countOps(readCooldownProviderOps(t, opsFile), "health", "recover"); r != 0 {
		t.Fatalf("recover count = %d, want 0 (a concurrent reservation holds the cooldown)", r)
	}
	if err == nil || !strings.Contains(err.Error(), "unhealthy") || strings.Contains(err.Error(), "not recovering") {
		t.Fatalf("healthBeadsProvider() error = %v, want the plain unhealthy error", err)
	}
	if got := readProviderRecoveryHistory(t, cityPath).LastProviderAttemptAt; got == nil || !got.Equal(concurrent) {
		t.Fatalf("last_provider_attempt_at = %v, want the concurrent reservation %v kept", got, concurrent)
	}
	if journal, err := os.ReadFile(layout.LogFile); err == nil && strings.Contains(string(journal), "clock moved backwards") {
		t.Fatalf("journal = %q, want no clock reversal reported for a concurrent reservation", journal)
	}
}

func TestReserveManagedDoltProviderRecoverAttemptLocked(t *testing.T) {
	const cooldown = 30 * time.Second
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	at := func(offset time.Duration) *time.Time {
		v := now.Add(offset)
		return &v
	}
	tests := []struct {
		name         string
		recorded     *time.Time
		wantReserved bool
		wantReset    bool
	}{
		{name: "no reservation", wantReserved: true},
		{name: "reservation inside the cooldown", recorded: at(-5 * time.Second)},
		{name: "concurrent reservation just ahead of now", recorded: at(time.Millisecond)},
		{name: "reservation just under a cooldown ahead", recorded: at(cooldown - time.Second)},
		{name: "expired reservation", recorded: at(-cooldown - time.Second), wantReserved: true},
		{name: "reservation a cooldown or more ahead after the clock moved backwards", recorded: at(time.Hour), wantReserved: true, wantReset: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			layout := strictProviderLayout(t, cityPath)
			if err := os.MkdirAll(layout.PackStateDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := writeManagedDoltRecoveryState(managedDoltRecoveryStatePath(layout), managedDoltRecoveryState{LastProviderAttemptAt: tt.recorded}); err != nil {
				t.Fatalf("seed history: %v", err)
			}
			var diag bytes.Buffer

			reserved, err := reserveManagedDoltProviderRecoverAttemptLocked(layout, now, cooldown, &diag)

			if err != nil || reserved != tt.wantReserved {
				t.Fatalf("reserve = %v, %v; want %v, nil", reserved, err, tt.wantReserved)
			}
			if reset := strings.Contains(diag.String(), "clock moved backwards"); reset != tt.wantReset {
				t.Fatalf("diagnostics = %q, want a clock-reversal reset reported: %v", diag.String(), tt.wantReset)
			}
			got := readProviderRecoveryHistory(t, cityPath).LastProviderAttemptAt
			want := tt.recorded
			if tt.wantReserved {
				want = &now
			}
			if (got == nil) != (want == nil) || (got != nil && !got.Equal(*want)) {
				t.Fatalf("last_provider_attempt_at = %v, want %v", got, want)
			}
		})
	}
}

// TestHealthBeadsProviderHistoryFaultFallsBackToProcessCooldown makes the
// recovery history unreadable. Patrol recovery must still run, with the
// fault journaled, and this process's own cooldown must still space the
// attempts.
func TestHealthBeadsProviderHistoryFaultFallsBackToProcessCooldown(t *testing.T) {
	t.Setenv(managedDoltRecoverCallerEnv, "")
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	opsFile := writeBreakerAwarePreflightFakes(t, cityPath, "unhealthy")
	layout := strictProviderLayout(t, cityPath)
	if err := os.MkdirAll(managedDoltRecoveryStatePath(layout), 0o755); err != nil {
		t.Fatalf("make history unreadable: %v", err)
	}
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	prevNow := providerRecoverNow
	providerRecoverNow = func() time.Time { return now }
	t.Cleanup(func() { providerRecoverNow = prevNow })

	_ = healthBeadsProvider(cityPath)
	if _, r := countOps(readCooldownProviderOps(t, opsFile), "health", "recover"); r != 1 {
		t.Fatalf("recover count = %d, want 1 (a recovery history fault must not disable recovery)", r)
	}
	assertProviderHealthJournaled(t, layout, "action=history-diagnostic", "this process's own recovery cooldown")

	now = now.Add(providerRecoverCooldown() / 2)
	err := healthBeadsProvider(cityPath)
	if _, r := countOps(readCooldownProviderOps(t, opsFile), "health", "recover"); r != 1 {
		t.Fatalf("recover count inside the process cooldown = %d, want still 1", r)
	}
	if err == nil || !strings.Contains(err.Error(), "unhealthy") || strings.Contains(err.Error(), "not recovering") {
		t.Fatalf("healthBeadsProvider() inside the process cooldown error = %v, want the plain unhealthy error", err)
	}

	now = now.Add(providerRecoverCooldown())
	_ = healthBeadsProvider(cityPath)
	if _, r := countOps(readCooldownProviderOps(t, opsFile), "health", "recover"); r != 2 {
		t.Fatalf("recover count after the process cooldown = %d, want 2", r)
	}
}

func strictProviderLayout(t *testing.T, cityPath string) managedDoltRuntimeLayout {
	t.Helper()
	layout, err := resolveManagedDoltRuntimeLayoutStrict(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayoutStrict: %v", err)
	}
	return layout
}

// assertProviderHealthJournaled requires the managed dolt log at layout to
// hold a recovery journal line with every field in want, and each such line
// to be attributed to the provider health check that wrote it in-process.
func assertProviderHealthJournaled(t *testing.T, layout managedDoltRuntimeLayout, want ...string) {
	t.Helper()
	journal, err := os.ReadFile(layout.LogFile)
	if err != nil {
		t.Fatalf("read recovery journal: %v", err)
	}
	matched := 0
	for _, line := range strings.Split(string(journal), "\n") {
		if !strings.Contains(line, "gc dolt recovery:") || !containsEvery(line, want) {
			continue
		}
		matched++
		if !strings.Contains(line, `caller="provider-health"`) {
			t.Errorf("journal line %q is not attributed to the provider health check, want caller=\"provider-health\"", line)
		}
	}
	if matched == 0 {
		t.Fatalf("journal = %q, want a recovery line with %q", journal, want)
	}
}

func containsEvery(s string, fields []string) bool {
	for _, field := range fields {
		if !strings.Contains(s, field) {
			return false
		}
	}
	return true
}

// holdProviderLifecycleLock takes the city's managed dolt lifecycle lock on a
// descriptor of its own, as another process's in-flight recovery or start
// would, and keeps it until the test ends.
func holdProviderLifecycleLock(t *testing.T, layout managedDoltRuntimeLayout) {
	t.Helper()
	holder, err := openManagedDoltLifecycleLockAt(layout)
	if err != nil {
		t.Fatalf("open lifecycle lock: %v", err)
	}
	if locked, err := tryManagedDoltLifecycleLock(holder); err != nil || !locked {
		releaseManagedDoltLifecycleLock(holder)
		t.Fatalf("hold lifecycle lock: locked=%v err=%v", locked, err)
	}
	t.Cleanup(func() { releaseManagedDoltLifecycleLock(holder) })
}

// writeHealthyProviderFake installs the provider fakes with a health op that
// succeeds and returns the ops log path.
func writeHealthyProviderFake(t *testing.T, cityPath string) string {
	t.Helper()
	opsFile := writeBreakerAwarePreflightFakes(t, cityPath, "")
	body := fmt.Sprintf("#!/bin/sh\nset -eu\nprintf '%%s\\n' \"$1\" >> %q\ncase \"$1\" in\n  health) exit 0 ;;\n  *) exit 2 ;;\nesac\n", opsFile)
	if err := os.WriteFile(gcBeadsBdScriptPath(cityPath), []byte(body), 0o755); err != nil {
		t.Fatalf("write healthy provider fake: %v", err)
	}
	return opsFile
}

// TestHealthBeadsProviderInsideCooldownSkipsTheLifecycleLock holds the
// lifecycle lock, as an in-flight recovery does, while a reservation is
// inside the cooldown. The check must return the plain unhealthy error at
// once instead of waiting on the lock.
func TestHealthBeadsProviderInsideCooldownSkipsTheLifecycleLock(t *testing.T) {
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	opsFile := writeBreakerAwarePreflightFakes(t, cityPath, "unhealthy")
	layout := strictProviderLayout(t, cityPath)
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	reserved := now.Add(-5 * time.Second)
	if err := os.MkdirAll(layout.PackStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedDoltRecoveryState(managedDoltRecoveryStatePath(layout), managedDoltRecoveryState{LastProviderAttemptAt: &reserved}); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	prevNow := providerRecoverNow
	providerRecoverNow = func() time.Time { return now }
	t.Cleanup(func() { providerRecoverNow = prevNow })
	holdProviderLifecycleLock(t, layout)

	started := time.Now()
	err := healthBeadsProvider(cityPath)
	waited := time.Since(started)

	if err == nil || !strings.Contains(err.Error(), "unhealthy") || strings.Contains(err.Error(), "not recovering") {
		t.Fatalf("healthBeadsProvider() error = %v, want the plain unhealthy error inside the cooldown", err)
	}
	if waited > 3*time.Second {
		t.Fatalf("healthBeadsProvider() took %s inside the cooldown, want no wait on the held lifecycle lock", waited)
	}
	if _, r := countOps(readCooldownProviderOps(t, opsFile), "health", "recover"); r != 0 {
		t.Fatalf("recover count = %d, want 0 inside the cooldown", r)
	}
}

// TestHealthBeadsProviderLifecycleLockWaitHonorsContext holds the lifecycle
// lock while a check needs it to write the recovery history. The check must
// give up when its own deadline passes, not after the history lock timeout,
// and leave the history as it was.
func TestHealthBeadsProviderLifecycleLockWaitHonorsContext(t *testing.T) {
	identity := managedDoltServerIdentity{PID: 4242, StartTime: "123456", Port: 3306}
	tests := []struct {
		name    string
		healthy bool
	}{
		{name: "unhealthy check reserving a recovery"},
		{name: "healthy check clearing a streak", healthy: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			writeMinimalCityToml(t, cityPath)
			var opsFile string
			if tt.healthy {
				opsFile = writeHealthyProviderFake(t, cityPath)
			} else {
				opsFile = writeBreakerAwarePreflightFakes(t, cityPath, "unhealthy")
			}
			layout := strictProviderLayout(t, cityPath)
			now := time.Now().UTC()
			if tt.healthy {
				seedUnresponsiveHistory(t, layout, identity, now.Add(-time.Minute), now.Add(-30*time.Second), 2)
			}
			holdProviderLifecycleLock(t, layout)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			started := time.Now()
			err := healthBeadsProviderContext(ctx, cityPath, true)
			waited := time.Since(started)

			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("healthBeadsProviderContext() error = %v, want the caller's deadline", err)
			}
			if waited > 5*time.Second {
				t.Fatalf("healthBeadsProviderContext() took %s, want it bounded by the 1s caller deadline", waited)
			}
			if _, r := countOps(readCooldownProviderOps(t, opsFile), "health", "recover"); r != 0 {
				t.Fatalf("recover count = %d, want 0", r)
			}
			state := readProviderRecoveryHistory(t, cityPath)
			if tt.healthy && (state.Streak == nil || state.Streak.Failures != 2) {
				t.Fatalf("streak = %+v, want it intact when the clear gave up", state.Streak)
			}
			if !tt.healthy && state.LastProviderAttemptAt != nil {
				t.Fatalf("last_provider_attempt_at = %v, want no reservation when the wait gave up", state.LastProviderAttemptAt)
			}
		})
	}
}

// TestHealthBeadsProviderReportsABusyLifecycleLockAsInFlight holds the
// lifecycle lock past the history lock timeout outside any cooldown. The
// check must not recover, and must say that another lifecycle operation is
// in flight rather than that the cooldown is unavailable.
func TestHealthBeadsProviderReportsABusyLifecycleLockAsInFlight(t *testing.T) {
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	opsFile := writeBreakerAwarePreflightFakes(t, cityPath, "unhealthy")
	layout := strictProviderLayout(t, cityPath)
	holdProviderLifecycleLock(t, layout)
	prevTimeout := managedDoltHistoryLockTimeout
	managedDoltHistoryLockTimeout = 200 * time.Millisecond
	t.Cleanup(func() { managedDoltHistoryLockTimeout = prevTimeout })

	err := healthBeadsProvider(cityPath)

	if err == nil || !strings.Contains(err.Error(), "unhealthy") || !strings.Contains(err.Error(), "another managed dolt lifecycle operation is in flight") {
		t.Fatalf("healthBeadsProvider() error = %v, want the unhealthy error and the in-flight lifecycle operation", err)
	}
	if strings.Contains(err.Error(), "cooldown is unavailable") {
		t.Fatalf("healthBeadsProvider() error = %v, want a busy lock not reported as an unavailable cooldown", err)
	}
	if _, r := countOps(readCooldownProviderOps(t, opsFile), "health", "recover"); r != 0 {
		t.Fatalf("recover count = %d, want 0 while another lifecycle operation holds the lock", r)
	}
	if reserved := readProviderRecoveryHistory(t, cityPath).LastProviderAttemptAt; reserved != nil {
		t.Fatalf("last_provider_attempt_at = %v, want no reservation while the lock is busy", reserved)
	}
}
