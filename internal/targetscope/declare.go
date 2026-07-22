// Member declaration: the single-winner, immutable, pre-launch commit of a
// member's target scope.
//
// WHY A MEMBER NEEDS ITS OWN DECLARATION. When a plain task T is launched as a
// targeted graph workflow, the clean resolved scope reaches the generated
// STAGES through the root-chain walk, but it does not reach T. Normalization
// creates a separate convoy that merely TRACKS T; the targeted-graph branch
// passes an empty source ID, so no back-link from T to the root is ever
// written; and the inherited walk runs forward only, never in reverse along
// convoy tracks. Meanwhile the close gate loads T ITSELF. So without a member
// declaration, a freshly-launched scoped workflow still lets T's stale flat
// keys be behaviorally authoritative — the exact poison the object exists to
// kill. Stamping the scope onto T directly makes T authoritative with no
// reverse walk anywhere.
//
// WHY SINGLE-WINNER EXCLUSION IS MANDATORY. SetMetadata is an UNCONDITIONAL
// assignment on every backend. Two launches A and B that both read "absent" can
// write different valid scopes X and Y in the order absent → X → Y, each
// re-reading and seeing its own last write, and BOTH proceed — a two-winner
// execution. Re-reading after an unconditional write can never turn
// absent → value into an atomic transition, so there is deliberately NO
// unfenced write path here: every declaration commits its read → decide → write
// under a compare-and-set.
//
// PHASE ORDERING replaces the cross-store transaction we cannot have. A member
// lives in its owning store while the workflow root lives in the graph store,
// and no store-local transaction spans that seam. So all member declarations
// commit BEFORE any root is materialized: a rejected or failed launch leaves
// only immutable-correct member pins and no root or stages — nothing new to
// orphan.
//
// PINS ARE DELIBERATE AND ARE NOT ROLLED BACK. A launch that rejects after some
// members were declared leaves those declarations in place. They are safe not
// because they are unobservable — a pinned member is a pre-existing Ready()
// bead whose own dispatch reads the pin — but because the protocol only ever
// commits the trusted-resolved value or rejects. A retry re-resolves the same
// scope and no-ops. A retry that deliberately RETARGETS the member is rejected
// by the immutability rule: the first declaration permanently binds the member,
// even if no launch ever commits. Recovery from a deliberately-wrong first
// declaration is an administrative act, because a blind rollback would
// reintroduce the lost-update race this protocol exists to close.
package targetscope

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// Outcome reports what a declaration did.
type Outcome int

const (
	// OutcomeNoop means the member already carried an equal valid scope.
	// Re-declaration is idempotent, which is what makes a retry after a
	// partially-declared launch safe.
	OutcomeNoop Outcome = iota
	// OutcomeCommitted means this declarer won the compare-and-set.
	OutcomeCommitted
)

func (o Outcome) String() string {
	if o == OutcomeCommitted {
		return "committed"
	}
	return "noop"
}

var (
	// ErrDeclarationConflict reports that the member already carries a
	// DIFFERENT valid scope. The launch must be rejected before any root is
	// materialized. It is not retryable: member scope is immutable.
	ErrDeclarationConflict = errors.New("target scope already declared with a different value")

	// ErrDeclarationUnsupported reports that the member's store offers no
	// metadata compare-and-set. The launch is rejected rather than served by
	// an unserialized write.
	ErrDeclarationUnsupported = errors.New("member store cannot declare a target scope: no metadata compare-and-set capability")

	// ErrDeclarationRace reports that a concurrent declarer committed first
	// and its value is not ours. The loser rejects, fail-closed.
	ErrDeclarationRace = errors.New("target scope declared concurrently with a different value")
)

// DeclareStore is the store surface the protocol needs: read the member, and
// compare-and-set one metadata key on it.
type DeclareStore interface {
	Get(id string) (beads.Bead, error)
}

