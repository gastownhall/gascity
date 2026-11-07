package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fanOutFixture is a city whose city scope and every rig scope are
// provider-owned. The provider script appends "<op> <scope root>" to a log for
// every run, fails `health` for a scope while <scope>/unhealthy exists and fails
// `recover` while <scope>/recover-fails exists, so a test chooses which scopes
// are unhealthy and reads back exactly which scope ops a call started.
type fanOutFixture struct {
	t      *testing.T
	city   string
	scopes []string // the city first, then each rig in config order
	log    string
}

func newFanOutFixture(t *testing.T, rigs int) *fanOutFixture {
	t.Helper()
	t.Setenv("GC_SUSPENDED", "")
	stubBeadsScopeIdleRetired(t, false)
	city := t.TempDir()
	fx := &fanOutFixture{t: t, city: city, log: filepath.Join(city, "provider-ops")}
	fx.scopes = []string{normalizePathForCompare(city)}
	toml := "[workspace]\nname = \"test\"\n"
	script := filepath.Join(city, "provider.sh")
	toml += "[beads]\nprovider = \"exec:" + script + "\"\n"
	for i := 1; i <= rigs; i++ {
		rel := fmt.Sprintf("rigs/r%d", i)
		if err := os.MkdirAll(filepath.Join(city, rel), 0o755); err != nil {
			t.Fatal(err)
		}
		toml += fmt.Sprintf("[[rigs]]\nname = \"r%d\"\npath = %q\n", i, rel)
		fx.scopes = append(fx.scopes, normalizePathForCompare(filepath.Join(city, rel)))
	}
	body := fmt.Sprintf(`#!/bin/sh
scope=$(dirname "$BEADS_DIR")
printf '%%s %%s\n' "$1" "$scope" >> %q
if [ "$1" = health ] && [ -e "$scope/unhealthy" ]; then
  exit 1
fi
if [ "$1" = recover ] && [ -e "$scope/recover-fails" ]; then
  exit 1
fi
`, fx.log)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(toml), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	for _, scope := range fx.scopes {
		if err := persistProviderScopeOwnership(city, scope, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
			t.Fatal(err)
		}
		if err := markProviderScopeOwnershipReady(city, scope); err != nil {
			t.Fatal(err)
		}
	}
	return fx
}

// setUnhealthy makes `health` fail for exactly the named scopes (by index).
func (fx *fanOutFixture) setUnhealthy(idx ...int) {
	fx.t.Helper()
	bad := make(map[int]bool, len(idx))
	for _, i := range idx {
		bad[i] = true
	}
	for i, scope := range fx.scopes {
		marker := filepath.Join(scope, "unhealthy")
		if bad[i] {
			if err := os.WriteFile(marker, nil, 0o644); err != nil { //nolint:gosec // fixture
				fx.t.Fatal(err)
			}
		} else if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			fx.t.Fatal(err)
		}
	}
}

// ops returns every scope op the provider ran so far as "<op> s<index>".
func (fx *fanOutFixture) ops() []string {
	fx.t.Helper()
	data, err := os.ReadFile(fx.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		fx.t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		op, scope, _ := strings.Cut(line, " ")
		name := scope
		for i, root := range fx.scopes {
			if scope == root {
				name = fmt.Sprintf("s%d", i)
			}
		}
		out = append(out, op+" "+name)
	}
	return out
}

// health runs one healthBeadsProviderContext call and returns the ops it
// started.
func (fx *fanOutFixture) health() (ops []string, err error) {
	fx.t.Helper()
	before := len(fx.ops())
	err = healthBeadsProviderContext(context.Background(), fx.city, false)
	return fx.ops()[before:], err
}

// fanOutClock is the recover clock for a fixture: it advances `step` for every
// op the provider has logged, because the provider runs outside this process
// and cannot move a fake clock itself. Changing step later only affects ops
// logged after the change, so the clock never runs backwards.
type fanOutClock struct {
	fx   *fanOutFixture
	t0   time.Time
	step time.Duration
	acc  time.Duration
	seen int
}

func installFanOutClock(t *testing.T, fx *fanOutFixture, step time.Duration) *fanOutClock {
	t.Helper()
	c := &fanOutClock{fx: fx, t0: time.Unix(1_700_000_000, 0).UTC(), step: step}
	prevNow, prevCooldown := providerRecoverNow, providerRecoverCooldown
	providerRecoverNow = c.now
	providerRecoverCooldown = func() time.Duration { return 30 * time.Second }
	t.Cleanup(func() { providerRecoverNow, providerRecoverCooldown = prevNow, prevCooldown })
	return c
}

