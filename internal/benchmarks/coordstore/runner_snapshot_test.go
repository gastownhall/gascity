package coordstore

import (
	"fmt"
	"testing"
	"time"
)

func TestRunnerHistogramSnapshotDeepCopiesResults(t *testing.T) {
	r := NewRunner(nil, WorkloadConfig{}, SeedResult{})
	r.record(opPointRead, 10*time.Millisecond, nil)
	r.record(opPointRead, 20*time.Millisecond, fmt.Errorf("boom"))

	snapshot := r.HistogramSnapshot()
	got := snapshot["Get"]
	if got == nil {
		t.Fatalf("snapshot missing Get result: %#v", snapshot)
	}
	if got.Samples != 2 || got.Errors != 1 || got.H.Count() != 2 {
		t.Fatalf("snapshot result = %#v", got)
	}
	if got == r.results["Get"] || got.H == r.results["Get"].H {
		t.Fatalf("HistogramSnapshot must deep-copy operation results and histograms")
	}

	r.record(opPointRead, 30*time.Millisecond, nil)
	if got.Samples != 2 || got.H.Count() != 2 {
		t.Fatalf("snapshot changed after runner mutation: samples=%d histogram=%d", got.Samples, got.H.Count())
	}
	got.Samples = 99
	got.H.Add(time.Second)
	if r.results["Get"].Samples != 3 || r.results["Get"].H.Count() != 3 {
		t.Fatalf("mutating snapshot changed runner result: %#v", r.results["Get"])
	}
}
