package workladder

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Version is the one cell version this package reads and writes.
const Version = 1

// MaxStartedClaims is the work_started cell's length; the writer drops the
// oldest entry past it.
const MaxStartedClaims = 8

// DecodeIssue is an unreadable cell: an unknown version, a parse error, or a
// value outside the cell's vocabulary. The ladder treats its own unreadable
// cell as absent; an unreadable work_started reads as every claim started.
type DecodeIssue struct {
	Cell   string // "work_ladder" or "work_started"
	Reason string
}

func (d *DecodeIssue) Error() string { return d.Cell + ": " + d.Reason }

var (
	kindNames = map[Kind]string{KindTrigger: "trigger", KindContinuation: "continuation", KindClaim: "claim"}
	rungNames = map[Rung]string{RungAssigned: "assigned", RungClaimed: "claimed", RungStarted: "started", RungEscalated: "escalated"}
	srcNames  = map[StartSource]string{
		SrcAgent: "agent", SrcExecutionClaimStarted: "execution_claim_started",
		SrcCurrentClaim: "current_claim", SrcCurrentlyProcessing: "currently_processing", SrcClaimSession: "claim_session",
	}
	// phaseNames has no RateLimitClear: a clear episode writes no phase.
	phaseNames = map[RateLimitPhase]string{RateLimitOpen: "open", RateLimitRestarted: "restarted", RateLimitExhausted: "exhausted"}
	// rateLimitAlerts are the alert reasons an episode records raising.
	rateLimitAlerts = map[string]bool{
		AlertRateLimitedFreshRefused: true, AlertRateLimitedAgain: true, AlertRateLimitedHeld: true, AlertRateLimitedQuota: true,
	}
)

func (k Kind) String() string        { return nameOr(kindNames, k) }
func (r Rung) String() string        { return nameOr(rungNames, r) }
func (s StartSource) String() string { return nameOr(srcNames, s) }

func (ph RateLimitPhase) String() string {
	if ph == RateLimitClear {
		return "clear"
	}
	return nameOr(phaseNames, ph)
}

func nameOr[T ~uint8](names map[T]string, v T) string {
	if n, ok := names[v]; ok {
		return n
	}
	return fmt.Sprintf("unknown(%d)", uint8(v))
}

func parseName[T comparable](names map[T]string, s string) (T, bool) {
	for v, n := range names {
		if n == s {
			return v, true
		}
	}
	var zero T
	return zero, false
}

type wireLadder struct {
	wireHead
	// RateLimit is seat-level; older builds ignore it.
	RateLimit *wireRateLimit `json:"rate_limit,omitempty"`
	Entries   []wireEntry    `json:"entries"`
	Release   *wireRelease   `json:"release,omitempty"`
}

// wireHead is the cell's scalar fields, inlined in its text.
type wireHead struct {
	V           int    `json:"v"`
	Gen         int64  `json:"gen"`
	LastNudge   string `json:"last_nudge,omitempty"`
	RecycledGen int64  `json:"recycled_gen,omitempty"`
	// Delivered is seat-level; older builds ignore it.
	Delivered string `json:"delivered,omitempty"`
}

// wireRateLimit is the seat's rate-limit state; absent when it holds
// neither an episode nor a fresh restart. A clear phase writes no phase.
type wireRateLimit struct {
	Phase     string `json:"phase,omitempty"`
	Conv      int64  `json:"conv,omitempty"`
	Since     string `json:"since,omitempty"`
	Checked   string `json:"checked,omitempty"`
	FreshAt   string `json:"fresh_at,omitempty"`
	FromConv  int64  `json:"from_conv,omitempty"`
	Alerted   string `json:"alerted,omitempty"`
	LastFresh string `json:"last_fresh,omitempty"`
}

type wireEntry struct {
	Store      string `json:"store"`
	Bead       string `json:"bead"`
	Kind       string `json:"kind"`
	Rung       string `json:"rung"`
	Since      string `json:"since"`
	Attempts   int    `json:"attempts"`
	Paused     int    `json:"paused"`
	Last       string `json:"last"`
	Next       string `json:"next"`
	Recycles   int    `json:"recycles"`
	Escalated  bool   `json:"escalated"`
	Seed       string `json:"seed"`
	RecycleAt  string `json:"recycle_at"`
	Unreleased bool   `json:"unreleased,omitempty"`
	Refused    int    `json:"refused,omitempty"`
	// Notice is the entry's notice mark; older builds ignore the field.
	Notice *wireNotice `json:"notice,omitempty"`
	// AwaitsHuman marks an entry whose episode is a wait on a person; older
	// builds ignore the field.
	AwaitsHuman bool `json:"awaits_human,omitempty"`
}