func (c *fanOutClock) now() time.Time {
	n := len(c.fx.ops())
	c.acc += time.Duration(n-c.seen) * c.step
	c.seen = n
	return c.t0.Add(c.acc)
}

func (c *fanOutClock) advance(d time.Duration) { c.acc += d }

func countOp(ops []string, want string) int {
	n := 0
	for _, op := range ops {
		if op == want {
			n++
		}
	}
	return n
}

func assertOps(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("scope ops = %v (%d), want %v (%d)", got, len(got), want, len(want))
	}
}

// notProbed returns the scope names listed after "not probed:" in err, as
// "s<index>".
func (fx *fanOutFixture) notProbed(err error) []string {
	fx.t.Helper()
	if err == nil {
		fx.t.Fatal("error = nil, want one naming the scopes that were not probed")
	}
	for _, line := range strings.Split(err.Error(), "\n") {
		_, list, found := strings.Cut(line, "not probed:")
		if !found {
			continue
		}
		// The caller may append text after the list on the same line.
		list, _, _ = strings.Cut(list, ")")
		var names []string
		for _, root := range strings.Split(list, ",") {
			root = strings.TrimSpace(root)
			for i, scope := range fx.scopes {
				if root == scope {
					names = append(names, fmt.Sprintf("s%d", i))
				}
			}
		}
		return names
	}
	fx.t.Fatalf("error %q does not name the scopes that were not probed", err)
	return nil
}

// A recover that fails does not cost its siblings their confirming probes:
// every started recover, failed or not, is probed again.
func TestProviderOwnedFailedRecoverDoesNotSkipSiblingConfirmations(t *testing.T) {
	fx := newFanOutFixture(t, 1)
	fx.setUnhealthy(0, 1)
	if err := os.WriteFile(filepath.Join(fx.scopes[0], "recover-fails"), nil, 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	installFanOutClock(t, fx, time.Second)

	ops, err := fx.health()
	assertOps(t, ops, []string{"health s0", "health s1", "recover s0", "recover s1", "health s0", "health s1"})
	if want := fmt.Sprintf("provider-owned scope %q recover", fx.scopes[0]); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to report the failed recover (%s)", err, want)
	}
}

