package opjournal

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// FactKind names an occurrence fact. The set is closed.
type FactKind string

// The six fact kinds.
const (
	FactSessionStarted      FactKind = "session.started"
	FactSessionEnded        FactKind = "session.ended"
	FactTurnStarted         FactKind = "turn.started"
	FactTurnEnded           FactKind = "turn.ended"
	FactInteractionRaised   FactKind = "interaction.raised"
	FactInteractionResolved FactKind = "interaction.resolved"
)

var factKinds = []FactKind{
	FactSessionStarted, FactSessionEnded, FactTurnStarted,
	FactTurnEnded, FactInteractionRaised, FactInteractionResolved,
}

// FactKinds returns the six fact kinds.
func FactKinds() []FactKind { return slices.Clone(factKinds) }

// FactBasis says how a fact is known. It is X11 D8's gc.fact.basis, and it is
// the one encoding of provenance: DES-O1 §5.3's "Detail: observed" and its
// "inferred" end are both spelled Basis observed, and Detail carries only the
// provider's own word for the end. This build writes only the values below;
// a row may carry a newer build's value, which decodes as read and reads as
// unknown. Every new fact names one; empty is only read, from a row written
// before the key existed, and means unknown.
type FactBasis string

// The fact bases.
const (
	// FactBasisRuntime is an occurrence the provider recorded: a protocol
	// event or a transcript entry. The fact's At is the source's time, or
	// the observer's clock when the source gives no time.
	FactBasisRuntime FactBasis = "runtime"
	// FactBasisObserved is an occurrence gc inferred from what it saw, such
	// as a turn end read off an idle boundary. The fact's At is the
	// observer's clock.
	FactBasisObserved FactBasis = "observed"
	// FactBasisLane is an occurrence the lane itself caused where no runtime
	// record exists.
	FactBasisLane FactBasis = "lane"
)

var factBases = []FactBasis{FactBasisRuntime, FactBasisObserved, FactBasisLane}

// FactBases returns the fact bases.
func FactBases() []FactBasis { return slices.Clone(factBases) }

// Fact is one occurrence, never coalesced: a session started or ended, a turn
// started or ended, an interaction raised or resolved. Its id is keyed by the
// occurrence, so writing it twice is create-if-absent.
type Fact struct {
	ID           string
	Kind         FactKind
	Conversation string
	// Generation is the session the fact happened in; it is >= 1.
	Generation int64
	// Turn identifies the turn of a turn.* fact and is empty otherwise.
	Turn string
	// Interaction identifies the occurrence of an interaction.* fact and is
	// empty otherwise. It must identify one raising, not just the request:
	// a runtime may raise the same request id again (some derive ids from
	// content), and the fact id, which keys respond (DES-O0API §3.2), would
	// otherwise merge the second raising into the first. The producer adds a
	// per-raising discriminator to the request id.
	Interaction string
	// CauseOps are the op ids that caused the fact, if any.
	CauseOps []string
	// Reason is an open token, such as a session.ended reason or a
	// turn.ended reason (TurnEndReason).
	Reason string
	// Basis says how the fact is known; see FactBasis. Like Reason and
	// Detail it is not part of the fact's identity: the first writer of an
	// occurrence decides it.
	Basis FactBasis
	// Detail is an optional open token the source gave for the occurrence,
	// such as a provider stop reason (end_turn, max_tokens, refusal). It is
	// never a provenance word; that is Basis.
	Detail string
	At     time.Time
}

// Fact metadata keys.
const (
	keyFactSchema       = "gc.fact.schema"
	keyFactKind         = "gc.fact.kind"
	keyFactConversation = "gc.fact.conversation"
	keyFactGeneration   = "gc.fact.generation"
	keyFactTurn         = "gc.fact.turn"
	keyFactInteraction  = "gc.fact.interaction"
	keyFactCauseOps     = "gc.fact.cause_ops"
	keyFactReason       = "gc.fact.reason"
	keyFactBasis        = "gc.fact.basis"
	keyFactDetail       = "gc.fact.detail"
	keyFactAt           = "gc.fact.at"
)

// occurrence returns the turn or interaction that, with the kind and
// generation, identifies the occurrence.
func (f Fact) occurrence() string {
	switch {
	case strings.HasPrefix(string(f.Kind), "turn."):
		return f.Turn
	case strings.HasPrefix(string(f.Kind), "interaction."):
		return f.Interaction
	default:
		return ""
	}
}

// validateIdentity checks the fields that key the fact.
func (f Fact) validateIdentity() error {
	if !slices.Contains(factKinds, f.Kind) {
		return fmt.Errorf("%w: fact kind %q", ErrUnsupportedKind, f.Kind)
	}
	if err := validateConversation(f.Conversation); err != nil {
		return err
	}
	if f.Generation < 1 {
		return invalidf("fact generation %d must be >= 1", f.Generation)
	}
	isTurn := strings.HasPrefix(string(f.Kind), "turn.")
	isInteraction := strings.HasPrefix(string(f.Kind), "interaction.")
	if isTurn {
		if err := validateText("fact turn", f.Turn, maxClientKeyLen); err != nil {
			return err
		}
	} else if f.Turn != "" {
		return invalidf("%s facts carry no turn", f.Kind)
	}
	if isInteraction {
		if err := validateText("fact interaction", f.Interaction, maxClientKeyLen); err != nil {
			return err
		}
	} else if f.Interaction != "" {
		return invalidf("%s facts carry no interaction", f.Kind)
	}
	return nil
}

