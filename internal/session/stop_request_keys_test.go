package session

import "testing"

// TestRestartStopRequestClears (#7635 ruling, v5.9p): a runtime started
// outside PreWake clears whatever stop request or operator's drain mark the
// row holds, and writes nothing for a row that holds none.
func TestRestartStopRequestClears(t *testing.T) {
	meta := map[string]string{DrainAckIncarnationKey: "3", DrainAckAtKey: "x", OperatorDrainKey: "3", "state": "active"}
	got := restartStopRequestClears(meta)
	if len(got) != 3 || got[DrainAckIncarnationKey] != "" || got[OperatorDrainKey] != "" {
		t.Fatalf("clears = %v, want the ack and the operator's mark", got)
	}
	if _, ok := got[OperatorDrainKey]; !ok {
		t.Fatalf("clears = %v, want the operator's mark cleared", got)
	}
	if got := restartStopRequestClears(map[string]string{"state": "active"}); len(got) != 0 {
		t.Fatalf("clears = %v on a bare row, want none", got)
	}
}

// TestReadStopRequest (V2-18) pins the content gate's reader of the stop
// request's controller half: a set reason whose incarnation names the row's
// generation, read as cmd/gc's activeStop reads it, and nothing under
// StopRequestsIgnored.
func TestReadStopRequest(t *testing.T) {
	const at = "2026-10-22T12:00:00Z"
	triple := func(gen, reason, incarnation string) map[string]string {
		m := map[string]string{DrainIntentAtKey: at}
		if gen != "-" {
			m["generation"] = gen
		}
		if reason != "-" {
			m[DrainIntentReasonKey] = reason
		}
		if incarnation != "-" {
			m[DrainIntentIncarnationKey] = incarnation
		}
		return m
	}
	noAt := triple("3", "idle", "3")
	delete(noAt, DrainIntentAtKey)
	cleared := triple("3", "idle", "3")
	for k, v := range ClearStopRequestPatch() {
		cleared[k] = v
	}
	for _, tc := range []struct {
		name    string
		meta    map[string]string
		reading StopRequestReading
		want    bool
	}{
		{"at the row's generation", triple("3", "idle", "3"), StopRequestsLive, true},
		{"another incarnation", triple("3", "idle", "2"), StopRequestsLive, false},
		{"no time: v2 signals it at once", noAt, StopRequestsLive, true},
		{"no reason", triple("3", "-", "3"), StopRequestsLive, false},
		{"no incarnation", triple("3", "idle", "-"), StopRequestsLive, false},
		{"blank reason", triple("3", "  ", "3"), StopRequestsLive, false},
		{"padded", triple(" 3 ", " idle ", " 3 "), StopRequestsLive, true},
		{"absent generation is 0", triple("-", "idle", "0"), StopRequestsLive, true},
		{"empty generation is 0", triple("", "idle", "0"), StopRequestsLive, true},
		{"malformed generation is 0", triple("x", "idle", "0"), StopRequestsLive, true},
		{"generation normalized", triple("03", "idle", "3"), StopRequestsLive, true},
		{"incarnation not normalized", triple("3", "idle", "03"), StopRequestsLive, false},
		{"out-of-range generation clamps", triple("9223372036854775808", "idle", "9223372036854775807"), StopRequestsLive, true},
		{"the ack half alone", map[string]string{"generation": "3", DrainAckIncarnationKey: "3", DrainAckAtKey: at}, StopRequestsLive, false},
		{"the operator's drain alone", map[string]string{"generation": "3", OperatorDrainKey: "3"}, StopRequestsLive, false},
		{"nil metadata", nil, StopRequestsLive, false},
		{"cleared", cleared, StopRequestsLive, false},
		{"a legacy controller ignores it", triple("3", "idle", "3"), StopRequestsIgnored, false},
		{"the zero reading is live", triple("3", "idle", "3"), StopRequestReading(0), true},
		{"an unknown reading is live", triple("3", "idle", "3"), StopRequestReading(7), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReadStopRequest(tc.meta, tc.reading).Requested(); got != tc.want {
				t.Fatalf("ReadStopRequest(%v, %d).Requested() = %v, want %v", tc.meta, tc.reading, got, tc.want)
			}
		})
	}
	if (StopRequest{}).Requested() {
		t.Fatal("the zero StopRequest requests a stop")
	}
}
