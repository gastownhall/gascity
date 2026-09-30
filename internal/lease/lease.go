// Package lease encodes and decodes the lease record that marks a unit of
// work as owned. The wire form is a single string field:
//
//	v1|<epoch>|<holder>|<expires_at>|<state>|<attempts>
//
// The package is a pure value codec with no store handle and no I/O. Absence
// of the record is the unleased condition, distinct from the three states,
// and is carried by Lease rather than by a zero-valued Record.
package lease

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// State is the lifecycle position recorded in a lease.
type State string

const (
	// StateHeld means the holder owns the work until ExpiresAt.
	StateHeld State = "held"
	// StateParked means the holder released the work without completing it.
	StateParked State = "parked"
	// StateDead means the record is retired and is not a claim.
	StateDead State = "dead"
)

const (
	version    = "v1"
	fieldSep   = "|"
	noExpiry   = "-"
	fieldCount = 6

	holderSessionSep  = "@"
	holderInstanceSep = "#"
)

// Holder identifies one incarnation of a seat. A restarted seat keeps its
// agent and session names but takes a fresh InstanceToken, so it is a
// different Holder and does not inherit the previous incarnation's ownership.
type Holder struct {
	Agent         string
	Session       string
	InstanceToken string
}

// NewHolder builds a Holder from its three parts, rejecting empty parts and
// parts containing any separator used by the wire form.
func NewHolder(agent, session, instanceToken string) (Holder, error) {
	for name, part := range map[string]string{
		"agent":          agent,
		"session":        session,
		"instance token": instanceToken,
	} {
		if part == "" {
			return Holder{}, fmt.Errorf("building holder: %s is empty", name)
		}
		if !utf8.ValidString(part) {
			return Holder{}, fmt.Errorf("building holder: %s is not valid UTF-8", name)
		}
		if strings.IndexFunc(part, unicode.IsControl) >= 0 {
			return Holder{}, fmt.Errorf("building holder: %s %q contains a control character", name, part)
		}
		if strings.ContainsAny(part, fieldSep+holderSessionSep+holderInstanceSep) {
			return Holder{}, fmt.Errorf("building holder: %s %q contains a separator", name, part)
		}
	}
	return Holder{Agent: agent, Session: session, InstanceToken: instanceToken}, nil
}

// String renders the holder as <agent>@<session>#<instance_token>.
func (h Holder) String() string {
	return h.Agent + holderSessionSep + h.Session + holderInstanceSep + h.InstanceToken
}

func parseHolder(raw string) (Holder, error) {
	seat, instanceToken, ok := strings.Cut(raw, holderInstanceSep)
	if !ok {
		return Holder{}, fmt.Errorf("holder %q has no %q separator", raw, holderInstanceSep)
	}
	agent, session, ok := strings.Cut(seat, holderSessionSep)
	if !ok {
		return Holder{}, fmt.Errorf("holder %q has no %q separator", raw, holderSessionSep)
	}
	holder, err := NewHolder(agent, session, instanceToken)
	if err != nil {
		return Holder{}, fmt.Errorf("holder %q: %w", raw, err)
	}
	if holder.String() != raw {
		return Holder{}, fmt.Errorf("holder %q does not round-trip", raw)
	}
	return holder, nil
}

// Record is a decoded lease. Records are comparable with == only once they
// are canonical, which Decode guarantees and Canonical provides for a
// caller-built value.
type Record struct {
	Epoch     int64
	Holder    Holder
	ExpiresAt time.Time
	State     State
	Attempts  int64
}

// Canonical returns the record with its expiry in the form Decode produces:
// UTC with any monotonic reading stripped and every zero instant normalized.
func (r Record) Canonical() Record {
	r.ExpiresAt = r.ExpiresAt.UTC().Round(0)
	return r
}

// Validate reports whether the record can be encoded: a known state, a
// nonnegative epoch and attempt count, a well-formed holder, and an expiry
// that is present exactly when the state is StateHeld.
func (r Record) Validate() error {
	switch r.State {
	case StateHeld, StateParked, StateDead:
	default:
		return fmt.Errorf("state %q is not one of %q, %q, %q", r.State, StateHeld, StateParked, StateDead)
	}
	if r.Epoch < 0 {
		return fmt.Errorf("epoch %d is negative", r.Epoch)
	}
	if r.Attempts < 0 {
		return fmt.Errorf("attempts %d is negative", r.Attempts)
	}
	if _, err := NewHolder(r.Holder.Agent, r.Holder.Session, r.Holder.InstanceToken); err != nil {
		return err
	}
	if r.State == StateHeld {
		if r.ExpiresAt.IsZero() {
			return fmt.Errorf("state %q carries no expiry", StateHeld)
		}
		if err := checkExpiryRoundTrips(r.ExpiresAt); err != nil {
			return err
		}
		return nil
	}
	if !r.ExpiresAt.IsZero() {
		return fmt.Errorf("state %q carries expiry %s, want none", r.State, formatExpiry(r.ExpiresAt))
	}
	return nil
}

// Encode renders a record in the wire form. It validates first and returns
// the empty string with an error for any record that is not encodable.
func Encode(r Record) (string, error) {
	if err := r.Validate(); err != nil {
		return "", fmt.Errorf("encoding lease: %w", err)
	}
	expiry := noExpiry
	if r.State == StateHeld {
		expiry = formatExpiry(r.ExpiresAt)
	}
	return strings.Join([]string{
		version,
		strconv.FormatInt(r.Epoch, 10),
		r.Holder.String(),
		expiry,
		string(r.State),
		strconv.FormatInt(r.Attempts, 10),
	}, fieldSep), nil
}

