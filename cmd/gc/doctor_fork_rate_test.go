package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/doctor"
)

// forkRateCheckWith builds a forkRateCheck that returns the given /proc/stat
// snapshots in order (one per read) with an instant sleep, for deterministic
// testing without touching the real host.
func forkRateCheckWith(stats []string, warnPerSec float64) *forkRateCheck {
	i := 0
	return &forkRateCheck{
		sampleInterval: time.Second,
		warnPerSec:     warnPerSec,
		readProcStat: func() (string, error) {
			if i >= len(stats) {
				return "", errors.New("no more snapshots")
			}
			s := stats[i]
			i++
			return s, nil
		},
		sleep: func(time.Duration) {},
	}
}

func TestForkRateCheck_HighRateWarns(t *testing.T) {
	// 1000 -> 1600 over a 1s window = 600 forks/s, well above the 100/s warn.
	c := forkRateCheckWith([]string{"cpu 1 2 3\nprocesses 1000\n", "cpu 1 2 3\nprocesses 1600\n"}, 100)
	r := c.Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning (600/s >= 100)", r.Status)
	}
	if r.Severity != doctor.SeverityAdvisory {
		t.Fatalf("severity = %v, want SeverityAdvisory (observability, never gates)", r.Severity)
	}
	if !strings.Contains(r.Message, "600") {
		t.Fatalf("message should report the rate, got %q", r.Message)
	}
	if len(r.Details) == 0 {
		t.Fatalf("a warning should carry remediation Details (bpftrace + DoltLite/in-process)")
	}
}

func TestForkRateCheck_LowRateOK(t *testing.T) {
	// 1000 -> 1050 over 1s = 50 forks/s, below the warn threshold.
	c := forkRateCheckWith([]string{"processes 1000\n", "processes 1050\n"}, 100)
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK (50/s < 100)", r.Status)
	}
	if !strings.Contains(r.Message, "50") {
		t.Fatalf("message should report the rate, got %q", r.Message)
	}
}

func TestForkRateCheck_NonLinuxSkips(t *testing.T) {
	// /proc/stat without a "processes" line (non-Linux host): skip, never warn.
	c := forkRateCheckWith([]string{"cpu  1 2 3 4\n", "cpu  1 2 3 4\n"}, 100)
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK (skipped, not a false warning)", r.Status)
	}
	if !strings.Contains(strings.ToLower(r.Message), "skip") {
		t.Fatalf("message should indicate skipped, got %q", r.Message)
	}
}

func TestForkRateCheck_ReadErrorSkips(t *testing.T) {
	c := &forkRateCheck{
		sampleInterval: time.Second,
		warnPerSec:     100,
		readProcStat:   func() (string, error) { return "", errors.New("no /proc") },
		sleep:          func(time.Duration) {},
	}
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK (read error -> skip)", r.Status)
	}
}

func TestParseProcessesCounter(t *testing.T) {
	if n, ok := parseProcessesCounter("cpu 1 2\nprocesses 4242\nctxt 99\n"); !ok || n != 4242 {
		t.Fatalf("parseProcessesCounter = (%d,%v), want (4242,true)", n, ok)
	}
	if _, ok := parseProcessesCounter("cpu 1 2\nctxt 99\n"); ok {
		t.Fatalf("parseProcessesCounter should return ok=false when 'processes' is absent")
	}
}
