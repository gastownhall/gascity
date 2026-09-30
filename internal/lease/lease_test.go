package lease

import (
	"math"
	"strings"
	"testing"
	"time"
)

func mustHolder(t *testing.T, agent, session, token string) Holder {
	t.Helper()
	h, err := NewHolder(agent, session, token)
	if err != nil {
		t.Fatalf("NewHolder(%q, %q, %q): %v", agent, session, token, err)
	}
	return h
}

func TestRoundTripHeld(t *testing.T) {
	expires := time.Date(2026, 8, 11, 4, 5, 6, 123456789, time.UTC)
	rec := Record{
		Epoch:     7,
		Holder:    mustHolder(t, "worker-a", "seat-3", "inst-01"),
		ExpiresAt: expires,
		State:     StateHeld,
		Attempts:  2,
	}
	encoded, err := Encode(rec)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := "v1|7|worker-a@seat-3#inst-01|2026-08-11T04:05:06.123456789Z|held|2"
	if encoded != want {
		t.Fatalf("Encode = %q, want %q", encoded, want)
	}
	got, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode(%q): %v", encoded, err)
	}
	if got != rec {
		t.Fatalf("round trip = %+v, want %+v", got, rec)
	}
	if !got.ExpiresAt.Equal(expires) {
		t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, expires)
	}
}

func TestRoundTripHeldNormalizesToUTC(t *testing.T) {
	zone := time.FixedZone("test-offset", 5*3600)
	rec := Record{
		Epoch:     1,
		Holder:    mustHolder(t, "worker-a", "seat-1", "inst-01"),
		ExpiresAt: time.Date(2026, 8, 11, 9, 0, 0, 0, zone),
		State:     StateHeld,
	}
	encoded, err := Encode(rec)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(encoded, "2026-08-11T04:00:00Z") {
		t.Fatalf("Encode = %q, want a UTC timestamp", encoded)
	}
	got, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode(%q): %v", encoded, err)
	}
	if !got.ExpiresAt.Equal(rec.ExpiresAt) {
		t.Fatalf("ExpiresAt = %v, want the same instant as %v", got.ExpiresAt, rec.ExpiresAt)
	}
}

func TestRoundTripParkedAndDead(t *testing.T) {
	for _, tc := range []struct {
		state State
		want  string
	}{
		{StateParked, "v1|4|worker-b@seat-9#inst-77|-|parked|3"},
		{StateDead, "v1|4|worker-b@seat-9#inst-77|-|dead|3"},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			rec := Record{
				Epoch:    4,
				Holder:   mustHolder(t, "worker-b", "seat-9", "inst-77"),
				State:    tc.state,
				Attempts: 3,
			}
			encoded, err := Encode(rec)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if encoded != tc.want {
				t.Fatalf("Encode = %q, want %q", encoded, tc.want)
			}
			got, err := Decode(encoded)
			if err != nil {
				t.Fatalf("Decode(%q): %v", encoded, err)
			}
			if got != rec {
				t.Fatalf("round trip = %+v, want %+v", got, rec)
			}
			if !got.ExpiresAt.IsZero() {
				t.Fatalf("ExpiresAt = %v, want the zero time for %s", got.ExpiresAt, tc.state)
			}
		})
	}
}

