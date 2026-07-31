package citysource

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/pkg/citytransport"
)

// advanceTo runs one clean export cycle up to seq n and returns the state.
func advanceTo(t *testing.T, env testEnv, n uint64) CityState {
	t.Helper()
	st := CityState{SourceID: env.enroll.Identity.SourceID, Epoch: 1}
	st, rep, err := env.exportCycle(t, st, env.log.offers(1, n), func(u citytransport.Upload) citytransport.Ack {
		return ackAll(u, citytransport.OutcomeAdmit)
	})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if rep.To != n {
		t.Fatalf("setup expected watermark %d, got %d", n, rep.To)
	}
	return st
}

// THE HAZARD, CASE 1 — the log is reset and the consumer's cursor is now above
// the head. Under the legacy `Seq > cursor` filter this is silent starvation:
// the consumer matches nothing and reports nothing, forever.
func TestResetBelowWatermarkIsLoudNotSilent(t *testing.T) {
	env := newTestEnv(t, newFakeLog(40, "orig"))
	st := advanceTo(t, env, 40)

	// Operator deletes .gc/events.jsonl and its archives. seq restarts at 1.
	env.log.Reset(5, "rebuilt")

	faulted, err := env.prod.Observe(st)
	if !errors.Is(err, ErrSeqRegression) {
		t.Fatalf("a reset below the watermark must be LOUD; got %v", err)
	}
	if faulted.Fault != FaultSeqRegression {
		t.Fatalf("the fault must be durable so a restart does not re-discover it: %+v", faulted)
	}
	if !strings.Contains(faulted.FaultDetail, "head 5") || !strings.Contains(faulted.FaultDetail, "watermark 40") {
		t.Fatalf("the fault detail must name the evidence: %q", faulted.FaultDetail)
	}

	// And the halt is total: nothing maps, nothing checkpoints.
	if _, err := env.prod.Map(faulted, env.set, env.log.offers(1, 5)); !errors.Is(err, ErrFaulted) {
		t.Fatalf("a faulted city must export nothing, got %v", err)
	}
	_, _, err = env.prod.Checkpoint(faulted, citytransport.Upload{}, citytransport.Ack{})
	if !errors.Is(err, ErrFaulted) {
		t.Fatalf("a faulted city must not checkpoint, got %v", err)
	}
}

// THE HAZARD, CASE 2 — the case a head-vs-cursor check CANNOT catch. The log was
// reset and has already grown back past the stale watermark, so head > watermark
// and every seq filter looks healthy. The producer would resume mid-stream on
// records it never saw the start of. Only the anchor check sees this.
func TestRebuiltLogPastWatermarkIsCaughtByAnchor(t *testing.T) {
	env := newTestEnv(t, newFakeLog(40, "orig"))
	st := advanceTo(t, env, 40)

	// Reset, then let the rebuilt log grow well past the old watermark.
	env.log.Reset(60, "rebuilt")

	head, _ := env.log.Head("alpha")
	if head <= st.Watermark {
		t.Fatalf("this test is only meaningful with head (%d) > watermark (%d)", head, st.Watermark)
	}

	faulted, err := env.prod.Observe(st)
	if !errors.Is(err, ErrLineageBreak) {
		t.Fatalf("a rebuilt log past the watermark must be caught by the anchor; got %v", err)
	}
	if faulted.Fault != FaultLineageBreak {
		t.Fatalf("want a durable lineage fault, got %+v", faulted)
	}
}

// A truncated log that lost the anchored record entirely.
func TestAnchorAbsenceIsALineageBreak(t *testing.T) {
	env := newTestEnv(t, newFakeLog(10, "orig"))
	st := advanceTo(t, env, 10)
	delete(env.log.events, st.AnchorSeq)

	faulted, err := env.prod.Observe(st)
	if !errors.Is(err, ErrLineageBreak) {
		t.Fatalf("want ErrLineageBreak, got %v", err)
	}
	if !strings.Contains(faulted.FaultDetail, "absent") {
		t.Fatalf("detail should name the absence: %q", faulted.FaultDetail)
	}
}

// A healthy log passes both checks and stays clean.
func TestHealthyLogObservesClean(t *testing.T) {
	env := newTestEnv(t, newFakeLog(10, "orig"))
	st := advanceTo(t, env, 10)
	for s := uint64(11); s <= 20; s++ {
		env.log.append(s, "orig")
	}
	next, err := env.prod.Observe(st)
	if err != nil {
		t.Fatalf("a growing healthy log must not fault: %v", err)
	}
	if next.Faulted() {
		t.Fatalf("unexpected fault: %+v", next)
	}
}

