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
)

func (k Kind) String() string        { return nameOr(kindNames, k) }
func (r Rung) String() string        { return nameOr(rungNames, r) }
func (s StartSource) String() string { return nameOr(srcNames, s) }

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
	V           int          `json:"v"`
	Gen         int64        `json:"gen"`
	LastNudge   string       `json:"last_nudge,omitempty"`
	RecycledGen int64        `json:"recycled_gen,omitempty"`
	Entries     []wireEntry  `json:"entries"`
	Release     *wireRelease `json:"release,omitempty"`
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
	w := wireLadder{V: Version, Gen: r.Gen, LastNudge: fmtTime(r.LastNudge), RecycledGen: r.RecycledGen, Entries: []wireEntry{}}
	for _, e := range sortedEntries(r.Entries) {
		seed := ""
		if e.Seed != 0 {
			seed = e.Seed.String()
		}
		w.Entries = append(w.Entries, wireEntry{
			Store: e.Key.Store, Bead: e.Key.Bead, Kind: e.Kind.String(), Rung: e.Rung.String(),
			Since: fmtTime(e.Since), Attempts: e.Attempts, Paused: e.Paused, Last: fmtTime(e.Last),
			Next: fmtTime(e.Next), Recycles: e.Recycles, Escalated: e.Escalated, Seed: seed,
			RecycleAt: fmtTime(e.RecycleAt), Unreleased: e.Unreleased, Refused: e.Refused,
		})
	}
	if r.Release != nil {
		w.Release = &wireRelease{Store: r.Release.Key.Store, Bead: r.Release.Key.Bead, Gen: r.Release.Gen, At: fmtTime(r.Release.At)}
	}
	b, err := json.Marshal(w)
	if err != nil { // unreachable: every field is a string, number or bool
		panic(err)
	}
	return string(b)
}

// DecodeLadder reads a work_ladder cell. "" is an absent cell (V == 0, no
// error). Anything unreadable is a *DecodeIssue with a zero Record.
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
	seen := map[ClaimKey]bool{}
	for i, we := range w.Entries {
		e := Entry{Key: ClaimKey{Store: we.Store, Bead: we.Bead}, Attempts: we.Attempts, Paused: we.Paused, Recycles: we.Recycles, Escalated: we.Escalated, Unreleased: we.Unreleased, Refused: we.Refused}
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
