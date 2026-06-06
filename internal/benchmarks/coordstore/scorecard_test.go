package coordstore

import (
	"strings"
	"testing"
	"time"
)

func TestScoreMemoryPassUsesHeapInuseDelta(t *testing.T) {
	mem := MemReport{
		HeapInuseBaseline: 300 * 1024 * 1024,
		HeapInusePeak:     600 * 1024 * 1024,
		HeapInuseSteady:   320 * 1024 * 1024,
		HeapInuseDelta:    200 * 1024 * 1024,
		Sampled:           true,
	}

	sc := Score("backend", "workload", time.Second, 1, 0, nil, nil, mem)

	if !sc.MemPass {
		t.Fatalf("MemPass = false, want true for delta below target")
	}
	if !sc.Passed() {
		t.Fatalf("Passed = false, want true for delta below target")
	}

	var out strings.Builder
	sc.PrintTable(&out)
	got := out.String()
	for _, want := range []string{
		"HeapInuse delta",
		"HeapInuse peak (informational)",
		"600.0MiB peak  \u2190 exceeds 256.0MiB",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("PrintTable output missing %q:\n%s", want, got)
		}
	}
}

func TestScoreMemoryFailsWhenHeapInuseDeltaExceedsTarget(t *testing.T) {
	mem := MemReport{
		HeapInuseBaseline: 10 * 1024 * 1024,
		HeapInusePeak:     300 * 1024 * 1024,
		HeapInuseSteady:   280 * 1024 * 1024,
		HeapInuseDelta:    320 * 1024 * 1024,
		Sampled:           true,
	}

	sc := Score("backend", "workload", time.Second, 1, 0, nil, nil, mem)

	if sc.MemPass {
		t.Fatalf("MemPass = true, want false for delta above target")
	}
	if sc.Passed() {
		t.Fatalf("Passed = true, want false for delta above target")
	}

	var out strings.Builder
	sc.PrintTable(&out)
	got := out.String()
	if !strings.Contains(got, "FAIL") || !strings.Contains(got, "delta 320.0MiB > target 256.0MiB") {
		t.Fatalf("PrintTable output missing delta failure:\n%s", got)
	}
}

func TestScoreMemoryUnsampledDoesNotAddMemoryTarget(t *testing.T) {
	sc := Score("backend", "workload", time.Second, 1, 0, nil, nil, MemReport{})

	if !sc.MemPass {
		t.Fatalf("MemPass = false, want true when memory is unsampled")
	}
	if sc.TotalTargets() != 0 {
		t.Fatalf("TotalTargets = %d, want 0 for no measured operation or memory targets", sc.TotalTargets())
	}

	var out strings.Builder
	sc.PrintTable(&out)
	if strings.Contains(out.String(), "Memory:") {
		t.Fatalf("PrintTable output includes memory section for unsampled memory:\n%s", out.String())
	}
}
