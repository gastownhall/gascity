package latency

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func sampleReport() Report {
	return Report{
		ClaimWait: []ClaimWaitGroup{
			{Pool: "worker-pool", Stats: computeDurationStats([]int64{1000, 2000, 3000})},
		},
		GateQueueWait: []GateQueueWaitGroup{
			{Formula: "mol-verify", StepID: "check", Stats: computeDurationStats([]int64{500, 1500})},
		},
		GateBounce: []GateBounceGroup{
			{Formula: "mol-verify", Definitions: 3, Bounces: 2, BounceRate: 2.0 / 3.0},
		},
	}
}

func TestFormatTable_HappyPath(t *testing.T) {
	var buf bytes.Buffer
	if err := FormatTable(&buf, sampleReport()); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Claim wait per pool", "worker-pool",
		"Gate queue wait", "mol-verify", "check",
		"Gate bounce rate", "66.7%",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q\n%s", want, out)
		}
	}
}

func TestFormatTable_EmptySectionsPrintNoData(t *testing.T) {
	var buf bytes.Buffer
	if err := FormatTable(&buf, Report{}); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	out := buf.String()
	if strings.Count(out, "(no data)") != 3 {
		t.Errorf("expected 3 empty-section markers, got:\n%s", out)
	}
}

func TestFormatTable_SkippedCountReported(t *testing.T) {
	var buf bytes.Buffer
	r := sampleReport()
	r.Skipped = 4
	if err := FormatTable(&buf, r); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	if !strings.Contains(buf.String(), "4 bead event(s) skipped") {
		t.Errorf("expected skipped count in output, got:\n%s", buf.String())
	}
}

func TestFormatJSON_RoundTrips(t *testing.T) {
	var buf bytes.Buffer
	r := sampleReport()
	if err := FormatJSON(&buf, r); err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}
	var parsed Report
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(parsed.ClaimWait) != 1 || parsed.ClaimWait[0].Pool != "worker-pool" {
		t.Errorf("claim wait round-trip failed: %+v", parsed.ClaimWait)
	}
	if len(parsed.GateQueueWait) != 1 || parsed.GateQueueWait[0].StepID != "check" {
		t.Errorf("gate queue wait round-trip failed: %+v", parsed.GateQueueWait)
	}
	if len(parsed.GateBounce) != 1 || parsed.GateBounce[0].Bounces != 2 {
		t.Errorf("gate bounce round-trip failed: %+v", parsed.GateBounce)
	}
}