func TestDecodeMalformed(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"empty string", ""},
		{"too few fields", "v1|1|worker-a@seat-1#inst-01|-|parked"},
		{"too many fields", "v1|1|worker-a@seat-1#inst-01|-|parked|1|extra"},
		{"trailing separator", "v1|1|worker-a@seat-1#inst-01|-|parked|1|"},
		{"wrong version tag", "v2|1|worker-a@seat-1#inst-01|-|parked|1"},
		{"empty version tag", "|1|worker-a@seat-1#inst-01|-|parked|1"},
		{"unknown state", "v1|1|worker-a@seat-1#inst-01|-|zombie|1"},
		{"empty state", "v1|1|worker-a@seat-1#inst-01|-||1"},
		{"uppercase state", "v1|1|worker-a@seat-1#inst-01|-|PARKED|1"},
		{"time on parked", "v1|1|worker-a@seat-1#inst-01|2026-08-11T04:05:06Z|parked|1"},
		{"time on dead", "v1|1|worker-a@seat-1#inst-01|2026-08-11T04:05:06Z|dead|1"},
		{"dash on held", "v1|1|worker-a@seat-1#inst-01|-|held|1"},
		{"empty expiry on held", "v1|1|worker-a@seat-1#inst-01||held|1"},
		{"empty expiry on parked", "v1|1|worker-a@seat-1#inst-01||parked|1"},
		{"unparseable expiry on held", "v1|1|worker-a@seat-1#inst-01|tomorrow|held|1"},
		{"non-RFC3339 expiry on held", "v1|1|worker-a@seat-1#inst-01|2026-08-11 04:05:06|held|1"},
		{"zero expiry on held", "v1|1|worker-a@seat-1#inst-01|0001-01-01T00:00:00Z|held|1"},
		{"non-numeric epoch", "v1|abc|worker-a@seat-1#inst-01|-|parked|1"},
		{"empty epoch", "v1||worker-a@seat-1#inst-01|-|parked|1"},
		{"negative epoch", "v1|-3|worker-a@seat-1#inst-01|-|parked|1"},
		{"negative one epoch", "v1|-1|worker-a@seat-1#inst-01|-|parked|1"},
		{"epoch past the signed maximum", "v1|9223372036854775808|worker-a@seat-1#inst-01|-|parked|1"},
		{"epoch past the unsigned maximum", "v1|18446744073709551616|worker-a@seat-1#inst-01|-|parked|1"},
		{"epoch with plus sign", "v1|+3|worker-a@seat-1#inst-01|-|parked|1"},
		{"epoch with whitespace", "v1| 3|worker-a@seat-1#inst-01|-|parked|1"},
		{"epoch with leading zero", "v1|00|worker-a@seat-1#inst-01|-|parked|0"},
		{"epoch with leading zeroes", "v1|007|worker-a@seat-1#inst-01|-|parked|0"},
		{"negative zero epoch", "v1|-0|worker-a@seat-1#inst-01|-|parked|0"},
		{"attempts with leading zero", "v1|1|worker-a@seat-1#inst-01|-|parked|00"},
		{"attempts with leading zeroes", "v1|1|worker-a@seat-1#inst-01|-|parked|007"},
		{"negative zero attempts", "v1|1|worker-a@seat-1#inst-01|-|parked|-0"},
		{"attempts with plus sign", "v1|1|worker-a@seat-1#inst-01|-|parked|+3"},
		{"hex epoch", "v1|0x10|worker-a@seat-1#inst-01|-|parked|1"},
		{"non-numeric attempts", "v1|1|worker-a@seat-1#inst-01|-|parked|many"},
		{"empty attempts", "v1|1|worker-a@seat-1#inst-01|-|parked|"},
		{"negative attempts", "v1|1|worker-a@seat-1#inst-01|-|parked|-1"},
		{"negative three attempts", "v1|1|worker-a@seat-1#inst-01|-|parked|-3"},
		{"attempts past the signed maximum", "v1|1|worker-a@seat-1#inst-01|-|parked|9223372036854775808"},
		{"attempts past the unsigned maximum", "v1|1|worker-a@seat-1#inst-01|-|parked|18446744073709551616"},
		{"comma fraction on held", "v1|1|worker-a@seat-1#inst-01|2026-08-11T04:05:06,5Z|held|0"},
		{"sub-nanosecond fraction on held", "v1|1|worker-a@seat-1#inst-01|2026-08-11T04:05:06.1234567899Z|held|0"},
		{"empty holder", "v1|1||-|parked|1"},
		{"holder without instance token", "v1|1|worker-a@seat-1|-|parked|1"},
		{"holder without session", "v1|1|worker-a#inst-01|-|parked|1"},
		{"holder with empty agent", "v1|1|@seat-1#inst-01|-|parked|1"},
		{"holder with empty session", "v1|1|worker-a@#inst-01|-|parked|1"},
		{"holder with empty instance token", "v1|1|worker-a@seat-1#|-|parked|1"},
		{"holder with extra at sign", "v1|1|worker-a@seat@1#inst-01|-|parked|1"},
		{"holder with extra hash", "v1|1|worker-a@seat-1#inst#01|-|parked|1"},
		{"holder separators swapped", "v1|1|worker-a#seat-1@inst-01|-|parked|1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decode(tc.raw)
			if err == nil {
				t.Fatalf("Decode(%q) = %+v, want an error", tc.raw, got)
			}
			if !strings.Contains(err.Error(), "decoding lease") {
				t.Fatalf("Decode(%q) error = %v, want it to name the operation", tc.raw, err)
			}
			if got != (Record{}) {
				t.Fatalf("Decode(%q) = %+v, want the zero Record alongside the error", tc.raw, got)
			}
		})
	}
}

