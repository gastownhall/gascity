package api

import (
	"strings"
	"testing"
	"time"
)

func TestKeysetCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 7, 11, 12, 30, 45, 123456789, time.UTC)
	in := keysetCursor{Kind: cursorKindCreatedID, CreatedAt: ts, ID: "gc-42"}
	tok := encodeKeysetCursor(in)
	if !strings.HasPrefix(tok, "v1:") {
		t.Fatalf("token %q lacks the v1: version prefix", tok)
	}
	out, err := decodeKeysetCursor(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Kind != cursorKindCreatedID || out.ID != "gc-42" || !out.CreatedAt.Equal(ts) {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}

func TestKeysetCursorSeqRoundTrip(t *testing.T) {
	in := keysetCursor{Kind: cursorKindSeq, Seq: 98765}
	out, err := decodeKeysetCursor(encodeKeysetCursor(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Kind != cursorKindSeq || out.Seq != 98765 {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}

func TestKeysetCursorRejectsGarbage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"legacy offset cursor", "NTA"},          // base64("50") — the old format
		{"no prefix", "eyJrIjoiY2IifQ"},          // valid b64 JSON, missing v1:
		{"unknown version", "v9:eyJrIjoiY2IifQ"}, //
		{"bad base64", "v1:!!!not-base64!!!"},    //
		{"bad json", "v1:bm90LWpzb24"},           // base64("not-json")
		{"unknown kind", "v1:eyJrIjoienoifQ"},    // {"k":"zz"}
		{"cb missing id", "v1:eyJrIjoiY2IifQ"},   // {"k":"cb"} — no id
		{"whitespace", "  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeKeysetCursor(tc.token); err == nil {
				t.Fatalf("decodeKeysetCursor(%q) = nil error, want rejection", tc.token)
			}
		})
	}
}

// TestKeysetCursorZeroCreatedAtRoundTrips pins the accept-what-you-mint
// invariant: degraded rows (NULL/unparseable created_at from a drifted store)
// carry zero timestamps, sort to the created-DESC tail, and the server mints
// boundaries from them. The decoder must accept those tokens or a walk wedges
// in a 400 loop at the tail.
func TestKeysetCursorZeroCreatedAtRoundTrips(t *testing.T) {
	tok := encodeKeysetCursor(keysetCursor{Kind: cursorKindCreatedID, ID: "gc-7"})
	out, err := decodeKeysetCursor(tok)
	if err != nil {
		t.Fatalf("decode of a server-minted zero-CreatedAt token failed: %v", err)
	}
	if out.Kind != cursorKindCreatedID || out.ID != "gc-7" || !out.CreatedAt.IsZero() {
		t.Fatalf("round trip = %+v, want zero-CreatedAt cb boundary for gc-7", out)
	}
}

func TestKeysetCursorKindMismatchDetectable(t *testing.T) {
	// A seq cursor decoded where a cb cursor is expected: the decode succeeds
	// (it is a valid token) — the caller checks Kind. Pin that Kind survives.
	tok := encodeKeysetCursor(keysetCursor{Kind: cursorKindSeq, Seq: 7})
	out, err := decodeKeysetCursor(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Kind == cursorKindCreatedID {
		t.Fatal("seq cursor decoded as cb kind")
	}
}
