package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// cpuPSI is a /proc/pressure/cpu body whose "some avg10" is avg10.
func cpuPSI(avg10 string) []byte {
	return []byte("some avg10=" + avg10 + " avg60=1.00 avg300=1.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
}

// fakeCPUPressure serves body (or err) as the CPU PSI file for the test, on a
// platform that exposes PSI: the gate reads the fake as it reads
// /proc/pressure/cpu on Linux, whatever the platform running the test.
func fakeCPUPressure(t *testing.T, body []byte, err error) {
	t.Helper()
	savedRead, savedSupported := hostPressureReadFile, hostPressureSupported
	t.Cleanup(func() { hostPressureReadFile, hostPressureSupported = savedRead, savedSupported })
	hostPressureReadFile = func(string) ([]byte, error) { return body, err }
	hostPressureSupported = true
}

// highHostPressure is a reading above the default threshold.
var highHostPressure = hostPressure{Avg10: 80, Threshold: defaultHostPressureThreshold, High: true}

// AC-4: the PSI parser reads the requested window off the "some" line.
func TestReadPSIAvgReadsTheWindow(t *testing.T) {
	read := func(string) ([]byte, error) {
		return []byte("some avg10=12.50 avg60=3.25 avg300=1.00 total=9\nfull avg10=99.00 avg60=99.00 avg300=99.00 total=9\n"), nil
	}
	for window, want := range map[string]float64{"avg10": 12.5, "avg60": 3.25} {
		got, err := readPSIAvg(read, "/x", window)
		if err != nil || got != want {
			t.Fatalf("readPSIAvg(%s) = %v, %v; want %v", window, got, err, want)
		}
	}
}

// AC-4: CPU pressure is high when some avg10 is above the threshold, which
// defaults to 50 and is overridable.
func TestHostPressureAgainstTheThreshold(t *testing.T) {
	for _, tc := range []struct {
		avg10, env string
		high       bool
	}{
		{avg10: "50.01", high: true},
		{avg10: "50.00", high: false},
		{avg10: "80.00", env: "90", high: false},
		{avg10: "80.00", env: "20", high: true},
	} {
		t.Setenv(hostPressureThresholdEnv, tc.env)
		fakeCPUPressure(t, cpuPSI(tc.avg10), nil)
		if got := readHostPressure(nil); got.High != tc.high {
			t.Fatalf("avg10=%s threshold env %q: reading %+v, want high=%t", tc.avg10, tc.env, got, tc.high)
		}
	}
}

// AC-4: high CPU pressure holds starts with cause host-pressure; an
// unreadable suspension file wins over it.
func TestDecideStartHoldOnHostPressure(t *testing.T) {
	if h := decideStartHold(startHoldInput{HostPressure: highHostPressure}); h.Cause != causeHostPressure {
		t.Fatalf("high pressure: hold %+v, want cause %q", h, causeHostPressure)
	}
	if h := decideStartHold(startHoldInput{HostPressure: highHostPressure, SuspensionErr: errUnreadableSuspension}); h.Cause != causeSuspensionUnreadable {
		t.Fatalf("high pressure and unreadable file: hold %+v, want cause %q", h, causeSuspensionUnreadable)
	}
	if h := decideStartHold(startHoldInput{HostPressure: hostPressure{Avg10: 10, Threshold: 50}}); h.held() {
		t.Fatalf("low pressure: hold %+v, want none", h)
	}
}

// AC-4 (v2 admission): under host pressure the pass's stops and drains are
// admitted, its start and create deferred with cause host-pressure, and no
// wake token spent.
func TestAdmitHostPressureDefersOnlyStartsAndCreates(t *testing.T) {
	in := admitIn(5, 4)
	in.Hold = decideStartHold(startHoldInput{HostPressure: highHostPressure})
	res := admit(in, []intent{startOn(1, "", 0), poolCreate("w", ""), rowIntent(intentStop, 2), rowIntent(intentDrainBegin, 3)})
	causes := causesOf(res.Deferred)
	if len(res.Admitted) != 2 || causes["start:gc-1"] != causeHostPressure || causes["create:"] != causeHostPressure {
		t.Fatalf("admitted %v deferred %v: want the stop and drain through, the start and create held", keysOf(res.Admitted), causes)
	}
	if res.Bucket.Tokens != in.Bucket.Tokens || !res.NextBackoff.IsZero() {
		t.Fatalf("bucket %v -> %v, next backoff %v: a held start spent a token or backed off", in.Bucket.Tokens, res.Bucket.Tokens, res.NextBackoff)
	}
}

// AC-4 (v2 pass): the gather's CPU reading holds the pass's create.
func TestPassHoldsStartsUnderHostPressure(t *testing.T) {
	f := newGatherFixture(t, routedDemandBead("gc-r1"))
	f.env.HostPressure = func() hostPressure { return highHostPressure }
	rec := f.passRecord(t)
	if len(rec.Admitted) != 0 || len(rec.Deferred) == 0 {
		t.Fatalf("admitted %v deferred %+v, want the create held", intentKeys(rec.Admitted), rec.Deferred)
	}
	for _, it := range rec.Deferred {
		if it.Kind == intentCreate && it.Cause != causeHostPressure {
			t.Fatalf("deferred %+v, want cause %q", it, causeHostPressure)
		}
	}
}

// AC-4 (legacy): the host-pressure hold defers every start candidate before
// any start, logged as deferred_by_host_pressure.
func TestLegacyHostPressureHoldDefersEveryCandidate(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "b")
	a := e.candidate(t, "a", time.Second)
	b := e.candidate(t, "b", time.Second)
	hold := withStartHold(decideStartHold(startHoldInput{HostPressure: highHostPressure}))
	if woken := e.runWith([]startExecutionOption{hold}, false, a, b); woken != 0 {
		t.Fatalf("woken = %d, want 0 under the hold\n%s", woken, e.log.String())
	}
	if n := e.sp.CountCalls("Start", "a") + e.sp.CountCalls("Start", "b"); n != 0 {
		t.Fatalf("%d start(s) under the hold", n)
	}
	if got := strings.Count(e.log.String(), string(TraceOutcomeDeferredByHostPressure)); got != 2 {
		t.Fatalf("log names %s %d time(s), want 2\n%s", TraceOutcomeDeferredByHostPressure, got, e.log.String())
	}
}

