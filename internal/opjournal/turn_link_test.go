package opjournal

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// linkAt is a delivery time whose sub-millisecond digits are over half a
// millisecond, so truncating it to the millisecond and rounding it differ:
// the tests see truncation at work.
var linkAt = time.Date(2026, 10, 14, 12, 0, 0, 123_756_789, time.UTC)

const linkGen int64 = 7

// stampedNow is a deliverer's instant in a non-UTC zone, its sub-millisecond
// digits over half a millisecond like linkAt's; stampedMillis is the same
// instant as a runtime stamps it on the message: in UTC, truncated to the
// millisecond.
var (
	stampedNow    = time.Date(2026, 10, 14, 14, 0, 0, 160_745_678, time.FixedZone("UTC+2", 2*60*60))
	stampedMillis = stampedNow.UTC().Truncate(time.Millisecond)
)

func linkRec(mode LinkMode) DeliveryRecord {
	return DeliveryRecord{
		Seq: 42, At: linkAt, Generation: linkGen, Epoch: 1,
		Mode: mode, Ack: AckDelivered,
	}
}

func seen(id string, startedAt time.Time) SeenTurn {
	return SeenTurn{ID: id, Generation: linkGen, StartedAt: startedAt}
}

// sourced is evidence from a readable, same-clock source whose first read
// succeeded and whose probe saw no live turn.
func sourced(turns ...SeenTurn) TurnEvidence {
	return TurnEvidence{
		HasSource:   true,
		SameClock:   true,
		LiveAtProbe: TriNo,
		Known:       []string{"old-1", "old-2"},
		Seen:        turns,
		FirstRead:   FirstTurnRead{Done: true},
	}
}

func TestTriZeroValueIsUnknown(t *testing.T) {
	var tri Tri
	if tri != TriUnknown {
		t.Fatalf("zero Tri = %d, want TriUnknown", tri)
	}
	if TriNo == TriUnknown || TriYes == TriUnknown || TriNo == TriYes {
		t.Fatalf("Tri values collide: unknown=%d no=%d yes=%d", TriUnknown, TriNo, TriYes)
	}
}

func TestLinkTokens(t *testing.T) {
	tokens := map[string]string{
		string(LinkStarts): "starts", string(LinkJoins): "joins",
		string(AckDelivered): "delivered", string(AckDeliveredUnobserved): "delivered_unobserved", string(AckInBand): "in_band",
		string(LinkExact): "exact", string(LinkCorrelated): "correlated", string(LinkAmbiguous): "ambiguous", string(LinkNone): "none",
		string(BasisRuntimeTurn): "runtime_turn", string(BasisHookPayload): "hook_payload", string(BasisNoSource): "no_source",
		string(BasisRequestedAt): "requested_at", string(BasisNoCandidate): "no_candidate", string(BasisJoinedLiveTurn): "joined_live_turn",
		string(BasisACPSerial): "acp_serial", string(BasisHistoryDiff): "history_diff", string(BasisCompetingTurn): "competing_turn",
		string(BasisReadFailed): "read_failed",
	}
	for got, want := range tokens {
		if got != want {
			t.Errorf("token %q, want %q", got, want)
		}
	}
}

