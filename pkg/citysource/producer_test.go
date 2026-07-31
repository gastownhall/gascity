package citysource

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/pkg/citytransport"
)

var testNow = time.Date(2026, 7, 30, 1, 31, 44, 0, time.UTC)

// fakeLog is an in-memory stand-in for .gc/events.jsonl. Reset() models the
// hazard: the log is deleted and starts again at seq 1 with no marker of any
// kind, exactly as the real source behaves.
type fakeLog struct {
	events  map[uint64]citytransport.Offer
	head    uint64
	headErr error
	atErr   error
}

func newFakeLog(n uint64, tag string) *fakeLog {
	l := &fakeLog{events: map[uint64]citytransport.Offer{}}
	for s := uint64(1); s <= n; s++ {
		l.append(s, tag)
	}
	return l
}

func (l *fakeLog) append(seq uint64, tag string) {
	l.events[seq] = citytransport.Offer{
		Seq:      seq,
		Type:     "bead.created",
		RecordTS: testNow.Add(time.Duration(seq) * time.Second).Format(time.RFC3339),
		Ref:      tag + "-" + string(rune('a'+int(seq%26))),
	}
	if seq > l.head {
		l.head = seq
	}
}

// Reset deletes the log and rebuilds it with n events under a new tag, so the
// records at any given seq are genuinely different from the ones before.
func (l *fakeLog) Reset(n uint64, tag string) {
	l.events = map[uint64]citytransport.Offer{}
	l.head = 0
	for s := uint64(1); s <= n; s++ {
		l.append(s, tag)
	}
}

func (l *fakeLog) Head(string) (uint64, error) {
	if l.headErr != nil {
		return 0, l.headErr
	}
	return l.head, nil
}

func (l *fakeLog) OfferAt(_ string, seq uint64) (citytransport.Offer, bool, error) {
	if l.atErr != nil {
		return citytransport.Offer{}, false, l.atErr
	}
	o, ok := l.events[seq]
	return o, ok, nil
}

// offers returns the mapped-shape offers for [from, to] as the source would.
func (l *fakeLog) offers(from, to uint64) []citytransport.Offer {
	var out []citytransport.Offer
	for s := from; s <= to; s++ {
		if o, ok := l.events[s]; ok {
			out = append(out, o)
		}
	}
	return out
}

type testEnv struct {
	enroll Enrollment
	set    PolicySet
	priv   ed25519.PrivateKey
	log    *fakeLog
	prod   *Producer
}

func newTestEnv(t *testing.T, log *fakeLog) testEnv {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	enroll := Enrollment{
		Identity:            Identity{SourceID: "src_city_7f2a", CityHash: "c0ffee", City: "alpha"},
		PinnedSourceDigest:  "sha256:source-v1",
		PinnedContentDigest: "sha256:content-v1",
		TrustedKeys:         map[string]ed25519.PublicKey{"contract-governance-v1": pub},
	}
	src, err := SignPolicy(SignedPolicy{
		Kind: PolicySource, Digest: "sha256:source-v1", Issuer: IssuerSourcePrincipal,
		NotAfter: testNow.Add(24 * time.Hour),
	}, "contract-governance-v1", priv)
	if err != nil {
		t.Fatalf("sign source policy: %v", err)
	}
	con, err := SignPolicy(SignedPolicy{
		Kind: PolicyContent, Digest: "sha256:content-v1", Issuer: IssuerSourcePrincipal,
		NotAfter: testNow.Add(24 * time.Hour),
	}, "contract-governance-v1", priv)
	if err != nil {
		t.Fatalf("sign content policy: %v", err)
	}
	prod, err := NewProducer(enroll, log, func() time.Time { return testNow })
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return testEnv{enroll: enroll, set: PolicySet{Source: src, Content: con}, priv: priv, log: log, prod: prod}
}

// ackAll builds a response admitting every offered seq.
func ackAll(up citytransport.Upload, outcome citytransport.Outcome) citytransport.Ack {
	a := citytransport.Ack{RequestID: "req", Epoch: up.Epoch}
	for _, o := range up.Events {
		a.Results = append(a.Results, citytransport.Result{Seq: o.Seq, Outcome: outcome})
	}
	return a
}

// exportCycle runs map -> ack -> checkpoint, the whole producer loop.
func (e testEnv) exportCycle(t *testing.T, st CityState, offers []citytransport.Offer, ack func(citytransport.Upload) citytransport.Ack) (CityState, CheckpointReport, error) {
	t.Helper()
	up, err := e.prod.Map(st, e.set, offers)
	if err != nil {
		return st, CheckpointReport{}, err
	}
	return e.prod.Checkpoint(st, up, ack(up))
}

