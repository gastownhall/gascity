package session

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
)

// ContentHoldKey is a conversation row's content hold (DES-O1 D21, §10.5):
// "<principal>@<RFC3339>" while its normal-precedence content is held. Only
// the conversation lane reads it; Stop sets it and resume clears it, and no
// lifecycle transition touches it (it is conversation-scoped).
const ContentHoldKey = "content_hold"

// contentHoldByMax bounds a hold's principal, as opjournal bounds principals.
const contentHoldByMax = 256

// ErrInvalidContentHold reports a hold that is not "<principal>@<RFC3339>":
// a writer's malformed hold, or a stored cell that does not parse (which
// readers still take as held, ContentHeldInfo).
var ErrInvalidContentHold = errors.New("invalid content hold")

// ContentHold is a conversation's content hold: the principal that held its
// queue and when. The zero value is no hold.
type ContentHold struct {
	By string    // the principal, as opjournal's principal grammar spells it
	At time.Time // UTC, whole seconds
}

// NewContentHold returns the hold principal by takes at at, normalized to UTC
// whole seconds. It refuses a principal opjournal would refuse and a zero
// time, wrapping ErrInvalidContentHold.
func NewContentHold(by string, at time.Time) (ContentHold, error) {
	if err := validContentHoldBy(by); err != nil {
		return ContentHold{}, err
	}
	if at.IsZero() {
		return ContentHold{}, fmt.Errorf("%w: the hold has no time", ErrInvalidContentHold)
	}
	return ContentHold{By: by, At: at.UTC().Truncate(time.Second)}, nil
}

// validContentHoldBy mirrors opjournal's principal rule (session cannot
// import opjournal): non-empty, at most 256 bytes, valid UTF-8, no control
// character, and no surrounding space.
func validContentHoldBy(by string) error {
	switch {
	case by == "":
		return fmt.Errorf("%w: the hold names no principal", ErrInvalidContentHold)
	case len(by) > contentHoldByMax:
		return fmt.Errorf("%w: the principal is longer than %d bytes", ErrInvalidContentHold, contentHoldByMax)
	case !utf8.ValidString(by):
		return fmt.Errorf("%w: the principal is not valid UTF-8", ErrInvalidContentHold)
	case strings.TrimSpace(by) != by:
		return fmt.Errorf("%w: the principal %q has surrounding space", ErrInvalidContentHold, by)
	case strings.ContainsFunc(by, unicode.IsControl):
		return fmt.Errorf("%w: the principal %q has a control character", ErrInvalidContentHold, by)
	}
	return nil
}

// String is the cell's value: the principal, "@", and the hold's UTC time to
// the second in RFC 3339. It does not validate; the writers do.
func (h ContentHold) String() string {
	return h.By + "@" + h.At.UTC().Format(time.RFC3339)
}

// IsZero reports whether h is no hold.
func (h ContentHold) IsZero() bool { return h.By == "" && h.At.IsZero() }

// ParseContentHold reads a content_hold cell. A blank cell is no hold. The
// cell splits at its last "@", since a principal may hold one (an email
// subject); the time may carry an offset or fractional seconds and comes back
// in UTC whole seconds. Anything else wraps ErrInvalidContentHold, and a
// reader still takes such a cell as held (ContentHeldInfo).
func ParseContentHold(raw string) (ContentHold, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ContentHold{}, nil
	}
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return ContentHold{}, fmt.Errorf("%w: %q has no \"@\"", ErrInvalidContentHold, raw)
	}
	by, stamp := raw[:at], raw[at+1:]
	if err := validContentHoldBy(by); err != nil {
		return ContentHold{}, fmt.Errorf("reading content hold %q: %w", raw, err)
	}
	when, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return ContentHold{}, fmt.Errorf("%w: %q has no RFC 3339 time: %w", ErrInvalidContentHold, raw, err)
	}
	return NewContentHold(by, when)
}

