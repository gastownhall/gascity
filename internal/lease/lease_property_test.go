package lease

import (
	"math"
	"testing"
	"time"

	"pgregory.net/rapid"
)

const (
	minHeldExpiryUnix int64 = -62135596799
	maxHeldExpiryUnix int64 = 253402300799
)

func drawHolderPart(rt *rapid.T, label string) string {
	return rapid.StringMatching(`[^\x00-\x1f\x7f-\x9f|@#]+`).Draw(rt, label)
}

func drawRecord(rt *rapid.T) Record {
	state := rapid.SampledFrom([]State{StateHeld, StateParked, StateDead}).Draw(rt, "state")
	rec := Record{
		Epoch: rapid.Int64Range(0, math.MaxInt64).Draw(rt, "epoch"),
		Holder: Holder{
			Agent:         drawHolderPart(rt, "agent"),
			Session:       drawHolderPart(rt, "session"),
			InstanceToken: drawHolderPart(rt, "instanceToken"),
		},
		State:    state,
		Attempts: rapid.Int64Range(0, math.MaxInt64).Draw(rt, "attempts"),
	}
	if state == StateHeld {
		sec := rapid.Int64Range(minHeldExpiryUnix, maxHeldExpiryUnix).Draw(rt, "expirySeconds")
		nsec := rapid.Int64Range(0, 999_999_999).Draw(rt, "expiryNanos")
		offset := rapid.IntRange(-14*3600, 14*3600).Draw(rt, "zoneOffset")
		rec.ExpiresAt = time.Unix(sec, nsec).In(time.FixedZone("drawn", offset))
	}
	return rec
}

func mutateWire(rt *rapid.T, s string) string {
	runes := []rune(s)
	last := len(runes) - 1
	i := rapid.IntRange(0, last).Draw(rt, "index")
	r := rapid.Rune().Draw(rt, "rune")
	switch rapid.IntRange(0, 2).Draw(rt, "op") {
	case 0:
		return string(runes[:i]) + string(r) + string(runes[i:])
	case 1:
		return string(runes[:i]) + string(runes[i+1:])
	default:
		runes[i] = r
		return string(runes)
	}
}

func TestPropertyEncodeDecodeRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		rec := drawRecord(rt)
		wire, err := Encode(rec)
		if err != nil {
			rt.Fatalf("Encode(%+v): %v", rec, err)
		}
		got, err := Decode(wire)
		if err != nil {
			rt.Fatalf("Decode(%q): %v", wire, err)
		}
		if want := rec.Canonical(); got != want {
			rt.Fatalf("Decode(Encode(rec)) = %+v, want %+v", got, want)
		}
		again, err := Encode(got)
		if err != nil {
			rt.Fatalf("Encode(decoded): %v", err)
		}
		if again != wire {
			rt.Fatalf("Encode(Decode(wire)) = %q, want %q", again, wire)
		}
		l, err := Lookup(map[string]string{"lease": wire}, "lease")
		if err != nil {
			rt.Fatalf("Lookup: %v", err)
		}
		if l.IsOpaque() {
			rt.Fatalf("Lookup(%q) returned an opaque lease", wire)
		}
		found, ok := l.Record()
		if !ok || found != rec.Canonical() {
			rt.Fatalf("Lookup = (%+v, %v), want (%+v, true)", found, ok, rec.Canonical())
		}
	})
}

func TestPropertyDecodeAcceptsOnlyCanonicalAndFailsClosed(t *testing.T) {
	check := func(rt *rapid.T, s string) {
		rec, err := Decode(s)
		l, lerr := DecodeLease(s, true)
		if err == nil {
			wire, encErr := Encode(rec)
			if encErr != nil {
				rt.Fatalf("Encode(Decode(%q)): %v", s, encErr)
			}
			if wire != s {
				rt.Fatalf("Decode accepted %q, which re-encodes to %q", s, wire)
			}
			if lerr != nil {
				rt.Fatalf("DecodeLease(%q) errored while Decode accepted it: %v", s, lerr)
			}
			return
		}
		if lerr == nil {
			rt.Fatalf("DecodeLease(%q) accepted a value Decode rejected: %v", s, err)
		}
		if l.IsUnleased() {
			rt.Fatalf("DecodeLease(%q) reported unleased on error", s)
		}
		if !l.IsOpaque() {
			rt.Fatalf("DecodeLease(%q) did not report opaque on error", s)
		}
		got, ok := l.Record()
		if ok || got != (Record{}) {
			rt.Fatalf("DecodeLease(%q) error result = (%+v, %v), want the zero Record and false", s, got, ok)
		}
	}
	t.Run("arbitrary", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			check(rt, rapid.String().Draw(rt, "input"))
		})
	})
	t.Run("near-miss", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			wire, err := Encode(drawRecord(rt))
			if err != nil {
				rt.Fatalf("Encode: %v", err)
			}
			check(rt, mutateWire(rt, wire))
		})
	})
}
