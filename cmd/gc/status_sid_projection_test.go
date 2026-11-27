package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/runtime/observation"
)

func TestStatusSIDCLIProjectionRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 7, 14, 50, 0, 0, time.UTC)
	p := &observation.RuntimeSessions{Schema: observation.RuntimeSessionsSchema, ObservedAt: now, ProviderComplete: true, Sessions: []observation.RuntimeSessionStatus{{ID: "manual", Template: "tenders/gastown.polecat", AgentName: "adhoc", RuntimeName: "actual-handle", Provider: "*tmux.seamBackedProvider", Running: true}}}
	s := cityStatusSnapshot{RuntimeSessions: p, Controller: ControllerJSON{Running: true}}
	j := cityStatusJSONFromSnapshot(s, StatusSummaryJSON{})
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	var decoded StatusJSON
	if json.Unmarshal(b, &decoded) != nil || decoded.RuntimeSessions == nil {
		t.Fatal("CLI lost SID projection")
	}
	if !decoded.RuntimeSessions.ObservedAt.Equal(now) || decoded.RuntimeSessions.Sessions[0].RuntimeName != "actual-handle" || !decoded.Health.Usable || decoded.Partial {
		t.Fatal("CLI changed original projection/legacy health")
	}
	if (api.StatusView{RuntimeSessions: p}).RuntimeSessions != p {
		t.Fatal("typed view lost projection")
	}
}