// ContentHeldInfo reports whether info's conversation holds its content: any
// non-blank cell does, readable or not, so a cell no reader understands never
// releases the queue it was written to hold (fail safe).
func ContentHeldInfo(info Info) bool { return strings.TrimSpace(info.ContentHoldRaw) != "" }

// validContentHold checks a hold a writer is about to write, as
// NewContentHold does. String writes its time in UTC whole seconds.
func validContentHold(h ContentHold) error {
	_, err := NewContentHold(h.By, h.At)
	return err
}

// ContentHoldItem is SetContentHold's write as one guarded-batch item: an
// unguarded update of row id that sets exactly the content_hold cell. A
// producer that must not make an op durable without its hold (a Stop's
// interrupt) writes it in the op's batch (opjournal.Store.ProposeWith).
func ContentHoldItem(id string, h ContentHold) (beads.GuardedItem, error) {
	if strings.TrimSpace(id) == "" {
		return beads.GuardedItem{}, fmt.Errorf("%w: the hold names no session", ErrInvalidContentHold)
	}
	if err := validContentHold(h); err != nil {
		return beads.GuardedItem{}, fmt.Errorf("holding content of session %q: %w", id, err)
	}
	return beads.GuardedItem{Update: &beads.GuardedUpdate{
		ID:   id,
		Opts: beads.UpdateOpts{Metadata: map[string]string{ContentHoldKey: h.String()}},
	}}, nil
}

// SetContentHold holds session id's normal-precedence content under h: one
// single-key write on the row, which re-stamps any hold that stands, so the
// last Stop names the hold and a resume that read an older one cannot clear
// it. The same stamp again writes nothing. It refuses a malformed hold, a
// missing or non-session row, and a closed row (ErrSessionClosed).
func (s *Store) SetContentHold(id string, h ContentHold) error {
	if err := validContentHold(h); err != nil {
		return fmt.Errorf("holding content of session %q: %w", id, err)
	}
	b, err := s.validatedBead(id)
	if err != nil {
		return fmt.Errorf("holding content of session %q: %w", id, err)
	}
	if b.Status == "closed" {
		return fmt.Errorf("holding content of session %q: %w", id, ErrSessionClosed)
	}
	stamp := h.String()
	if b.Metadata[ContentHoldKey] == stamp {
		return nil
	}
	if err := s.setMetadataValue(b.ID, ContentHoldKey, stamp); err != nil {
		return fmt.Errorf("holding content of session %q: %w", id, err)
	}
	return nil
}

// ClearContentHold releases session id's content hold, but only the hold the
// caller read: a value CAS from seen to "". It reports whether this call
// cleared it and the cell that stands afterwards: "" once nothing is held
// (whoever cleared it), or the newer hold that refused the CAS, for the
// caller to re-read and decide on. A blank seen writes nothing and returns
// the stored cell. A store with no metadata CAS refuses with an error
// wrapping beads.ErrConditionalWriteUnsupported rather than clearing blind.
// A closed row's hold clears like any other.
func (s *Store) ClearContentHold(id, seen string) (cleared bool, standing string, err error) {
	b, err := s.validatedBead(id)
	if err != nil {
		return false, "", fmt.Errorf("releasing content of session %q: %w", id, err)
	}
	if strings.TrimSpace(seen) == "" {
		return false, b.Metadata[ContentHoldKey], nil
	}
	writer, ok := beads.MetadataCASWriterFor(s.store.Store)
	if !ok {
		return false, "", fmt.Errorf("releasing content of session %q: %w", id, beads.ErrConditionalWriteUnsupported)
	}
	swapped, err := writer.CompareAndSetMetadataKey(b.ID, ContentHoldKey, seen, "")
	if err != nil {
		return false, "", fmt.Errorf("releasing content of session %q: %w", id, err)
	}
	if swapped {
		return true, "", nil
	}
	after, err := s.validatedBead(b.ID)
	if err != nil {
		return false, "", fmt.Errorf("re-reading content hold of session %q: %w", id, err)
	}
	return false, after.Metadata[ContentHoldKey], nil
}