// FactID returns the pinned id of f in the namespace prefix:
// <prefix>-fact-<hash16(conversation, kind, generation, turn|interaction)>.
func FactID(prefix string, f Fact) (string, error) {
	if err := validatePrefix(prefix); err != nil {
		return "", err
	}
	if err := f.validateIdentity(); err != nil {
		return "", err
	}
	return prefix + factIDInfix + factIDSuffix(f), nil
}

func factIDSuffix(f Fact) string {
	return hash16(factIDTag, f.Conversation, string(f.Kind), strconv.FormatInt(f.Generation, 10), f.occurrence())
}

// EncodeFact renders f as its bead row under prefix. Every new fact names its
// basis. Fact rows are closed on create; a fact is an occurrence, not a
// request.
func EncodeFact(prefix string, f Fact) (beads.Bead, error) {
	id, err := FactID(prefix, f)
	if err != nil {
		return beads.Bead{}, err
	}
	if f.ID != "" && f.ID != id {
		return beads.Bead{}, invalidf("fact id %q does not match its occurrence (%s)", f.ID, id)
	}
	if err := validateFactBody(f); err != nil {
		return beads.Bead{}, err
	}
	if !slices.Contains(factBases, f.Basis) {
		return beads.Bead{}, invalidf("fact basis %q is not one of %v", f.Basis, factBases)
	}
	meta := map[string]string{
		keyFactSchema:       strconv.Itoa(SchemaRev),
		keyFactKind:         string(f.Kind),
		keyFactConversation: f.Conversation,
		keyFactGeneration:   strconv.FormatInt(f.Generation, 10),
		keyFactTurn:         f.Turn,
		keyFactInteraction:  f.Interaction,
		keyFactCauseOps:     strings.Join(f.CauseOps, ","),
		keyFactReason:       f.Reason,
		keyFactBasis:        string(f.Basis),
		keyFactDetail:       f.Detail,
		keyFactAt:           formatTime(f.At),
	}
	for k, v := range meta {
		if v == "" {
			delete(meta, k)
		}
	}
	return beads.Bead{
		ID:        id,
		Title:     "fact " + string(f.Kind) + " " + f.Conversation,
		Status:    beadStatusClosed,
		Type:      BeadType,
		Labels:    []string{LabelFact, FactConversationLabel(f.Conversation)},
		Metadata:  meta,
		NoHistory: true,
	}, nil
}

func validateFactBody(f Fact) error {
	if f.At.IsZero() {
		return invalidf("fact time is zero")
	}
	for _, id := range f.CauseOps {
		if !validOpID(id) {
			return invalidf("fact cause %q is not an op id", id)
		}
	}
	if f.Reason != "" {
		if err := validateToken("fact reason", f.Reason); err != nil {
			return err
		}
	}
	if f.Basis != "" {
		if err := validateToken("fact basis", string(f.Basis)); err != nil {
			return err
		}
	}
	if f.Detail != "" {
		return validateToken("fact detail", f.Detail)
	}
	return nil
}

// DecodeFact reads a fact row. An unknown kind is ErrUnsupportedKind; a
// malformed row, or one whose id is not <prefix>-fact-<hash of its occurrence>
// under a valid prefix, is ErrCorruptOp. A basis a newer build added is kept
// as read: it is not part of the fact's identity.
func DecodeFact(b beads.Bead) (Fact, error) {
	if b.Type != BeadType || !slices.Contains(b.Labels, LabelFact) {
		return Fact{}, corruptf(b.ID, "not a fact row (type %q, labels %v)", b.Type, b.Labels)
	}
	d := metaDecoder{id: b.ID, meta: b.Metadata}
	if err := d.schema(keyFactSchema); err != nil {
		return Fact{}, err
	}
	kind := FactKind(d.required(keyFactKind))
	if d.err != nil {
		return Fact{}, d.err
	}
	if !slices.Contains(factKinds, kind) {
		return Fact{}, fmt.Errorf("%w: row %s has fact kind %q", ErrUnsupportedKind, b.ID, kind)
	}
	f := Fact{
		ID:           b.ID,
		Kind:         kind,
		Conversation: d.required(keyFactConversation),
		Generation:   d.int64(keyFactGeneration),
		Turn:         d.meta[keyFactTurn],
		Interaction:  d.meta[keyFactInteraction],
		Reason:       d.meta[keyFactReason],
		Basis:        FactBasis(d.meta[keyFactBasis]),
		Detail:       d.meta[keyFactDetail],
		At:           d.time(keyFactAt),
	}
	if causes := d.meta[keyFactCauseOps]; causes != "" {
		f.CauseOps = strings.Split(causes, ",")
	}
	if d.err != nil {
		return Fact{}, d.err
	}
	if err := f.validateIdentity(); err != nil {
		return Fact{}, corruptf(b.ID, "%v", err)
	}
	if err := validateFactBody(f); err != nil {
		return Fact{}, corruptf(b.ID, "%v", err)
	}
	if !slices.Contains(b.Labels, FactConversationLabel(f.Conversation)) {
		return Fact{}, corruptf(b.ID, "missing label %s", FactConversationLabel(f.Conversation))
	}
	prefix, ok := strings.CutSuffix(b.ID, factIDInfix+factIDSuffix(f))
	if !ok {
		return Fact{}, corruptf(b.ID, "id does not match its occurrence")
	}
	if err := validatePrefix(prefix); err != nil {
		return Fact{}, corruptf(b.ID, "id prefix: %v", err)
	}
	return f, nil
}