// Declare commits scope onto member under a single-winner exclusion.
//
// The mechanism is chosen by the store's CAPABILITY, never by the operator's
// conditional-writes mode. That mode encodes a policy about general fenced
// updates — a different concern with a legacy path to fall back to. A member
// declaration has no legacy path: the compare-and-set is its only correct
// implementation, so gating it on a rollout flag would leave it with nothing
// under the default. Capability is fixed per store, so every concurrent
// declarer of one member uses the same mechanism and no mixed-mechanism race
// is possible.
//
// Returns OutcomeNoop when the member already carries an equal scope, and an
// error the caller must treat as a LAUNCH REJECTION in every other failing
// case.
func Declare(store DeclareStore, memberID string, scope Scope) (Outcome, error) {
	if memberID == "" {
		return OutcomeNoop, fmt.Errorf("targetscope: declare requires a member id")
	}
	blob, err := Marshal(scope)
	if err != nil {
		return OutcomeNoop, err
	}
	// The narrow metadata-CAS capability, NOT ResolveConditionalWriter. The
	// latter deliberately refuses on stores whose revision fence is unsound,
	// and that refusal must stay intact — a single-key value-CAS needs no
	// revision token, so it is available on stores where the fenced trio is
	// not.
	casStore, ok := store.(beads.Store)
	if !ok {
		return OutcomeNoop, ErrDeclarationUnsupported
	}
	writer, ok := beads.MetadataCASWriterFor(casStore)
	if !ok {
		return OutcomeNoop, fmt.Errorf("%w: member %s", ErrDeclarationUnsupported, memberID)
	}

	current, err := readDeclared(store, memberID)
	if err != nil {
		return OutcomeNoop, err
	}
	if decided, outcome, err := decideAgainst(current, scope, memberID); decided {
		return outcome, err
	}

	// Absent → declare. expected is the EXACT string read above: an empty
	// expected matches a key that is absent or present-but-empty, which are
	// indistinguishable to callers and are both "not yet declared" here.
	declareRaceBarrier()
	swapped, err := writer.CompareAndSetMetadataKey(memberID, beadmeta.TargetScopeMetadataKey, current.Raw, blob)
	if err != nil {
		// Includes stores that report conditional writes unsupported at the
		// instance level (a read-only store, for one). Fail closed.
		return OutcomeNoop, fmt.Errorf("declaring target scope on %s: %w", memberID, err)
	}
	if swapped {
		return OutcomeCommitted, nil
	}

	// (false, nil) is a LOST RACE, not an error: another declarer moved the
	// value between our read and our write. Re-read and re-apply the same
	// decision. Converging on an equal value is benign; anything else — a
	// different scope, a corrupt one, or a value that vanished again — rejects.
	current, err = readDeclared(store, memberID)
	if err != nil {
		return OutcomeNoop, err
	}
	if decided, outcome, err := decideAgainst(current, scope, memberID); decided {
		return outcome, err
	}
	return OutcomeNoop, fmt.Errorf("%w: member %s lost the declaration race but shows no declared scope on re-read", ErrDeclarationRace, memberID)
}

// StampMemberScope declares a member scope with a plain, unfenced write, for the
// specific callers whose single-writer exclusion comes from an EXTERNAL lock
// rather than a store compare-and-set — a drain item's ItemRootKey lock, for
// one. It exists so those callers are not FAIL-CLOSED when their store forwards
// reads and writes but not the metadata-CAS capability (a decorating store, a
// backend without value-CAS): the launch is served by a guarded stamp instead of
// rejected.
//
// It is deliberately NOT a fallback inside Declare. Declare stays CAS-only with
// no unfenced write path, because the general caller has no external lock and an
// unfenced absent->value write there would reintroduce the lost-update race the
// protocol exists to close. A caller reaches for this ONLY when it can name the
// lock that serializes it.
//
// The IMMUTABILITY guard is preserved EXPLICITLY — it is the enforcement the CAS
// would otherwise carry, and dropping the CAS without it would ship a silent
// retarget. A present-valid-different member rejects, a present-valid-equal
// member no-ops, an invalid one rejects, and only an absent member is written.
// The ONLY thing given up versus Declare is the atomic absent->value transition,
// which the caller's lock and a deterministic per-member scope make safe.
func StampMemberScope(store beads.Store, memberID string, scope Scope) (Outcome, error) {
	if memberID == "" {
		return OutcomeNoop, fmt.Errorf("targetscope: stamp requires a member id")
	}
	blob, err := Marshal(scope)
	if err != nil {
		return OutcomeNoop, err
	}
	current, err := readDeclared(store, memberID)
	if err != nil {
		return OutcomeNoop, err
	}
	if decided, outcome, err := decideAgainst(current, scope, memberID); decided {
		return outcome, err
	}
	if err := store.SetMetadata(memberID, beadmeta.TargetScopeMetadataKey, blob); err != nil {
		return OutcomeNoop, fmt.Errorf("stamping target scope on %s: %w", memberID, err)
	}
	return OutcomeCommitted, nil
}