func TestCorrelateTurn(t *testing.T) {
	ms := time.Millisecond
	cut := linkAt.Truncate(ms)
	readErr := errors.New("transcript torn at the tail")

	tests := []struct {
		name    string
		rec     func() DeliveryRecord
		ev      func() TurnEvidence
		want    TurnLink
		decided bool
	}{
		// Row 1.
		{
			name: "row 1: runtime receipt names the turn",
			rec:  func() DeliveryRecord { r := linkRec(LinkStarts); r.MessageID = "m-1"; r.TurnID = "run-1:1"; return r },
			ev:   func() TurnEvidence { return TurnEvidence{} },
			want: TurnLink{Turn: "run-1:1", Confidence: LinkExact, Basis: BasisRuntimeTurn}, decided: true,
		},
		{
			name: "row 1: in-band hook payload names the turn",
			rec:  func() DeliveryRecord { r := linkRec(LinkJoins); r.Ack = AckInBand; r.TurnID = "prompt-9"; return r },
			ev:   func() TurnEvidence { return TurnEvidence{} },
			want: TurnLink{Turn: "prompt-9", Confidence: LinkExact, Basis: BasisHookPayload}, decided: true,
		},
		{
			name: "row 1: receipt beats a conflicting history diff",
			rec:  func() DeliveryRecord { r := linkRec(LinkStarts); r.TurnID = "receipt-turn"; return r },
			ev:   func() TurnEvidence { return sourced(seen("diff-turn", cut)) },
			want: TurnLink{Turn: "receipt-turn", Confidence: LinkExact, Basis: BasisRuntimeTurn}, decided: true,
		},
		{
			name: "row 1: payload beats a conflicting history diff",
			rec:  func() DeliveryRecord { r := linkRec(LinkJoins); r.Ack = AckInBand; r.TurnID = "payload-turn"; return r },
			ev:   func() TurnEvidence { return sourced(seen("diff-turn", cut)) },
			want: TurnLink{Turn: "payload-turn", Confidence: LinkExact, Basis: BasisHookPayload}, decided: true,
		},
		// Row 2.
		{
			name: "row 2: no turn source",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev:   func() TurnEvidence { ev := sourced(seen("t1", cut)); ev.HasSource = false; return ev },
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoSource}, decided: true,
		},
		// Row 3.
		{
			name: "row 3: T3 V1 requestedAt equals the message createdAt",
			rec: func() DeliveryRecord {
				r := linkRec(LinkStarts)
				r.MessageID = "m-1"
				r.MessageAt = linkAt.Add(40 * ms)
				return r
			},
			ev: func() TurnEvidence {
				ev := sourced(seen("other", linkAt.Add(20*ms)), seen("mine", linkAt.Add(40*ms)))
				ev.SameClock = false
				return ev
			},
			want: TurnLink{Turn: "mine", Confidence: LinkCorrelated, Basis: BasisRequestedAt}, decided: true,
		},
		{
			// The deliverer holds the pre-format instant; the source reports
			// the millisecond stamp.
			name: "row 3: a MessageAt finer than the source's millisecond stamp still matches",
			rec:  func() DeliveryRecord { r := linkRec(LinkStarts); r.MessageAt = stampedNow; return r },
			ev: func() TurnEvidence {
				ev := sourced(seen("mine", stampedMillis))
				ev.SameClock = false
				return ev
			},
			want: TurnLink{Turn: "mine", Confidence: LinkCorrelated, Basis: BasisRequestedAt}, decided: true,
		},
		{
			name: "row 3: a start one millisecond off does not match",
			rec:  func() DeliveryRecord { r := linkRec(LinkStarts); r.MessageAt = stampedNow; return r },
			ev: func() TurnEvidence {
				ev := sourced(seen("other", stampedMillis.Add(ms)))
				ev.SameClock = false
				return ev
			},
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
		{
			name: "row 3: polling missed the turn gives none, never a history-diff guess",
			rec:  func() DeliveryRecord { r := linkRec(LinkStarts); r.MessageAt = linkAt.Add(40 * ms); return r },
			ev: func() TurnEvidence {
				ev := sourced(seen("later", linkAt.Add(900*ms)))
				ev.SameClock = false
				return ev
			},
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
		{
			name: "row 3: failed reads give read_failed",
			rec:  func() DeliveryRecord { r := linkRec(LinkStarts); r.MessageAt = linkAt.Add(40 * ms); return r },
			ev: func() TurnEvidence {
				ev := sourced()
				ev.SameClock = false
				ev.FirstRead = FirstTurnRead{}
				ev.ReadErr = readErr
				return ev
			},
			want: TurnLink{Confidence: LinkNone, Basis: BasisReadFailed}, decided: false,
		},
		{
			name: "row 3: a T3 V1 read shows the turn's start unknown, then its requestedAt",
			rec:  func() DeliveryRecord { r := linkRec(LinkStarts); r.MessageAt = linkAt.Add(40 * ms); return r },
			ev: func() TurnEvidence {
				ev := sourced(seen("mine", time.Time{}), seen("mine", linkAt.Add(40*ms)))
				ev.SameClock = false
				return ev
			},
			want: TurnLink{Turn: "mine", Confidence: LinkCorrelated, Basis: BasisRequestedAt}, decided: true,
		},
		{
			// The stamp cannot tell apart two turns starting within one
			// millisecond.
			name: "row 3: of two candidates in the message's millisecond, the first observed wins",
			rec:  func() DeliveryRecord { r := linkRec(LinkStarts); r.MessageAt = stampedNow; return r },
			ev: func() TurnEvidence {
				ev := sourced(seen("first", stampedMillis), seen("second", stampedMillis))
				ev.SameClock = false
				return ev
			},
			want: TurnLink{Turn: "first", Confidence: LinkCorrelated, Basis: BasisRequestedAt}, decided: true,
		},
		// Row 4.
		{
			name: "row 4: joins, live turn still current at the first read",
			rec:  func() DeliveryRecord { return linkRec(LinkJoins) },
			ev: func() TurnEvidence {
				ev := sourced(SeenTurn{ID: "live", Generation: linkGen, StartedAt: linkAt.Add(-time.Minute), Running: true})
				ev.LiveAtProbe, ev.LiveTurn = TriYes, "live"
				ev.Known = append(ev.Known, "live")
				ev.FirstRead = FirstTurnRead{Done: true, Current: "live"}
				return ev
			},
			want: TurnLink{Turn: "live", Confidence: LinkCorrelated, Basis: BasisJoinedLiveTurn}, decided: true,
		},
		{
			name: "row 7: joins whose live turn was replaced at the first read",
			rec:  func() DeliveryRecord { return linkRec(LinkJoins) },
			ev: func() TurnEvidence {
				ev := sourced(SeenTurn{ID: "next", Generation: linkGen, StartedAt: linkAt.Add(5 * ms), Running: true})
				ev.LiveAtProbe, ev.LiveTurn = TriYes, "live"
				ev.Known = append(ev.Known, "live")
				ev.FirstRead = FirstTurnRead{Done: true, Current: "next"}
				return ev
			},
			want: TurnLink{Turn: "next", Confidence: LinkAmbiguous, Basis: BasisCompetingTurn}, decided: true,
		},
		{
			name: "row 8: joins whose live turn ended with nothing after it",
			rec:  func() DeliveryRecord { return linkRec(LinkJoins) },
			ev: func() TurnEvidence {
				ev := sourced()
				ev.LiveAtProbe, ev.LiveTurn = TriYes, "live"
				ev.FirstRead = FirstTurnRead{Done: true}
				return ev
			},
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
		{
			name: "row 8: joins before the first read succeeded is undecided",
			rec:  func() DeliveryRecord { return linkRec(LinkJoins) },
			ev: func() TurnEvidence {
				ev := sourced()
				ev.LiveAtProbe, ev.LiveTurn = TriYes, "live"
				ev.FirstRead = FirstTurnRead{}
				return ev
			},
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
		{
			name: "row 8: joins whose probe saw a live turn without an id",
			rec:  func() DeliveryRecord { return linkRec(LinkJoins) },
			ev: func() TurnEvidence {
				ev := sourced()
				ev.LiveAtProbe = TriYes
				ev.FirstRead = FirstTurnRead{Done: true}
				return ev
			},
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
		{
			name: "row 3 before row 4: joins with a message stamp never falls back to the live turn",
			rec: func() DeliveryRecord {
				r := linkRec(LinkJoins)
				r.MessageAt = linkAt.Add(40 * time.Millisecond)
				return r
			},
			ev: func() TurnEvidence {
				ev := sourced()
				ev.LiveAtProbe, ev.LiveTurn = TriYes, "live"
				ev.Known = append(ev.Known, "live")
				ev.FirstRead = FirstTurnRead{Done: true, Current: "live"}
				return ev
			},
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
		// Rows 5 and 6.
		{
			name: "row 5: ACP's single new record is exact",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev:   func() TurnEvidence { ev := sourced(seen("acp-1", linkAt.Add(ms))); ev.Serial = true; return ev },
			want: TurnLink{Turn: "acp-1", Confidence: LinkExact, Basis: BasisACPSerial}, decided: true,
		},
		{
			name: "row 6: the same evidence without Serial is correlated",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev:   func() TurnEvidence { return sourced(seen("acp-1", linkAt.Add(ms))) },
			want: TurnLink{Turn: "acp-1", Confidence: LinkCorrelated, Basis: BasisHistoryDiff}, decided: true,
		},
		{
			name: "row 6: candidate at At truncated to the millisecond is included",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev:   func() TurnEvidence { return sourced(seen("t1", cut)) },
			want: TurnLink{Turn: "t1", Confidence: LinkCorrelated, Basis: BasisHistoryDiff}, decided: true,
		},
		{
			name: "row 6: a not-same-clock source keeps a turn stamped before At",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev:   func() TurnEvidence { ev := sourced(seen("t1", cut.Add(-time.Hour))); ev.SameClock = false; return ev },
			want: TurnLink{Turn: "t1", Confidence: LinkCorrelated, Basis: BasisHistoryDiff}, decided: true,
		},
		{
			name: "row 6: known turns and other generations are not candidates",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev: func() TurnEvidence {
				return sourced(
					seen("old-1", linkAt.Add(ms)),
					SeenTurn{ID: "prev-gen", Generation: linkGen - 1, StartedAt: linkAt.Add(2 * ms)},
					seen("t1", linkAt.Add(3*ms)),
				)
			},
			want: TurnLink{Turn: "t1", Confidence: LinkCorrelated, Basis: BasisHistoryDiff}, decided: true,
		},
		{
			name: "row 6: an earlier sighting of the same id in another generation is a different turn",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev: func() TurnEvidence {
				return sourced(
					SeenTurn{ID: "X", Generation: linkGen + 1, StartedAt: linkAt.Add(ms)},
					seen("X", linkAt.Add(ms)),
				)
			},
			want: TurnLink{Turn: "X", Confidence: LinkCorrelated, Basis: BasisHistoryDiff}, decided: true,
		},
		{
			name: "row 8: a later sighting of the same id in another generation does not lend its start",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev: func() TurnEvidence {
				return sourced(seen("X", time.Time{}), SeenTurn{ID: "X", Generation: linkGen + 1, StartedAt: linkAt.Add(ms)})
			},
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
		{
			name: "row 6: one turn repeated by several reads is one candidate",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev: func() TurnEvidence {
				running := seen("t1", linkAt.Add(ms))
				running.Running = true
				return sourced(running, seen("t1", linkAt.Add(ms)))
			},
			want: TurnLink{Turn: "t1", Confidence: LinkCorrelated, Basis: BasisHistoryDiff}, decided: true,
		},
		// Row 7.
		{
			// Row 4 needs LinkJoins: a starts delivery is never a join, even
			// when the probe's live turn is still current at the first read.
			name: "row 7: starts whose live turn is still current at the first read",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev: func() TurnEvidence {
				ev := sourced(seen("a", linkAt.Add(ms)))
				ev.LiveAtProbe, ev.LiveTurn = TriYes, "old-2"
				ev.FirstRead = FirstTurnRead{Done: true, Current: "old-2"}
				return ev
			},
			want: TurnLink{Turn: "a", Confidence: LinkAmbiguous, Basis: BasisCompetingTurn}, decided: true,
		},
		{
			name: "row 8: starts whose live turn is still current at the first read, with no candidate",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev: func() TurnEvidence {
				ev := sourced()
				ev.LiveAtProbe, ev.LiveTurn = TriYes, "old-2"
				ev.FirstRead = FirstTurnRead{Done: true, Current: "old-2"}
				return ev
			},
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
		{
			name: "row 7: a second candidate",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev: func() TurnEvidence {
				ev := sourced(seen("a", linkAt.Add(ms)), seen("b", linkAt.Add(2*ms)))
				ev.Serial = true
				return ev
			},
			want: TurnLink{Turn: "a", Confidence: LinkAmbiguous, Basis: BasisCompetingTurn}, decided: true,
		},
		{
			name: "row 7: a live turn at the probe",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev: func() TurnEvidence {
				ev := sourced(seen("a", linkAt.Add(ms)))
				ev.LiveAtProbe, ev.LiveTurn = TriYes, "old-2"
				ev.Serial = true
				return ev
			},
			want: TurnLink{Turn: "a", Confidence: LinkAmbiguous, Basis: BasisCompetingTurn}, decided: true,
		},
		{
			// Known may lack the probe's live turn, as when a deliverer builds
			// it from the source's recent turns alone. On a source whose clock
			// is not the lane host's, no cutoff drops that turn, and it is
			// still never a candidate.
			name: "row 8: starts whose live turn Known lacks, before the delivery's turn shows",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev: func() TurnEvidence {
				ev := sourced(SeenTurn{ID: "live", Generation: linkGen, StartedAt: linkAt.Add(-time.Minute), Running: true})
				ev.SameClock = false
				ev.LiveAtProbe, ev.LiveTurn = TriYes, "live"
				ev.FirstRead = FirstTurnRead{Done: true, Current: "live"}
				return ev
			},
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
		{
			name: "row 7: starts whose live turn Known lacks, once the delivery's turn shows",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev: func() TurnEvidence {
				ev := sourced(
					SeenTurn{ID: "live", Generation: linkGen, StartedAt: linkAt.Add(-time.Minute), Running: true},
					seen("next", linkAt.Add(5*ms)),
				)
				ev.SameClock = false
				ev.LiveAtProbe, ev.LiveTurn = TriYes, "live"
				ev.FirstRead = FirstTurnRead{Done: true, Current: "live"}
				return ev
			},
			want: TurnLink{Turn: "next", Confidence: LinkAmbiguous, Basis: BasisCompetingTurn}, decided: true,
		},
		{
			name: "row 8: joins whose live turn Known lacks ended before the first read",
			rec:  func() DeliveryRecord { return linkRec(LinkJoins) },
			ev: func() TurnEvidence {
				ev := sourced(seen("live", linkAt.Add(-time.Minute)))
				ev.SameClock = false
				ev.LiveAtProbe, ev.LiveTurn = TriYes, "live"
				ev.FirstRead = FirstTurnRead{Done: true}
				return ev
			},
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
		{
			name: "row 7: an unknown probe, even on a serial source",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev: func() TurnEvidence {
				ev := sourced(seen("a", linkAt.Add(ms)))
				ev.LiveAtProbe, ev.Serial = TriUnknown, true
				return ev
			},
			want: TurnLink{Turn: "a", Confidence: LinkAmbiguous, Basis: BasisCompetingTurn}, decided: true,
		},
		{
			name: "row 7: a turn first seen with its start unknown keeps its first-sighting place",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev: func() TurnEvidence {
				return sourced(seen("a", time.Time{}), seen("b", linkAt.Add(2*ms)), seen("a", linkAt.Add(ms)))
			},
			want: TurnLink{Turn: "a", Confidence: LinkAmbiguous, Basis: BasisCompetingTurn}, decided: true,
		},
		{
			name: "row 6: a seen turn without an id is never a candidate",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev: func() TurnEvidence {
				ev := sourced(seen("", linkAt.Add(ms)), seen("t1", linkAt.Add(2*ms)))
				ev.SameClock = false
				return ev
			},
			want: TurnLink{Turn: "t1", Confidence: LinkCorrelated, Basis: BasisHistoryDiff}, decided: true,
		},
		{
			name: "row 8: an idle read's empty turn id is no candidate, even on a serial source",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev: func() TurnEvidence {
				ev := sourced(seen("", time.Time{}))
				ev.SameClock, ev.Serial = false, true
				return ev
			},
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
		// Row 8.
		{
			name: "row 8: same-clock candidate one millisecond before At is excluded",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev:   func() TurnEvidence { return sourced(seen("early", cut.Add(-ms))) },
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
		{
			name: "row 8: a same-clock turn whose start no read knew is excluded",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev:   func() TurnEvidence { return sourced(seen("t1", time.Time{}), seen("t1", time.Time{})) },
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
		{
			name: "row 8: a candidate of another generation is ignored",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev: func() TurnEvidence {
				return sourced(SeenTurn{ID: "prev-gen", Generation: linkGen + 1, StartedAt: linkAt.Add(ms)})
			},
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
		{
			name: "row 8: only known turns seen",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev:   func() TurnEvidence { return sourced(seen("old-1", linkAt.Add(ms)), seen("old-2", linkAt.Add(ms))) },
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
		{
			name: "row 8: every read failed gives read_failed",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev:   func() TurnEvidence { ev := sourced(); ev.FirstRead = FirstTurnRead{}; ev.ReadErr = readErr; return ev },
			want: TurnLink{Confidence: LinkNone, Basis: BasisReadFailed}, decided: false,
		},
		{
			name: "row 8: a later failed read after a successful one is no_candidate",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev:   func() TurnEvidence { ev := sourced(); ev.ReadErr = readErr; return ev },
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
		{
			name: "row 8: no read yet",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev:   func() TurnEvidence { ev := sourced(); ev.FirstRead = FirstTurnRead{}; return ev },
			want: TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}, decided: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, decided := CorrelateTurn(tt.rec(), tt.ev())
			if got != tt.want || decided != tt.decided {
				t.Fatalf("CorrelateTurn = %+v, decided %v; want %+v, decided %v", got, decided, tt.want, tt.decided)
			}
		})
	}
}

// TestCorrelateTurnDecidedOnlyOnceACandidateExists walks one delivery's reads:
// failed, empty, then the candidate. Only the last decides.
func TestCorrelateTurnDecidedOnlyOnceACandidateExists(t *testing.T) {
	rec := linkRec(LinkStarts)
	ev := sourced()
	ev.FirstRead = FirstTurnRead{}
	ev.ReadErr = errors.New("unreadable")
	if link, decided := CorrelateTurn(rec, ev); decided || link.Basis != BasisReadFailed {
		t.Fatalf("after a failed read: %+v decided %v", link, decided)
	}
	ev.FirstRead, ev.ReadErr = FirstTurnRead{Done: true}, nil
	if link, decided := CorrelateTurn(rec, ev); decided || link.Basis != BasisNoCandidate {
		t.Fatalf("after an empty read: %+v decided %v", link, decided)
	}
	ev.Seen = append(ev.Seen, seen("t1", linkAt.Add(time.Millisecond)))
	if link, decided := CorrelateTurn(rec, ev); !decided || link.Turn != "t1" {
		t.Fatalf("after the candidate: %+v decided %v", link, decided)
	}
}

// linkGenerator builds random delivery records and evidence for the
// invariance property.
type linkGenerator struct {
	r *rand.Rand
	n int
}

func newLinkGenerator(seed1, seed2 uint64) *linkGenerator {
	return &linkGenerator{r: rand.New(rand.NewPCG(seed1, seed2^0x9e3779b97f4a7c15))}
}

func (g *linkGenerator) id(prefix string) string {
	g.n++
	return fmt.Sprintf("%s-%d", prefix, g.n)
}

func (g *linkGenerator) tri() Tri { return []Tri{TriUnknown, TriNo, TriYes}[g.r.IntN(3)] }

// at returns a time near linkAt, at or after its millisecond when after.
func (g *linkGenerator) at(after bool) time.Time {
	d := time.Duration(g.r.IntN(5000)) * time.Millisecond
	if after {
		return linkAt.Truncate(time.Millisecond).Add(d)
	}
	return linkAt.Truncate(time.Millisecond).Add(-time.Millisecond - d)
}

func (g *linkGenerator) record() DeliveryRecord {
	rec := linkRec([]LinkMode{LinkStarts, LinkJoins}[g.r.IntN(2)])
	rec.Ack = []AckKind{AckDelivered, AckDeliveredUnobserved, AckInBand}[g.r.IntN(3)]
	if g.r.IntN(6) == 0 {
		rec.TurnID = g.id("receipt")
	}
	if g.r.IntN(4) == 0 {
		rec.MessageAt = g.at(true)
	}
	return rec
}

func (g *linkGenerator) evidence(rec DeliveryRecord) TurnEvidence {
	ev := TurnEvidence{
		HasSource:   g.r.IntN(8) != 0,
		SameClock:   g.r.IntN(2) == 0,
		Serial:      g.r.IntN(3) == 0,
		LiveAtProbe: g.tri(),
		FirstRead:   FirstTurnRead{Done: g.r.IntN(5) != 0},
	}
	for range g.r.IntN(4) {
		ev.Known = append(ev.Known, g.id("known"))
	}
	if ev.LiveAtProbe == TriYes && len(ev.Known) > 0 && g.r.IntN(4) != 0 {
		ev.LiveTurn = ev.Known[0]
	}
	for range g.r.IntN(4) {
		st := seen(g.id("turn"), g.at(true))
		switch {
		case !rec.MessageAt.IsZero() && g.r.IntN(2) == 0:
			st.StartedAt = rec.MessageAt
		case g.r.IntN(6) == 0:
			st.StartedAt = time.Time{}
		}
		if g.r.IntN(10) == 0 {
			st.ID = ""
		}
		st.Running = g.r.IntN(2) == 0
		ev.Seen = append(ev.Seen, st)
	}
	// A later read repeats a turn, sometimes now with its start known.
	if len(ev.Seen) > 0 && g.r.IntN(3) == 0 {
		again := ev.Seen[g.r.IntN(len(ev.Seen))]
		if again.StartedAt.IsZero() {
			again.StartedAt = g.at(true)
		}
		ev.Seen = append(ev.Seen, again)
	}
	if ev.FirstRead.Done && g.r.IntN(2) == 0 {
		if ev.LiveTurn != "" && g.r.IntN(2) == 0 {
			ev.FirstRead.Current = ev.LiveTurn
		} else if len(ev.Seen) > 0 {
			ev.FirstRead.Current = ev.Seen[0].ID
		}
	}
	if g.r.IntN(4) == 0 {
		ev.ReadErr = errors.New("read failed")
	}
	return ev
}

func insertSeen(r *rand.Rand, turns []SeenTurn, st SeenTurn) []SeenTurn {
	i := r.IntN(len(turns) + 1)
	out := make([]SeenTurn, 0, len(turns)+1)
	out = append(out, turns[:i]...)
	out = append(out, st)
	return append(out, turns[i:]...)
}

func cloneEvidence(ev TurnEvidence) TurnEvidence {
	ev.Known = append([]string(nil), ev.Known...)
	ev.Seen = append([]SeenTurn(nil), ev.Seen...)
	return ev
}

// linkMutation is a change to the evidence that must not change the link.
type linkMutation struct {
	name     string
	sameOnly bool // applies only to a same-clock source
	apply    func(g *linkGenerator, rec DeliveryRecord, e TurnEvidence) TurnEvidence
}

var linkMutations = []linkMutation{
	{name: "permute Known", apply: func(g *linkGenerator, _ DeliveryRecord, e TurnEvidence) TurnEvidence {
		g.r.Shuffle(len(e.Known), func(a, b int) { e.Known[a], e.Known[b] = e.Known[b], e.Known[a] })
		return e
	}},
	{name: "add another generation's turn", apply: func(g *linkGenerator, rec DeliveryRecord, e TurnEvidence) TurnEvidence {
		st := SeenTurn{ID: g.id("other-gen"), Generation: rec.Generation + 1 + int64(g.r.IntN(3)), StartedAt: g.at(true)}
		// Another generation's turn may carry an id this generation's turns use.
		if len(e.Seen) > 0 && g.r.IntN(2) == 0 {
			st.ID = e.Seen[g.r.IntN(len(e.Seen))].ID
		}
		if !rec.MessageAt.IsZero() {
			st.StartedAt = rec.MessageAt
		}
		e.Seen = insertSeen(g.r, e.Seen, st)
		return e
	}},
	{name: "add a known turn", apply: func(g *linkGenerator, rec DeliveryRecord, e TurnEvidence) TurnEvidence {
		id := g.id("known-late")
		e.Known = append(e.Known, id)
		st := seen(id, g.at(true))
		if !rec.MessageAt.IsZero() {
			st.StartedAt = rec.MessageAt
		}
		e.Seen = insertSeen(g.r, e.Seen, st)
		return e
	}},
	{name: "add a pre-At turn", sameOnly: true, apply: func(g *linkGenerator, _ DeliveryRecord, e TurnEvidence) TurnEvidence {
		e.Seen = insertSeen(g.r, e.Seen, seen(g.id("pre"), g.at(false)))
		return e
	}},
}

// checkLinkInvariants asserts what every link must satisfy, whatever row
// decided it.
func checkLinkInvariants(rec DeliveryRecord, ev TurnEvidence, link TurnLink, decided bool) error {
	if (link.Turn == "") != (link.Confidence == LinkNone) {
		return fmt.Errorf("turn %q with confidence %s", link.Turn, link.Confidence)
	}
	if !decided && (link.Confidence != LinkNone || (link.Basis != BasisNoCandidate && link.Basis != BasisReadFailed)) {
		return fmt.Errorf("undecided link %+v", link)
	}
	if link.Confidence == LinkExact && link.Basis != BasisRuntimeTurn && link.Basis != BasisHookPayload && link.Basis != BasisACPSerial {
		return fmt.Errorf("exact link on basis %s", link.Basis)
	}
	if link.Turn != "" && link.Turn != rec.TurnID && link.Turn != ev.LiveTurn &&
		!slices.ContainsFunc(ev.Seen, func(s SeenTurn) bool { return s.ID == link.Turn }) {
		return fmt.Errorf("turn %q is not in the evidence", link.Turn)
	}
	return nil
}

// TestCorrelateTurnInvariance checks the link invariants on random evidence
// and pins what the link must not depend on: the order of Known, and seen
// turns the rule excludes (another generation, a known turn, or on a
// same-clock source a turn that started before At). Each mutation draws from
// its own generator seeded by (seed, case, mutation), so a failure's seed and
// case reproduce it.
func TestCorrelateTurnInvariance(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3, 0xd1ce, 0x5eed} {
		g := newLinkGenerator(seed, seed)
		for i := range 2000 {
			rec := g.record()
			ev := g.evidence(rec)
			wantLink, wantDecided := CorrelateTurn(rec, ev)
			if err := checkLinkInvariants(rec, ev, wantLink, wantDecided); err != nil {
				t.Fatalf("seed %d case %d: %v\nrec %+v\nev %+v", seed, i, err, rec, ev)
			}
			for k, m := range linkMutations {
				if m.sameOnly && !ev.SameClock {
					continue
				}
				mg := newLinkGenerator(seed, uint64(i)<<8|uint64(k))
				mutated := m.apply(mg, rec, cloneEvidence(ev))
				gotLink, gotDecided := CorrelateTurn(rec, mutated)
				if gotLink != wantLink || gotDecided != wantDecided {
					t.Fatalf("seed %d case %d, %s: %+v decided %v, want %+v decided %v\nrec %+v\nev %+v\nmutated %+v",
						seed, i, m.name, gotLink, gotDecided, wantLink, wantDecided, rec, ev, mutated)
				}
			}
		}
	}
}