// AC01 happy: a normal event maps and both signed digests plus the stable
// source identity reach the wire.
func TestMapCarriesBothDigestsAndStableIdentity(t *testing.T) {
	env := newTestEnv(t, newFakeLog(3, "orig"))
	st := CityState{SourceID: env.enroll.Identity.SourceID, Epoch: 1}
	up, err := env.prod.Map(st, env.set, env.log.offers(1, 3))
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if up.SourceContractDigest != "sha256:source-v1" || up.ContentPolicyDigest != "sha256:content-v1" {
		t.Fatalf("both signed digests must be recorded on the upload: %+v", up)
	}
	if up.SourceID != "src_city_7f2a" || up.Epoch != 1 {
		t.Fatalf("stable identity/epoch missing: %+v", up)
	}
	for _, o := range up.Events {
		if o.SemanticHash == "" {
			t.Fatalf("every offer needs a semantic hash: %+v", o)
		}
	}
}

// AC01 edge / S-006 edge: missing, stale, unequal, or unknown policy stops
// export — and therefore stops checkpoint advancement, since nothing ships.
func TestMissingStaleUnequalUnknownPolicyStopsExport(t *testing.T) {
	base := newTestEnv(t, newFakeLog(3, "orig"))
	st := CityState{SourceID: base.enroll.Identity.SourceID, Epoch: 1}
	offers := base.log.offers(1, 3)

	otherPub, _, _ := ed25519.GenerateKey(nil)

	// Each mutation gets the subtest's own signing key, so a case that changes a
	// signed field can re-sign it legitimately and still be testing what its
	// name says rather than accidentally testing a bad signature.
	cases := map[string]struct {
		mutate func(*PolicySet, *Enrollment, ed25519.PrivateKey)
		want   error
	}{
		"missing source signature": {
			func(s *PolicySet, _ *Enrollment, _ ed25519.PrivateKey) { s.Source.Signature = nil }, ErrPolicyMissing,
		},
		"missing content digest": {
			func(s *PolicySet, _ *Enrollment, _ ed25519.PrivateKey) { s.Content.Digest = "" }, ErrPolicyMissing,
		},
		"no expiry is not current": {
			func(s *PolicySet, _ *Enrollment, k ed25519.PrivateKey) {
				s.Source.NotAfter = time.Time{}
				s.Source, _ = SignPolicy(s.Source, "contract-governance-v1", k)
			}, ErrPolicyMissing,
		},
		"stale source": {
			func(s *PolicySet, _ *Enrollment, k ed25519.PrivateKey) {
				s.Source.NotAfter = testNow.Add(-time.Hour)
				s.Source, _ = SignPolicy(s.Source, "contract-governance-v1", k)
			}, ErrPolicyStale,
		},
		"stale content": {
			func(s *PolicySet, _ *Enrollment, k ed25519.PrivateKey) {
				s.Content.NotAfter = testNow.Add(-time.Minute)
				s.Content, _ = SignPolicy(s.Content, "contract-governance-v1", k)
			}, ErrPolicyStale,
		},
		"unequal source digest": {
			func(_ *PolicySet, e *Enrollment, _ ed25519.PrivateKey) { e.PinnedSourceDigest = "sha256:source-v2" }, ErrPolicyMismatch,
		},
		"unequal content digest": {
			func(_ *PolicySet, e *Enrollment, _ ed25519.PrivateKey) { e.PinnedContentDigest = "sha256:content-v2" }, ErrPolicyMismatch,
		},
		"unknown signing key": {
			func(_ *PolicySet, e *Enrollment, _ ed25519.PrivateKey) {
				e.TrustedKeys = map[string]ed25519.PublicKey{"contract-governance-v1": otherPub}
			}, ErrPolicyUnknown,
		},
		"forged signature": {
			// Flipping a signed field WITHOUT re-signing must read as forgery.
			func(s *PolicySet, _ *Enrollment, _ ed25519.PrivateKey) { s.Content.ContentPermitted = true }, ErrPolicyUnknown,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t, base.log)
			set, enroll := env.set, env.enroll
			tc.mutate(&set, &enroll, env.priv)
			prod, err := NewProducer(enroll, env.log, func() time.Time { return testNow })
			if err != nil {
				t.Fatalf("NewProducer: %v", err)
			}
			if _, err := prod.Map(st, set, offers); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

// The content opt-in has no unsigned path: without permission, content is
// stripped before the semantic hash is taken.
func TestContentShipsOnlyUnderSignedPermission(t *testing.T) {
	env := newTestEnv(t, newFakeLog(1, "orig"))
	st := CityState{SourceID: env.enroll.Identity.SourceID, Epoch: 1}
	withContent := env.log.offers(1, 1)
	withContent[0].Title = "a human bead title"
	withContent[0].Formula = "build-and-ship"

	up, err := env.prod.Map(st, env.set, withContent)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if up.Events[0].Title != "" || up.Events[0].Formula != "" {
		t.Fatalf("content must be stripped without a signed permission: %+v", up.Events[0])
	}
	deniedHash := up.Events[0].SemanticHash

	permitted := env.set
	permitted.Content.ContentPermitted = true
	permitted.Content, err = SignPolicy(permitted.Content, "contract-governance-v1", env.priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	up2, err := env.prod.Map(st, permitted, withContent)
	if err != nil {
		t.Fatalf("Map (permitted): %v", err)
	}
	if up2.Events[0].Title == "" {
		t.Fatal("content must ship under a signed permission")
	}
	if up2.Events[0].SemanticHash == deniedHash {
		t.Fatal("the semantic hash must distinguish a record that gained content")
	}
}

// AC02 happy: restart resumes at the next event, from durable state only.
func TestRestartResumesFromDurableWatermark(t *testing.T) {
	env := newTestEnv(t, newFakeLog(5, "orig"))
	st := CityState{SourceID: env.enroll.Identity.SourceID, Epoch: 1}

	st, rep, err := env.exportCycle(t, st, env.log.offers(1, 3), func(u citytransport.Upload) citytransport.Ack {
		return ackAll(u, citytransport.OutcomeAdmit)
	})
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if rep.To != 3 || rep.Admitted != 3 {
		t.Fatalf("want watermark 3 / 3 admitted, got %+v", rep)
	}

	// Simulate a process restart: nothing survives but CityState.
	restarted, err := NewProducer(env.enroll, env.log, func() time.Time { return testNow })
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	env.prod = restarted
	if st, err = env.prod.Observe(st); err != nil {
		t.Fatalf("Observe after restart: %v", err)
	}
	up, err := env.prod.Map(st, env.set, env.log.offers(4, 5))
	if err != nil {
		t.Fatalf("Map after restart: %v", err)
	}
	if up.Events[0].Seq != 4 {
		t.Fatalf("restart must resume at seq 4, got %d", up.Events[0].Seq)
	}
}

// AC02 edge: a gap blocks the checkpoint — it does not skip.
func TestGapBlocksCheckpoint(t *testing.T) {
	env := newTestEnv(t, newFakeLog(5, "orig"))
	st := CityState{SourceID: env.enroll.Identity.SourceID, Epoch: 1}

	next, rep, err := env.exportCycle(t, st, env.log.offers(1, 5), func(u citytransport.Upload) citytransport.Ack {
		a := citytransport.Ack{RequestID: "req", Epoch: u.Epoch}
		for _, o := range u.Events {
			out := citytransport.OutcomeAdmit
			if o.Seq >= 3 {
				out = citytransport.OutcomeParkGap
			}
			a.Results = append(a.Results, citytransport.Result{Seq: o.Seq, Outcome: out})
		}
		return a
	})
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if next.Watermark != 2 {
		t.Fatalf("watermark must stop below the park, got %d", next.Watermark)
	}
	if rep.StoppedAt != 3 || rep.StopReason != string(citytransport.OutcomeParkGap) {
		t.Fatalf("the stop must be explicit: %+v", rep)
	}
	if rep.Parked != 3 {
		t.Fatalf("parked count must be reported, got %d", rep.Parked)
	}
}

// AC02 edge: a changed duplicate quarantines and blocks the checkpoint.
func TestChangedDuplicateBlocksCheckpoint(t *testing.T) {
	env := newTestEnv(t, newFakeLog(4, "orig"))
	st := CityState{SourceID: env.enroll.Identity.SourceID, Epoch: 1}

	next, rep, err := env.exportCycle(t, st, env.log.offers(1, 4), func(u citytransport.Upload) citytransport.Ack {
		a := citytransport.Ack{RequestID: "req", Epoch: u.Epoch}
		for _, o := range u.Events {
			out := citytransport.OutcomeAdmit
			if o.Seq == 2 {
				out = citytransport.OutcomeQuarantineConflict
			}
			a.Results = append(a.Results, citytransport.Result{Seq: o.Seq, Outcome: out})
		}
		return a
	})
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if next.Watermark != 1 {
		t.Fatalf("a conflict at seq 2 must hold the watermark at 1, got %d", next.Watermark)
	}
	if rep.Quarantined != 1 || rep.StopReason != string(citytransport.OutcomeQuarantineConflict) {
		t.Fatalf("quarantine must be explicit: %+v", rep)
	}
	// Seqs 3 and 4 were admitted by the server but are NOT contiguous with the
	// watermark, so they must not move it.
	if rep.Admitted != 3 {
		t.Fatalf("counts report what the server did (3 admits), got %d", rep.Admitted)
	}
}

// A benign duplicate is acknowledged and advances: that is what makes the
// at-least-once re-delivery after a crash safe.
func TestBenignDuplicateAdvances(t *testing.T) {
	env := newTestEnv(t, newFakeLog(3, "orig"))
	st := CityState{SourceID: env.enroll.Identity.SourceID, Epoch: 1}
	next, rep, err := env.exportCycle(t, st, env.log.offers(1, 3), func(u citytransport.Upload) citytransport.Ack {
		return ackAll(u, citytransport.OutcomeAckDuplicate)
	})
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if next.Watermark != 3 || rep.Duplicated != 3 {
		t.Fatalf("duplicates are acknowledged and advance: %+v", rep)
	}
}

// A result the server never returned stops the run — silence is not consent.
func TestMissingResultStopsCheckpoint(t *testing.T) {
	env := newTestEnv(t, newFakeLog(3, "orig"))
	st := CityState{SourceID: env.enroll.Identity.SourceID, Epoch: 1}
	next, rep, err := env.exportCycle(t, st, env.log.offers(1, 3), func(u citytransport.Upload) citytransport.Ack {
		a := citytransport.Ack{RequestID: "req", Epoch: u.Epoch}
		for _, o := range u.Events {
			if o.Seq == 2 {
				continue
			}
			a.Results = append(a.Results, citytransport.Result{Seq: o.Seq, Outcome: citytransport.OutcomeAdmit})
		}
		return a
	})
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if next.Watermark != 1 || rep.StopReason != "no_result" {
		t.Fatalf("a missing result must stop the run: %+v", rep)
	}
}

func TestUnsolicitedAckIsRejected(t *testing.T) {
	env := newTestEnv(t, newFakeLog(2, "orig"))
	st := CityState{SourceID: env.enroll.Identity.SourceID, Epoch: 1}
	_, _, err := env.exportCycle(t, st, env.log.offers(1, 2), func(u citytransport.Upload) citytransport.Ack {
		a := ackAll(u, citytransport.OutcomeAdmit)
		a.Results = append(a.Results, citytransport.Result{Seq: 99, Outcome: citytransport.OutcomeAdmit})
		return a
	})
	if !errors.Is(err, ErrUnsolicitedAck) {
		t.Fatalf("want ErrUnsolicitedAck, got %v", err)
	}
}

// A digest rejection invalidates the entire cycle, including seqs the server
// admitted before it noticed.
func TestDigestMismatchInAckBlocksAllAdvancement(t *testing.T) {
	env := newTestEnv(t, newFakeLog(3, "orig"))
	st := CityState{SourceID: env.enroll.Identity.SourceID, Epoch: 1}
	next, _, err := env.exportCycle(t, st, env.log.offers(1, 3), func(u citytransport.Upload) citytransport.Ack {
		a := citytransport.Ack{RequestID: "req", Epoch: u.Epoch}
		a.Results = append(a.Results,
			citytransport.Result{Seq: 1, Outcome: citytransport.OutcomeAdmit},
			citytransport.Result{Seq: 2, Outcome: citytransport.OutcomeContractDigestMismatch, ReasonCode: "stale_source_contract"},
			citytransport.Result{Seq: 3, Outcome: citytransport.OutcomeAdmit},
		)
		return a
	})
	if !errors.Is(err, ErrPolicyMismatch) {
		t.Fatalf("want ErrPolicyMismatch, got %v", err)
	}
	if next.Watermark != 0 {
		t.Fatalf("no seq may advance under a rejected contract, got watermark %d", next.Watermark)
	}
}