// A transient probe failure is NOT a reset. Faulting on I/O would demand an
// operator signature to recover from a disk hiccup.
func TestProbeErrorIsNotAReset(t *testing.T) {
	env := newTestEnv(t, newFakeLog(10, "orig"))
	st := advanceTo(t, env, 10)

	env.log.headErr = errors.New("EIO")
	next, err := env.prod.Observe(st)
	if err == nil {
		t.Fatal("a probe error must surface")
	}
	if errors.Is(err, ErrSeqRegression) || errors.Is(err, ErrLineageBreak) {
		t.Fatalf("an I/O error must not be classified as a reset: %v", err)
	}
	if next.Faulted() {
		t.Fatalf("an I/O error must not fault the city: %+v", next)
	}

	env.log.headErr = nil
	env.log.atErr = errors.New("EIO")
	if next, err = env.prod.Observe(st); next.Faulted() {
		t.Fatalf("an anchor read error must not fault the city: %+v (%v)", next, err)
	}
}

// A fault clears ONLY through a valid signed declaration, and clearing it zeroes
// the watermark at the new epoch.
func TestSignedResetClearsFaultAndMintsNewEpoch(t *testing.T) {
	env := newTestEnv(t, newFakeLog(40, "orig"))
	st := advanceTo(t, env, 40)
	env.log.Reset(5, "rebuilt")
	faulted, _ := env.prod.Observe(st)

	decl, err := SignReset(ResetDeclaration{
		SourceID: env.enroll.Identity.SourceID, OldEpoch: 1, NewEpoch: 2,
		Reason: ReasonSourceRebuild, DeclaredStartTS: testNow,
		Issuer: IssuerOperatorBreakglass, ContractDigest: env.enroll.PinnedSourceDigest,
	}, "contract-governance-v1", env.priv)
	if err != nil {
		t.Fatalf("SignReset: %v", err)
	}

	cleared, err := env.prod.ApplyReset(faulted, decl)
	if err != nil {
		t.Fatalf("ApplyReset: %v", err)
	}
	if cleared.Faulted() {
		t.Fatalf("a valid declaration must clear the fault: %+v", cleared)
	}
	if cleared.Epoch != 2 || cleared.Watermark != 0 || cleared.AnchorSeq != 0 {
		t.Fatalf("reset must mint epoch 2 with a zero watermark and no stale anchor: %+v", cleared)
	}
	if cleared.SourceID != st.SourceID {
		t.Fatalf("a reset changes the epoch, never the source identity: %q -> %q", st.SourceID, cleared.SourceID)
	}

	// The rebuilt log now exports from seq 1 under the new epoch.
	up, err := env.prod.Map(cleared, env.set, env.log.offers(1, 5))
	if err != nil {
		t.Fatalf("Map after reset: %v", err)
	}
	if up.Epoch != 2 || up.Events[0].Seq != 1 {
		t.Fatalf("the new epoch restarts at seq 1: %+v", up)
	}
}

