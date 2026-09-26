package api

import "testing"

// TestEventStreamInputResolveAfterSeq owns the reconnect-cursor matrix for
// GET /v0/city/{city}/events/stream. ok=false must cover every cursor the
// stream cannot place, so the handler head-starts instead of reading the
// cursor as 0, which Watch treats as "replay the entire retained history".
func TestEventStreamInputResolveAfterSeq(t *testing.T) {
	cases := []struct {
		name        string
		lastEventID string
		afterSeq    string
		wantSeq     uint64
		wantOK      bool
	}{
		{name: "absent", wantOK: false},
		{name: "last-event-id", lastEventID: "42", wantSeq: 42, wantOK: true},
		{name: "after_seq", afterSeq: "17", wantSeq: 17, wantOK: true},
		{name: "last-event-id wins", lastEventID: "42", afterSeq: "17", wantSeq: 42, wantOK: true},
		{name: "explicit zero is a cursor", lastEventID: "0", wantSeq: 0, wantOK: true},
		{name: "unparseable last-event-id", lastEventID: "abc", wantOK: false},
		{name: "trailing garbage", lastEventID: "12abc", wantOK: false},
		{name: "overflow", lastEventID: "18446744073709551616", wantOK: false},
		{name: "present last-event-id wins even when unparseable", lastEventID: "abc", afterSeq: "17", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &EventStreamInput{LastEventID: tc.lastEventID, AfterSeq: tc.afterSeq}
			seq, ok := in.resolveAfterSeq()
			if seq != tc.wantSeq || ok != tc.wantOK {
				t.Fatalf("resolveAfterSeq() = (%d, %v), want (%d, %v)", seq, ok, tc.wantSeq, tc.wantOK)
			}
		})
	}
}

// TestEventStreamInputResolveRejectsMalformedAfterSeq owns after_seq
// validation: anything that is not a non-negative integer is a 422 before the
// stream commits, never a silent Watch(0). Last-Event-ID is not validated
// here — EventSource clients stop reconnecting on any non-200.
func TestEventStreamInputResolveRejectsMalformedAfterSeq(t *testing.T) {
	cases := []struct {
		name        string
		afterSeq    string
		lastEventID string
		wantErr     bool
	}{
		{name: "absent", wantErr: false},
		{name: "valid", afterSeq: "17", wantErr: false},
		{name: "zero", afterSeq: "0", wantErr: false},
		{name: "garbage", afterSeq: "nope", wantErr: true},
		{name: "negative", afterSeq: "-1", wantErr: true},
		{name: "padded", afterSeq: " 5 ", wantErr: true},
		{name: "overflow", afterSeq: "18446744073709551616", wantErr: true},
		{name: "unparseable last-event-id is not rejected", lastEventID: "abc", wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &EventStreamInput{LastEventID: tc.lastEventID, AfterSeq: tc.afterSeq}
			if errs := in.Resolve(nil); (len(errs) > 0) != tc.wantErr {
				t.Fatalf("Resolve() = %v, want error: %v", errs, tc.wantErr)
			}
		})
	}
}