func TestDecodedFormsAreCanonical(t *testing.T) {
	for _, raw := range []string{
		"v1|0|worker-a@seat-1#inst-01|-|parked|0",
		"v1|1|worker-a@seat-1#inst-01|-|parked|1",
		"v1|7|worker-a@seat-1#inst-01|-|dead|3",
		"v1|9223372036854775807|worker-a@seat-1#inst-01|-|parked|9223372036854775807",
		"v1|2|worker-a@seat-1#inst-01|2026-08-11T04:05:06Z|held|1",
		"v1|2|worker-a@seat-1#inst-01|2026-08-11T04:05:06.5Z|held|1",
	} {
		t.Run(raw, func(t *testing.T) {
			rec, err := Decode(raw)
			if err != nil {
				t.Fatalf("Decode(%q): %v", raw, err)
			}
			got, err := Encode(rec)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if got != raw {
				t.Fatalf("Encode(Decode(%q)) = %q, want the input unchanged", raw, got)
			}
		})
	}
}

func TestCountOverflowBoundary(t *testing.T) {
	maxRec := Record{
		Epoch:    math.MaxInt64,
		Holder:   mustHolder(t, "worker-a", "seat-1", "inst-01"),
		State:    StateParked,
		Attempts: math.MaxInt64,
	}
	encoded, err := Encode(maxRec)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if want := "v1|9223372036854775807|worker-a@seat-1#inst-01|-|parked|9223372036854775807"; encoded != want {
		t.Fatalf("Encode = %q, want %q", encoded, want)
	}
	got, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode(%q): %v", encoded, err)
	}
	if got != maxRec {
		t.Fatalf("round trip = %+v, want %+v", got, maxRec)
	}
	if _, err := Decode("v1|9223372036854775808|worker-a@seat-1#inst-01|-|parked|1"); err == nil {
		t.Fatal("Decode of an epoch one past math.MaxInt64 succeeded, want an error")
	}
	if _, err := Decode("v1|1|worker-a@seat-1#inst-01|-|parked|9223372036854775808"); err == nil {
		t.Fatal("Decode of an attempt count one past math.MaxInt64 succeeded, want an error")
	}
}

func TestEpochZeroIsLegal(t *testing.T) {
	const raw = "v1|0|worker-a@seat-1#inst-01|-|parked|0"
	got, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode(%q): %v", raw, err)
	}
	want := Record{
		Epoch:  0,
		Holder: mustHolder(t, "worker-a", "seat-1", "inst-01"),
		State:  StateParked,
	}
	if got != want {
		t.Fatalf("Decode(%q) = %+v, want %+v", raw, got, want)
	}
	encoded, err := Encode(got)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if encoded != raw {
		t.Fatalf("Encode = %q, want %q", encoded, raw)
	}

	positive := Record{
		Epoch:    1,
		Holder:   mustHolder(t, "worker-a", "seat-1", "inst-01"),
		State:    StateParked,
		Attempts: 0,
	}
	encodedPositive, err := Encode(positive)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if want := "v1|1|worker-a@seat-1#inst-01|-|parked|0"; encodedPositive != want {
		t.Fatalf("Encode = %q, want %q", encodedPositive, want)
	}
	decodedPositive, err := Decode(encodedPositive)
	if err != nil {
		t.Fatalf("Decode(%q): %v", encodedPositive, err)
	}
	if decodedPositive != positive {
		t.Fatalf("round trip = %+v, want %+v", decodedPositive, positive)
	}
}

