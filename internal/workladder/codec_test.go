package workladder

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC)

func key(b string) ClaimKey { return ClaimKey{Store: "rig:gascity", Bead: b} }

// The §6.1 examples are the wire format the agent (W1) and the controller
// (W3) share; they must decode as written.
func TestDecodeDesignExamples(t *testing.T) {
	s, err := DecodeStarted(`{"v":1,"claims":[
  {"store":"rig:gascity","bead":"ga-12ab","gen":7,"at":"2026-10-17T20:00:00Z","via":"claim"}
]}`)
	if err != nil {
		t.Fatalf("DecodeStarted: %v", err)
	}
	want := Started{V: 1, Claims: []StartedClaim{{Key: key("ga-12ab"), Gen: 7, At: t0, Via: ViaClaim}}}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("DecodeStarted = %+v, want %+v", s, want)
	}
	if !s.Names(key("ga-12ab")) || s.Names(key("ga-99zz")) || s.Names(ClaimKey{Store: "rig:other", Bead: "ga-12ab"}) {
		t.Fatalf("Names wrong for %+v", s)
	}

	r, err := DecodeLadder(`{"v":1,"gen":7,
 "entries":[
  {"store":"rig:gascity","bead":"ga-12ab","kind":"claim","rung":"started",
   "since":"2026-10-17T20:00:00Z","attempts":2,"paused":0,"last":"2026-10-17T20:04:30Z","next":"2026-10-17T20:10:30Z",
   "recycles":0,"escalated":false,"seed":"agent","recycle_at":""}],
 "release":{"store":"rig:gascity","bead":"ga-99zz","gen":7,"at":"2026-10-17T20:01:00Z"}
}`)
	if err != nil {
		t.Fatalf("DecodeLadder: %v", err)
	}
	wantR := Record{V: 1, Gen: 7, Entries: []Entry{{
		Key: key("ga-12ab"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 2,
		Last: t0.Add(4*time.Minute + 30*time.Second), Next: t0.Add(10*time.Minute + 30*time.Second), Seed: SrcAgent,
	}}, Release: &ReleaseMark{Key: key("ga-99zz"), Gen: 7, At: t0.Add(time.Minute)}}
	if !reflect.DeepEqual(r, wantR) {
		t.Fatalf("DecodeLadder = %+v, want %+v", r, wantR)
	}
}

func TestLadderRoundTrip(t *testing.T) {
	r := Record{V: 1, Gen: 9, Entries: []Entry{
		{Key: key("b"), Kind: KindTrigger, Rung: RungAssigned, Since: t0, Attempts: 3, Paused: 2, Last: t0.Add(time.Minute), Next: t0.Add(time.Hour), RecycleAt: t0.Add(2 * time.Minute)},
		{Key: key("a"), Kind: KindContinuation, Rung: RungAssigned, Since: t0, Escalated: true},
		{Key: ClaimKey{Store: "", Bead: "c"}, Kind: KindClaim, Rung: RungEscalated, Since: t0, Recycles: 1, Seed: SrcCurrentlyProcessing},
		{Key: key("d"), Kind: KindClaim, Rung: RungClaimed, Since: t0.Add(123456789 * time.Nanosecond), Unreleased: true, Refused: 2},
		{Key: key("e"), Kind: KindClaim, Rung: RungStarted, Since: t0, Seed: SrcClaimSession},
	}, Release: &ReleaseMark{Key: key("d"), Gen: 9, At: t0}, LastNudge: t0.Add(time.Second), RecycledGen: 8}
	got, err := DecodeLadder(EncodeLadder(r))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	want := r
	want.Entries = sortedEntries(r.Entries)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}
	if EncodeLadder(got) != EncodeLadder(r) {
		t.Fatal("encoding is not canonical in entry order")
	}
	for _, src := range []StartSource{SrcAgent, SrcExecutionClaimStarted, SrcCurrentClaim, SrcCurrentlyProcessing, SrcClaimSession} {
		r := Record{V: 1, Entries: []Entry{{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Seed: src}}}
		got, err := DecodeLadder(EncodeLadder(r))
		if err != nil || got.Entries[0].Seed != src {
			t.Fatalf("seed %v: got %+v, %v", src, got, err)
		}
	}
}