// Decode parses the wire form of a present lease record. It accepts only
// strings Encode emits and returns an error for anything else, so a malformed
// value is never read as absence.
func Decode(s string) (Record, error) {
	rec, err := decode(s)
	if err != nil {
		return Record{}, fmt.Errorf("decoding lease %q: %w", s, err)
	}
	return rec, nil
}

func decode(s string) (Record, error) {
	parts := strings.Split(s, fieldSep)
	if len(parts) != fieldCount {
		return Record{}, fmt.Errorf("got %d fields, want %d", len(parts), fieldCount)
	}
	if parts[0] != version {
		return Record{}, fmt.Errorf("version tag %q, want %q", parts[0], version)
	}
	epoch, err := parseCount(parts[1])
	if err != nil {
		return Record{}, fmt.Errorf("epoch: %w", err)
	}
	holder, err := parseHolder(parts[2])
	if err != nil {
		return Record{}, err
	}
	state := State(parts[4])
	switch state {
	case StateHeld, StateParked, StateDead:
	default:
		return Record{}, fmt.Errorf("state %q is not one of %q, %q, %q", state, StateHeld, StateParked, StateDead)
	}
	expiresAt, err := parseExpiry(parts[3], state)
	if err != nil {
		return Record{}, err
	}
	attempts, err := parseCount(parts[5])
	if err != nil {
		return Record{}, fmt.Errorf("attempts: %w", err)
	}
	return Record{
		Epoch:     epoch,
		Holder:    holder,
		ExpiresAt: expiresAt,
		State:     state,
		Attempts:  attempts,
	}, nil
}

func parseCount(raw string) (int64, error) {
	if raw == "" {
		return 0, fmt.Errorf("field is empty")
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a decimal count: %w", raw, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%q is negative", raw)
	}
	if rendered := strconv.FormatInt(n, 10); rendered != raw {
		return 0, fmt.Errorf("count %q is not in the encoded form, which is %q", raw, rendered)
	}
	return n, nil
}

func parseExpiry(raw string, state State) (time.Time, error) {
	if state != StateHeld {
		if raw != noExpiry {
			return time.Time{}, fmt.Errorf("state %q carries expiry %q, want %q", state, raw, noExpiry)
		}
		return time.Time{}, nil
	}
	if raw == noExpiry {
		return time.Time{}, fmt.Errorf("state %q carries %q, want a timestamp", StateHeld, noExpiry)
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("expiry %q is not RFC3339: %w", raw, err)
	}
	if parsed.IsZero() {
		return time.Time{}, fmt.Errorf("expiry %q is the zero time", raw)
	}
	utc := parsed.UTC()
	if rendered := formatExpiry(utc); rendered != raw {
		return time.Time{}, fmt.Errorf("expiry %q is not in the encoded form, which is %q", raw, rendered)
	}
	return utc, nil
}

func formatExpiry(ts time.Time) string {
	return ts.UTC().Format(time.RFC3339Nano)
}

func checkExpiryRoundTrips(ts time.Time) error {
	rendered := formatExpiry(ts)
	parsed, err := time.Parse(time.RFC3339Nano, rendered)
	if err != nil || !parsed.Equal(ts) {
		return fmt.Errorf("expiry %v is not representable as RFC3339", ts)
	}
	return nil
}

// Lease is a lease record or the absence of one. The zero Lease is the
// unleased condition.
type Lease struct {
	present bool
	opaque  bool
	record  Record
}

// Unleased returns the absent lease.
func Unleased() Lease {
	return Lease{}
}

func opaque() Lease {
	return Lease{present: true, opaque: true}
}

// Present wraps a record as a present lease. An invalid record yields the
// fail-closed lease and an error, so it can masquerade neither as ownership
// nor as absence.
func Present(r Record) (Lease, error) {
	if err := r.Validate(); err != nil {
		return opaque(), fmt.Errorf("building lease: %w", err)
	}
	return Lease{present: true, record: r}, nil
}

// IsUnleased reports whether the record was absent.
func (l Lease) IsUnleased() bool {
	return !l.present
}

// IsOpaque reports whether a present lease could not be decoded or validated.
func (l Lease) IsOpaque() bool {
	return l.opaque
}

// Record returns the decoded record and true when it is usable, and the zero
// Record and false when the lease is unleased or opaque.
func (l Lease) Record() (Record, bool) {
	if !l.present || l.opaque {
		return Record{}, false
	}
	return l.record, true
}

// DecodeLease turns a raw value plus its presence flag into a Lease. An
// absent value is the unleased condition. A present value that fails to
// decode yields an opaque lease together with the error.
func DecodeLease(raw string, present bool) (Lease, error) {
	if !present {
		return Unleased(), nil
	}
	rec, err := Decode(raw)
	if err != nil {
		return opaque(), err
	}
	return Lease{present: true, record: rec}, nil
}

// Lookup reads the lease stored under key. A missing key, including on a nil
// map, is the unleased condition.
func Lookup(meta map[string]string, key string) (Lease, error) {
	raw, ok := meta[key]
	return DecodeLease(raw, ok)
}
