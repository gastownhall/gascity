package clock

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// withTestHooks sets the link-time stamp for one test and captures the
// one-time notice.
func withTestHooks(t *testing.T, stamp string) *bytes.Buffer {
	t.Helper()
	prevHooks, prevTo := testHooks, announceTo
	var out bytes.Buffer
	testHooks, announceTo, announceOnce = stamp, &out, sync.Once{}
	t.Cleanup(func() { testHooks, announceTo, announceOnce = prevHooks, prevTo, sync.Once{} })
	return &out
}

func TestBackstopDividesOnlyInATestHooksBinary(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"1", 30 * time.Second},
		{"6", 5 * time.Second},
		{" 6 ", 5 * time.Second},
		{"30", time.Second},
		{"31", time.Second},   // clamped to MaxBackstopSpeedup
		{"1000", time.Second}, // clamped
		{"0", 30 * time.Second},
		{"-6", 30 * time.Second},
		{"1.5", 30 * time.Second},
		{"fast", 30 * time.Second},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			withTestHooks(t, testHooksEnabled)
			t.Setenv(BackstopSpeedupEnv, tc.raw)
			if got := Backstop(30 * time.Second); got != tc.want {
				t.Errorf("%s=%q: Backstop(30s) = %s, want %s", BackstopSpeedupEnv, tc.raw, got, tc.want)
			}
		})
	}
}

// A binary without the link-time stamp (a release build, go build, the
// ordinary //cmd/gc:gc) ignores the variable, whatever its value, and says
// so on stderr.
func TestBackstopIgnoresTheVariableWithoutTestHooks(t *testing.T) {
	for _, stamp := range []string{"", "yes", "1", "Enabled"} {
		out := withTestHooks(t, stamp)
		t.Setenv(BackstopSpeedupEnv, "6")
		if got := Backstop(30 * time.Second); got != 30*time.Second {
			t.Errorf("stamp %q: Backstop(30s) = %s, want it unchanged", stamp, got)
		}
		if !strings.Contains(out.String(), "ignored: this gc binary is not built with test hooks") {
			t.Errorf("stamp %q: notice = %q, want the ignored warning", stamp, out.String())
		}
	}
}

func TestBackstopAnnouncesAnActiveHookOnce(t *testing.T) {
	out := withTestHooks(t, testHooksEnabled)
	t.Setenv(BackstopSpeedupEnv, "6")
	Backstop(time.Minute)
	Backstop(time.Minute)
	if n := strings.Count(out.String(), "TEST HOOK ACTIVE"); n != 1 {
		t.Errorf("notice printed %d times, want once:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "gc: WARNING: "+BackstopSpeedupEnv) {
		t.Errorf("notice = %q, want a WARNING naming %s", out.String(), BackstopSpeedupEnv)
	}
}

func TestBackstopIsSilentAndUnchangedWhenUnset(t *testing.T) {
	out := withTestHooks(t, testHooksEnabled)
	t.Setenv(BackstopSpeedupEnv, "")
	if got := Backstop(30 * time.Second); got != 30*time.Second {
		t.Errorf("Backstop(30s) = %s with the variable empty", got)
	}
	if out.Len() != 0 {
		t.Errorf("an unset variable printed %q", out.String())
	}
}

func TestBackstopLeavesANonPositiveDurationAlone(t *testing.T) {
	withTestHooks(t, testHooksEnabled)
	t.Setenv(BackstopSpeedupEnv, "6")
	for _, d := range []time.Duration{0, -time.Second} {
		if got := Backstop(d); got != d {
			t.Errorf("Backstop(%s) = %s, want it unchanged", d, got)
		}
	}
}

func TestAnnounceBackstopSpeedupWritesTheNoticeOnceToTheGivenWriter(t *testing.T) {
	withTestHooks(t, testHooksEnabled)
	t.Setenv(BackstopSpeedupEnv, "6")
	var first, second bytes.Buffer
	AnnounceBackstopSpeedup(&first)
	AnnounceBackstopSpeedup(&second)
	Backstop(time.Minute)
	if !strings.Contains(first.String(), BackstopActiveNotice) {
		t.Errorf("first notice = %q, want %q", first.String(), BackstopActiveNotice)
	}
	if second.Len() != 0 {
		t.Errorf("second notice = %q, want nothing (once per process)", second.String())
	}
}
