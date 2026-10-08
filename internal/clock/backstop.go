package clock

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BackstopSpeedupEnv is a test-only knob: a whole number N >= 2 divides the
// cadence of every periodic backstop a city controller runs on its own clock
// (Backstop's callers: the patrol tick, cooldown orders, the bead caches'
// reconcile, the order-tracking watchdog, the autoclose sweep, the order
// rescan, and the backstop lanes' polls) by N, clamped to
// MaxBackstopSpeedup.
//
// It exists for acceptance tests that assert what a running city does NOT
// do over a window long enough for every backstop to come round several
// times (test/acceptance's suspension quiescence rows): shrinking the window
// and every cadence by the same factor keeps the assertion's strength (each
// backstop comes round as many times inside the window) while the test runs
// in a fraction of the time.
//
// Only a gc binary stamped with test hooks honors it (testHooks below): the
// testonly //cmd/gc:gc_testhooks link. A release build (goreleaser), `go
// build`, `go install` and //cmd/gc:gc never carry the stamp, so in them
// the variable changes nothing, and gc says so on stderr.
const BackstopSpeedupEnv = "GC_TEST_BACKSTOP_SPEEDUP"

// MaxBackstopSpeedup clamps BackstopSpeedupEnv, so a 30 s backstop never
// runs more often than once a second.
const MaxBackstopSpeedup = 30

// testHooks is "enabled" only in a binary linked with
// -X github.com/gastownhall/gascity/internal/clock.testHooks=enabled
// (cmd/gc/BUILD.bazel gc_testhooks; scripts/cmd_gc_testhooks_test.go keeps
// every other link, and the release config, free of it). It is a link-time
// stamp rather than an environment check so that nothing a running process
// can be given turns the hook on in a release binary.
var testHooks string

const testHooksEnabled = "enabled"

// TestHooksEnabled reports whether this binary was linked with test hooks.
func TestHooksEnabled() bool { return testHooks == testHooksEnabled }

var announceOnce sync.Once

// announceTo is where the one-time notice goes; a var so tests can capture it.
var announceTo io.Writer = os.Stderr

// BackstopSpeedup returns the factor in force: BackstopSpeedupEnv clamped to
// [1, MaxBackstopSpeedup] in a test-hooks binary, 1 otherwise. The first
// call in a process that sees the variable set prints one loud notice on
// stderr saying whether it took effect; the controller's first cadence read
// is at startup, so the notice heads its log.
func BackstopSpeedup() int {
	raw, set := os.LookupEnv(BackstopSpeedupEnv)
	if !set || strings.TrimSpace(raw) == "" {
		return 1
	}
	n, note := resolveBackstopSpeedup(raw, TestHooksEnabled())
	announce(announceTo, raw, note)
	return n
}

// AnnounceBackstopSpeedup prints BackstopSpeedup's one-time notice to w now,
// if the variable is set: gc's process entry calls it, so every gc process
// run with the test hook (or with the variable but no hook) says so first.
func AnnounceBackstopSpeedup(w io.Writer) {
	raw, set := os.LookupEnv(BackstopSpeedupEnv)
	if !set || strings.TrimSpace(raw) == "" {
		return
	}
	_, note := resolveBackstopSpeedup(raw, TestHooksEnabled())
	announce(w, raw, note)
}

func announce(w io.Writer, raw, note string) {
	announceOnce.Do(func() {
		fmt.Fprintf(w, "gc: WARNING: %s=%q: %s\n", BackstopSpeedupEnv, raw, note) //nolint:errcheck // best-effort notice
	})
}

// resolveBackstopSpeedup is BackstopSpeedup's decision for a set variable:
// the factor and the notice that explains it.
func resolveBackstopSpeedup(raw string, hooks bool) (int, string) {
	if !hooks {
		return 1, "ignored: this gc binary is not built with test hooks (test-only knob)"
	}
	n, clamped, ok := ParseBackstopSpeedup(raw)
	switch {
	case !ok:
		return 1, "ignored: not a whole number >= 1"
	case clamped:
		return n, fmt.Sprintf(BackstopActiveNotice+": clamped to %d; every controller backstop cadence is divided by %d", n, n)
	case n == 1:
		return 1, "test hook present but 1: cadences unchanged"
	default:
		return n, fmt.Sprintf(BackstopActiveNotice+": every controller backstop cadence is divided by %d", n)
	}
}

// BackstopActiveNotice is the text a test-hooks gc prints on stderr when
// BackstopSpeedupEnv took effect. A test that shortens its own window by the
// same factor checks for it, so a gc that ignored the variable fails the
// test instead of weakening it.
const BackstopActiveNotice = "TEST HOOK ACTIVE"

// ParseBackstopSpeedup parses a BackstopSpeedupEnv value the way a
// test-hooks gc does: a whole number, clamped to MaxBackstopSpeedup
// (clamped reports that). ok is false for anything else, which gc treats as 1.
// Tests use it to shrink their own windows by the factor gc applies.
func ParseBackstopSpeedup(raw string) (n int, clamped, ok bool) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 {
		return 1, false, false
	}
	if n > MaxBackstopSpeedup {
		return MaxBackstopSpeedup, true, true
	}
	return n, false, true
}

// Backstop returns d divided by BackstopSpeedup: d itself unless a
// test-hooks binary runs with BackstopSpeedupEnv set.
func Backstop(d time.Duration) time.Duration {
	n := BackstopSpeedup()
	if n <= 1 || d <= 0 {
		return d
	}
	return d / time.Duration(n)
}