type wireNotice struct {
	Op     string `json:"op"`
	At     string `json:"at"`
	Paused bool   `json:"paused,omitempty"`
}

type wireRelease struct {
	Store string `json:"store"`
	Bead  string `json:"bead"`
	Gen   int64  `json:"gen"`
	At    string `json:"at"`
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// EncodeLadder is the work_ladder cell's text for r, entries in key order.
// An absent record (V == 0) encodes as "".
func EncodeLadder(r Record) string {
	if r.V == 0 {
		return ""
	}
	b, err := json.Marshal(toWire(r))
	if err != nil { // unreachable: every field is a string, number or bool
		panic(err)
	}
	return string(b)
}

// sameText reports whether a and b encode to the same cell text, without
// encoding either: the text is a function of the wire form, which holds
// every field the text does, so comparing wire forms misses none.
func sameText(a, b Record) bool {
	if a.V == 0 || b.V == 0 {
		return a.V == b.V
	}
	wa, wb := toWire(a), toWire(b)
	return wa.wireHead == wb.wireHead && sameRef(wa.RateLimit, wb.RateLimit) && sameRef(wa.Release, wb.Release) &&
		slices.EqualFunc(wa.Entries, wb.Entries, func(x, y wireEntry) bool {
			nx, ny := x.Notice, y.Notice
			x.Notice, y.Notice = nil, nil
			return x == y && sameRef(nx, ny)
		})
}

// sameRef reports whether a and b are both nil or point at equal values.
func sameRef[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// toWire is r's wire form, entries in key order.
func toWire(r Record) wireLadder {
	w := wireLadder{
		wireHead: wireHead{
			V: Version, Gen: r.Gen, LastNudge: fmtTime(r.LastNudge), RecycledGen: r.RecycledGen, Delivered: fmtTime(r.Delivered),
		},
		RateLimit: encodeRateLimit(r.RateLimit), Entries: []wireEntry{},
	}
	for _, e := range sortedEntries(r.Entries) {
		seed := ""
		if e.Seed != 0 {
			seed = e.Seed.String()
		}
		w.Entries = append(w.Entries, wireEntry{
			Store: e.Key.Store, Bead: e.Key.Bead, Kind: e.Kind.String(), Rung: e.Rung.String(),
			Since: fmtTime(e.Since), Attempts: e.Attempts, Paused: e.Paused, Last: fmtTime(e.Last),
			Next: fmtTime(e.Next), Recycles: e.Recycles, Escalated: e.Escalated, Seed: seed,
			RecycleAt: fmtTime(e.RecycleAt), Unreleased: e.Unreleased, Refused: e.Refused, Notice: encodeNotice(e.Notice),
			AwaitsHuman: e.AwaitsHuman,
		})
	}
	if r.Release != nil {
		w.Release = &wireRelease{Store: r.Release.Key.Store, Bead: r.Release.Key.Bead, Gen: r.Release.Gen, At: fmtTime(r.Release.At)}
	}
	return w
}

func encodeRateLimit(rl RateLimit) *wireRateLimit {
	if rl.IsZero() {
		return nil
	}
	w := &wireRateLimit{
		Conv: rl.Conv, Since: fmtTime(rl.Since), Checked: fmtTime(rl.Checked), FreshAt: fmtTime(rl.FreshAt),
		FromConv: rl.FromConv, Alerted: rl.Alerted, LastFresh: fmtTime(rl.LastFresh),
	}
	if rl.Phase != RateLimitClear {
		w.Phase = rl.Phase.String()
	}
	return w
}

// decodeRateLimit reads the seat's rate-limit state; an unknown phase or
// alert reason is a reason the cell is unreadable.
func decodeRateLimit(w *wireRateLimit) (RateLimit, string) {
	if w == nil {
		return RateLimit{}, ""
	}
	rl := RateLimit{Conv: w.Conv, FromConv: w.FromConv, Alerted: w.Alerted}
	if w.Phase != "" {
		ph, ok := parseName(phaseNames, w.Phase)
		if !ok {
			return RateLimit{}, fmt.Sprintf("rate_limit: unknown phase %q", w.Phase)
		}
		rl.Phase = ph
	}
	if w.Alerted != "" && !rateLimitAlerts[w.Alerted] {
		return RateLimit{}, fmt.Sprintf("rate_limit: unknown alert %q", w.Alerted)
	}
	for _, f := range []struct {
		dst  *time.Time
		src  string
		name string
	}{{&rl.Since, w.Since, "since"}, {&rl.Checked, w.Checked, "checked"}, {&rl.FreshAt, w.FreshAt, "fresh_at"}, {&rl.LastFresh, w.LastFresh, "last_fresh"}} {
		t, err := parseTime(f.src)
		if err != nil {
			return RateLimit{}, fmt.Sprintf("rate_limit: %s %q: %v", f.name, f.src, err)
		}
		*f.dst = t
	}
	return rl, ""
}

func encodeNotice(m NoticeMark) *wireNotice {
	if m.IsZero() {
		return nil
	}
	return &wireNotice{Op: m.Op, At: fmtTime(m.At), Paused: m.Paused}
}

// DecodeLadder reads a work_ladder cell. "" is an absent cell (V == 0, no
// error). Anything unreadable is a *DecodeIssue with a zero Record. Fields
// it does not know are ignored, and the next EncodeLadder drops them: the
// per-claim rate-limit flags earlier builds wrote on entries
// (rate_limit_alerted, fresh_recycled, fresh_served, fresh_queued) read as
// absent.
func DecodeLadder(raw string) (Record, error) {
	if strings.TrimSpace(raw) == "" {
		return Record{}, nil
	}
	bad := func(format string, a ...any) (Record, error) {
		return Record{}, &DecodeIssue{Cell: "work_ladder", Reason: fmt.Sprintf(format, a...)}
	}
	var w wireLadder
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return bad("parse: %v", err)
	}
	if w.V != Version {
		return bad("unknown version %d", w.V)
	}
	r := Record{V: Version, Gen: w.Gen, RecycledGen: w.RecycledGen}
	lastNudge, err := parseTime(w.LastNudge)
	if err != nil {
		return bad("last_nudge %q: %v", w.LastNudge, err)
	}
	r.LastNudge = lastNudge
	if r.Delivered, err = parseTime(w.Delivered); err != nil {
		return bad("delivered %q: %v", w.Delivered, err)
	}
	rl, issue := decodeRateLimit(w.RateLimit)
	if issue != "" {
		return bad("%s", issue)
	}
	r.RateLimit = rl
	seen := map[ClaimKey]bool{}
	for i, we := range w.Entries {
		e := Entry{
			Key: ClaimKey{Store: we.Store, Bead: we.Bead}, Attempts: we.Attempts, Paused: we.Paused, Recycles: we.Recycles,
			Escalated: we.Escalated, Unreleased: we.Unreleased, Refused: we.Refused, AwaitsHuman: we.AwaitsHuman,
		}
		if e.Key.Bead == "" {
			return bad("entry %d: empty bead", i)
		}
		if seen[e.Key] {
			return bad("entry %d: duplicate %s/%s", i, e.Key.Store, e.Key.Bead)
		}
		seen[e.Key] = true
		if e.Attempts < 0 || e.Paused < 0 || e.Recycles < 0 || e.Refused < 0 {
			return bad("entry %d: negative count", i)
		}
		var ok bool
		if e.Kind, ok = parseName(kindNames, we.Kind); !ok {
			return bad("entry %d: unknown kind %q", i, we.Kind)
		}
		if e.Rung, ok = parseName(rungNames, we.Rung); !ok {
			return bad("entry %d: unknown rung %q", i, we.Rung)
		}
		if we.Seed != "" {
			if e.Seed, ok = parseName(srcNames, we.Seed); !ok {
				return bad("entry %d: unknown seed %q", i, we.Seed)
			}
		}
		for _, f := range []struct {
			dst *time.Time
			src string
		}{{&e.Since, we.Since}, {&e.Last, we.Last}, {&e.Next, we.Next}, {&e.RecycleAt, we.RecycleAt}} {
			t, err := parseTime(f.src)
			if err != nil {
				return bad("entry %d: time %q: %v", i, f.src, err)
			}
			*f.dst = t
		}
		if we.Notice != nil {
			if we.Notice.Op == "" {
				return bad("entry %d: notice with no op", i)
			}
			at, err := parseTime(we.Notice.At)
			if err != nil || at.IsZero() {
				return bad("entry %d: notice time %q", i, we.Notice.At)
			}
			e.Notice = NoticeMark{Op: we.Notice.Op, At: at, Paused: we.Notice.Paused}
		}
		r.Entries = append(r.Entries, e)
	}
	if w.Release != nil {
		at, err := parseTime(w.Release.At)
		if err != nil {
			return bad("release: time %q: %v", w.Release.At, err)
		}
		if w.Release.Bead == "" {
			return bad("release: empty bead")
		}
		r.Release = &ReleaseMark{Key: ClaimKey{Store: w.Release.Store, Bead: w.Release.Bead}, Gen: w.Release.Gen, At: at}
	}
	r.Entries = sortedEntries(r.Entries)
	return r, nil
}

// Started is the work_started cell, written only by the agent's hook.
type Started struct {
	V      int
	Claims []StartedClaim // newest first, at most MaxStartedClaims
}

// StartedClaim is one hand-over: the hook gave Key to a turn of generation
// Gen at At, minting the claim ("claim") or adopting an assignment
// ("adopt").
type StartedClaim struct {
	Key ClaimKey
	Gen int64
	At  time.Time
	Via string
	// Resident marks a claim whose Key.Store is the store it resides in, as
	// the hook proved it, the way every reader keys it; an unmarked claim's
	// store is the work leg the hook ran in, unproven: an older hook's entry,
	// or an adoption whose residence the hook could not prove
	// (internal/session.WorkStartedEntry).
	Resident bool
}

// Started via values.
const (
	ViaClaim = "claim"
	ViaAdopt = "adopt"
)

// Names reports whether s holds key on a row at generation gen. A claim
// recorded with no store (the hook could not resolve it) names its bead in
// every store, and a key with no store matches its bead under any. A claim
// not marked Resident also names its bead under any store at its own
// generation: its store is the leg the hook ran in, which for a
// binding-resident claim reached through a rig seat is not its residence.
// The agent's writer (internal/session.WorkStartedEntry.NamesAt) reads it the
// same way.
func (s Started) Names(key ClaimKey, gen int64) bool {
	return slices.ContainsFunc(s.Claims, func(c StartedClaim) bool {
		return SameClaim(c.Key, key) ||
			!c.Resident && gen > 0 && c.Gen == gen && SameClaim(ClaimKey{Bead: c.Key.Bead}, ClaimKey{Bead: key.Bead})
	})
}

// SameClaim reports whether a and b name the same claim: the same bead, in
// the same store or with either store unknown.
func SameClaim(a, b ClaimKey) bool {
	return a.Bead != "" && a.Bead == b.Bead && (a.Store == "" || b.Store == "" || a.Store == b.Store)
}

type wireStarted struct {
	V      int            `json:"v"`
	Claims []wireStartedC `json:"claims"`
}

type wireStartedC struct {
	Store    string `json:"store"`
	Bead     string `json:"bead"`
	Gen      int64  `json:"gen"`
	At       string `json:"at"`
	Via      string `json:"via"`
	Resident bool   `json:"resident,omitempty"`
}

// EncodeStarted is the work_started cell's text for s, in s's order.
func EncodeStarted(s Started) string {
	w := wireStarted{V: Version, Claims: []wireStartedC{}}
	for _, c := range s.Claims {
		w.Claims = append(w.Claims, wireStartedC{Store: c.Key.Store, Bead: c.Key.Bead, Gen: c.Gen, At: fmtTime(c.At), Via: c.Via, Resident: c.Resident})
	}
	b, err := json.Marshal(w)
	if err != nil { // unreachable: every field is a string or number
		panic(err)
	}
	return string(b)
}

// DecodeStarted reads a work_started cell. "" is an empty cell (no claims,
// no error). Anything unreadable is a *DecodeIssue: the caller must then
// treat every outstanding claim as started. It reads exactly what the
// agent's writer reads (internal/session.DecodeWorkStarted: the JSON and the
// version, and every claim's time, which the writer's time.Time requires),
// so the two never disagree on whether the cell is readable.
func DecodeStarted(raw string) (Started, error) {
	if strings.TrimSpace(raw) == "" {
		return Started{V: Version}, nil
	}
	bad := func(format string, a ...any) (Started, error) {
		return Started{}, &DecodeIssue{Cell: "work_started", Reason: fmt.Sprintf(format, a...)}
	}
	var w wireStarted
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return bad("parse: %v", err)
	}
	if w.V != Version {
		return bad("unknown version %d", w.V)
	}
	s := Started{V: Version}
	for i, c := range w.Claims {
		if c.At == "" {
			return bad("claim %d: no time", i)
		}
		at, err := parseTime(c.At)
		if err != nil {
			return bad("claim %d: time %q: %v", i, c.At, err)
		}
		s.Claims = append(s.Claims, StartedClaim{Key: ClaimKey{Store: c.Store, Bead: c.Bead}, Gen: c.Gen, At: at, Via: c.Via, Resident: c.Resident})
	}
	return s, nil
}

func sortedEntries(es []Entry) []Entry {
	out := slices.Clone(es)
	slices.SortFunc(out, func(a, b Entry) int { return a.Key.compare(b.Key) })
	return out
}
