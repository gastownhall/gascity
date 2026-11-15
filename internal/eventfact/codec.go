package eventfact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// Fact metadata keys. The identity keys name the writer; every other key is
// gc.ev.<the events.Event JSON tag> of the field it holds.
const (
	keySchema = "gc.ev.schema"

	keySession = "gc.ev.session"
	keyGen     = "gc.ev.gen"
	keyOrigin  = "gc.ev.origin"
	keyN       = "gc.ev.n"

	keyType             = "gc.ev.type"
	keyTs               = "gc.ev.ts"
	keyActor            = "gc.ev.actor"
	keySubject          = "gc.ev.subject"
	keyMessage          = "gc.ev.message"
	keyPayload          = "gc.ev.payload"
	keyPayloadDropped   = "gc.ev.payload_dropped"
	keyRunID            = "gc.ev.run_id"
	keySessionID        = "gc.ev.session_id"
	keyStepID           = "gc.ev.step_id"
	keyDependsOnStepIDs = "gc.ev.depends_on_step_ids"
)

// Encode renders f as its event-fact row under prefix, the sessions store's
// mint prefix: a closed, no-history create, ready for a guarded batch or a
// plain Create. The payload is stored compacted, or omitted with its size
// recorded when it is larger than MaxPayload. A fact outside the schema is
// ErrInvalid, and so is one whose type events.AuthorshipOf does not call
// AuthorWorker.
func Encode(prefix string, f Fact) (beads.Bead, error) {
	if err := f.validateIdentity(); err != nil {
		return beads.Bead{}, err
	}
	id, err := pinnedID(prefix, f)
	if err != nil {
		return beads.Bead{}, err
	}
	if f.ID != "" && f.ID != id {
		return beads.Bead{}, invalidf("fact id %q does not match its writer (%s)", f.ID, id)
	}
	if f.PayloadDropped != 0 {
		return beads.Bead{}, invalidf("PayloadDropped is set by Decode, not by the writer")
	}
	e := f.Event
	if err := validateEvent(e); err != nil {
		return beads.Bead{}, err
	}
	payload, dropped, err := storedPayload(e.Payload)
	if err != nil {
		return beads.Bead{}, err
	}
	dependsOn, err := encodeDependsOn(e.DependsOnStepIDs)
	if err != nil {
		return beads.Bead{}, err
	}
	meta := map[string]string{
		keySchema:           strconv.Itoa(FactSchemaRev),
		keySession:          f.Session,
		keyGen:              strconv.FormatInt(f.Generation, 10),
		keyOrigin:           f.Origin.String(),
		keyN:                strconv.FormatUint(f.N, 10),
		keyType:             e.Type,
		keyTs:               e.Ts.UTC().Format(time.RFC3339Nano),
		keyActor:            e.Actor,
		keySubject:          e.Subject,
		keyMessage:          e.Message,
		keyPayload:          payload,
		keyRunID:            e.RunID,
		keySessionID:        e.SessionID,
		keyStepID:           e.StepID,
		keyDependsOnStepIDs: dependsOn,
	}
	if dropped > 0 {
		meta[keyPayloadDropped] = strconv.Itoa(dropped)
	}
	for k, v := range meta {
		if v == "" {
			delete(meta, k)
		}
	}
	return beads.Bead{
		ID:        id,
		Title:     "event " + e.Type,
		Status:    beadStatusClosed,
		Type:      BeadType,
		Labels:    []string{Label, SessionLabel(f.Session)},
		Metadata:  meta,
		NoHistory: true,
	}, nil
}

// storedPayload returns the payload as the row stores it: compacted, or empty
// with its compacted size when that is over MaxPayload. A payload that is not
// JSON is ErrInvalid; the recorder could not write it either. So is one with
// invalid UTF-8 inside a string: json.Compact keeps it, and the store's JSON
// would rewrite it to U+FFFD, so the row could not come back as written.
func storedPayload(raw json.RawMessage) (payload string, dropped int, err error) {
	if len(raw) == 0 {
		return "", 0, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", 0, invalidf("payload is not JSON: %v", err)
	}
	if !utf8.Valid(buf.Bytes()) {
		return "", 0, invalidf("payload is not valid UTF-8")
	}
	if buf.Len() > MaxPayload {
		return "", buf.Len(), nil
	}
	return buf.String(), 0, nil
}

