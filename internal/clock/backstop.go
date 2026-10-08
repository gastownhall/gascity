package clock

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// BackstopSpeedupEnv is a test-only knob: a whole number N from 2 to
// MaxBackstopSpeedup divides the cadence of every periodic backstop a city
// controller runs on its own clock (Backstop's callers: the patrol tick,
// cooldown orders, the bead caches' reconcile, the order-tracking watchdog,
// the autoclose sweep, the order rescan, and the backstop lanes' polls) by N.
// Unset, empty, malformed or out of range, it changes nothing.
//
// It exists for acceptance tests that assert what a running city does NOT
// do over a window long enough for every backstop to come round several
// times (test/acceptance's suspension quiescence rows): shrinking the window
// and every cadence by the same factor keeps the assertion's strength (each
// backstop comes round as many times inside the window) while the test runs
// in a fraction of the time. Production never sets it.
const BackstopSpeedupEnv = "GC_TEST_BACKSTOP_SPEEDUP"

// MaxBackstopSpeedup bounds BackstopSpeedupEnv, so a 30 s backstop never runs
// more often than once a second.
const MaxBackstopSpeedup = 30

// BackstopSpeedup returns the factor BackstopSpeedupEnv asks for, or 1.
func BackstopSpeedup() int {
	raw := strings.TrimSpace(os.Getenv(BackstopSpeedupEnv))
	if raw == "" {
		return 1
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > MaxBackstopSpeedup {
		return 1
	}
	return n
}

// Backstop returns d divided by BackstopSpeedup: d itself unless a test set
// BackstopSpeedupEnv.
func Backstop(d time.Duration) time.Duration {
	n := BackstopSpeedup()
	if n <= 1 || d <= 0 {
		return d
	}
	return d / time.Duration(n)
}
