package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The incarnation epoch (I-MINT): every call that creates a runtime process
// for a session row is preceded, under that row's runtime lease, by one CAS
// that advances generation to g+1 (g read in the same attempt), mints a fresh
// instance_token, stamps last_woke_at, and carries forward or bumps
// continuation_epoch. The new process's identity env is built only from the
// minted value, so two processes never share (id, generation). The
// generation is GC_RUNTIME_EPOCH and the operation journal's session; the
// incarnation's digest is the @gc_fence a tmux pane publishes (Fence).

// Incarnation is the identity a minted start runs under. Only MintIncarnation
// and a new row's create construct one (unexported fields; a test binary also
// has RecordedIncarnationForTest), so a start cannot be handed an identity it
// did not mint.
type Incarnation struct {
	ID                string // session row id
	Generation        int64  // ≥ 1; GC_RUNTIME_EPOCH
	ContinuationEpoch int64  // GC_CONTINUATION_EPOCH
	token             string // GC_INSTANCE_TOKEN; never logged, never in argv
	MintedAt          time.Time
	By                ActorKind
	minted            bool
	// freshContext is whether the mint opened a fresh context: it bumped the
	// continuation epoch and consumed the row's pending reset.
	freshContext bool
	// prior is what the mint replaced, its identity keys as the row held
	// them: the withdrawal of a mint no process ran under (unmintIncarnation).
	prior MetadataPatch
}

// Valid reports whether i was minted and names a whole incarnation.
func (i Incarnation) Valid() bool {
	return i.minted && i.ID != "" && i.Generation >= 1 && i.ContinuationEpoch >= 1 && i.token != ""
}

// Fence is the input fence of the runtime i starts.
func (i Incarnation) Fence() runtime.InputFence {
	return runtime.InputFence{Generation: i.Generation, Epoch: i.ContinuationEpoch, Token: i.token}
}

// Env is i's identity environment: GC_RUNTIME_EPOCH, GC_CONTINUATION_EPOCH,
// GC_INSTANCE_TOKEN and BEADS_HOLDER_TOKEN, as RuntimeEnv publishes them.
func (i Incarnation) Env() map[string]string {
	return map[string]string{
		runtime.RuntimeEpochEnv:      strconv.FormatInt(i.Generation, 10),
		runtime.ContinuationEpochEnv: strconv.FormatInt(i.ContinuationEpoch, 10),
		runtime.InstanceTokenEnv:     i.token,
		"BEADS_HOLDER_TOKEN":         i.token,
	}
}

// String shows the session, generation, epoch and fence digest, never the
// token.
func (i Incarnation) String() string {
	return fmt.Sprintf("incarnation(%s %s)", i.ID, i.Fence())
}

// Format prints String under every verb, so no verb reaches the token
// through the struct's fields.
func (i Incarnation) Format(s fmt.State, _ rune) { _, _ = fmt.Fprint(s, i.String()) }

// recordedBy reports whether row metadata still records i: its generation
// and its token.
func (i Incarnation) recordedBy(meta map[string]string) bool {
	return strings.TrimSpace(meta["generation"]) == strconv.FormatInt(i.Generation, 10) &&
		strings.TrimSpace(meta["instance_token"]) == i.token
}

// withContinuationEpoch is i publishing epoch instead: the legacy bridge's
// late continuation reset (ensureRunningRuntimeOnly).
func (i Incarnation) withContinuationEpoch(epoch int) Incarnation {
	i.ContinuationEpoch = int64(epoch)
	return i
}

// IdentityPatch is the row write that records i, as MintIncarnation writes
// it: generation, instance_token, continuation_epoch and last_woke_at, and
// the continuation reset consumed when the mint opened a fresh context. A
// caller folds it onto its snapshot of the row.
func (i Incarnation) IdentityPatch() MetadataPatch {
	patch := MetadataPatch{
		"generation":         strconv.FormatInt(i.Generation, 10),
		"instance_token":     i.token,
		"continuation_epoch": strconv.FormatInt(i.ContinuationEpoch, 10),
		"last_woke_at":       i.MintedAt.UTC().Format(time.RFC3339),
	}
	if i.freshContext {
		patch["continuation_reset_pending"] = ""
	}
	return patch
}

// createdIncarnation is the incarnation a new row is created with: the
// create is its mint (create-if-absent, generation 1).
func createdIncarnation(id, token string, by ActorKind, now time.Time) Incarnation {
	return Incarnation{ID: id, Generation: DefaultGeneration, ContinuationEpoch: DefaultContinuationEpoch, token: token, MintedAt: now, By: by, minted: true}
}

// RecordedIncarnationForTest is the incarnation info's row records, as a
// test hands a start whose PreWake its fixture already wrote. It panics
// outside a test binary: production starts run only under a minted one.
func RecordedIncarnationForTest(info Info) Incarnation {
	if !testing.Testing() {
		panic("session: RecordedIncarnationForTest outside a test")
	}
	f, _ := InputFenceFor(info) // zero, so not Valid, for a row with no token
	return Incarnation{ID: info.ID, Generation: f.Generation, ContinuationEpoch: f.Epoch, token: f.Token, By: ActorController, minted: true}
}

