package coordstore

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Target describes a single performance requirement from discovery.md.
type Target struct {
	// Op is the operation name (matches OperationResult.Op).
	Op string
	// Name is a human-readable description.
	Name string
	// P99 is the p99 latency requirement. Zero means no p99 target.
	P99 time.Duration
	// Max is an absolute maximum (used for single-invocation operations like
	// PrimeScan). Zero means no max target.
	Max time.Duration
	// MinThroughput is the minimum sustained throughput in ops/sec.
	// Zero means no throughput target.
	MinThroughput float64
}

// DiscoveryTargets are the performance requirements from discovery.md.
// Source: docs/coordination-store/discovery.md §Targets.
var DiscoveryTargets = []Target{
	{
		Op:   "Get",
		Name: "point read (FR-3)",
		P99:  1 * time.Millisecond,
	},
	{
		Op:   "FilterScan",
		Name: "filter scan main tier (FR-2)",
		P99:  10 * time.Millisecond,
	},
	{
		Op:   "EphemeralFilterScan",
		Name: "filter scan ephemeral tier / mail poll (FR-8)",
		P99:  10 * time.Millisecond,
	},
	{
		Op:   "BatchGet",
		Name: "batch-by-id-set fetch (FR-4)",
		P99:  5 * time.Millisecond,
	},
	{
		Op:   "Create",
		Name: "per-record create (FR-1)",
		P99:  5 * time.Millisecond,
	},
	{
		Op:   "Update",
		Name: "per-record update (FR-1)",
		P99:  5 * time.Millisecond,
	},
	{
		Op:   "SetMetadataBatch",
		Name: "intra-record multi-key atomic write (FR-5)",
		P99:  5 * time.Millisecond,
	},
	{
		Op:   "Ready",
		Name: "ready semantics scan (FR-9)",
		P99:  10 * time.Millisecond,
	},
	{
		Op:   "PrimeScan",
		Name: "background prime at 10k rows (FR-15)",
		Max:  5 * time.Second,
	},
	{
		Op:            "MailPoll",
		Name:          "mail-poll read throughput",
		MinThroughput: 150,
	},
}

// HeapInusePeakTarget is the memory ceiling from discovery.md: the store must
// hold its working set in a bounded heap. Source: docs/coordination-store/
// discovery.md §Targets (RAM ≤ 256MB HeapInuse peak).
const HeapInusePeakTarget = 256 * 1024 * 1024

// HeapInuseDeltaTarget is the maximum workload-induced heap growth allowed.
const HeapInuseDeltaTarget = 256 * 1024 * 1024

// MemReport captures memory consumption observed during a workload run.
// Baseline is measured after seeding and before workload execution; peak is
// sampled during the run; steady is measured after the workload stops.
// AllocDelta is the cumulative bytes allocated over the run
// (TotalAlloc end − start), a churn proxy.
type MemReport struct {
	// HeapInuseBaseline is runtime.MemStats.HeapInuse before the workload.
	HeapInuseBaseline uint64
	// HeapInusePeak is the maximum runtime.MemStats.HeapInuse seen, in bytes.
	HeapInusePeak uint64
	// HeapInuseSteady is runtime.MemStats.HeapInuse after the workload.
	HeapInuseSteady uint64
	// HeapInuseDelta is workload-induced heap growth: peak minus baseline.
	HeapInuseDelta uint64
	// RSSBaseline is the process resident set size before the workload, in
	// bytes. Zero if unavailable.
	RSSBaseline uint64
	// RSSPeak is the maximum process resident set size seen, in bytes,
	// sampled from /proc/self/status (VmRSS). Zero if unavailable.
	RSSPeak uint64
	// RSSSteady is the process resident set size after the workload, in bytes.
	// Zero if unavailable.
	RSSSteady uint64
	// AllocDelta is the total bytes allocated during the run
	// (runtime.MemStats.TotalAlloc end − start).
	AllocDelta uint64
	// Sampled is true if at least one sample was collected.
	Sampled bool
}

// ScorecardResult is the outcome of a single target check.
type ScorecardResult struct {
	Target Target
	// Actual values measured.
	ActualP99        time.Duration
	ActualMax        time.Duration
	ActualThroughput float64
	// Pass is true if the backend meets the target.
	Pass bool
	// Reason explains why the target was not met, or is empty on pass.
	Reason string
	// Measured is false if no samples were collected for this operation.
	Measured bool
}

