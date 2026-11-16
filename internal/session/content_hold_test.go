package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

var holdAt = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func mustHold(t *testing.T, by string, at time.Time) ContentHold {
	t.Helper()
	h, err := NewContentHold(by, at)
	if err != nil {
		t.Fatalf("NewContentHold(%q, %v): %v", by, at, err)
	}
	return h
}

// holdRow is a session row in a MemStore, behind its session Store.
func holdRow(t *testing.T, mem *beads.MemStore, meta map[string]string) (*Store, string) {
	t.Helper()
	if meta == nil {
		meta = map[string]string{}
	}
	meta["state"] = "active"
	b, err := mem.Create(beads.Bead{Type: BeadType, Labels: []string{LabelSession}, Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(beads.SessionStore{Store: mem}), b.ID
}

func holdBead(t *testing.T, mem *beads.MemStore, id string) beads.Bead {
	t.Helper()
	b, err := mem.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestContentHoldEncodesAndParses pins the cell's form: the principal, "@",
// the time in UTC to the nanosecond (RFC 3339 with fractional seconds); a
// reader splits at the last "@".
func TestContentHoldEncodesAndParses(t *testing.T) {
	east := time.FixedZone("east", 2*60*60)
	h := mustHold(t, "t3:e/user:a@b.c", time.Date(2026, 10, 20, 14, 0, 0, 999_000_000, east))
	if got, want := h.String(), "t3:e/user:a@b.c@2026-10-20T12:00:00.999Z"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	back, err := ParseContentHold(h.String())
	if err != nil || back != h {
		t.Fatalf("Parse(%q) = %+v, %v; want %+v", h.String(), back, err, h)
	}
	if got, want := mustHold(t, "operator", holdAt).String(), "operator@2026-10-20T12:00:00Z"; got != want {
		t.Fatalf("String() of a whole second = %q, want %q", got, want)
	}
	for raw, want := range map[string]ContentHold{
		"  operator@2026-10-20T12:00:00Z  ":                    {By: "operator", At: holdAt},
		"operator@2026-10-20T14:00:00+02:00":                   {By: "operator", At: holdAt},
		"operator@2026-10-20T12:00:00.75Z":                     {By: "operator", At: holdAt.Add(750 * time.Millisecond)},
		"operator@2026-10-20T12:00:00.000000001Z":              {By: "operator", At: holdAt.Add(time.Nanosecond)},
		"a@b@c@2026-10-20T12:00:00Z":                           {By: "a@b@c", At: holdAt},
		"t3:env-1/user:alice@example.com@2026-10-20T12:00:00Z": {By: "t3:env-1/user:alice@example.com", At: holdAt},
	} {
		got, err := ParseContentHold(raw)
		if err != nil || got != want || got.At.Location() != time.UTC {
			t.Errorf("Parse(%q) = %+v, %v; want %+v in UTC", raw, got, err, want)
		}
	}
	for _, blank := range []string{"", "   "} {
		if got, err := ParseContentHold(blank); err != nil || !got.IsZero() {
			t.Errorf("Parse(%q) = %+v, %v; want the zero hold", blank, got, err)
		}
	}
	raw := ContentHold{By: "operator", At: time.Date(2026, 10, 20, 14, 0, 0, 999_000_000, east)}
	if got, want := raw.String(), "operator@2026-10-20T12:00:00.999Z"; got != want {
		t.Fatalf("String() of an unnormalized hold = %q, want %q", got, want)
	}
	if h.IsZero() || !(ContentHold{}).IsZero() {
		t.Fatal("IsZero: a hold reads zero or the zero value does not")
	}
}

// TestContentHoldRefusesMalformed: a cell or a writer's hold that is not
// "<principal>@<RFC3339>" is ErrInvalidContentHold.
func TestContentHoldRefusesMalformed(t *testing.T) {
	long := strings.Repeat("p", 257)
	for _, raw := range []string{
		"operator", "@2026-10-20T12:00:00Z", "operator@", "operator@yesterday",
		"oper\x07ator@2026-10-20T12:00:00Z", " operator @2026-10-20T12:00:00Z",
		long + "@2026-10-20T12:00:00Z", "op\xffer@2026-10-20T12:00:00Z",
	} {
		if _, err := ParseContentHold(raw); !errors.Is(err, ErrInvalidContentHold) {
			t.Errorf("Parse(%q) = %v, want ErrInvalidContentHold", raw, err)
		}
	}
	for _, by := range []string{"", " operator", "operator ", "op\nerator", long, "op\xffer"} {
		if _, err := NewContentHold(by, holdAt); !errors.Is(err, ErrInvalidContentHold) {
			t.Errorf("NewContentHold(%q) = %v, want ErrInvalidContentHold", by, err)
		}
	}
	if _, err := NewContentHold(strings.Repeat("p", 256), holdAt); err != nil {
		t.Errorf("a 256-byte principal: %v", err)
	}
	if _, err := NewContentHold("operator", time.Time{}); !errors.Is(err, ErrInvalidContentHold) {
		t.Errorf("a zero time = %v, want ErrInvalidContentHold", err)
	}
}

// TestContentHeldInfoIsFailSafe: any non-blank cell holds, readable or not.
func TestContentHeldInfoIsFailSafe(t *testing.T) {
	for raw, want := range map[string]bool{
		"": false, "  ": false,
		"operator@2026-10-20T12:00:00Z": true, "garbage": true, "x@not-a-time": true,
	} {
		if got := ContentHeldInfo(Info{ContentHoldRaw: raw}); got != want {
			t.Errorf("ContentHeldInfo(%q) = %v, want %v", raw, got, want)
		}
	}
}

// TestSetContentHold: the writer stamps the row, re-stamps a standing hold
// on every Stop, and refuses what it cannot hold.
func TestSetContentHold(t *testing.T) {
	mem := beads.NewMemStore()
	s, id := holdRow(t, mem, nil)
	first := mustHold(t, "operator", holdAt)
	if err := s.SetContentHold(id, first); err != nil {
		t.Fatal(err)
	}
	if got := holdBead(t, mem, id).Metadata[ContentHoldKey]; got != first.String() {
		t.Fatalf("cell = %q, want %q", got, first.String())
	}
	second := mustHold(t, "t3:e/user:bob", holdAt.Add(time.Minute))
	if err := s.SetContentHold(id, second); err != nil {
		t.Fatal(err)
	}
	if got := holdBead(t, mem, id).Metadata[ContentHoldKey]; got != second.String() {
		t.Fatalf("after a second Stop the cell = %q, want the newer %q", got, second.String())
	}
	rev := holdBead(t, mem, id).Revision
	if err := s.SetContentHold(id, second); err != nil {
		t.Fatal(err)
	}
	if got := holdBead(t, mem, id).Revision; got == rev {
		t.Fatal("the same stamp again wrote nothing; a Stop always writes, never trusting a cached read")
	}
	if err := s.SetContentHold(id, ContentHold{}); !errors.Is(err, ErrInvalidContentHold) {
		t.Errorf("zero hold = %v, want ErrInvalidContentHold", err)
	}

	if err := s.SetContentHold("gc-missing", first); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("missing id = %v, want beads.ErrNotFound", err)
	}
	other, err := mem.Create(beads.Bead{Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetContentHold(other.ID, first); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("non-session bead = %v, want ErrSessionNotFound", err)
	}
	if got := holdBead(t, mem, other.ID).Metadata[ContentHoldKey]; got != "" {
		t.Errorf("the non-session bead was written: %q", got)
	}
	_, closedID := holdRow(t, mem, nil)
	if err := mem.Close(closedID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetContentHold(closedID, first); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("closed row = %v, want ErrSessionClosed", err)
	}
	if got := holdBead(t, mem, closedID).Metadata[ContentHoldKey]; got != "" {
		t.Errorf("the closed row was held: %q", got)
	}
}

// TestContentHoldSubSecondStopsKeepTheNewerHold: two Stops by one principal
// in the same second stamp distinct holds, so a resume that read the first
// cannot clear the second (D4).
func TestContentHoldSubSecondStopsKeepTheNewerHold(t *testing.T) {
	mem := beads.NewMemStore()
	s, id := holdRow(t, mem, nil)
	if err := s.SetContentHold(id, mustHold(t, "operator", holdAt.Add(100*time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	seen := holdBead(t, mem, id).Metadata[ContentHoldKey]
	second := mustHold(t, "operator", holdAt.Add(600*time.Millisecond))
	if err := s.SetContentHold(id, second); err != nil {
		t.Fatal(err)
	}
	cleared, standing, err := s.ClearContentHold(id, seen)
	if err != nil || cleared || standing != second.String() {
		t.Fatalf("stale resume = (%v, %q, %v), want (false, %q, nil)", cleared, standing, err, second.String())
	}
	if got := holdBead(t, mem, id).Metadata[ContentHoldKey]; got != second.String() {
		t.Fatalf("cell = %q, want the newer hold %q", got, second.String())
	}
}

// TestClearContentHold: the clear is a value CAS from the hold the caller
// read; it never clears blind.
func TestClearContentHold(t *testing.T) {
	older := mustHold(t, "operator", holdAt).String()
	newer := mustHold(t, "operator", holdAt.Add(time.Minute)).String()

	mem := beads.NewMemStore()
	s, id := holdRow(t, mem, map[string]string{ContentHoldKey: older})
	cleared, standing, err := s.ClearContentHold(id, older)
	if err != nil || !cleared || standing != "" {
		t.Fatalf("matching seen = (%v, %q, %v), want (true, \"\", nil)", cleared, standing, err)
	}
	if got := holdBead(t, mem, id).Metadata[ContentHoldKey]; got != "" {
		t.Fatalf("cell after the clear = %q", got)
	}
	if cleared, standing, err = s.ClearContentHold(id, older); err != nil || cleared || standing != "" {
		t.Fatalf("already clear = (%v, %q, %v), want (false, \"\", nil)", cleared, standing, err)
	}

	rev := holdBead(t, mem, id).Revision
	if cleared, standing, err = s.ClearContentHold(id, ""); err != nil || cleared || standing != "" {
		t.Fatalf("blank seen on a clear row = (%v, %q, %v), want (false, \"\", nil)", cleared, standing, err)
	}
	if got := holdBead(t, mem, id).Revision; got != rev {
		t.Fatalf("a blank seen wrote a clear row (revision %d -> %d)", rev, got)
	}

	_, stale := holdRow(t, mem, map[string]string{ContentHoldKey: newer})
	if cleared, standing, err = s.ClearContentHold(stale, older); err != nil || cleared || standing != newer {
		t.Fatalf("stale seen = (%v, %q, %v), want (false, %q, nil)", cleared, standing, err, newer)
	}
	if got := holdBead(t, mem, stale).Metadata[ContentHoldKey]; got != newer {
		t.Fatalf("a stale resume cleared the newer hold: %q", got)
	}
	rev = holdBead(t, mem, stale).Revision
	for _, blank := range []string{"", "  "} {
		if cleared, standing, err = s.ClearContentHold(stale, blank); err != nil || cleared || standing != newer {
			t.Fatalf("blank seen %q = (%v, %q, %v), want (false, %q, nil)", blank, cleared, standing, err, newer)
		}
	}
	if got := holdBead(t, mem, stale).Revision; got != rev {
		t.Fatalf("a blank seen wrote the row (revision %d -> %d)", rev, got)
	}

	_, garbage := holdRow(t, mem, map[string]string{ContentHoldKey: "garbage"})
	if cleared, _, err = s.ClearContentHold(garbage, "garbage"); err != nil || !cleared {
		t.Fatalf("an unreadable cell = (%v, %v), want it cleared", cleared, err)
	}

	if _, _, err := s.ClearContentHold("gc-missing", older); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("missing id = %v, want beads.ErrNotFound", err)
	}

	blind := beads.NewMemStore()
	bs, bid := holdRow(t, blind, map[string]string{ContentHoldKey: older})
	blind.DisableConditionalWrites = true
	if _, _, err := bs.ClearContentHold(bid, older); !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		t.Fatalf("no metadata CAS = %v, want ErrConditionalWriteUnsupported", err)
	}
	if got := holdBead(t, blind, bid).Metadata[ContentHoldKey]; got != older {
		t.Fatalf("a store without a CAS was cleared blind: %q", got)
	}
}

// TestContentHoldItem: the batch item is an unguarded update of exactly the
// one cell, refused for every row SetContentHold refuses.
func TestContentHoldItem(t *testing.T) {
	mem := beads.NewMemStore()
	s, id := holdRow(t, mem, nil)
	h := mustHold(t, "operator", holdAt)
	item, err := s.ContentHoldItem(id, h)
	if err != nil {
		t.Fatal(err)
	}
	if item.Create != nil || item.Update == nil || item.Update.ID != id || item.Update.ExpectedRevision != nil {
		t.Fatalf("item = %+v, want an unguarded update of %s", item, id)
	}
	if m := item.Update.Opts.Metadata; len(m) != 1 || m[ContentHoldKey] != h.String() {
		t.Fatalf("item metadata = %v, want exactly {%s: %s}", m, ContentHoldKey, h.String())
	}
	if _, err := mem.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{item}}); err != nil {
		t.Fatal(err)
	}
	if got := holdBead(t, mem, id).Metadata[ContentHoldKey]; got != h.String() {
		t.Fatalf("cell after the batch = %q", got)
	}
	if _, err := s.ContentHoldItem(id, ContentHold{By: "operator"}); !errors.Is(err, ErrInvalidContentHold) {
		t.Errorf("invalid hold = %v, want ErrInvalidContentHold", err)
	}
	other, err := mem.Create(beads.Bead{Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	_, closedID := holdRow(t, mem, nil)
	if err := mem.Close(closedID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id   string
		want error
	}{{" ", nil}, {"gc-missing", beads.ErrNotFound}, {other.ID, ErrSessionNotFound}, {closedID, ErrSessionClosed}} {
		item, itemErr := s.ContentHoldItem(tc.id, h)
		setErr := s.SetContentHold(tc.id, h)
		if itemErr == nil || setErr == nil || (tc.want != nil && (!errors.Is(itemErr, tc.want) || !errors.Is(setErr, tc.want))) {
			t.Errorf("row %q: item %+v, %v; SetContentHold %v; want both refused (%v)", tc.id, item, itemErr, setErr, tc.want)
		}
	}
}

// TestContentHoldIsAConversationCellOutsideThePremise: no v2 fence compares
// the cell.
func TestContentHoldIsAConversationCellOutsideThePremise(t *testing.T) {
	f, ok := LookupField(ContentHoldKey)
	if !ok || f.Class != ClassConversation || !f.NewKey || f.CASOnly || f.InPremise() {
		t.Fatalf("%s registered as %+v (found %v); want ClassConversation, NewKey, not CASOnly, outside the premise", ContentHoldKey, f, ok)
	}
	names := func(ss []Site) []string {
		var out []string
		for _, s := range ss {
			out = append(out, s.File+":"+s.Func)
		}
		return out
	}
	if got := strings.Join(names(f.Writers), ","); got != "internal/session/content_hold.go:Store.SetContentHold,internal/session/content_hold.go:Store.ContentHoldItem" {
		t.Errorf("writers = %s", got)
	}
	if got := strings.Join(names(f.Clears), ","); got != "internal/session/content_hold.go:Store.ClearContentHold" {
		t.Errorf("clears = %s", got)
	}
}
