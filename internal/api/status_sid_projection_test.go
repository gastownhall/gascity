package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

func TestStatusSIDGeneratedViewPreservesProjection(t *testing.T) {
	now := time.Date(2026, 10, 7, 14, 50, 0, 0, time.UTC)
	rows := []genclient.RuntimeSessionStatus{{Id: "manual", Template: "tenders/gastown.polecat", AgentName: "adhoc", RuntimeName: "actual-handle", Provider: "*tmux.seamBackedProvider", Running: true}, {Id: "never", Template: "worker", Provider: "*tmux.seamBackedProvider"}}
	body := &genclient.StatusBody{RuntimeSessions: &genclient.RuntimeSessions{Schema: "gascity.runtime-sessions/v1", ObservedAt: now, ProviderComplete: true, Sessions: &rows}}
	v := statusViewFromGen(body)
	if v.RuntimeSessions == nil || !v.RuntimeSessions.ObservedAt.Equal(now) || len(v.RuntimeSessions.Sessions) != 2 || v.RuntimeSessions.Sessions[1].RuntimeName != "" {
		t.Fatal("projection changed through generated view")
	}
	data, err := json.Marshal(v.RuntimeSessions)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil || len(obj) != 4 {
		t.Fatal("projection schema changed")
	}
	if len(obj["sessions"].([]any)[0].(map[string]any)) != 6 {
		t.Fatal("row keys changed")
	}
	if v.Partial || len(v.Agents) != 0 {
		t.Fatal("projection changed legacy status")
	}
}
