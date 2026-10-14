package opjournal

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
)

// linkAt is a delivery time with sub-millisecond digits, so the tests see
// truncation to the millisecond at work.
var linkAt = time.Date(2026, 10, 14, 12, 0, 0, 123_456_789, time.UTC)

const linkGen int64 = 7

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
		BasisRuntimeTurn: "runtime_turn", BasisHookPayload: "hook_payload", BasisNoSource: "no_source",
		BasisRequestedAt: "requested_at", BasisNoCandidate: "no_candidate", BasisJoinedLiveTurn: "joined_live_turn",
		BasisACPSerial: "acp_serial", BasisHistoryDiff: "history_diff", BasisCompetingTurn: "competing_turn",
		BasisReadFailed: "read_failed",
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
			name: "row 7: an unknown probe",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev:   func() TurnEvidence { ev := sourced(seen("a", linkAt.Add(ms))); ev.LiveAtProbe = TriUnknown; return ev },
			want: TurnLink{Turn: "a", Confidence: LinkAmbiguous, Basis: BasisCompetingTurn}, decided: true,
		},
		// Row 8.
		{
			name: "row 8: same-clock candidate one millisecond before At is excluded",
			rec:  func() DeliveryRecord { return linkRec(LinkStarts) },
			ev:   func() TurnEvidence { return sourced(seen("early", cut.Add(-ms))) },
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
	if ev.LiveAtProbe == TriYes && len(ev.Known) > 0 {
		ev.LiveTurn = ev.Known[0]
	}
	for range g.r.IntN(4) {
		st := seen(g.id("turn"), g.at(true))
		if !rec.MessageAt.IsZero() && g.r.IntN(2) == 0 {
			st.StartedAt = rec.MessageAt
		}
		st.Running = g.r.IntN(2) == 0
		ev.Seen = append(ev.Seen, st)
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

// TestCorrelateTurnInvariance pins what the link must not depend on: the order
// of Known, and seen turns the rule excludes (another generation, a known turn,
// or on a same-clock source a turn that started before At).
func TestCorrelateTurnInvariance(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3, 0xd1ce, 0x5eed} {
		g := &linkGenerator{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
		for i := range 2000 {
			rec := g.record()
			ev := g.evidence(rec)
			wantLink, wantDecided := CorrelateTurn(rec, ev)

			mutations := map[string]func(TurnEvidence) TurnEvidence{
				"permute Known": func(e TurnEvidence) TurnEvidence {
					g.r.Shuffle(len(e.Known), func(a, b int) { e.Known[a], e.Known[b] = e.Known[b], e.Known[a] })
					return e
				},
				"add another generation's turn": func(e TurnEvidence) TurnEvidence {
					st := SeenTurn{ID: g.id("other-gen"), Generation: rec.Generation + 1 + int64(g.r.IntN(3)), StartedAt: g.at(true)}
					if !rec.MessageAt.IsZero() {
						st.StartedAt = rec.MessageAt
					}
					e.Seen = insertSeen(g.r, e.Seen, st)
					return e
				},
				"add a known turn": func(e TurnEvidence) TurnEvidence {
					id := g.id("known-late")
					e.Known = append(e.Known, id)
					st := seen(id, g.at(true))
					if !rec.MessageAt.IsZero() {
						st.StartedAt = rec.MessageAt
					}
					e.Seen = insertSeen(g.r, e.Seen, st)
					return e
				},
			}
			if ev.SameClock {
				mutations["add a pre-At turn"] = func(e TurnEvidence) TurnEvidence {
					e.Seen = insertSeen(g.r, e.Seen, seen(g.id("pre"), g.at(false)))
					return e
				}
			}
			for name, mutate := range mutations {
				mutated := mutate(cloneEvidence(ev))
				gotLink, gotDecided := CorrelateTurn(rec, mutated)
				if gotLink != wantLink || gotDecided != wantDecided {
					t.Fatalf("seed %d case %d, %s: %+v decided %v, want %+v decided %v\nrec %+v\nev %+v\nmutated %+v",
						seed, i, name, gotLink, gotDecided, wantLink, wantDecided, rec, ev, mutated)
				}
			}
		}
	}
}