func TestEncodeRejectsNegativeCounts(t *testing.T) {
	holder := mustHolder(t, "worker-a", "seat-1", "inst-01")
	for _, tc := range []struct {
		name string
		rec  Record
	}{
		{"negative epoch", Record{Epoch: -1, Holder: holder, State: StateParked}},
		{"negative attempts", Record{Epoch: 1, Holder: holder, State: StateParked, Attempts: -1}},
		{"most negative epoch", Record{Epoch: math.MinInt64, Holder: holder, State: StateParked}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Encode(tc.rec)
			if err == nil {
				t.Fatalf("Encode(%+v) = %q, want an error", tc.rec, got)
			}
			if got != "" {
				t.Fatalf("Encode(%+v) = %q, want the empty string alongside the error", tc.rec, got)
			}
		})
	}
}

func TestRoundTripStripsMonotonicClock(t *testing.T) {
	now := time.Now()
	if now.String() == now.Round(0).String() {
		t.Skip("time.Now did not carry a monotonic reading on this platform")
	}
	rec := Record{
		Epoch:     1,
		Holder:    mustHolder(t, "worker-a", "seat-1", "inst-01"),
		ExpiresAt: now,
		State:     StateHeld,
	}
	encoded, err := Encode(rec)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode(%q): %v", encoded, err)
	}
	if !got.ExpiresAt.Equal(now) {
		t.Fatalf("ExpiresAt = %v, want the same instant as %v", got.ExpiresAt, now)
	}
}

func TestEncodeRejectsInvalidRecords(t *testing.T) {
	holder := mustHolder(t, "worker-a", "seat-1", "inst-01")
	stamp := time.Date(2026, 8, 11, 4, 5, 6, 0, time.UTC)
	for _, tc := range []struct {
		name string
		rec  Record
	}{
		{"unknown state", Record{Epoch: 1, Holder: holder, State: State("zombie")}},
		{"empty state", Record{Epoch: 1, Holder: holder}},
		{"zero holder", Record{Epoch: 1, State: StateParked}},
		{"held without expiry", Record{Epoch: 1, Holder: holder, State: StateHeld}},
		{"parked with expiry", Record{Epoch: 1, Holder: holder, ExpiresAt: stamp, State: StateParked}},
		{"dead with expiry", Record{Epoch: 1, Holder: holder, ExpiresAt: stamp, State: StateDead}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Encode(tc.rec)
			if err == nil {
				t.Fatalf("Encode(%+v) = %q, want an error", tc.rec, got)
			}
			if got != "" {
				t.Fatalf("Encode(%+v) = %q, want the empty string alongside the error", tc.rec, got)
			}
		})
	}
}

func TestEncodeChecksRFC3339YearRange(t *testing.T) {
	holder := mustHolder(t, "worker-a", "seat-1", "inst-01")
	for _, tc := range []struct {
		name    string
		expires time.Time
		wantErr bool
	}{
		{"year below range", time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{"year zero", time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"year above range", time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := Record{Epoch: 1, Holder: holder, ExpiresAt: tc.expires, State: StateHeld}
			wire, err := Encode(rec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Encode(%v) = %q, want an error", tc.expires, wire)
				}
				return
			}
			if err != nil {
				t.Fatalf("Encode(%v): %v", tc.expires, err)
			}
			got, err := Decode(wire)
			if err != nil {
				t.Fatalf("Decode(%q): %v", wire, err)
			}
			if !got.ExpiresAt.Equal(tc.expires) {
				t.Fatalf("round trip expiry = %v, want %v", got.ExpiresAt, tc.expires)
			}
		})
	}
}

