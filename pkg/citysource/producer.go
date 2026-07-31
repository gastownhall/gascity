package citysource

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/pkg/citytransport"
)

// Producer maps and exports one enrolled City source. It holds no cursor state
// of its own: every method takes the durable CityState in and returns the next
// one out, so a caller cannot accidentally advance state on a path that
// returned an error.
type Producer struct {
	enroll Enrollment
	probe  LineageProbe
	now    func() time.Time
}

// NewProducer builds a Producer. now is injectable so the time-bound and
// policy-expiry paths are testable without sleeping.
func NewProducer(e Enrollment, probe LineageProbe, now func() time.Time) (*Producer, error) {
	if err := e.Identity.Validate(); err != nil {
		return nil, err
	}
	if probe == nil {
		return nil, fmt.Errorf("citysource: a LineageProbe is required; without read-back the lineage check cannot run")
	}
	if len(e.TrustedKeys) == 0 {
		return nil, fmt.Errorf("%w: enrollment carries no trusted signing keys", ErrPolicyUnknown)
	}
	if now == nil {
		now = time.Now
	}
	return &Producer{enroll: e, probe: probe, now: now}, nil
}

// Observe runs both reset checks against the current source log and returns the
// state to persist. A returned error always comes with a state carrying the
// fault, so the caller persists the halt rather than losing it on the next
// restart and re-discovering the same problem forever.
//
// Call this before every export cycle, not just at startup: a reset can land at
// any moment, and the whole point is that nothing else in the source will tell
// us.
func (p *Producer) Observe(st CityState) (CityState, error) {
	if st.Faulted() {
		// A faulted city stays faulted. Re-running the checks could "clear" a
		// fault by luck — a rebuilt log that happens to have grown a matching
		// anchor — and only a signed declaration may clear one.
		return st, fmt.Errorf("%w: %s (%s)", ErrFaulted, st.Fault, st.FaultDetail)
	}

	city := p.enroll.Identity.City
	head, err := p.probe.Head(city)
	if err != nil {
		// A probe error is NOT a reset. Treating an unreadable log as a rebuild
		// would fault a city over a transient I/O failure and demand an operator
		// signature to recover. Hold, and let the caller retry.
		return st, fmt.Errorf("citysource: read head for %s: %w", city, err)
	}

	// Check 1: seq regression. The head cannot legitimately fall below a seq the
	// server has already confirmed it holds.
	if st.Watermark > 0 && head < st.Watermark {
		detail := fmt.Sprintf("head %d < watermark %d at epoch %d", head, st.Watermark, st.Epoch)
		st.Fault, st.FaultDetail = FaultSeqRegression, detail
		return st, fmt.Errorf("%w: %s", ErrSeqRegression, detail)
	}

	// Check 2: lineage break. This is the check that catches what regression
	// cannot — a log that was reset and has ALREADY grown back past our
	// watermark. Without it, the producer would resume mid-stream on a stream it
	// never saw the start of, and never report anything.
	if st.AnchorSeq > 0 {
		at, ok, err := p.probe.OfferAt(city, st.AnchorSeq)
		if err != nil {
			return st, fmt.Errorf("citysource: read anchor seq %d for %s: %w", st.AnchorSeq, city, err)
		}
		if !ok {
			detail := fmt.Sprintf("anchor seq %d absent from log at head %d", st.AnchorSeq, head)
			st.Fault, st.FaultDetail = FaultLineageBreak, detail
			return st, fmt.Errorf("%w: %s", ErrLineageBreak, detail)
		}
		if got := SemanticHash(at); got != st.AnchorHash {
			detail := fmt.Sprintf("anchor seq %d hashes %s, acknowledged %s", st.AnchorSeq, got, st.AnchorHash)
			st.Fault, st.FaultDetail = FaultLineageBreak, detail
			return st, fmt.Errorf("%w: %s", ErrLineageBreak, detail)
		}
	}
	return st, nil
}

// ApplyReset clears a fault and moves the city to the declared new epoch.
//
// The watermark returns to 0 and the anchor is dropped: the prior epoch's
// admitted prefix stays immutable on the server, but nothing in the new log is
// acknowledged yet, and no anchor from the old log means anything in the new
// one.
func (p *Producer) ApplyReset(st CityState, d ResetDeclaration) (CityState, error) {
	if err := d.Verify(p.now(), p.enroll, st); err != nil {
		return st, err
	}
	return CityState{
		SourceID:  p.enroll.Identity.SourceID,
		Epoch:     d.NewEpoch,
		Watermark: 0,
	}, nil
}

// Map turns a batch of source offers into a signed Upload.
//
// The offers must arrive in ascending seq order starting at Watermark+1 — the
// source produces them that way and this package does not reorder, it verifies.
// Content fields survive only if the signed content policy permits them; with
// no permission they are stripped here, before the semantic hash is computed, so
// the hash reflects what actually ships.
func (p *Producer) Map(st CityState, set PolicySet, offers []citytransport.Offer) (citytransport.Upload, error) {
	if st.Faulted() {
		return citytransport.Upload{}, fmt.Errorf("%w: %s (%s)", ErrFaulted, st.Fault, st.FaultDetail)
	}
	current, err := p.enroll.Current(p.now(), set)
	if err != nil {
		// S-006 edge: missing/stale/unequal/unknown policy stops export here,
		// which is upstream of any checkpoint advancement.
		return citytransport.Upload{}, err
	}

	mapped := make([]citytransport.Offer, 0, len(offers))
	want := st.Watermark + 1
	for i, o := range offers {
		if o.Seq != want {
			return citytransport.Upload{}, fmt.Errorf(
				"citysource: offer %d has seq %d, expected %d: the producer verifies source order, it does not impose one",
				i, o.Seq, want)
		}
		want++
		if o.Type == "" || o.RecordTS == "" {
			return citytransport.Upload{}, fmt.Errorf("citysource: offer seq %d is missing type or record_ts", o.Seq)
		}
		if !current.Content.ContentPermitted {
			o.Title, o.Formula = "", ""
		}
		o.SemanticHash = SemanticHash(o)
		mapped = append(mapped, o)
	}

	return citytransport.Upload{
		SourceID:             p.enroll.Identity.SourceID,
		Epoch:                st.Epoch,
		SchemaVersion:        citytransport.SchemaVersion,
		SourceContractDigest: current.Source.Digest,
		ContentPolicyDigest:  current.Content.Digest,
		Events:               mapped,
	}, nil
}

