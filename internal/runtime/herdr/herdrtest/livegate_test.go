package herdrtest

import "testing"

func TestSkipReason(t *testing.T) {
	for _, tc := range []struct {
		name      string
		short     bool
		installed bool
		fastUnit  string
		liveTests string
		wantRun   bool
	}{
		{name: "short mode skips even when opted in", short: true, installed: true, fastUnit: "0", liveTests: "1"},
		{name: "missing binary skips even when opted in", installed: false, fastUnit: "0", liveTests: "1"},
		{name: "unit lane skips", installed: true, fastUnit: "1"},
		{name: "unset skips", installed: true},
		{name: "GC_FAST_UNIT=0 runs", installed: true, fastUnit: "0", wantRun: true},
		{name: "GC_HERDR_LIVE_TESTS=1 runs", installed: true, fastUnit: "1", liveTests: "1", wantRun: true},
		{name: "opt-in is whitespace tolerant", installed: true, fastUnit: "1", liveTests: " 1 ", wantRun: true},
		{name: "GC_HERDR_LIVE_TESTS=0 does not opt in", installed: true, fastUnit: "1", liveTests: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := SkipReason(tc.short, tc.installed, tc.fastUnit, tc.liveTests)
			if gotRun := reason == ""; gotRun != tc.wantRun {
				t.Fatalf("short=%v installed=%v GC_FAST_UNIT=%q GC_HERDR_LIVE_TESTS=%q: run=%v want run=%v (reason %q)",
					tc.short, tc.installed, tc.fastUnit, tc.liveTests, gotRun, tc.wantRun, reason)
			}
		})
	}
}