// encodeDependsOn keeps nil (unknown topology) apart from an empty list (a
// known root): nil is absent, empty is "[]".
func encodeDependsOn(ids *[]string) (string, error) {
	if ids == nil {
		return "", nil
	}
	list := *ids
	if list == nil {
		list = []string{}
	}
	data, err := json.Marshal(list)
	if err != nil {
		return "", invalidf("depends_on_step_ids: %v", err)
	}
	if len(data) > maxDependsOnLen {
		return "", invalidf("depends_on_step_ids is %d bytes, max %d", len(data), maxDependsOnLen)
	}
	return string(data), nil
}

// IsEventFact reports whether b is an event-fact row: type "op" with the
// gc:event-fact label. It reads nothing else, so a projector selects rows
// with it before decoding them.
func IsEventFact(b beads.Bead) bool {
	return b.Type == BeadType && slices.Contains(b.Labels, Label)
}

// Decode reads an event-fact row. It is the projector's trust boundary: a
// box can write a fact row without Encode (D-TRUST), so Decode accepts only a
// row Encode writes, every field in its canonical stored form and within its
// limit, and the type one a worker site may author. A row that is not one, is
// malformed or out of bounds, carries a Controller or Ledger type, or whose id
// is not <prefix>-ev-<hash of its writer> under a valid prefix is ErrCorrupt;
// a later schema is ErrUnsupportedSchema. A gc.ev.* key this build does not
// know is ignored (doc.go, "Schema evolution"). The row's status is not read:
// a store that creates rows open leaves a fact open until its writer closes
// it, and the fact is the same.
func Decode(b beads.Bead) (Fact, error) {
	if !IsEventFact(b) {
		return Fact{}, corruptf(b.ID, "not an event-fact row (type %q, labels %v)", b.Type, b.Labels)
	}
	d := metaReader{id: b.ID, meta: b.Metadata}
	if err := d.schema(keySchema, FactSchemaRev); err != nil {
		return Fact{}, err
	}
	d.required(keyGen)
	d.required(keyN)
	f := Fact{
		ID:         b.ID,
		Session:    d.required(keySession),
		Generation: d.int64(keyGen),
		N:          d.uint64(keyN),
		Event: events.Event{
			Type:      d.required(keyType),
			Ts:        d.time(keyTs),
			Actor:     d.meta[keyActor],
			Subject:   d.meta[keySubject],
			Message:   d.meta[keyMessage],
			RunID:     d.meta[keyRunID],
			SessionID: d.meta[keySessionID],
			StepID:    d.meta[keyStepID],
		},
		PayloadDropped: d.int(keyPayloadDropped),
	}
	origin := d.required(keyOrigin)
	d.required(keyTs)
	if d.err != nil {
		return Fact{}, d.err
	}
	o, err := ParseOrigin(origin)
	if err != nil {
		return Fact{}, corruptf(b.ID, "%s: %v", keyOrigin, err)
	}
	f.Origin = o
	if payload := d.meta[keyPayload]; payload != "" {
		if f.PayloadDropped != 0 {
			return Fact{}, corruptf(b.ID, "%s and %s are both set", keyPayload, keyPayloadDropped)
		}
		stored, dropped, err := storedPayload(json.RawMessage(payload))
		switch {
		case err != nil:
			return Fact{}, corruptf(b.ID, "%s: %v", keyPayload, err)
		case dropped > 0:
			return Fact{}, corruptf(b.ID, "%s is %d bytes, max %d", keyPayload, dropped, MaxPayload)
		case stored != payload:
			return Fact{}, corruptf(b.ID, "%s is not compacted JSON", keyPayload)
		}
		f.Event.Payload = json.RawMessage(payload)
	}
	if d.meta[keyPayloadDropped] != "" && f.PayloadDropped <= MaxPayload {
		return Fact{}, corruptf(b.ID, "%s %d is not a size over %d", keyPayloadDropped, f.PayloadDropped, MaxPayload)
	}
	if raw := d.meta[keyDependsOnStepIDs]; raw != "" {
		var ids []string
		if err := json.Unmarshal([]byte(raw), &ids); err != nil || ids == nil {
			return Fact{}, corruptf(b.ID, "%s %q is not a JSON array of strings", keyDependsOnStepIDs, raw)
		}
		stored, err := encodeDependsOn(&ids)
		if err != nil {
			return Fact{}, corruptf(b.ID, "%v", err)
		}
		if stored != raw {
			return Fact{}, corruptf(b.ID, "%s is not the array as Encode writes it", keyDependsOnStepIDs)
		}
		f.Event.DependsOnStepIDs = &ids
	}
	if err := f.validateIdentity(); err != nil {
		return Fact{}, corruptf(b.ID, "%v", err)
	}
	if err := validateEvent(f.Event); err != nil {
		return Fact{}, corruptf(b.ID, "%v", err)
	}
	if !slices.Contains(b.Labels, SessionLabel(f.Session)) {
		return Fact{}, corruptf(b.ID, "missing label %s", SessionLabel(f.Session))
	}
	// The hash is base32, so the last "-ev-" ends the prefix.
	cut := strings.LastIndex(b.ID, "-"+idFamily+"-")
	if cut < 0 {
		return Fact{}, corruptf(b.ID, "id is not <prefix>-%s-<hash>", idFamily)
	}
	want, err := pinnedID(b.ID[:cut], f)
	if err != nil {
		return Fact{}, corruptf(b.ID, "id prefix: %v", err)
	}
	if want != b.ID {
		return Fact{}, corruptf(b.ID, "id does not match its writer (want %s)", want)
	}
	return f, nil
}

