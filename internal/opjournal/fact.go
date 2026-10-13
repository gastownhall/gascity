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
	// Interaction identifies the request of an interaction.* fact and is
	// empty otherwise.
	Interaction string
	// CauseOps are the op ids that caused the fact, if any.
	CauseOps []string
	// Reason is an open token, such as a session.ended reason.
	Reason string
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

// EncodeFact renders f as its bead row under prefix. Fact rows are closed on
// create; a fact is an occurrence, not a request.
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
	meta := map[string]string{
		keyFactSchema:       strconv.Itoa(SchemaRev),
		keyFactKind:         string(f.Kind),
		keyFactConversation: f.Conversation,
		keyFactGeneration:   strconv.FormatInt(f.Generation, 10),
		keyFactTurn:         f.Turn,
		keyFactInteraction:  f.Interaction,
		keyFactCauseOps:     strings.Join(f.CauseOps, ","),
		keyFactReason:       f.Reason,
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
		return validateToken("fact reason", f.Reason)
	}
	return nil
}

// DecodeFact reads a fact row. An unknown kind is ErrUnsupportedKind; a
// malformed row, or one whose id is not <prefix>-fact-<hash of its occurrence>
// under a valid prefix, is ErrCorruptOp.
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
