package api

import (
	"encoding/json"
	"testing"
	"time"
)

// originalWorkerOperationEventPayload mirrors WorkerOperationEventPayload's
// shape on main before issue #1252 (1a) added the cost/latency fields.
// Used as a baseline so we can measure the per-event byte overhead the
// new fields add — the figure the umbrella issue (#1184) committed to
// reporting before declaring 1a done.
type originalWorkerOperationEventPayload struct {
	OpID        string    `json:"op_id"`
	Operation   string    `json:"operation"`
	Result      string    `json:"result"`
	SessionID   string    `json:"session_id,omitempty"`
	SessionName string    `json:"session_name,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	Transport   string    `json:"transport,omitempty"`
	Template    string    `json:"template,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	DurationMs  int64     `json:"duration_ms"`
	Queued      *bool     `json:"queued,omitempty"`
	Delivered   *bool     `json:"delivered,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// realisticBaseline mimics the populated fields a typical session-lifecycle
// event carries on main today. Field values approximate observed payloads
// from existing dev cities.
func realisticBaseline() originalWorkerOperationEventPayload {
	return originalWorkerOperationEventPayload{
		OpID:        "0a1b2c3d4e5f6789",
		Operation:   "message",
		Result:      "succeeded",
		SessionID:   "01HF8XYZ4N5W7V8K9P0Q1R2S3T",
		SessionName: "rig/polecat-1",
		Provider:    "claude",
		Transport:   "tmux",
		Template:    "polecat",
		StartedAt:   time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC),
		FinishedAt:  time.Date(2026, 4, 25, 12, 0, 1, 234000000, time.UTC),
		DurationMs:  1234,
	}
}

// realisticExtended populates the same fields plus every 1a field with
// values an actually-instrumented production event would carry. Used to
// measure the worst-case overhead — the upper bound a fully-wired event
// adds versus the baseline.
func realisticExtended() WorkerOperationEventPayload {
	b := realisticBaseline()
	return WorkerOperationEventPayload{
		OpID:                b.OpID,
		Operation:           b.Operation,
		Result:              b.Result,
		SessionID:           b.SessionID,
		SessionName:         b.SessionName,
		Provider:            b.Provider,
		Transport:           b.Transport,
		Template:            b.Template,
		StartedAt:           b.StartedAt,
		FinishedAt:          b.FinishedAt,
		DurationMs:          b.DurationMs,
		Model:               "claude-opus-4-7",
		AgentName:           "rig/polecat-1",
		PromptVersion:       "v3",
		PromptSHA:           "f29c0c4ddae64f6f8f7e3d2a1b0c1f3e5a7c9d2b4e6f8091a3b5c7d9e1f3a5b7",
		BeadID:              "rig-1234",
		PromptTokens:        2500,
		CompletionTokens:    1200,
		CacheReadTokens:     18000,
		CacheCreationTokens: 4500,
		LatencyMs:           4321,
		CostUSDEstimate:     0.04567,
	}
}

// realisticPartial populates only the 1a fields that have data sources
// wired in PR #1272 today: AgentName flows from session.Info.Alias on
// every operation. The remaining 1a fields stay at zero (omitempty
// suppresses them on the wire), so this is the realistic per-event
// overhead until follow-up wiring lands for the rest.
func realisticPartial() WorkerOperationEventPayload {
	b := realisticBaseline()
	return WorkerOperationEventPayload{
		OpID:        b.OpID,
		Operation:   b.Operation,
		Result:      b.Result,
		SessionID:   b.SessionID,
		SessionName: b.SessionName,
		Provider:    b.Provider,
		Transport:   b.Transport,
		Template:    b.Template,
		StartedAt:   b.StartedAt,
		FinishedAt:  b.FinishedAt,
		DurationMs:  b.DurationMs,
		AgentName:   "rig/polecat-1",
	}
}

// TestWorkerOperationPayloadByteOverhead measures the JSON-encoded
// per-event size for three scenarios and prints the diffs. Acts as a
// regression alarm if the overhead drifts significantly upward (a
// failure threshold) and as the canonical citation for the umbrella
// issue's promised number.
func TestWorkerOperationPayloadByteOverhead(t *testing.T) {
	baseRaw, err := json.Marshal(realisticBaseline())
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	partialRaw, err := json.Marshal(realisticPartial())
	if err != nil {
		t.Fatalf("marshal partial: %v", err)
	}
	fullRaw, err := json.Marshal(realisticExtended())
	if err != nil {
		t.Fatalf("marshal full: %v", err)
	}

	baseSize := len(baseRaw)
	partialSize := len(partialRaw)
	fullSize := len(fullRaw)

	partialOverhead := partialSize - baseSize
	fullOverhead := fullSize - baseSize

	t.Logf("baseline event size:                %d bytes", baseSize)
	t.Logf("realistic-partial event size:       %d bytes (+%d, %.1f%%)",
		partialSize, partialOverhead, 100*float64(partialOverhead)/float64(baseSize))
	t.Logf("fully-instrumented event size:      %d bytes (+%d, %.1f%%)",
		fullSize, fullOverhead, 100*float64(fullOverhead)/float64(baseSize))

	// Sanity bounds. Partial population (just AgentName) should add ~30
	// bytes; full population should land somewhere south of 300 extra
	// bytes. Numbers above that suggest a regression — a new field with
	// large data, or a botched omitempty tag.
	if partialOverhead > 100 {
		t.Errorf("partial overhead %d bytes/event is higher than expected (~30)", partialOverhead)
	}
	if fullOverhead > 400 {
		t.Errorf("full overhead %d bytes/event is higher than expected (<400)", fullOverhead)
	}

	// City event-rate scenarios. Numbers come from inspecting events.jsonl
	// in three real dev cities at 2026-04-26: total event rates ranged
	// from 800 to 1900 events/hour with worker.operation fractions
	// approximately matching the session-lifecycle event share. The
	// scenarios below bracket the realistic range an operator might see.
	scenarios := []struct {
		name          string
		eventsPerHour int
	}{
		{"idle city (mostly polling)", 100},
		{"typical city (a few active agents)", 600},
		{"busy city (heavy multi-agent loop)", 2000},
	}
	for _, s := range scenarios {
		partialPerHour := partialOverhead * s.eventsPerHour
		fullPerHour := fullOverhead * s.eventsPerHour
		t.Logf("%s @ %d events/hour:", s.name, s.eventsPerHour)
		t.Logf("  partial wiring (today): +%s/hour, +%s/day",
			humanBytes(partialPerHour),
			humanBytes(partialPerHour*24),
		)
		t.Logf("  full wiring (all fields): +%s/hour, +%s/day",
			humanBytes(fullPerHour),
			humanBytes(fullPerHour*24),
		)
	}
}

// humanBytes formats a byte count in B/KB/MB without external dependencies.
func humanBytes(n int) string {
	switch {
	case n < 1024:
		return formatInt(n) + " B"
	case n < 1024*1024:
		return formatFloat(float64(n)/1024) + " KB"
	default:
		return formatFloat(float64(n)/(1024*1024)) + " MB"
	}
}

func formatInt(n int) string {
	return formatFloat(float64(n))
}

func formatFloat(f float64) string {
	// Two significant digits is enough for an order-of-magnitude
	// citation; avoiding fmt.Sprintf to keep this self-contained.
	if f == float64(int64(f)) {
		return intStr(int64(f))
	}
	whole := int64(f)
	frac := int64((f - float64(whole)) * 10)
	return intStr(whole) + "." + intStr(frac)
}

func intStr(n int64) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