// MintRequest is one mint: the row its caller decided from, who mints, and
// the extra keys the same CAS lands.
type MintRequest struct {
	Expect Info // the row the caller decided from; its lifecycle facts are the premise
	By     ActorKind
	Now    time.Time
	// Patch returns extra keys landing in the same CAS, decided on that
	// attempt's read: PreWakePatch's for the legacy start; nil for the
	// Manager's starts. The mint's identity keys win over its own.
	Patch func(fresh Info, inc Incarnation) MetadataPatch
	// FreshContext reports whether the mint opens a fresh context, decided
	// on that attempt's read as Patch is: one bumps continuation_epoch and
	// consumes a pending continuation reset (a fresh wake, or the reset the
	// row records). nil opens none.
	FreshContext func(fresh Info) bool
}

var (
	// ErrMintSuperseded refuses a mint whose row moved since it was read:
	// its lifecycle facts, generation, token or lease record.
	ErrMintSuperseded = errors.New("mint refused: the row moved since it was read")
	// ErrMintNoCAS refuses a mint on a store that cannot fence: a
	// require-mode store, as every v2 city runs, without conditional writes.
	// A store with conditional writes off (a legacy city) warns once and
	// writes blind, as the runtime lease does.
	ErrMintNoCAS = errors.New("mint refused: the session store cannot fence")
	// ErrMintClosed refuses a mint on a closed row.
	ErrMintClosed = errors.New("mint refused: the session row is closed")
	// ErrIncarnationNotMinted refuses a start or confirm handed an
	// incarnation no mint made, or one of another row.
	ErrIncarnationNotMinted = errors.New("session: the incarnation was not minted for this row")
)

// mintAttempts bounds a mint's lost revision fences.
const mintAttempts = 3

// mintNoCASWarned latches MintIncarnation's one warning for a store with
// conditional writes off.
var mintNoCASWarned atomic.Bool

// MintIncarnation advances the row's generation and mints a token by one
// UpdateIfMatch at a fresh read's revision, under lease. Each attempt
// re-reads, re-checks Expect's lifecycle facts and Expect's generation and
// token and that the row still records lease (mintPremiseHolds), and
// computes g+1 from that read. Up to three attempts; a fence lost every time
// is ErrMintSuperseded. A nil lease checks none, as ApplyPatchIfLifecycleUnchangedUnder.
// ctx only ends the attempts early: a done ctx writes nothing and returns its
// error.
func (s *Store) MintIncarnation(ctx context.Context, lease *RuntimeLease, req MintRequest) (Incarnation, error) {
	id := req.Expect.ID
	switch {
	case id == "" || req.Now.IsZero():
		return Incarnation{}, fmt.Errorf("minting session %q: a row and a time are required", id)
	case lease != nil && lease.id != "" && lease.id != id:
		return Incarnation{}, fmt.Errorf("minting session %q: the lease is on session %q", id, lease.id)
	}
	writer, diag, err := beads.ResolveConditionalWriter(s.store)
	if err != nil {
		return Incarnation{}, fmt.Errorf("%w: session %q: %w", ErrMintNoCAS, id, err)
	}
	if writer == nil {
		warnMintNoCAS(diag)
	}
	for attempt := 0; attempt < mintAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Incarnation{}, fmt.Errorf("minting session %q: %w", id, err)
		}
		if lease != nil && !lease.HeldAt(lease.clock()) { // released or lapsed: the lease moved
			return Incarnation{}, fmt.Errorf("%w: %s: %w", ErrMintSuperseded, id, ErrRuntimeLeaseLost)
		}
		bead, err := s.freshBead(id)
		if err != nil {
			return Incarnation{}, err
		}
		if bead.Status == "closed" {
			return Incarnation{}, fmt.Errorf("%w: %s", ErrMintClosed, id)
		}
		fresh := infoFromPersistedBead(bead)
		if !mintPremiseHolds(req.Expect, fresh, lease, bead.Metadata) {
			return Incarnation{}, fmt.Errorf("%w: %s", ErrMintSuperseded, id)
		}
		inc := nextIncarnation(fresh, req)
		inc.prior = MetadataPatch{}
		for k := range inc.IdentityPatch() {
			inc.prior[k] = bead.Metadata[k]
		}
		patch := MetadataPatch{}
		if req.Patch != nil {
			maps.Copy(patch, req.Patch(fresh, inc))
		}
		maps.Copy(patch, inc.IdentityPatch())
		if writer == nil {
			if err := s.ApplyPatch(id, patch); err != nil {
				UnmintDisposition(id, patch)
				return Incarnation{}, fmt.Errorf("minting session %q: %w", id, err)
			}
			return inc, nil
		}
		err = writer.UpdateIfMatch(id, bead.Revision, beads.UpdateOpts{Metadata: map[string]string(patch)})
		if err != nil {
			UnmintDisposition(id, patch) // the write did not land
		}
		switch {
		case err == nil:
			return inc, nil
		case errors.Is(err, beads.ErrConditionalWriteUnsupported):
			return Incarnation{}, fmt.Errorf("%w: session %q: %w", ErrMintNoCAS, id, err)
		case !beads.IsPreconditionFailed(err):
			return Incarnation{}, fmt.Errorf("minting session %q: %w", id, err)
		}
	}
	return Incarnation{}, fmt.Errorf("%w: %s: lost the revision fence %d times", ErrMintSuperseded, id, mintAttempts)
}