// Every way a declaration can be wrong, including the load-bearing one:
// credential rotation is never a reset reason.
func TestInvalidResetDeclarationsAreRejected(t *testing.T) {
	env := newTestEnv(t, newFakeLog(40, "orig"))
	st := advanceTo(t, env, 40)
	env.log.Reset(5, "rebuilt")
	faulted, _ := env.prod.Observe(st)

	valid := ResetDeclaration{
		SourceID: env.enroll.Identity.SourceID, OldEpoch: 1, NewEpoch: 2,
		Reason: ReasonSourceRebuild, DeclaredStartTS: testNow,
		Issuer: IssuerOperatorBreakglass, ContractDigest: env.enroll.PinnedSourceDigest,
	}

	cases := map[string]func(ResetDeclaration) ResetDeclaration{
		"rotation as reason":       func(d ResetDeclaration) ResetDeclaration { d.Reason = "credential_rotation"; return d },
		"key rotation as reason":   func(d ResetDeclaration) ResetDeclaration { d.Reason = "key_rotation"; return d },
		"empty reason":             func(d ResetDeclaration) ResetDeclaration { d.Reason = ""; return d },
		"unenrolled issuer":        func(d ResetDeclaration) ResetDeclaration { d.Issuer = "some_service"; return d },
		"skips an epoch":           func(d ResetDeclaration) ResetDeclaration { d.NewEpoch = 3; return d },
		"supersedes a stale epoch": func(d ResetDeclaration) ResetDeclaration { d.OldEpoch = 0; return d },
		"foreign source":           func(d ResetDeclaration) ResetDeclaration { d.SourceID = "src_other"; return d },
		"stale contract digest":    func(d ResetDeclaration) ResetDeclaration { d.ContractDigest = "sha256:source-v0"; return d },
		"no start ts":              func(d ResetDeclaration) ResetDeclaration { d.DeclaredStartTS = time.Time{}; return d },
		"start ts far in future": func(d ResetDeclaration) ResetDeclaration {
			d.DeclaredStartTS = testNow.Add(MaxResetFutureSkew + time.Minute)
			return d
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			signed, err := SignReset(mutate(valid), "contract-governance-v1", env.priv)
			if err != nil {
				t.Fatalf("SignReset: %v", err)
			}
			out, err := env.prod.ApplyReset(faulted, signed)
			if !errors.Is(err, ErrInvalidReset) {
				t.Fatalf("want ErrInvalidReset, got %v", err)
			}
			if !out.Faulted() {
				t.Fatalf("a rejected declaration must leave the city faulted: %+v", out)
			}
		})
	}

	t.Run("unsigned", func(t *testing.T) {
		if _, err := env.prod.ApplyReset(faulted, valid); !errors.Is(err, ErrInvalidReset) {
			t.Fatalf("an unsigned declaration must be rejected, got %v", err)
		}
	})

	t.Run("tampered after signing", func(t *testing.T) {
		signed, _ := SignReset(valid, "contract-governance-v1", env.priv)
		signed.Reason = ReasonDataLoss
		if _, err := env.prod.ApplyReset(faulted, signed); !errors.Is(err, ErrInvalidReset) {
			t.Fatalf("tampering must break the signature, got %v", err)
		}
	})
}

// ROT-1: rotation changes the credential and nothing else. The producer holds no
// credential at all, which is the structural version of that guarantee — but the
// drill proves the state it does hold is untouched by a rotation cycle.
func TestCredentialRotationPreservesEverything(t *testing.T) {
	env := newTestEnv(t, newFakeLog(10, "orig"))
	st := advanceTo(t, env, 5)
	before := st

	// A rotation happens out of band, mid-stream: the transport's TokenProvider
	// returns a new bearer. Nothing in the producer's inputs changes.
	tokens := citytransport.RotatingToken("old-bearer", "new-bearer")
	for i := 0; i < 3; i++ {
		if _, err := tokens(t.Context()); err != nil {
			t.Fatalf("token: %v", err)
		}
	}

	after, err := env.prod.Observe(st)
	if err != nil {
		t.Fatalf("rotation must not disturb observation: %v", err)
	}
	if after != before {
		t.Fatalf("rotation changed durable state:\nbefore %+v\nafter  %+v", before, after)
	}

	up, err := env.prod.Map(after, env.set, env.log.offers(6, 8))
	if err != nil {
		t.Fatalf("Map after rotation: %v", err)
	}
	if up.SourceID != before.SourceID || up.Epoch != before.Epoch {
		t.Fatalf("rotation must preserve source identity and epoch: %+v", up)
	}
	if up.Events[0].Seq != 6 {
		t.Fatalf("rotation must not disturb the resume point, got seq %d", up.Events[0].Seq)
	}
}

// The credential must never reach the wire, and neither must the key id: both
// are internal evidence.
func TestNoCredentialOrKeyIDOnTheWire(t *testing.T) {
	env := newTestEnv(t, newFakeLog(3, "orig"))
	st := CityState{SourceID: env.enroll.Identity.SourceID, Epoch: 1}
	up, err := env.prod.Map(st, env.set, env.log.offers(1, 3))
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	raw, err := citytransport.EncodeUpload(up)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, banned := range []string{"key_id", "contract-governance-v1", "bearer", "Bearer", "token", "salt", "alpha"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("upload leaks %q: %s", banned, raw)
		}
	}
	// The salted city hash is the partition key; the cleartext city name is not
	// on the wire at all (it rides the path segment as CityHash).
	if !strings.Contains(string(raw), "src_city_7f2a") {
		t.Fatalf("the stable source id must be present: %s", raw)
	}
}
