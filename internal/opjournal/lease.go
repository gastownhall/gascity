package opjournal

import (
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// maxLeaseLen is bd's assignee limit in runes. bd refuses a longer assignee,
// so a lease that renders longer is refused before any write.
const maxLeaseLen = 255

// Lease is a lane's hold on an executing op, stored as the bead's assignee:
// <lane>@<conversation>#<gen>#<nonce>@<deadline RFC3339Nano UTC>, at most
// maxLeaseLen runes. O0 only encodes and decodes it; lanes write it in O1.
type Lease struct {
	Lane         string
	Conversation string
	Generation   int64
	Nonce        string
	Deadline     time.Time
}

// IsZero reports whether no lease is held.
func (l Lease) IsZero() bool { return l == Lease{} }

// String renders the lease in its assignee form. The zero lease renders "".
func (l Lease) String() string {
	if l.IsZero() {
		return ""
	}
	return l.Lane + "@" + l.Conversation + "#" + strconv.FormatInt(l.Generation, 10) + "#" +
		l.Nonce + "@" + formatTime(l.Deadline)
}

// validate refuses a lease whose parts would not parse back or whose assignee
// form bd would not store.
func (l Lease) validate() error {
	if err := validateLeasePart("lane", l.Lane); err != nil {
		return err
	}
	if err := validateConversation(l.Conversation); err != nil {
		return err
	}
	if l.Generation < 1 {
		return invalidf("lease generation %d must be >= 1", l.Generation)
	}
	if err := validateLeasePart("nonce", l.Nonce); err != nil {
		return err
	}
	if l.Deadline.IsZero() {
		return invalidf("lease deadline is zero")
	}
	if n := utf8.RuneCountInString(l.String()); n > maxLeaseLen {
		return invalidf("lease is %d runes, max %d (bd's assignee limit)", n, maxLeaseLen)
	}
	return nil
}

func validateLeasePart(what, v string) error {
	if v == "" || len(v) > maxTokenLen || strings.ContainsAny(v, "@#") ||
		strings.IndexFunc(v, unicode.IsSpace) >= 0 || hasControl(v) {
		return invalidf("lease %s %q is empty or contains '@', '#' or whitespace", what, v)
	}
	return nil
}

// ParseLease parses an assignee written by Lease.String.
func ParseLease(s string) (Lease, error) {
	lane, rest, ok := strings.Cut(s, "@")
	if !ok {
		return Lease{}, invalidf("lease %q has no '@'", s)
	}
	at := strings.LastIndexByte(rest, '@')
	if at < 0 {
		return Lease{}, invalidf("lease %q has no deadline", s)
	}
	body, deadline := rest[:at], rest[at+1:]
	parts := strings.Split(body, "#")
	if len(parts) != 3 {
		return Lease{}, invalidf("lease %q is not <lane>@<conversation>#<gen>#<nonce>@<deadline>", s)
	}
	gen, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return Lease{}, invalidf("lease %q generation: %v", s, err)
	}
	when, err := parseTime(deadline)
	if err != nil {
		return Lease{}, invalidf("lease %q deadline: %v", s, err)
	}
	l := Lease{Lane: lane, Conversation: parts[0], Generation: gen, Nonce: parts[2], Deadline: when}
	if err := l.validate(); err != nil {
		return Lease{}, err
	}
	if l.String() != s {
		return Lease{}, invalidf("lease %q is not canonical", s)
	}
	return l, nil
}

// formatTime renders t as RFC3339Nano in UTC.
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// storableTime reports whether t survives formatTime and parseTime: RFC3339
// writes four-digit years, so a time outside years 0 through 9999 would leave
// a row that no longer decodes.
func storableTime(t time.Time) bool {
	y := t.UTC().Year()
	return y >= 0 && y <= 9999
}

// parseTime parses an RFC3339Nano time into UTC.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