// metaReader reads typed metadata values, keeping the first error.
type metaReader struct {
	id   string
	meta map[string]string
	err  error
}

func (d *metaReader) fail(key, format string, args ...any) {
	if d.err == nil {
		d.err = corruptf(d.id, "%s: %s", key, fmt.Sprintf(format, args...))
	}
}

// schema reads a row's schema revision. A revision from 1 up to current is
// one this build reads (each later revision only adds to the one before it,
// or its reader keeps decoding the earlier ones); a later revision is a newer
// build's row, ErrUnsupportedSchema, never skipped silently.
func (d *metaReader) schema(key string, current int) error {
	v := d.meta[key]
	if v == "" {
		return corruptf(d.id, "%s is missing", key)
	}
	rev, err := strconv.Atoi(v)
	if err != nil || strconv.Itoa(rev) != v || rev < 1 {
		return corruptf(d.id, "%s %q is not a revision", key, v)
	}
	if rev > current {
		return fmt.Errorf("%w: row %s has revision %d, this build reads up to %d", ErrUnsupportedSchema, d.id, rev, current)
	}
	return nil
}

func (d *metaReader) required(key string) string {
	v := d.meta[key]
	if v == "" {
		d.fail(key, "missing")
	}
	return v
}

func (d *metaReader) int64(key string) int64 {
	v := d.meta[key]
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != v {
		d.fail(key, "%q is not a canonical integer", v)
	}
	return n
}

// int reads an integer that must fit this platform's int: a wider value is
// refused, never wrapped.
func (d *metaReader) int(key string) int {
	v := d.meta[key]
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || strconv.Itoa(n) != v {
		d.fail(key, "%q is not a canonical integer within int range", v)
	}
	return n
}

func (d *metaReader) uint64(key string) uint64 {
	v := d.meta[key]
	if v == "" {
		return 0
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != v {
		d.fail(key, "%q is not a canonical unsigned integer", v)
	}
	return n
}

// time reads a timestamp in the one form the codecs write: RFC 3339 with
// nanoseconds, in UTC.
func (d *metaReader) time(key string) time.Time {
	v := d.meta[key]
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		d.fail(key, "%q: %v", v, err)
		return time.Time{}
	}
	if t.UTC().Format(time.RFC3339Nano) != v {
		d.fail(key, "%q is not a canonical UTC RFC 3339 time", v)
	}
	return t
}
