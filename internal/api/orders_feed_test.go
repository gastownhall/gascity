package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestParseOrdersFeedLimitCapsLargeValues(t *testing.T) {
	if got := parseOrdersFeedLimit(""); got != 50 {
		t.Fatalf("default limit = %d, want 50", got)
	}
	if got := parseOrdersFeedLimit("25"); got != 25 {
		t.Fatalf("parsed limit = %d, want 25", got)
	}
	if got := parseOrdersFeedLimit("999999"); got != maxOrdersFeedLimit {
		t.Fatalf("capped limit = %d, want %d", got, maxOrdersFeedLimit)
	}
}

func TestOrderTrackingStatusTreatsWispFailedAsFailed(t *testing.T) {
	bead := beads.Bead{
		Status: "closed",
		Labels: []string{"order-tracking", "wisp", "wisp-failed"},
	}
	if got := orderTrackingStatus(bead); got != "failed" {
		t.Fatalf("orderTrackingStatus = %q, want failed", got)
	}
}

func TestParseMonitorTimestampAcceptsRFC3339AndNano(t *testing.T) {
	base := "2026-03-26T14:06:31+01:00"
	if got := parseMonitorTimestamp(base); got.IsZero() {
		t.Fatalf("parseMonitorTimestamp(%q) = zero, want parsed timestamp", base)
	}

	nano := "2026-03-26T14:06:31.123456789+01:00"
	got := parseMonitorTimestamp(nano)
	if got.IsZero() {
		t.Fatalf("parseMonitorTimestamp(%q) = zero, want parsed timestamp", nano)
	}
	if got.Nanosecond() != 123456789 {
		t.Fatalf("nanoseconds = %d, want 123456789", got.Nanosecond())
	}
	if got.Format("2006-01-02T15:04:05.999999999Z07:00") != nano {
		t.Fatalf("formatted timestamp = %q, want %q", got.Format("2006-01-02T15:04:05.999999999Z07:00"), nano)
	}
}
