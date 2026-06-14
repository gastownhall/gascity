package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/doctor"
)

// forkRateCheck reports the host's process-creation (fork) rate — the dominant,
// and routinely misdiagnosed, driver of "high load" on Gas City hosts.
//
// Gas City's CLI-agent + per-command data plane means most gc/bd operations
// fork: `gc` forks `bd.real` per command, which in turn talks to a per-city
// `dolt` sql-server. A busy city therefore spends its load on *process churn*,
// not CPU work. Operators repeatedly misread the resulting high load average as
// CPU saturation — it is not: the load average counts runnable + uninterruptible
// tasks, which a fork storm inflates while CPU may be far from saturated
// (measured in the field at load ~25 with CPU ~66% busy and ~96% of forks coming
// from bd.real + dolt + gc, vs ~0.4% from the agents themselves). This check
// surfaces the actual fork rate so the misdiagnosis is caught early and the
// operator can reach for the right remedy.
//
// Pure observability (SeverityAdvisory): it reads /proc/stat only, never mutates
// anything, and never gates. On non-Linux hosts (no /proc/stat) it reports OK
// and skips. The durable remedies it points at are the embedded DoltLite backend
// (no per-city dolt sql-server) and the in-process bead store (no gc->bd.real
// fork per command); this check is the watch that tells an operator whether
// those are worth adopting.
type forkRateCheck struct {
	// sampleInterval is the window over which the fork delta is measured.
	sampleInterval time.Duration
	// warnPerSec is the forks/sec at or above which the check warns.
	warnPerSec float64
	// readProcStat returns the contents of /proc/stat. Injectable for tests.
	readProcStat func() (string, error)
	// sleep waits the sample interval. Injectable for tests.
	sleep func(time.Duration)
}

const (
	defaultForkRateSampleInterval = time.Second
	// defaultForkRateWarnPerSec is a heuristic starting threshold. Sustained
	// process creation above this on a steady-state city is almost always the
	// per-command fork cascade (gc -> bd.real -> dolt), not real work. It is a
	// field-tuned default, not a hard limit; expose it via config if a city's
	// healthy baseline legitimately runs hotter.
	defaultForkRateWarnPerSec = 100.0
)

func newForkRateCheck() *forkRateCheck {
	return &forkRateCheck{
		sampleInterval: defaultForkRateSampleInterval,
		warnPerSec:     defaultForkRateWarnPerSec,
		readProcStat: func() (string, error) {
			b, err := os.ReadFile("/proc/stat")
			return string(b), err
		},
		sleep: time.Sleep,
	}
}

func (c *forkRateCheck) Name() string                     { return "fork-rate" }
func (c *forkRateCheck) CanFix() bool                     { return false }
func (c *forkRateCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (c *forkRateCheck) WarmupEligible() bool             { return false }

// parseProcessesCounter extracts the cumulative "processes" (fork) counter from
// /proc/stat contents. The kernel increments it on every fork/clone of a new
// task. Returns ok=false when the field is absent (e.g. a non-Linux host).
func parseProcessesCounter(stat string) (int64, bool) {
	for _, line := range strings.Split(stat, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "processes" {
			n, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, false
			}
			return n, true
		}
	}
	return 0, false
}

func (c *forkRateCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	res := &doctor.CheckResult{Name: c.Name(), Severity: doctor.SeverityAdvisory}

	n1, ok := c.sampleProcessesCounter()
	if !ok {
		res.Status = doctor.StatusOK
		res.Message = "fork-rate: /proc/stat 'processes' counter unavailable (non-Linux host?) — skipped"
		return res
	}
	c.sleep(c.sampleInterval)
	n2, ok := c.sampleProcessesCounter()
	if !ok {
		res.Status = doctor.StatusOK
		res.Message = "fork-rate: /proc/stat second read failed — skipped"
		return res
	}

	secs := c.sampleInterval.Seconds()
	if secs <= 0 {
		secs = 1
	}
	perSec := float64(n2-n1) / secs
	if perSec < 0 {
		// Counter wrapped or host rebooted mid-sample; treat as unknown.
		res.Status = doctor.StatusOK
		res.Message = "fork-rate: counter went backwards (reboot mid-sample?) — skipped"
		return res
	}

	if perSec >= c.warnPerSec {
		res.Status = doctor.StatusWarning
		res.Message = fmt.Sprintf("high process fork rate: %.0f forks/s (warn >= %.0f/s) — likely the per-command data-plane fork storm, not CPU load", perSec, c.warnPerSec)
		res.Details = []string{
			"A high fork rate — not CPU work — is what inflates the load average on Gas City hosts:",
			"the load average counts runnable + uninterruptible tasks, so a fork storm reads as 'high",
			"load' while CPU may be far from saturated. Don't infer CPU saturation from load alone.",
			"Usual driver: the per-command data plane — gc forks bd.real per command, which talks to a",
			"per-city dolt sql-server (gc + bd.real + dolt typically dominate; the agents are a rounding error).",
			"Confirm the sources (needs root): bpftrace -e 'tracepoint:sched:sched_process_fork { @[comm] = count(); }'",
			"Durable remedies: the embedded DoltLite backend (no per-city dolt sql-server) and the",
			"in-process bead store (no gc->bd.real fork per command).",
		}
		return res
	}

	res.Status = doctor.StatusOK
	res.Message = fmt.Sprintf("process fork rate: %.0f forks/s (sampled over %s)", perSec, c.sampleInterval)
	return res
}

// sampleProcessesCounter reads /proc/stat once and returns the cumulative fork
// counter, or ok=false if it cannot be read/parsed.
func (c *forkRateCheck) sampleProcessesCounter() (int64, bool) {
	stat, err := c.readProcStat()
	if err != nil {
		return 0, false
	}
	return parseProcessesCounter(stat)
}