// raceBarrier, when set, runs between the read and the compare-and-set.
//
// It exists because the interleaving that makes an unconditional write unsafe
// is the one the Go scheduler almost never produces on its own: with a fast
// in-memory store, one declarer typically finishes entirely before the next
// one reads, so the SECOND declarer's initial read catches the conflict and the
// compare-and-set is never actually exercised. A race test without this hook
// passes just as happily against a plainly-unsafe unconditional write, which
// makes it worse than no test at all.
//
// Production cost is a nil check per declaration.
var raceBarrier func()

func declareRaceBarrier() {
	if raceBarrier != nil {
		raceBarrier()
	}
}

// readDeclared loads the member and parses its current declaration.
func readDeclared(store DeclareStore, memberID string) (Resolution, error) {
	bead, err := store.Get(memberID)
	if err != nil {
		return Resolution{}, fmt.Errorf("reading target scope of %s: %w", memberID, err)
	}
	return Parse(bead.Metadata[beadmeta.TargetScopeMetadataKey]), nil
}

// decideAgainst applies the immutability rule to an observed declaration.
//
// decided=false means "absent — go declare"; every other state is terminal.
func decideAgainst(current Resolution, want Scope, memberID string) (decided bool, outcome Outcome, err error) {
	switch current.State {
	case StateValid:
		if current.Scope.Equal(want) {
			return true, OutcomeNoop, nil
		}
		return true, OutcomeNoop, fmt.Errorf("%w: member %s is declared %s, launch resolved %s",
			ErrDeclarationConflict, memberID, describe(current.Scope), describe(want))
	case StateInvalid:
		// Never overwrite a corrupt object: we cannot tell whether it encodes
		// an intent we would be destroying.
		return true, OutcomeNoop, fmt.Errorf("member %s carries an unusable target scope: %w", memberID, current.Reason)
	default:
		return false, OutcomeNoop, nil
	}
}

func describe(s Scope) string {
	branch := s.Branch
	if branch == "" {
		branch = "<unknown>"
	}
	worktree := s.Worktree
	if worktree == "" {
		worktree = "<unknown>"
	}
	return fmt.Sprintf("{branch:%s worktree:%s}", branch, worktree)
}

// Member pairs a member id with the scope resolved specifically FOR it.
//
// Scopes are per member on purpose. A heterogeneous convoy has no honest single
// branch/worktree, and computing one winner across members by scanning them
// would invent an authority that does not exist.
type Member struct {
	ID    string
	Store DeclareStore
	Scope Scope
}

// DeclareAll runs the declaration phase for every resolved member.
//
// It is the caller's contract that this completes successfully BEFORE the root
// is materialized. On the first rejection it stops and returns the error, and
// the caller must abandon the launch: declarations already committed stay
// pinned (immutable-correct and retry-reusable), and because no root exists
// yet, no claimable graph work has been orphaned.
func DeclareAll(members []Member) error {
	for _, m := range members {
		if _, err := Declare(m.Store, m.ID, m.Scope); err != nil {
			return err
		}
	}
	return nil
}