// nextIncarnation is the incarnation after fresh's: generation g+1 from this
// read (a missing or malformed generation reads 0, as PreWake reads it), a
// new token, and the continuation epoch carried or, for a context
// req.FreshContext opens on this read, bumped.
func nextIncarnation(fresh Info, req MintRequest) Incarnation {
	gen, err := strconv.ParseInt(strings.TrimSpace(fresh.Generation), 10, 64)
	if err != nil || gen < 0 {
		gen = 0
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(fresh.ContinuationEpoch), 10, 64)
	if err != nil || epoch <= 0 {
		epoch = DefaultContinuationEpoch
	}
	freshContext := req.FreshContext != nil && req.FreshContext(fresh)
	if freshContext {
		epoch++
	}
	return Incarnation{
		ID: fresh.ID, Generation: gen + 1, ContinuationEpoch: epoch, token: NewInstanceToken(),
		MintedAt: req.Now.UTC().Truncate(time.Second), By: req.By, minted: true, freshContext: freshContext,
	}
}

// clock is the lease's own clock, the wall clock when it has none.
func (l *RuntimeLease) clock() time.Time {
	if l.now == nil {
		return time.Now()
	}
	return l.now()
}

func warnMintNoCAS(diag *beads.BeadsDiagnostic) {
	if mintNoCASWarned.Swap(true) {
		return
	}
	reason := "conditional_writes is off"
	if diag != nil {
		reason = diag.PreflightReason
	}
	log.Printf("WARNING: session mint: the session store has no conditional writes (%s); "+
		"a start's incarnation mint is checked by a re-read only", reason)
}

// ConfirmIncarnation lands a start's confirm patch only while the row still
// records inc (generation and token), fenced at a fresh read's revision; a
// lost fence re-reads, up to three times. It replaces the blind confirm
// writes. False means the row no longer records inc (a newer mint, or the
// row closed) and nothing was written. Without conditional writes the patch
// is written after the re-read check, as ApplyPatchIfLifecycleUnchanged.
func (s *Store) ConfirmIncarnation(inc Incarnation, patch MetadataPatch) (bool, error) {
	if !inc.Valid() {
		return false, fmt.Errorf("confirming session %q: %w", inc.ID, ErrIncarnationNotMinted)
	}
	if len(patch) == 0 { // nothing to write: whether the row still records inc
		b, err := s.freshBead(inc.ID)
		if err != nil {
			return false, err
		}
		return b.Status != "closed" && inc.recordedBy(b.Metadata), nil
	}
	res, err := s.commitIf(inc.ID, patch, nil, startCommitMaxAttempts, s.freshBead, func(b beads.Bead) bool {
		return inc.recordedBy(b.Metadata)
	})
	switch {
	case err != nil:
		return false, err
	case res == CommitContended:
		return false, fmt.Errorf("confirming session %q: lost the revision fence %d times", inc.ID, startCommitMaxAttempts)
	}
	return res == CommitLanded, nil
}

// unmintIncarnation withdraws a mint no process ran under (a start that
// converged on a runtime already up, or failed before or at its launch): it
// writes back what the mint replaced (the generation, token, continuation
// epoch, last_woke_at and, where the mint consumed one, the reset marker),
// fenced like ConfirmIncarnation and only while the row still records inc.
// False means the row moved on and nothing was written. A legacy PreWake's
// own keys (MintRequest.Patch) are its caller's to undo.
func (s *Store) unmintIncarnation(inc Incarnation) (bool, error) {
	if !inc.Valid() || inc.prior == nil {
		return false, fmt.Errorf("withdrawing session %q: %w", inc.ID, ErrIncarnationNotMinted)
	}
	res, err := s.commitIf(inc.ID, inc.prior, nil, startCommitMaxAttempts, s.freshBead, func(b beads.Bead) bool {
		return inc.recordedBy(b.Metadata)
	})
	switch {
	case err != nil:
		return false, err
	case res == CommitContended:
		return false, fmt.Errorf("withdrawing session %q: lost the revision fence %d times", inc.ID, startCommitMaxAttempts)
	}
	return res == CommitLanded, nil
}
