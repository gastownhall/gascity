package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProbeComponentVersionsParsesBothVersions(t *testing.T) {
	got := probeComponentVersions(
		func() (string, error) { return "2.0.7", nil },
		func() (string, error) { return "1.0.4", nil },
	)
	if got.Dolt != "2.0.7" {
		t.Errorf("Dolt = %q, want 2.0.7", got.Dolt)
	}
	if got.Beads != "1.0.4" {
		t.Errorf("Beads = %q, want 1.0.4", got.Beads)
	}
}

func TestProbeComponentVersionsOmitsAndLogsOnFailure(t *testing.T) {
	var buf bytes.Buffer
	defer captureLog(t, &buf)()

	got := probeComponentVersions(
		func() (string, error) { return "", fmt.Errorf("dolt: executable file not found") },
		func() (string, error) { return "1.0.4", nil },
	)
	if got.Dolt != "" {
		t.Errorf("Dolt = %q, want empty on probe failure", got.Dolt)
	}
	if got.Beads != "1.0.4" {
		t.Errorf("Beads = %q, want 1.0.4", got.Beads)
	}
	if !strings.Contains(buf.String(), "dolt version probe failed") {
		t.Errorf("expected dolt probe failure to be logged, got %q", buf.String())
	}
}

func TestResolveComponentVersionsProbesOnce(t *testing.T) {
	var calls int32
	s := &Server{componentVersionsProbe: func() componentVersions {
		atomic.AddInt32(&calls, 1)
		return componentVersions{Dolt: "2.0.7", Beads: "1.0.4"}
	}}
	for i := 0; i < 3; i++ {
		got := s.resolveComponentVersions()
		if got.Dolt != "2.0.7" || got.Beads != "1.0.4" {
			t.Fatalf("resolveComponentVersions() = %+v", got)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("probe called %d times, want 1 (cached for process lifetime)", n)
	}
}

func TestBuildStatusBodyIncludesComponentVersions(t *testing.T) {
	state := newFakeState(t)
	s := &Server{state: state, componentVersionsProbe: func() componentVersions {
		return componentVersions{Dolt: "2.0.7", Beads: "1.0.4"}
	}}

	body := s.buildStatusBody(false)
	if body.DoltVersion != "2.0.7" {
		t.Errorf("DoltVersion = %q, want 2.0.7", body.DoltVersion)
	}
	if body.BeadsVersion != "1.0.4" {
		t.Errorf("BeadsVersion = %q, want 1.0.4", body.BeadsVersion)
	}
}

func TestStatusBodyOmitsEmptyComponentVersions(t *testing.T) {
	out, err := json.Marshal(StatusBody{Name: "c"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "dolt_version") {
		t.Errorf("dolt_version present for empty value; want omitted: %s", s)
	}
	if strings.Contains(s, "beads_version") {
		t.Errorf("beads_version present for empty value; want omitted: %s", s)
	}
}

// captureLog redirects the standard logger to buf for the duration of a test,
// returning a restore func.
func captureLog(t *testing.T, buf *bytes.Buffer) func() {
	t.Helper()
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	return func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}
}
