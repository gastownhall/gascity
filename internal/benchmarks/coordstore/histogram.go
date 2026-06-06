package coordstore

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Histogram accumulates operation latency samples and computes percentiles.
// It is safe for concurrent use via the Add method only during collection;
// percentile queries must happen after collection is complete.
type Histogram struct {
	samples []int64 // nanoseconds, unsorted during collection
	sorted  bool
}

// Add records a single latency sample.
func (h *Histogram) Add(d time.Duration) {
	h.samples = append(h.samples, d.Nanoseconds())
	h.sorted = false
}

// Count returns the number of samples.
func (h *Histogram) Count() int { return len(h.samples) }

// sort ensures samples are sorted for percentile computation.
func (h *Histogram) ensureSorted() {
	if h.sorted || len(h.samples) == 0 {
		return
	}
	sort.Slice(h.samples, func(i, j int) bool { return h.samples[i] < h.samples[j] })
	h.sorted = true
}

// Percentile returns the value at the given percentile (0–100).
// Returns 0 if the histogram has no samples.
func (h *Histogram) Percentile(p float64) time.Duration {
	h.ensureSorted()
	if len(h.samples) == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100.0*float64(len(h.samples)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(h.samples) {
		idx = len(h.samples) - 1
	}
	return time.Duration(h.samples[idx])
}

// P50 returns the 50th percentile latency.
func (h *Histogram) P50() time.Duration { return h.Percentile(50) }

// P95 returns the 95th percentile latency.
func (h *Histogram) P95() time.Duration { return h.Percentile(95) }

// P99 returns the 99th percentile latency.
func (h *Histogram) P99() time.Duration { return h.Percentile(99) }

// P999 returns the 99.9th percentile latency.
func (h *Histogram) P999() time.Duration { return h.Percentile(99.9) }

// Max returns the maximum latency sample.
func (h *Histogram) Max() time.Duration {
	h.ensureSorted()
	if len(h.samples) == 0 {
		return 0
	}
	return time.Duration(h.samples[len(h.samples)-1])
}

// Mean returns the arithmetic mean of all samples.
func (h *Histogram) Mean() time.Duration {
	if len(h.samples) == 0 {
		return 0
	}
	var sum int64
	for _, s := range h.samples {
		sum += s
	}
	return time.Duration(sum / int64(len(h.samples)))
}

func (h *Histogram) clone() *Histogram {
	if h == nil {
		return nil
	}
	return &Histogram{
		samples: append([]int64(nil), h.samples...),
		sorted:  h.sorted,
	}
}

// OperationResult holds the latency histogram for a single operation type.
type OperationResult struct {
	Op      string
	Samples int
	H       *Histogram
	Errors  int
}

// FormatDuration formats a duration as a short human-readable string.
func FormatDuration(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.2fs", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	case d >= time.Microsecond:
		return fmt.Sprintf("%.0fµs", float64(d)/float64(time.Microsecond))
	default:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	}
}
