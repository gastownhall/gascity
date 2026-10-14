package opjournal

import (
	"slices"
	"time"
)

// Tri is yes, no or unknown. The zero value is TriUnknown, so an unset value
// never reads as "no".
type Tri int8

// Tri values.
const (
	TriUnknown Tri = iota
	TriNo
	TriYes
)

// LinkMode is how a delivery meets the session's turns. The lane sets it from
// the plan: LinkJoins for a steer delivered mid-turn and for an in-band pass,
// LinkStarts for everything else, including a steer delivered at a boundary.
type LinkMode string

// Link modes.
const (
	LinkStarts LinkMode = "starts" // delivered at a boundary: it starts the next turn
	LinkJoins  LinkMode = "joins"  // steer, or an in-band pass: it joins the running turn
)

// AckKind is the runtime's answer to one delivery.
type AckKind string

// Runtime acks.
const (
	// AckDelivered: the runtime accepted the input (for some terminals, it saw
	// the session turn busy after the submit).
	AckDelivered AckKind = "delivered"
	// AckDeliveredUnobserved: the input was delivered, but the runtime never
	// saw the session turn busy.
	AckDeliveredUnobserved AckKind = "delivered_unobserved"
	// AckInBand: the input was injected into a starting turn by a hook.
	AckInBand AckKind = "in_band"
)

// DeliveryRecord is what Done records on every op of one delivery. It carries
// no text: an op's turn comes from correlating this record with observed
// turns, never from anything in the delivered text.
//
// At is required: on a same-clock source it is the cutoff for candidates, and
// a zero At would admit every unknown turn of the generation. Row 3 compares
// MessageAt with turn starts to the millisecond, so the deliverer may record
// the instant before the runtime formatted its stamp.
type DeliveryRecord struct {
	Seq        uint64    // lane_seq of the delivery's head op
	At         time.Time // lane host clock, just before the effect's first input
	Generation int64     // from the Take's fence
	Epoch      int64
	Mode       LinkMode
	Ack        AckKind   // delivered | delivered_unobserved | in_band
	MessageID  string    // the runtime's id for the delivered message, "" when it reports none
	MessageAt  time.Time // the creation time the runtime stamped on that message; zero when it reports none
	TurnID     string    // the turn a runtime receipt or a verified hook payload names
}

// SeenTurn is one turn a turn-history read showed. The deliverer appends
// each read's turns after the earlier reads' turns, and within one read
// oldest start first, so the first candidate is the earliest observed.
type SeenTurn struct {
	ID         string // the source's turn id; a sighting with no id is ignored
	Generation int64
	StartedAt  time.Time // the source's start time; zero when unknown
	Running    bool      // informational: the rule does not read it
}

// FirstTurnRead is the first turn-history read after the effect that
// succeeded. Row 4 of CorrelateTurn's rule reads "the probe's live turn is
// still current at the first read after the effect" from it alone, never from
// Seen, which merges every read and so has no read boundaries. The
// deliverer sets it once, from that read, and leaves it as is on later reads.
type FirstTurnRead struct {
	// Done is true once a read after the effect succeeded. While it is false,
	// row 4 cannot match, and a failed read leaves the link read_failed.
	Done bool
	// Current is the id of the turn that read showed running, "" when none
	// was running.
	Current string
}

// TurnEvidence is what a deliverer read of the session's turns, within the
// delivery's lease, from the probe up to the latest read after the effect.
//
// Known holds only turns of the delivery's generation: the rule matches it by
// id alone, so the deliverer leaves out an earlier incarnation's turns (a
// resumed transcript keeps them). Seen holds only the turns of reads that
// succeeded: a failed read adds none, not even the turns it parsed before it
// failed, so Seen stays empty while FirstRead.Done is false.
type TurnEvidence struct {
	HasSource   bool          // the session's turn starts are recorded and this process can read them
	SameClock   bool          // StartedAt is on the lane host's clock
	Serial      bool          // only gc's input can start a turn on this source
	LiveAtProbe Tri           // whether a turn was running at the probe
	LiveTurn    string        // the turn running at the probe, "" when unknown; never a candidate
	Known       []string      // the ids of the current and recent turns the probe's read returned; never candidates
	Seen        []SeenTurn    // every turn the successful reads after the effect showed, in observation order (see SeenTurn)
	FirstRead   FirstTurnRead // the first successful read after the effect
	ReadErr     error         // the last read failed; never read as "no turn"
}