// resetHostPressureWarnings clears the once-per-process warnings for a test.
func resetHostPressureWarnings(t *testing.T) {
	t.Helper()
	hostPressureReadErrorWarned.Store(false)
	hostPressureInvalidThresholdWarned.Store(false)
	t.Cleanup(func() {
		hostPressureReadErrorWarned.Store(false)
		hostPressureInvalidThresholdWarned.Store(false)
	})
}

// AC-6: a missing or unparseable CPU PSI file never holds a start and warns
// once per process, however many passes read it.
func TestHostPressureFailsOpenWithOneWarning(t *testing.T) {
	for name, tc := range map[string]struct {
		body []byte
		err  error
	}{
		"missing":     {err: os.ErrNotExist},
		"unparseable": {body: []byte("some avg10=garbage avg60=1.00\n")},
		"no some":     {body: []byte("full avg10=90.00 avg60=90.00\n")},
	} {
		t.Run(name, func(t *testing.T) {
			resetHostPressureWarnings(t)
			fakeCPUPressure(t, tc.body, tc.err)
			var stderr strings.Builder
			for i := 0; i < 3; i++ {
				p := readHostPressure(&stderr)
				if h := decideStartHold(startHoldInput{HostPressure: p}); h.held() {
					t.Fatalf("pass %d: hold %+v over an unreadable PSI file", i, h)
				}
			}
			if n := strings.Count(stderr.String(), "\n"); n != 1 || !strings.Contains(stderr.String(), hostPressurePath) {
				t.Fatalf("stderr has %d line(s), want one naming %s:\n%s", n, hostPressurePath, stderr.String())
			}
		})
	}
}

// AC-6: an invalid threshold warns once per process and uses the default; a
// threshold of 100 never holds.
func TestHostPressureThresholdEdges(t *testing.T) {
	for _, raw := range []string{"abc", "150", "-1", "NaN"} {
		t.Run(raw, func(t *testing.T) {
			resetHostPressureWarnings(t)
			t.Setenv(hostPressureThresholdEnv, raw)
			fakeCPUPressure(t, cpuPSI("60.00"), nil)
			var stderr strings.Builder
			for i := 0; i < 3; i++ {
				if p := readHostPressure(&stderr); p.Threshold != defaultHostPressureThreshold || !p.High {
					t.Fatalf("threshold %q: reading %+v, want the default %v holding", raw, p, defaultHostPressureThreshold)
				}
			}
			if n := strings.Count(stderr.String(), "\n"); n != 1 || !strings.Contains(stderr.String(), hostPressureThresholdEnv) {
				t.Fatalf("stderr has %d line(s), want one naming %s:\n%s", n, hostPressureThresholdEnv, stderr.String())
			}
		})
	}
	t.Run("100", func(t *testing.T) {
		resetHostPressureWarnings(t)
		t.Setenv(hostPressureThresholdEnv, "100")
		fakeCPUPressure(t, cpuPSI("100.00"), nil)
		var stderr strings.Builder
		if p := readHostPressure(&stderr); p.High || stderr.Len() != 0 {
			t.Fatalf("threshold 100: reading %+v stderr %q, want never high, no warning", p, stderr.String())
		}
	})
}

// AC-6: on a platform without PSI the CPU gate never holds and never warns.
func TestHostPressureOffLinuxNeverHolds(t *testing.T) {
	resetHostPressureWarnings(t)
	fakeCPUPressure(t, cpuPSI("99.00"), nil)
	hostPressureSupported = false // fakeCPUPressure's cleanup restores it
	var stderr strings.Builder
	if p := readHostPressure(&stderr); p.High || stderr.Len() != 0 {
		t.Fatalf("off Linux: reading %+v stderr %q, want never high, no warning", p, stderr.String())
	}
}
