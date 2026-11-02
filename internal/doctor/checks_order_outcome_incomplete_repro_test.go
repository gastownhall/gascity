package doctor

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// TestOrderOutcomeHealthyReproRecordedIncompleteOutcome: a monitored cooldown
// order whose latest run recorded order.completed followed by an order.skipped
// partial declaration (the controller's emit order) must surface a warning
// carrying the declaration message, not "last run succeeded".
func TestOrderOutcomeHealthyReproRecordedIncompleteOutcome(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrderInDir(t, filepath.Join(cityPath, "orders"), "export-sweep", "cooldown", "6h")

	msg := "partial: store unreachable; not run: rig:st (dolt not reachable)"
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderCompleted, Ts: now.Add(-time.Hour), Subject: "export-sweep", Actor: "controller"},
		events.Event{
			Type: events.OrderSkipped, Ts: now.Add(-time.Hour), Subject: "export-sweep", Actor: "controller", Message: msg,
			Payload: events.OrderSkippedPayloadJSON(events.OrderSkippedPayload{
				OrderName: "export-sweep", Outcome: "partial", Reason: "store unreachable",
				Scopes: []events.OrderSkippedScope{{Scope: "rig:st", Reason: "dolt not reachable"}},
			}),
		},
	)

	result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning for a recorded incomplete outcome; msg=%q details=%v", result.Status, result.Message, result.Details)
	}
	if joined := strings.Join(result.Details, "\n"); !strings.Contains(joined, msg) {
		t.Fatalf("details = %v, want them to carry the declaration message %q", result.Details, msg)
	}
}