// LinkConfidence grades a delivered op's link to its turn.
type LinkConfidence string

// Link confidences.
const (
	LinkExact      LinkConfidence = "exact"
	LinkCorrelated LinkConfidence = "correlated"
	LinkAmbiguous  LinkConfidence = "ambiguous"
	LinkNone       LinkConfidence = "none"
)

// LinkBasis names the row of CorrelateTurn's rule that decided a link. It is
// distinct from the fact bases of the journal's other records.
type LinkBasis string

// Link basis tokens.
const (
	BasisRuntimeTurn    LinkBasis = "runtime_turn"     // row 1: a runtime receipt names the turn
	BasisHookPayload    LinkBasis = "hook_payload"     // row 1: a verified hook payload names the turn
	BasisNoSource       LinkBasis = "no_source"        // row 2: no readable turn source
	BasisRequestedAt    LinkBasis = "requested_at"     // row 3: a turn's start equals the message's stamp
	BasisNoCandidate    LinkBasis = "no_candidate"     // rows 3 and 8: no turn matched
	BasisJoinedLiveTurn LinkBasis = "joined_live_turn" // row 4: joined the turn live at the probe
	BasisACPSerial      LinkBasis = "acp_serial"       // row 5: the one new turn on a serial source
	BasisHistoryDiff    LinkBasis = "history_diff"     // row 6: the one new turn
	BasisCompetingTurn  LinkBasis = "competing_turn"   // row 7: another turn could have consumed it
	BasisReadFailed     LinkBasis = "read_failed"      // rows 3 and 8: no read after the effect succeeded, and the last read failed
)

// TurnLink is a delivered op's turn, its confidence and the rule that decided
// it. Turn is "" when Confidence is LinkNone.
type TurnLink struct {
	Turn       string
	Confidence LinkConfidence
	Basis      LinkBasis
}

