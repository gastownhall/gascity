package reconcilekey

import (
	"strings"
	"testing"
)

func TestConstructorsProduceNormalizedKeys(t *testing.T) {
	tests := []struct {
		name string
		got  Key
		want Key
	}{
		{"allocator", Allocator(), Key{Kind: KindAllocator}},
		{"control dispatch", ControlDispatch(), Key{Kind: KindControlDispatch}},
		{"session", Session(" gc-1 "), Key{Kind: KindSession, SessionID: "gc-1"}},
		{"session named", SessionNamed(" worker-1 "), Key{Kind: KindSession, SessionName: "worker-1"}},
		{"session empty id degrades to allocator", Session("  "), Key{Kind: KindAllocator}},
		{"session empty name degrades to allocator", SessionNamed(""), Key{Kind: KindAllocator}},
		{"session ref with both", SessionRef("gc-1", "worker-1"), Key{Kind: KindSession, SessionID: "gc-1", SessionName: "worker-1"}},
		{"session ref name only", SessionRef(" ", "worker-1"), Key{Kind: KindSession, SessionName: "worker-1"}},
		{"session ref empty degrades to allocator", SessionRef("", ""), Key{Kind: KindAllocator}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("got %+v, want %+v", tt.got, tt.want)
			}
		})
	}
}

func TestNormalizeTreatsKeylessAsAllocator(t *testing.T) {
	tests := []struct {
		name string
		in   Key
		want Key
	}{
		{"zero key", Key{}, Allocator()},
		{"unknown kind", Key{Kind: "bogus", SessionID: "gc-1"}, Allocator()},
		{"session without identity", Key{Kind: KindSession, StoreRef: "rig:a"}, Allocator()},
		{"allocator drops stray fields", Key{Kind: KindAllocator, SessionID: "gc-1"}, Allocator()},
		{"control dispatch drops stray fields", Key{Kind: KindControlDispatch, SessionName: "x"}, ControlDispatch()},
		{"session trims", Key{Kind: KindSession, SessionID: " gc-1 ", SessionName: " n ", StoreRef: " r "}, Key{Kind: KindSession, SessionID: "gc-1", SessionName: "n", StoreRef: "r"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.Normalize(); got != tt.want {
				t.Fatalf("Normalize(%+v) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	for _, k := range []Key{
		Allocator(),
		ControlDispatch(),
		Session("gc-1"),
		SessionNamed("worker-1"),
		{Kind: KindSession, SessionID: "gc-2", StoreRef: "rig:alpha"},
		Session("gc-3\nnext-line\r\x00\x1b[0m"),
	} {
		payload := k.Encode()
		if strings.ContainsAny(payload, "\n\r") {
			t.Fatalf("Encode(%+v) = %q contains a line break; socket commands are line-framed", k, payload)
		}
		got, err := Decode(payload)
		if err != nil {
			t.Fatalf("Decode(%q): %v", payload, err)
		}
		if got != k {
			t.Fatalf("round trip %+v -> %q -> %+v", k, payload, got)
		}
	}
}

func TestDecodeRejectsMalformedPayload(t *testing.T) {
	if _, err := Decode("{not json"); err == nil {
		t.Fatal("Decode of malformed JSON succeeded, want error")
	}
}

func TestDecodeNormalizesPayload(t *testing.T) {
	got, err := Decode(`{"kind":"session"}`)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != Allocator() {
		t.Fatalf("Decode of an identity-less session key = %+v, want allocator", got)
	}
}

func TestEncodeEscapesLineBreaksAndControlCharsInIdentities(t *testing.T) {
	k := SessionRef("gc-1\nstop", "worker\r\n\x00\x1b[31m")
	payload := k.Encode()
	for _, c := range payload {
		if c < 0x20 || c == 0x7f {
			t.Fatalf("Encode(%+v) = %q contains raw control char %q; socket commands are line-framed", k, payload, c)
		}
	}
	got, err := Decode(payload)
	if err != nil {
		t.Fatalf("Decode(%q): %v", payload, err)
	}
	if got != k {
		t.Fatalf("round trip %+v -> %q -> %+v", k, payload, got)
	}
}
