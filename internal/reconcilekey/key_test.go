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

func TestSessionInStoreCarriesStoreRef(t *testing.T) {
	got := Session("gc-1").InStore(" rig:alpha ")
	want := Key{Kind: KindSession, SessionID: "gc-1", StoreRef: "rig:alpha"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got := Allocator().InStore("rig:alpha"); got != Allocator() {
		t.Fatalf("InStore on a non-session key = %+v, want allocator unchanged", got)
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
		Session("gc-2").InStore("rig:alpha"),
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

func TestStringIsReadable(t *testing.T) {
	tests := map[Key]string{
		Allocator():                        "allocator",
		ControlDispatch():                  "control_dispatch",
		Session("gc-1"):                    "session:gc-1",
		SessionNamed("worker-1"):           "session:name=worker-1",
		Session("gc-1").InStore("rig:a"):   "session:gc-1@rig:a",
		{Kind: KindSession, SessionID: ""}: "allocator",
	}
	for k, want := range tests {
		if got := k.String(); got != want {
			t.Fatalf("%+v.String() = %q, want %q", k, got, want)
		}
	}
}