// One call must not start a probe-recover-confirm ladder for every unhealthy
// scope. Ten unhealthy scopes at 100 s per op, against the per-call budget:
// two probes start before the budget runs out, then the guaranteed recover of
// the first unhealthy scope and its confirming probe. Today the same call
// starts 30 ops (10 probes, 10 recovers, 10 confirms).
func TestProviderOwnedFanOutIsBoundedPerCall(t *testing.T) {
	fx := newFanOutFixture(t, 9)
	fx.setUnhealthy(0, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	installFanOutClock(t, fx, 100*time.Second)

	ops, err := fx.health()
	if err == nil {
		t.Fatal("healthBeadsProviderContext() = nil, want the unhealthy scopes reported")
	}
	assertOps(t, ops, []string{"health s0", "health s1", "recover s0", "health s0"})
	if got, want := strings.Join(fx.notProbed(err), ","), "s2,s3,s4,s5,s6,s7,s8,s9"; got != want {
		t.Fatalf("not probed = %s, want %s", got, want)
	}
	// The second probe started inside the budget and ran to completion across
	// the budget boundary: its verdict is kept, not discarded.
	if want := fmt.Sprintf("provider-owned scope %q health", fx.scopes[1]); !strings.Contains(err.Error(), want) {
		t.Fatalf("error does not report the probe that outlived the budget (%s):\n%v", want, err)
	}
}

// A scope recovered a moment ago is not recovered again, but a different
// unhealthy scope still is, and the scope recovers again once the cooldown has
// passed.
func TestProviderOwnedRecoverCooldownIsPerScope(t *testing.T) {
	fx := newFanOutFixture(t, 2)
	clock := installFanOutClock(t, fx, time.Second)

	fx.setUnhealthy(0)
	ops, _ := fx.health()
	if got := countOp(ops, "recover s0"); got != 1 {
		t.Fatalf("first call recovered s0 %d times, want 1 (ops %v)", got, ops)
	}

	fx.setUnhealthy(0, 2)
	ops, err := fx.health()
	if got := countOp(ops, "recover s0"); got != 0 {
		t.Fatalf("second call inside the cooldown recovered s0 %d times, want 0 (ops %v)", got, ops)
	}
	if got := countOp(ops, "recover s2"); got != 1 {
		t.Fatalf("second call recovered the different unhealthy scope s2 %d times, want 1 (ops %v)", got, ops)
	}
	if err == nil || !strings.Contains(err.Error(), "recovery skipped (cooldown)") {
		t.Fatalf("error = %v, want it to name the scope whose recovery was skipped (cooldown)", err)
	}

	clock.advance(time.Minute)
	ops, _ = fx.health()
	if got := countOp(ops, "recover s0"); got != 1 {
		t.Fatalf("call after the cooldown recovered s0 %d times, want 1 (ops %v)", got, ops)
	}
}

// The scopes a call did not reach lead the next call, and a sweep that reaches
// every scope clears the position so the following call starts at the city
// again.
func TestProviderOwnedFanOutResumesAtTheFirstUnvisitedScope(t *testing.T) {
	fx := newFanOutFixture(t, 4)
	fx.setUnhealthy(0, 1, 2, 3, 4)
	clock := installFanOutClock(t, fx, 100*time.Second)

	ops, _ := fx.health()
	if len(ops) == 0 || ops[0] != "health s0" || countOp(ops, "health s2") != 0 {
		t.Fatalf("first call ops = %v, want it to start at s0 and stop before s2", ops)
	}

	ops, _ = fx.health()
	if len(ops) == 0 || ops[0] != "health s2" {
		t.Fatalf("second call ops = %v, want it to start at the first scope the first call did not reach (s2)", ops)
	}

	// Fast ops: this call can finish a sweep. It starts where the second left
	// off (s4) and wraps around to every scope.
	clock.step = time.Second
	ops, _ = fx.health()
	if len(ops) == 0 || ops[0] != "health s4" {
		t.Fatalf("third call ops = %v, want it to start at s4", ops)
	}
	for _, name := range []string{"s0", "s1", "s2", "s3", "s4"} {
		if countOp(ops, "health "+name) == 0 {
			t.Fatalf("third call never probed %s, so its sweep did not complete: %v", name, ops)
		}
	}

	ops, _ = fx.health()
	if len(ops) == 0 || ops[0] != "health s0" {
		t.Fatalf("call after a completed sweep ops = %v, want it to start at s0 again", ops)
	}
}

// The budget gates the start of a scope op, never an op that has started: a
// recover that began always gets its confirming probe, even when the budget ran
// out long before.
func TestProviderOwnedRecoverStillGetsItsConfirmingProbeAfterTheBudget(t *testing.T) {
	fx := newFanOutFixture(t, 1)
	fx.setUnhealthy(0, 1)
	installFanOutClock(t, fx, 200*time.Second)

	ops, err := fx.health()
	assertOps(t, ops, []string{"health s0", "recover s0", "health s0"})
	if got, want := strings.Join(fx.notProbed(err), ","), "s1"; got != want {
		t.Fatalf("not probed = %s, want %s", got, want)
	}
}

// The bound must not change what an ordinary call does: a single scope still
// runs probe, recover and confirm, and a healthy city runs one probe per scope
// and nothing else.
func TestProviderOwnedFanOutLeavesOrdinaryCallsAlone(t *testing.T) {
	t.Run("one unhealthy scope runs the whole ladder", func(t *testing.T) {
		fx := newFanOutFixture(t, 0)
		fx.setUnhealthy(0)
		installFanOutClock(t, fx, 100*time.Second)

		ops, err := fx.health()
		if err == nil {
			t.Fatal("healthBeadsProviderContext() = nil, want the scope that stayed unhealthy reported")
		}
		assertOps(t, ops, []string{"health s0", "recover s0", "health s0"})
	})
	t.Run("a healthy city runs one probe per scope", func(t *testing.T) {
		fx := newFanOutFixture(t, 3)
		installFanOutClock(t, fx, time.Second)

		ops, err := fx.health()
		if err != nil {
			t.Fatalf("healthBeadsProviderContext() = %v, want nil", err)
		}
		assertOps(t, ops, []string{"health s0", "health s1", "health s2", "health s3"})
	})
}