// CheckpointReport is the explicit account of one export cycle. Counts are
// reported rather than summarized so an operator can tell a healthy duplicate
// storm from a gap that is quietly holding the watermark.
type CheckpointReport struct {
	Epoch          uint64
	From           uint64
	To             uint64
	StoppedAt      uint64
	StopReason     string
	Admitted       int
	Replayed       int
	Duplicated     int
	Parked         int
	Quarantined    int
	Rejected       int
	Unacknowledged int
}

// Advanced reports whether the watermark moved.
func (r CheckpointReport) Advanced() bool { return r.To > r.From }

// Checkpoint folds a server acknowledgement into the durable state.
//
// The rule is contiguity, not count: the watermark advances across the longest
// unbroken run of acknowledged seqs starting at Watermark+1, and stops dead at
// the first seq that is parked, quarantined, rejected, missing from the
// response, or out of order. A gap does not "skip"; a changed duplicate does not
// "overwrite". Both simply hold the watermark where it was, so the next cycle
// re-offers from the same place and the hole stays visible instead of being
// stepped over.
//
// The anchor is re-pinned to the last acknowledged offer, which is what keeps
// the lineage check meaningful as the watermark moves.
func (p *Producer) Checkpoint(st CityState, up citytransport.Upload, ack citytransport.Ack) (CityState, CheckpointReport, error) {
	rep := CheckpointReport{Epoch: st.Epoch, From: st.Watermark, To: st.Watermark}
	if st.Faulted() {
		return st, rep, fmt.Errorf("%w: %s (%s)", ErrFaulted, st.Fault, st.FaultDetail)
	}
	if up.SourceID != st.SourceID && st.SourceID != "" {
		return st, rep, fmt.Errorf("citysource: upload names source %q, state is %q", up.SourceID, st.SourceID)
	}

	// A response for a different epoch can never advance this epoch's watermark.
	// It means the server reset underneath us, so it is a fault, not a retry.
	if ack.Epoch != 0 && ack.Epoch != st.Epoch {
		detail := fmt.Sprintf("acknowledgement epoch %d, uploaded under epoch %d", ack.Epoch, st.Epoch)
		st.Fault, st.FaultDetail = FaultEpochMismatch, detail
		return st, rep, fmt.Errorf("%w: %s", ErrEpochMismatch, detail)
	}

	offered := make(map[uint64]citytransport.Offer, len(up.Events))
	for _, o := range up.Events {
		offered[o.Seq] = o
	}
	results := make(map[uint64]citytransport.Result, len(ack.Results))
	for _, r := range ack.Results {
		if _, ok := offered[r.Seq]; !ok {
			// The server acknowledged something we never sent. Advancing on that
			// would let a confused server move our watermark past events we hold.
			return st, rep, fmt.Errorf("%w: seq %d", ErrUnsolicitedAck, r.Seq)
		}
		results[r.Seq] = r
		switch r.Outcome {
		case citytransport.OutcomeAdmit:
			rep.Admitted++
		case citytransport.OutcomeAckReplay:
			rep.Replayed++
		case citytransport.OutcomeAckDuplicate:
			rep.Duplicated++
		case citytransport.OutcomeParkGap:
			rep.Parked++
		case citytransport.OutcomeQuarantineConflict, citytransport.OutcomeQuarantineIncomparable,
			citytransport.OutcomeQuarantineTimeBound, citytransport.OutcomeQuarantineGapExpired:
			rep.Quarantined++
		case citytransport.OutcomeRejectInvalidReset, citytransport.OutcomeContractDigestMismatch:
			rep.Rejected++
		default:
			rep.Unacknowledged++
		}
	}

	// A digest rejection anywhere in the response invalidates the whole cycle:
	// the events were mapped under a contract the server no longer honors, so
	// none of them may count, not even the ones it happened to admit first.
	for _, r := range ack.Results {
		if r.Outcome == citytransport.OutcomeContractDigestMismatch || r.Outcome == citytransport.OutcomeRejectInvalidReset {
			rep.StoppedAt, rep.StopReason = r.Seq, string(r.Outcome)
			return st, rep, fmt.Errorf("%w: server rejected the upload's signed contract at seq %d (%s)",
				ErrPolicyMismatch, r.Seq, r.ReasonCode)
		}
	}

	next := st
	for seq := st.Watermark + 1; ; seq++ {
		r, ok := results[seq]
		if !ok {
			rep.StoppedAt, rep.StopReason = seq, "no_result"
			break
		}
		if !citytransport.Acknowledged(r.Outcome) {
			rep.StoppedAt, rep.StopReason = seq, string(r.Outcome)
			break
		}
		o := offered[seq]
		next.Watermark = seq
		next.AnchorSeq = seq
		next.AnchorHash = o.SemanticHash
	}
	next.SourceID = p.enroll.Identity.SourceID
	rep.To = next.Watermark
	return next, rep, nil
}