// Scorecard aggregates the pass/fail results for a backend+workload run.
type Scorecard struct {
	Backend  string
	Workload string
	Results  []ScorecardResult
	// Duration is the wall-clock time the workload ran.
	Duration time.Duration
	// TotalOps is the total number of operations issued.
	TotalOps int
	// Errors is the total number of operation errors.
	Errors int
	// Mem holds memory consumption observed during the run.
	Mem MemReport
	// MemPass reports whether the HeapInuseDelta target was met. Only
	// meaningful when Mem.Sampled is true.
	MemPass bool
}

// Passed returns true if all measured targets passed, including the memory
// target when memory was sampled.
func (s *Scorecard) Passed() bool {
	for _, r := range s.Results {
		if r.Measured && !r.Pass {
			return false
		}
	}
	if s.Mem.Sampled && !s.MemPass {
		return false
	}
	return true
}

// PassCount returns the number of measured targets that passed, including the
// memory target when memory was sampled.
func (s *Scorecard) PassCount() int {
	n := 0
	for _, r := range s.Results {
		if r.Measured && r.Pass {
			n++
		}
	}
	if s.Mem.Sampled && s.MemPass {
		n++
	}
	return n
}

// TotalTargets returns the number of measured targets, including the memory
// target when memory was sampled.
func (s *Scorecard) TotalTargets() int {
	n := 0
	for _, r := range s.Results {
		if r.Measured {
			n++
		}
	}
	if s.Mem.Sampled {
		n++
	}
	return n
}

// Score evaluates operation results against the discovery.md targets.
// results maps operation name → OperationResult.
// throughput maps operation name → ops/sec.
// mem carries the memory consumption observed during the run; pass an empty
// (Sampled=false) MemReport to skip the memory target.
func Score(backend, workload string, dur time.Duration, totalOps, totalErrors int,
	results map[string]*OperationResult, throughput map[string]float64, mem MemReport,
) Scorecard {
	sc := Scorecard{
		Backend:  backend,
		Workload: workload,
		Duration: dur,
		TotalOps: totalOps,
		Errors:   totalErrors,
		Mem:      mem,
		MemPass:  !mem.Sampled || mem.HeapInuseDelta <= HeapInuseDeltaTarget,
	}

	for _, t := range DiscoveryTargets {
		r := ScorecardResult{Target: t}
		op, ok := results[t.Op]
		if !ok || op == nil || op.Samples == 0 {
			r.Measured = false
			sc.Results = append(sc.Results, r)
			continue
		}
		r.Measured = true
		r.ActualP99 = op.H.P99()
		r.ActualMax = op.H.Max()
		r.ActualThroughput = throughput[t.Op]

		var reasons []string
		if t.P99 > 0 && r.ActualP99 > t.P99 {
			reasons = append(reasons, fmt.Sprintf("p99 %s > target %s",
				FormatDuration(r.ActualP99), FormatDuration(t.P99)))
		}
		if t.Max > 0 && r.ActualMax > t.Max {
			reasons = append(reasons, fmt.Sprintf("max %s > target %s",
				FormatDuration(r.ActualMax), FormatDuration(t.Max)))
		}
		if t.MinThroughput > 0 && r.ActualThroughput < t.MinThroughput {
			reasons = append(reasons, fmt.Sprintf("throughput %.0f/s < target %.0f/s",
				r.ActualThroughput, t.MinThroughput))
		}
		r.Pass = len(reasons) == 0
		r.Reason = strings.Join(reasons, "; ")
		sc.Results = append(sc.Results, r)
	}

	return sc
}

