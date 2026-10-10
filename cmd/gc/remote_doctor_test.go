package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/doctor"
)

func TestDoctorRemoteReadSubset(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	srv := fakeHealthServer("remote-doctor")
	t.Cleanup(srv.Close)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--city-url", srv.URL,
		"--city-name", "remote-city",
		"doctor",
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("remote doctor failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var report doctorJSONReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode doctor JSON: %v; stdout=%q", err, stdout.String())
	}
	if len(report.Results) != 5 {
		t.Fatalf("got %d results, want remote status plus four explicit skips: %#v", len(report.Results), report.Results)
	}
	if report.Results[0].Name != "remote-status" || report.Results[0].Status != "ok" {
		t.Fatalf("remote status result = %#v", report.Results[0])
	}
	for _, result := range report.Results[1:] {
		if result.Status != "ok" || !strings.HasPrefix(result.Message, "SKIPPED: ") {
			t.Errorf("local-only result = %#v, want an explicit skip", result)
		}
	}
}

type blockingDoctorCheck struct {
	release <-chan struct{}
}

func (c *blockingDoctorCheck) Name() string { return "remote-status" }

func (c *blockingDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	<-c.release
	return &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK}
}

func (c *blockingDoctorCheck) CanFix() bool { return false }

func (c *blockingDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *blockingDoctorCheck) WarmupEligible() bool { return false }

func TestDoctorRemoteCheckTimeout(t *testing.T) {
	release := make(chan struct{})
	check := &blockingDoctorCheck{release: release}
	d := &doctor.Doctor{CheckTimeout: 5 * time.Millisecond}
	d.Register(check)

	started := time.Now()
	report := d.RunCollect(&doctor.CheckContext{}, false)
	if elapsed := time.Since(started); elapsed >= 150*time.Millisecond {
		t.Fatalf("doctor waited for the slow check: %s", elapsed)
	}
	if len(report.Results) != 1 {
		t.Fatalf("got %d results, want one: %#v", len(report.Results), report.Results)
	}
	if !report.Results[0].TimedOut || report.Results[0].Severity != doctor.SeverityAdvisory {
		t.Fatalf("remote status result = %#v, want timed_out=true", report.Results)
	}
	close(release)
	d.Wait()
}
