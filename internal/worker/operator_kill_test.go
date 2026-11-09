package worker

import (
	"testing"
	"time"
)

// LiveSubagentsError's message measures each subagent's age at At, the moment
// the refusal was taken: Error formats only what the error holds, so it reads
// no clock (error formatting is reached from the reconciler's pure decide).
func TestLiveSubagentsErrorMeasuresAgesAtTheRefusal(t *testing.T) {
	started := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	err := &LiveSubagentsError{
		Subagents: []InFlightSubagent{
			{AgentID: "a1", Description: "Investigate the slow check", StartedAt: started},
			{AgentID: "a2"},
		},
		At: started.Add(65 * time.Second),
	}
	want := "refusing to destroy 2 live background subagent(s); retry with force: " +
		"Investigate the slow check (a1, running 1m5s); (no description) (a2, running unknown duration)"
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}