// PrintTable writes the scorecard as a human-readable table to w.
func (s *Scorecard) PrintTable(w io.Writer) {
	status := "PASS"
	if !s.Passed() {
		status = "FAIL"
	}
	fmt.Fprintf(w, "\n=== Scorecard: %s / %s — %s ===\n", s.Backend, s.Workload, status) //nolint:errcheck
	fmt.Fprintf(w, "  duration=%s  ops=%d  errors=%d  targets=%d/%d passed\n\n",         //nolint:errcheck
		FormatDuration(s.Duration), s.TotalOps, s.Errors, s.PassCount(), s.TotalTargets())

	const colW = 38
	fmt.Fprintf(w, "  %-*s  %-12s  %-12s  %-12s  %s\n", //nolint:errcheck
		colW, "Target", "P99", "Max", "Throughput", "Result")
	fmt.Fprintf(w, "  %s\n", strings.Repeat("-", colW+50)) //nolint:errcheck

	for _, r := range s.Results {
		result := "skip"
		p99 := "-"
		maxVal := "-"
		tput := "-"

		if r.Measured {
			if r.Pass {
				result = "PASS"
			} else {
				result = "FAIL  ← " + r.Reason
			}
			if r.Target.P99 > 0 {
				p99 = FormatDuration(r.ActualP99)
			}
			if r.Target.Max > 0 {
				maxVal = FormatDuration(r.ActualMax)
			}
			if r.Target.MinThroughput > 0 {
				tput = fmt.Sprintf("%.0f/s", r.ActualThroughput)
			}
		}

		fmt.Fprintf(w, "  %-*s  %-12s  %-12s  %-12s  %s\n", //nolint:errcheck
			colW, r.Target.Name, p99, maxVal, tput, result)
	}

	if s.Mem.Sampled {
		fmt.Fprintf(w, "\n  Memory:\n")                     //nolint:errcheck
		fmt.Fprintf(w, "  %-*s  %-12s  %-12s  %-12s  %s\n", //nolint:errcheck
			colW, "Metric", "Baseline", "Peak/Delta", "Steady", "Result")

		deltaResult := "PASS"
		if !s.MemPass {
			deltaResult = fmt.Sprintf("FAIL  ← delta %s > target %s",
				FormatBytes(s.Mem.HeapInuseDelta), FormatBytes(HeapInuseDeltaTarget))
		}
		fmt.Fprintf(w, "  %-*s  %-12s  %-12s  %-12s  %s\n", //nolint:errcheck
			colW, fmt.Sprintf("HeapInuse delta (≤%s)", FormatBytes(HeapInuseDeltaTarget)),
			FormatBytes(s.Mem.HeapInuseBaseline),
			FormatBytes(s.Mem.HeapInuseDelta),
			"-",
			deltaResult)

		peakNote := fmt.Sprintf("%s peak", FormatBytes(s.Mem.HeapInusePeak))
		if s.Mem.HeapInusePeak > HeapInusePeakTarget {
			peakNote += "  ← exceeds " + FormatBytes(HeapInusePeakTarget)
		}
		fmt.Fprintf(w, "  %-*s  %-12s  %-12s  %-12s  %s\n", //nolint:errcheck
			colW, "HeapInuse peak (informational)",
			FormatBytes(s.Mem.HeapInuseBaseline),
			FormatBytes(s.Mem.HeapInusePeak),
			FormatBytes(s.Mem.HeapInuseSteady),
			peakNote)
		rssBaseline := "-"
		if s.Mem.RSSPeak > 0 {
			rssBaseline = FormatBytes(s.Mem.RSSBaseline)
		}
		rssPeak := "-"
		if s.Mem.RSSPeak > 0 {
			rssPeak = FormatBytes(s.Mem.RSSPeak)
		}
		rssSteady := "-"
		if s.Mem.RSSSteady > 0 {
			rssSteady = FormatBytes(s.Mem.RSSSteady)
		}
		fmt.Fprintf(w, "  %-*s  %-12s  %-12s  %-12s\n", //nolint:errcheck
			colW, "RSS", rssBaseline, rssPeak, rssSteady)
		fmt.Fprintf(w, "  %-*s  %-12s\n", colW, "alloc delta (churn)", FormatBytes(s.Mem.AllocDelta)) //nolint:errcheck
	}

	fmt.Fprintln(w) //nolint:errcheck
}

// FormatBytes formats a byte count as a short human-readable string.
func FormatBytes(b uint64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case b >= gib:
		return fmt.Sprintf("%.2fGiB", float64(b)/float64(gib))
	case b >= mib:
		return fmt.Sprintf("%.1fMiB", float64(b)/float64(mib))
	case b >= kib:
		return fmt.Sprintf("%.1fKiB", float64(b)/float64(kib))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