func TestNewHolderRejectsMalformedParts(t *testing.T) {
	for _, tc := range []struct{ name, agent, session, token string }{
		{"empty agent", "", "seat-1", "inst-01"},
		{"empty session", "worker-a", "", "inst-01"},
		{"empty token", "worker-a", "seat-1", ""},
		{"agent carries the field separator", "work|er", "seat-1", "inst-01"},
		{"agent carries the session separator", "work@er", "seat-1", "inst-01"},
		{"agent carries the instance separator", "work#er", "seat-1", "inst-01"},
		{"session carries the field separator", "worker-a", "seat|1", "inst-01"},
		{"session carries the session separator", "worker-a", "seat@1", "inst-01"},
		{"session carries the instance separator", "worker-a", "seat#1", "inst-01"},
		{"token carries the field separator", "worker-a", "seat-1", "inst|01"},
		{"token carries the session separator", "worker-a", "seat-1", "inst@01"},
		{"token carries the instance separator", "worker-a", "seat-1", "inst#01"},
		{"agent carries a newline", "work\ner", "seat-1", "inst-01"},
		{"session carries a control character", "worker-a", "seat\x001", "inst-01"},
		{"token carries a control character", "worker-a", "seat-1", "inst\u008501"},
		{"agent is invalid UTF-8", string([]byte{'w', 0xff}), "seat-1", "inst-01"},
		{"session is invalid UTF-8", "worker-a", string([]byte{'s', 0xff}), "inst-01"},
		{"token is invalid UTF-8", "worker-a", "seat-1", string([]byte{'i', 0xff})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := NewHolder(tc.agent, tc.session, tc.token)
			if err == nil {
				t.Fatalf("NewHolder(%q, %q, %q) = %+v, want an error", tc.agent, tc.session, tc.token, h)
			}
			if h != (Holder{}) {
				t.Fatalf("NewHolder = %+v, want the zero Holder alongside the error", h)
			}
		})
	}
}

func TestHolderInequalityOnInstanceTokenAlone(t *testing.T) {
	first := mustHolder(t, "worker-a", "seat-1", "inst-01")
	second := mustHolder(t, "worker-a", "seat-1", "inst-02")
	if first == second {
		t.Fatal("holders differing only in instance token compared equal")
	}
	if first.Agent != second.Agent || first.Session != second.Session {
		t.Fatal("fixtures must differ only in the instance token")
	}

	expires := time.Date(2026, 8, 11, 4, 5, 6, 0, time.UTC)
	encodeHeld := func(h Holder) Record {
		rec := Record{Epoch: 1, Holder: h, ExpiresAt: expires, State: StateHeld}
		encoded, err := Encode(rec)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		decoded, err := Decode(encoded)
		if err != nil {
			t.Fatalf("Decode(%q): %v", encoded, err)
		}
		return decoded
	}
	a, b := encodeHeld(first), encodeHeld(second)
	if a.Holder == b.Holder {
		t.Fatal("decoded holders differing only in instance token compared equal")
	}
	if a == b {
		t.Fatal("decoded records differing only in instance token compared equal")
	}
	if a.Holder.String() == b.Holder.String() {
		t.Fatal("holder strings differing only in instance token collided")
	}
}

func TestUnleasedIsDistinctFromEveryState(t *testing.T) {
	unleased := Unleased()
	if !unleased.IsUnleased() {
		t.Fatal("Unleased().IsUnleased() = false")
	}
	if rec, ok := unleased.Record(); ok {
		t.Fatalf("Unleased().Record() = %+v, true; want the absent form", rec)
	}
	for _, state := range []State{StateHeld, StateParked, StateDead} {
		rec := Record{Epoch: 1, Holder: mustHolder(t, "worker-a", "seat-1", "inst-01"), State: state}
		if state == StateHeld {
			rec.ExpiresAt = time.Date(2026, 8, 11, 4, 5, 6, 0, time.UTC)
		}
		leased, err := Present(rec)
		if err != nil {
			t.Fatalf("Present(%+v): %v", rec, err)
		}
		if leased.IsUnleased() {
			t.Fatalf("Present(%+v).IsUnleased() = true", rec)
		}
		got, ok := leased.Record()
		if !ok || got != rec {
			t.Fatalf("Present(%+v).Record() = %+v, %v", rec, got, ok)
		}
		if leased == unleased {
			t.Fatalf("a %s lease compared equal to the unleased condition", state)
		}
	}
}

func TestZeroLeaseIsUnleased(t *testing.T) {
	var zero Lease
	if !zero.IsUnleased() {
		t.Fatal("the zero Lease is not unleased")
	}
	if _, ok := zero.Record(); ok {
		t.Fatal("the zero Lease yielded a record")
	}
	if zero != Unleased() {
		t.Fatal("the zero Lease differs from Unleased()")
	}
}

func TestLookupDistinguishesAbsentFromEmpty(t *testing.T) {
	const key = "lease"
	held := "v1|1|worker-a@seat-1#inst-01|2026-08-11T04:05:06Z|held|0"

	absent, err := Lookup(map[string]string{"other": held}, key)
	if err != nil {
		t.Fatalf("Lookup on an absent key: %v", err)
	}
	if !absent.IsUnleased() {
		t.Fatal("an absent key did not decode to the unleased condition")
	}

	empty, err := Lookup(map[string]string{key: ""}, key)
	if err == nil {
		t.Fatalf("Lookup on an empty value = %+v, want an error", empty)
	}
	if empty.IsUnleased() {
		t.Fatal("a failed Lookup reported the unleased condition, which lets an ignored error become a double claim")
	}

	nilMap, err := Lookup(nil, key)
	if err != nil {
		t.Fatalf("Lookup on a nil map: %v", err)
	}
	if !nilMap.IsUnleased() {
		t.Fatal("a nil map did not decode to the unleased condition")
	}

	present, err := Lookup(map[string]string{key: held}, key)
	if err != nil {
		t.Fatalf("Lookup on a present key: %v", err)
	}
	rec, ok := present.Record()
	if !ok {
		t.Fatal("a present, well-formed value did not yield a record")
	}
	if rec.State != StateHeld {
		t.Fatalf("State = %q, want %q", rec.State, StateHeld)
	}
}

func TestLookupCarriesEveryFieldThrough(t *testing.T) {
	const key = "lease"
	raw := "v1|7|worker-a@seat-1#inst-01|2026-08-11T04:05:06.5Z|held|3"

	l, err := Lookup(map[string]string{key: raw}, key)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	rec, ok := l.Record()
	if !ok {
		t.Fatal("a well-formed value did not yield a record")
	}

	if rec.Epoch != 7 {
		t.Errorf("Epoch = %d, want 7", rec.Epoch)
	}
	if rec.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", rec.Attempts)
	}
	if rec.State != StateHeld {
		t.Errorf("State = %q, want %q", rec.State, StateHeld)
	}
	want := mustHolder(t, "worker-a", "seat-1", "inst-01")
	if rec.Holder != want {
		t.Errorf("Holder = %+v, want %+v", rec.Holder, want)
	}
	if rec.Holder.InstanceToken != "inst-01" {
		t.Errorf("InstanceToken = %q, want %q", rec.Holder.InstanceToken, "inst-01")
	}
	wantExpiry := time.Date(2026, 8, 11, 4, 5, 6, 500000000, time.UTC)
	if !rec.ExpiresAt.Equal(wantExpiry) {
		t.Errorf("ExpiresAt = %v, want %v", rec.ExpiresAt, wantExpiry)
	}

	got, err := Encode(rec)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if got != raw {
		t.Errorf("Encode() = %q, want %q", got, raw)
	}
}