// CorrelateTurn decides a delivery's link to the turn that consumed it: the
// one it started (LinkStarts) or the one it joined (LinkJoins). decided is
// false while more evidence could still add a candidate; the deliverer reads
// again until it is true or its link wait passes, and then records the link
// as returned.
//
// Candidates are the Seen turns of rec.Generation that Known lacks and, when
// SameClock, that started at or after rec.At truncated to the millisecond, in
// observation order. Sightings of one turn (same generation and id) merge
// first (later reads repeat a turn): the merged turn sits at its first
// sighting and takes the first non-zero StartedAt any sighting carried, so a
// turn first seen with its start unknown still matches row 3 and still passes
// the same-clock cutoff. The same id in another generation is another turn.
// A sighting without an id is dropped. A same-clock turn whose start no sighting
// knew is excluded. The probe's LiveTurn is never a candidate, even when Known
// lacks it: it was running before the effect. The first matching row decides:
//
//  1. rec.TurnID set: that turn, exact (hook_payload for an in-band ack,
//     else runtime_turn).
//  2. No turn source: none, no_source.
//  3. rec.MessageAt set: the first candidate whose StartedAt equals it to
//     the millisecond, correlated, requested_at; with none, undecided none
//     (row 8's basis). It never falls through to a history diff. Only two
//     turns of the session starting within one millisecond give two such
//     candidates, and the first observed is taken, not called ambiguous.
//  4. LinkJoins, a live turn at the probe, and the first read after the
//     effect shows that turn still current: it, correlated, joined_live_turn.
//  5. Exactly one candidate, no live turn at the probe, and Serial: it,
//     exact, acp_serial.
//  6. Exactly one candidate and no live turn at the probe: it, correlated,
//     history_diff.
//  7. Any other candidate set: a second candidate, a live or unknown turn at
//     the probe, or (LinkJoins) the probe's live turn no longer current at
//     the first read. The first candidate, ambiguous, competing_turn.
//  8. No candidate: undecided none, read_failed when no read after the
//     effect succeeded and the last one failed, else no_candidate.
//
// decided is false only in row 3 without a match and in row 8.
func CorrelateTurn(rec DeliveryRecord, ev TurnEvidence) (link TurnLink, decided bool) {
	if rec.TurnID != "" {
		basis := BasisRuntimeTurn
		if rec.Ack == AckInBand {
			basis = BasisHookPayload
		}
		return TurnLink{Turn: rec.TurnID, Confidence: LinkExact, Basis: basis}, true
	}
	if !ev.HasSource {
		return TurnLink{Confidence: LinkNone, Basis: BasisNoSource}, true
	}
	candidates := linkCandidates(rec, ev)
	if !rec.MessageAt.IsZero() {
		stamp := rec.MessageAt.Truncate(time.Millisecond)
		for _, c := range candidates {
			if c.StartedAt.Truncate(time.Millisecond).Equal(stamp) {
				return TurnLink{Turn: c.ID, Confidence: LinkCorrelated, Basis: BasisRequestedAt}, true
			}
		}
		return noCandidate(ev), false
	}
	if rec.Mode == LinkJoins && ev.LiveAtProbe == TriYes && ev.LiveTurn != "" &&
		ev.FirstRead.Done && ev.FirstRead.Current == ev.LiveTurn {
		return TurnLink{Turn: ev.LiveTurn, Confidence: LinkCorrelated, Basis: BasisJoinedLiveTurn}, true
	}
	switch {
	case len(candidates) == 0:
		return noCandidate(ev), false
	case len(candidates) == 1 && ev.LiveAtProbe == TriNo && ev.Serial:
		return TurnLink{Turn: candidates[0].ID, Confidence: LinkExact, Basis: BasisACPSerial}, true
	case len(candidates) == 1 && ev.LiveAtProbe == TriNo:
		return TurnLink{Turn: candidates[0].ID, Confidence: LinkCorrelated, Basis: BasisHistoryDiff}, true
	default:
		// Row 4 already took a joins delivery whose live turn stayed current,
		// so every remaining candidate set has a competitor.
		return TurnLink{Turn: candidates[0].ID, Confidence: LinkAmbiguous, Basis: BasisCompetingTurn}, true
	}
}

// linkCandidates returns the seen turns that could be the delivery's, in
// observation order. Sightings merge by generation and id before any filter:
// a turn that several reads showed counts once, at its first sighting, with
// the first start time any sighting of that turn knew. A sighting without an id is never a turn.
func linkCandidates(rec DeliveryRecord, ev TurnEvidence) []SeenTurn {
	cutoff := rec.At.Truncate(time.Millisecond)
	var out []SeenTurn
	for _, t := range mergeSightings(ev.Seen) {
		if t.Generation != rec.Generation || t.ID == ev.LiveTurn || slices.Contains(ev.Known, t.ID) {
			continue
		}
		if ev.SameClock && t.StartedAt.Before(cutoff) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// mergeSightings folds the sightings of one turn into one, kept at its first
// sighting's position and given the first non-zero StartedAt any sighting
// carried. A turn is its generation and id: the same id in another generation
// is another turn. Sightings with an empty id are dropped.
func mergeSightings(seen []SeenTurn) []SeenTurn {
	type turnKey struct {
		generation int64
		id         string
	}
	var out []SeenTurn
	at := make(map[turnKey]int, len(seen))
	for _, t := range seen {
		if t.ID == "" {
			continue
		}
		k := turnKey{t.Generation, t.ID}
		i, ok := at[k]
		if !ok {
			at[k] = len(out)
			out = append(out, t)
			continue
		}
		if out[i].StartedAt.IsZero() {
			out[i].StartedAt = t.StartedAt
		}
	}
	return out
}

// noCandidate is the undecided link while no turn matched. A failed read is
// never read as "no turn".
func noCandidate(ev TurnEvidence) TurnLink {
	if ev.ReadErr != nil && !ev.FirstRead.Done {
		return TurnLink{Confidence: LinkNone, Basis: BasisReadFailed}
	}
	return TurnLink{Confidence: LinkNone, Basis: BasisNoCandidate}
}
