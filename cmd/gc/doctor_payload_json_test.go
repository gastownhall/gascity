package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/doctor"
)

// TestWriteDoctorJSONProjectsCheckPayloads pins the wire contract for a check's
// structured findings: `payload` carries whatever the check set, verbatim, and
// is absent for every check that set none.
//
// Absence matters as much as presence. Every existing `gc doctor --json`
// consumer parses results that now pass through a struct with one more field,
// and a payload key on all of them would be a wire change for checks that have
// nothing to say.
func TestWriteDoctorJSONProjectsCheckPayloads(t *testing.T) {
	cursors := proxyendpoint.Cursors{Main: 66, Ignored: 26}
	report := &doctor.Report{
		Passed: 2,
		Results: []*doctor.CheckResult{
			{
				Name:    "beads-store",
				Status:  doctor.StatusOK,
				Message: "bd-owned proxied store (bd CLI front door)",
				Payload: &doctor.BeadsStorePayload{
					Store:         "BdStore",
					PreflightGate: "proxied_provider",
					Endpoint: &doctor.ProxiedEndpointPayload{
						Root:             "/city/.beads/dolt",
						Host:             proxyendpoint.Host,
						Port:             45123,
						PID:              6001,
						Generation:       "6001:abcdef012345",
						Verdict:          "live",
						Evidence:         "argv+birth",
						BirthEvidence:    "match",
						IdlePolicy:       "never",
						IdlePolicySource: "argv",
						Probe:            "served",
						Cursors:          &cursors,
						ExpectedCursors:  cursors,
					},
				},
			},
			{Name: "city-config", Status: doctor.StatusOK, Message: "ok"},
		},
	}

	var buf bytes.Buffer
	if err := writeDoctorJSON(&buf, report); err != nil {
		t.Fatalf("writeDoctorJSON: %v", err)
	}

	var decoded struct {
		Results []struct {
			Name    string `json:"name"`
			Payload *struct {
				Store         string `json:"store"`
				PreflightGate string `json:"preflight_gate"`
				Endpoint      *struct {
					Port       int    `json:"port"`
					PID        int    `json:"pid"`
					Verdict    string `json:"verdict"`
					Evidence   string `json:"evidence"`
					IdlePolicy string `json:"idle_policy"`
					Probe      string `json:"probe"`
					Cursors    *struct {
						Main    int `json:"main"`
						Ignored int `json:"ignored"`
					} `json:"cursors"`
				} `json:"endpoint"`
			} `json:"payload"`
		} `json:"results"`
	}
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode doctor JSON: %v; out=%q", err, buf.String())
	}
	if len(decoded.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(decoded.Results))
	}

	got := decoded.Results[0].Payload
	if got == nil {
		t.Fatal("the beads-store result carried no payload")
	}
	if got.Store != "BdStore" || got.PreflightGate != "proxied_provider" {
		t.Errorf("payload = %+v, want BdStore behind the proxied_provider gate", got)
	}
	if got.Endpoint == nil {
		t.Fatal("the payload carried no endpoint")
	}
	endpoint := got.Endpoint
	if endpoint.Port != 45123 || endpoint.PID != 6001 {
		t.Errorf("endpoint = :%d pid=%d, want :45123 pid=6001", endpoint.Port, endpoint.PID)
	}
	if endpoint.Verdict != "live" || endpoint.Evidence != "argv+birth" || endpoint.IdlePolicy != "never" || endpoint.Probe != "served" {
		t.Errorf("endpoint = %+v, want live/argv+birth/never/served", endpoint)
	}
	if endpoint.Cursors == nil || endpoint.Cursors.Main != 66 || endpoint.Cursors.Ignored != 26 {
		t.Errorf("cursors = %+v, want main=66 ignored=26", endpoint.Cursors)
	}

	if decoded.Results[1].Payload != nil {
		t.Errorf("a check with no findings projected a payload: %+v", decoded.Results[1].Payload)
	}
	if n := strings.Count(buf.String(), `"payload"`); n != 1 {
		t.Fatalf("payload appears %d times, want exactly 1 (only the check that set one); out=%s", n, buf.String())
	}
}
