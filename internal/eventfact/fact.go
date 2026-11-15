package eventfact

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// Row shape.
const (
	// BeadType is the fact row's bead type: the journal row type.
	BeadType = opjournal.BeadType
	// Label marks an event-fact row.
	Label = "gc:event-fact"
	// FactSchemaRev is the fact-row schema revision this build writes. It
	// reads every revision from 1 up to it (doc.go, "Schema evolution").
	FactSchemaRev = 1

	labelSessionPrefix = "gc:event-fact-conv:"
	idFamily           = "ev"
	idTag              = "gascity/event-fact/v1"
	beadStatusClosed   = "closed"
)

// Field limits, in bytes.
const (
	// MaxPayload is the largest compacted payload a fact carries. A larger
	// one is omitted and its size recorded (Fact.PayloadDropped).
	MaxPayload = 16 << 10
	// MaxMessage bounds Event.Message.
	MaxMessage = 8 << 10

	maxSessionLen   = 200
	maxTypeLen      = 200
	maxActorLen     = 256
	maxSubjectLen   = 512
	maxIDFieldLen   = 256
	maxDependsOnLen = 8 << 10
)

// SessionLabel is the per-session key label of fact rows.
func SessionLabel(session string) string { return labelSessionPrefix + session }

// OriginKind says what kind of writer an origin is.
type OriginKind string

// The origin kinds.
const (
	// OriginProcess is one gc process writing its own facts (the direct
	// path).
	OriginProcess OriginKind = "proc"
	// OriginDaemon is one box daemon incarnation batching the facts of every
	// process in the box (BX-4b).
	OriginDaemon OriginKind = "daemon"
)

var nonceRe = regexp.MustCompile(`^[a-z0-9]{8,64}$`)

// Origin is one writer of facts: a kind and a nonce the writer drew when it
// started. Fact.N counts that writer's facts.
type Origin struct {
	Kind  OriginKind
	Nonce string
}

// NewOrigin draws a fresh origin of kind: a 128-bit random nonce, so two
// writers of one session and generation never share an origin.
func NewOrigin(kind OriginKind) (Origin, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Origin{}, fmt.Errorf("eventfact: drawing an origin nonce: %w", err)
	}
	o := Origin{Kind: kind, Nonce: hex.EncodeToString(b[:])}
	if err := o.Validate(); err != nil {
		return Origin{}, err
	}
	return o, nil
}

// ParseOrigin reads <kind>:<nonce>.
func ParseOrigin(s string) (Origin, error) {
	kind, nonce, ok := strings.Cut(s, ":")
	if !ok {
		return Origin{}, invalidf("origin %q is not <kind>:<nonce>", s)
	}
	o := Origin{Kind: OriginKind(kind), Nonce: nonce}
	if err := o.Validate(); err != nil {
		return Origin{}, err
	}
	return o, nil
}

// String renders <kind>:<nonce>.
func (o Origin) String() string { return string(o.Kind) + ":" + o.Nonce }

// Validate refuses an unknown kind or a nonce outside [a-z0-9]{8,64}.
func (o Origin) Validate() error {
	switch {
	case o.Kind != OriginProcess && o.Kind != OriginDaemon:
		return invalidf("origin kind %q is not %s or %s", o.Kind, OriginProcess, OriginDaemon)
	case !nonceRe.MatchString(o.Nonce):
		return invalidf("origin nonce %q is not [a-z0-9]{8,64}", o.Nonce)
	}
	return nil
}

// Fact is one event a worker-site writer recorded, as its event-fact row
// carries it.
type Fact struct {
	// ID is the pinned id. Decode sets it; Encode derives it and refuses a
	// non-empty ID that does not match.
	ID string
	// Session is the session the writing box serves: the row's key and its
	// label. It is not Event.SessionID, which names whatever the event names.
	Session string
	// Generation is that session's generation; it is >= 1.
	Generation int64
	// Origin is the writer, and N counts its facts from 1.
	Origin Origin
	N      uint64
	// Event is the event as the writer recorded it. Its type is one
	// events.AuthorshipOf calls AuthorWorker. Ts is required; Seq is never
	// stored, since the projector's recorder assigns it.
	Event events.Event
	// PayloadDropped is the compacted size of a payload over MaxPayload that
	// the row omits. Decode sets it; Encode computes it and refuses a fact
	// that sets it.
	PayloadDropped int
}