func TestFailedDecodeIsNotUnleased(t *testing.T) {
	bad, err := DecodeLease("v1|nope|worker-a@seat-1#inst-01|-|parked|0", true)
	if err == nil {
		t.Fatal("DecodeLease on a malformed record returned no error")
	}
	if bad.IsUnleased() {
		t.Error("DecodeLease error result reported unleased")
	}
	if !bad.IsOpaque() {
		t.Error("DecodeLease error result did not report opaque")
	}

	invalid, err := Present(Record{State: StateHeld})
	if err == nil {
		t.Fatal("Present on an invalid record returned no error")
	}
	if invalid.IsUnleased() {
		t.Error("Present error result reported unleased")
	}
	if !invalid.IsOpaque() {
		t.Error("Present error result did not report opaque")
	}

	rec, ok := invalid.Record()
	if ok {
		t.Fatal("the fail-closed lease reported a record")
	}
	if rec != (Record{}) {
		t.Errorf("the fail-closed lease carried %+v, want the zero Record", rec)
	}
	if rec.Holder == mustHolder(t, "worker-a", "seat-1", "inst-01") {
		t.Error("the fail-closed lease compared equal to a real holder")
	}
	badRec, ok := bad.Record()
	if ok || badRec != (Record{}) {
		t.Errorf("DecodeLease error result = (%+v, %v), want the zero Record and false", badRec, ok)
	}
}

func TestDecodeLeaseCarriesPresence(t *testing.T) {
	got, err := DecodeLease("", false)
	if err != nil {
		t.Fatalf("DecodeLease(absent): %v", err)
	}
	if !got.IsUnleased() {
		t.Fatal("an absent value did not decode to the unleased condition")
	}

	if _, err := DecodeLease("", true); err == nil {
		t.Fatal("DecodeLease of a present empty string succeeded, want an error")
	}
}

func TestStateStringsAreStable(t *testing.T) {
	for state, want := range map[State]string{
		StateHeld:   "held",
		StateParked: "parked",
		StateDead:   "dead",
	} {
		if string(state) != want {
			t.Fatalf("state %v renders as %q, want %q", state, string(state), want)
		}
	}
}

func TestCanonicalMakesRoundTripComparable(t *testing.T) {
	h := mustHolder(t, "worker-a", "seat-1", "inst-01")

	for _, tc := range []struct {
		name    string
		expires time.Time
	}{
		{"non-UTC zone", time.Date(2026, 8, 11, 6, 5, 6, 0, time.FixedZone("plus2", 2*3600))},
		{"monotonic reading", time.Now().Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			built := Record{Epoch: 1, Holder: h, ExpiresAt: tc.expires, State: StateHeld}

			wire, err := Encode(built)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			decoded, err := Decode(wire)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}

			reWire, err := Encode(decoded)
			if err != nil {
				t.Fatalf("Encode(decoded): %v", err)
			}
			if reWire != wire {
				t.Fatalf("re-encode = %q, want %q", reWire, wire)
			}

			if got := built.Canonical(); got != decoded {
				t.Errorf("built.Canonical() = %+v, want %+v", got, decoded)
			}
			if got := decoded.Canonical(); got != decoded {
				t.Errorf("Canonical is not idempotent on a decoded record: %+v", got)
			}
		})
	}
}

func TestCanonicalLeavesZeroExpiryAlone(t *testing.T) {
	h := mustHolder(t, "worker-a", "seat-1", "inst-01")
	for _, state := range []State{StateParked, StateDead} {
		rec := Record{Epoch: 1, Holder: h, State: state}
		got := rec.Canonical()
		if !got.ExpiresAt.IsZero() {
			t.Errorf("state %q: Canonical set an expiry %v", state, got.ExpiresAt)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("state %q: canonical record no longer validates: %v", state, err)
		}
	}
}

func TestCanonicalNormalizesZeroExpiryRepresentation(t *testing.T) {
	h := mustHolder(t, "worker-a", "seat-1", "inst-01")
	expires := time.Date(1, 1, 1, 1, 0, 0, 0, time.FixedZone("plus1", 3600))
	rec := Record{Epoch: 1, Holder: h, ExpiresAt: expires, State: StateParked}

	if !rec.ExpiresAt.IsZero() || rec.ExpiresAt.Location() == time.UTC {
		t.Fatalf("test expiry = %#v, want a noncanonical representation of the zero instant", rec.ExpiresAt)
	}
	wire, err := Encode(rec)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := Decode(wire)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := rec.Canonical(); got != decoded {
		t.Fatalf("Canonical() = %+v, want decoded record %+v", got, decoded)
	}
}