// Names reads a claim the hook recorded without a store as naming its bead
// in every store, as the writer does; an entry with no bead names nothing.
func TestStartedNames(t *testing.T) {
	s, err := DecodeStarted(`{"v":1,"claims":[{"store":"","bead":"ga-1","gen":1,"at":"2026-10-17T20:00:00Z","via":"claim"},{"store":"rig:a","bead":"ga-2","gen":1,"at":"2026-10-17T20:00:00Z","via":"adopt"},{"store":"rig:a","bead":"","gen":1,"at":"2026-10-17T20:00:00Z","via":"claim"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		key  ClaimKey
		want bool
	}{
		{ClaimKey{Store: "rig:x", Bead: "ga-1"}, true},
		{ClaimKey{Store: "city", Bead: "ga-1"}, true},
		{ClaimKey{Store: "rig:a", Bead: "ga-2"}, true},
		{ClaimKey{Store: "", Bead: "ga-2"}, true},
		{ClaimKey{Store: "rig:b", Bead: "ga-2"}, false},
		{ClaimKey{Store: "rig:a", Bead: ""}, false},
		{ClaimKey{Store: "rig:a", Bead: "ga-3"}, false},
	}
	for _, tc := range cases {
		if got := s.Names(tc.key); got != tc.want {
			t.Errorf("Names(%+v) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

func TestStartedRoundTrip(t *testing.T) {
	s := Started{V: 1, Claims: []StartedClaim{
		{Key: key("n"), Gen: 8, At: t0.Add(time.Second), Via: ViaAdopt},
		{Key: key("o"), Gen: 7, At: t0, Via: ViaClaim},
	}}
	got, err := DecodeStarted(EncodeStarted(s))
	if err != nil || !reflect.DeepEqual(got, s) {
		t.Fatalf("round trip = %+v, %v; want %+v", got, err, s)
	}
}

func TestDecodeAbsent(t *testing.T) {
	for _, raw := range []string{"", "  \n"} {
		r, err := DecodeLadder(raw)
		if err != nil || r.V != 0 {
			t.Fatalf("DecodeLadder(%q) = %+v, %v; want absent", raw, r, err)
		}
		s, err := DecodeStarted(raw)
		if err != nil || len(s.Claims) != 0 {
			t.Fatalf("DecodeStarted(%q) = %+v, %v; want empty", raw, s, err)
		}
	}
	if EncodeLadder(Record{}) != "" {
		t.Fatal("absent record must encode empty")
	}
}

func TestDecodeIssues(t *testing.T) {
	entry := func(f string) string {
		return `{"v":1,"gen":1,"entries":[{"store":"s","bead":"b","kind":"claim","rung":"started","since":"","attempts":0,"paused":0,"last":"","next":"","recycles":0,"escalated":false,"seed":"","recycle_at":""` + f + `}]}`
	}
	ladder := map[string]string{
		"parse":         `{"v":1,`,
		"not object":    `[]`,
		"version 0":     `{"gen":1,"entries":[]}`,
		"version 2":     `{"v":2,"gen":1,"entries":[]}`,
		"unknown kind":  entry(`,"kind":"poke"`),
		"unknown rung":  entry(`,"rung":"done"`),
		"unknown seed":  entry(`,"seed":"guess"`),
		"bad time":      entry(`,"since":"yesterday"`),
		"negative":      entry(`,"attempts":-1`),
		"empty bead":    entry(`,"bead":""`),
		"release bead":  `{"v":1,"gen":1,"entries":[],"release":{"store":"s","bead":"","gen":1,"at":""}}`,
		"release time":  `{"v":1,"gen":1,"entries":[],"release":{"store":"s","bead":"b","gen":1,"at":"x"}}`,
		"duplicate key": `{"v":1,"gen":1,"entries":[{"store":"s","bead":"b","kind":"claim","rung":"claimed"},{"store":"s","bead":"b","kind":"claim","rung":"claimed"}]}`,
	}
	for name, raw := range ladder {
		r, err := DecodeLadder(raw)
		var issue *DecodeIssue
		if !errors.As(err, &issue) || issue.Cell != "work_ladder" || r.V != 0 {
			t.Errorf("ladder %s: got %+v, %v; want a work_ladder DecodeIssue and an absent record", name, r, err)
		}
	}
	started := map[string]string{
		"parse":     `{"v":1,"claims":[`,
		"version 0": `{"claims":[]}`,
		"version 2": `{"v":2,"claims":[]}`,
		"bad time":  `{"v":1,"claims":[{"store":"s","bead":"b","gen":1,"at":"x","via":"claim"}]}`,
		"no time":   `{"v":1,"claims":[{"store":"s","bead":"b","gen":1,"at":"","via":"claim"}]}`,
		"no at":     `{"v":1,"claims":[{"store":"s","bead":"b","gen":1,"via":"claim"}]}`,
	}
	for name, raw := range started {
		_, err := DecodeStarted(raw)
		var issue *DecodeIssue
		if !errors.As(err, &issue) || issue.Cell != "work_started" {
			t.Errorf("started %s: got %v; want a work_started DecodeIssue", name, err)
		}
	}
}