// ID returns the pinned id of f's row in the namespace prefix, the sessions
// store's mint prefix.
func ID(prefix string, f Fact) (string, error) {
	if err := f.validateIdentity(); err != nil {
		return "", err
	}
	return pinnedID(prefix, f)
}

func pinnedID(prefix string, f Fact) (string, error) {
	id, err := opjournal.PinnedID(prefix, idFamily, idTag,
		f.Session, strconv.FormatInt(f.Generation, 10), f.Origin.String(), strconv.FormatUint(f.N, 10))
	if err != nil {
		return "", fmt.Errorf("%w: fact id: %w", ErrInvalid, err)
	}
	return id, nil
}

// validateIdentity checks the fields that key the fact.
func (f Fact) validateIdentity() error {
	switch {
	case f.Session == "":
		return invalidf("session is empty")
	case len(f.Session) > maxSessionLen:
		return invalidf("session is %d bytes, max %d", len(f.Session), maxSessionLen)
	case !utf8.ValidString(f.Session):
		return invalidf("session %q is not valid UTF-8", f.Session)
	case strings.IndexFunc(f.Session, isSpaceOrControl) >= 0:
		return invalidf("session %q contains whitespace or control characters", f.Session)
	case !beadmeta.ValidLabel(SessionLabel(f.Session)):
		return invalidf("session %q cannot sit in a label", f.Session)
	case f.Generation < 1:
		return invalidf("generation %d must be >= 1", f.Generation)
	case f.N < 1:
		return invalidf("n %d must be >= 1", f.N)
	}
	return f.Origin.Validate()
}

// validateEvent checks the event's fields against their limits, and its type
// against events.AuthorshipOf: a fact carries only a type a worker site may
// author. The payload is checked by the codec, which compacts it first.
func validateEvent(e events.Event) error {
	if err := checkText("type", e.Type, maxTypeLen); err != nil {
		return err
	}
	switch {
	case e.Type == "":
		return invalidf("event type is empty")
	case strings.IndexFunc(e.Type, isSpaceOrControl) >= 0:
		return invalidf("event type %q contains whitespace or control characters", e.Type)
	case e.Ts.IsZero():
		return invalidf("event time is zero")
	case e.Ts.UTC().Year() < 0 || e.Ts.UTC().Year() > 9999:
		return invalidf("event time %v is outside RFC 3339's years", e.Ts)
	}
	if a := events.AuthorshipOf(e.Type); a != events.AuthorWorker {
		return invalidf("event type %q is %s-authored; a fact carries only %s types", e.Type, a, events.AuthorWorker)
	}
	for _, field := range []struct {
		name, value string
		max         int
	}{
		{"actor", e.Actor, maxActorLen},
		{"subject", e.Subject, maxSubjectLen},
		{"message", e.Message, MaxMessage},
		{"run_id", e.RunID, maxIDFieldLen},
		{"session_id", e.SessionID, maxIDFieldLen},
		{"step_id", e.StepID, maxIDFieldLen},
	} {
		if err := checkText(field.name, field.value, field.max); err != nil {
			return err
		}
	}
	if e.DependsOnStepIDs != nil {
		for _, id := range *e.DependsOnStepIDs {
			if err := checkText("depends_on_step_ids entry", id, maxIDFieldLen); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkText refuses a value that is not UTF-8 (JSON would rewrite it, so it
// could not come back as written) or is longer than limit.
func checkText(name, v string, limit int) error {
	switch {
	case len(v) > limit:
		return invalidf("%s is %d bytes, max %d", name, len(v), limit)
	case !utf8.ValidString(v):
		return invalidf("%s is not valid UTF-8", name)
	}
	return nil
}

func isSpaceOrControl(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }
