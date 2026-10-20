package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The work cells on a session row (PHASE1-2 §6.1): work_started, written only
// by the seat's own `gc hook --claim`, and work_ladder, written only by the v2
// lane's work-ladder step. Each is one JSON value its owner rewrites whole by
// a CAS on the row (UpdateRowFenced), never blind (CASOnly), and outside the
// premise (ClassWork): a v2 effect that loses its CAS to one re-reads and
// re-decides. This file holds the work_started codec and the one field of
// work_ladder the hook reads, its pending release.
const (
	WorkStartedKey = "work_started"
	WorkLadderKey  = "work_ladder"
)

// How a claim reached a turn: one the hook minted, or an existing assignment
// it handed back.
const (
	WorkStartedViaClaim = "claim"
	WorkStartedViaAdopt = "adopt"
)

const (
	workCellVersion = 1
	// workStartedCap bounds the cell: the writer drops the oldest entry.
	workStartedCap = 8
)

// WorkStartedEntry is one claim the hook handed to a turn of this session.
// Store is the claim's store ref (cmd/gc claimStoreRef's form), "" when the
// hook could not resolve it; an entry with no store names its bead in every
// store.
//
// Resident marks an entry whose Store is the store the claim RESIDES in: the
// store its claim compare-and-swap or its adoption readback ran against,
// spelled as the census spells it, which is how every reader keys the claim.
// An unmarked entry was written by an older hook, which spelled Store as the
// work LEG it ran in. The two agree except for a bead resident in a relocated
// class binding claimed through a rig seat's leg (a graph step: the leg says
// rig:<rig>, its residence city), so an unmarked entry also names its bead
// under any store, at its own generation only (NamesAt).
type WorkStartedEntry struct {
	Store    string    `json:"store"`
	Bead     string    `json:"bead"`
	Gen      int64     `json:"gen"`
	At       time.Time `json:"at"`
	Via      string    `json:"via"`
	Resident bool      `json:"resident,omitempty"`
}

// WorkStarted is the work_started cell: the claims handed to a turn of this
// row, newest first. It is claim-sticky across the row's incarnations.
type WorkStarted struct {
	V      int                `json:"v"`
	Claims []WorkStartedEntry `json:"claims"`
}

// DecodeWorkStarted reads a work_started cell. An absent cell is an empty one.
// A cell it cannot read (bad JSON, or a version it does not know) is an error,
// which a release reader takes as "every claim is started" and a writer as a
// reason not to write. The legacy execution backstop's drain veto is the
// fail-closed exception (cmd/gc workStartedAtGen): there an unreadable cell is
// no evidence.
func DecodeWorkStarted(raw string) (WorkStarted, error) {
	if strings.TrimSpace(raw) == "" {
		return WorkStarted{V: workCellVersion}, nil
	}
	var w WorkStarted
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return WorkStarted{}, fmt.Errorf("decoding %s: %w", WorkStartedKey, err)
	}
	if w.V != workCellVersion {
		return WorkStarted{}, fmt.Errorf("decoding %s: unknown version %d", WorkStartedKey, w.V)
	}
	return w, nil
}

// EncodeWorkStarted is the cell's value for w.
func EncodeWorkStarted(w WorkStarted) string {
	w.V = workCellVersion
	if w.Claims == nil {
		w.Claims = []WorkStartedEntry{}
	}
	b, _ := json.Marshal(w) // strings, ints and times always marshal
	return string(b)
}

// sameClaim reports whether a claim recorded under (aStore, aBead) is the one
// named by (bStore, bBead). An unknown store on either side matches any.
func sameClaim(aStore, aBead, bStore, bBead string) bool {
	if aBead == "" || aBead != bBead {
		return false
	}
	return aStore == "" || bStore == "" || aStore == bStore
}

// NamesAt reports whether e names the claim (store, bead) on a row at
// generation gen: its own store and bead (sameClaim), at any generation, as
// the cell is claim-sticky; or, for an unmarked entry recorded at gen, its
// bead under any store, as the older hook may have spelled a binding-resident
// claim by its leg. The relaxation is as narrow as the evidence it reads: the
// same row, this incarnation's own record, and never a resident entry, whose
// store is exact. An unmarked entry from another generation (whose claim may
// since have moved) or a generation the reader could not read gets none.
func (e WorkStartedEntry) NamesAt(store, bead string, gen int64) bool {
	if sameClaim(e.Store, e.Bead, store, bead) {
		return true
	}
	return !e.Resident && gen > 0 && e.Gen == gen && sameClaim("", e.Bead, "", bead)
}

// Names reports whether w records the claim (store, bead) on a row at
// generation gen (WorkStartedEntry.NamesAt).
func (w WorkStarted) Names(store, bead string, gen int64) bool {
	for _, e := range w.Claims {
		if e.NamesAt(store, bead, gen) {
			return true
		}
	}
	return false
}

// With is w with e recorded first, every entry naming e's claim at e's
// generation dropped (NamesAt), and the oldest beyond the cap dropped.
func (w WorkStarted) With(e WorkStartedEntry) WorkStarted {
	out := WorkStarted{V: workCellVersion, Claims: []WorkStartedEntry{e}}
	for _, c := range w.Claims {
		if c.NamesAt(e.Store, e.Bead, e.Gen) {
			continue
		}
		if len(out.Claims) == workStartedCap {
			break
		}
		out.Claims = append(out.Claims, c)
	}
	return out
}

// Without is w with every entry naming (store, bead) at generation gen
// dropped (NamesAt).
func (w WorkStarted) Without(store, bead string, gen int64) WorkStarted {
	out := WorkStarted{V: workCellVersion}
	for _, c := range w.Claims {
		if !c.NamesAt(store, bead, gen) {
			out.Claims = append(out.Claims, c)
		}
	}
	return out
}

// Respelled is w with every unmarked entry that names bead at generation gen
// (NamesAt) re-spelled as resident in store, keeping its generation, time,
// way and place in the cell: the hook's migration of a claim an older hook
// recorded under its leg. changed reports whether any entry was re-spelled.
// An unknown store ("") is never written as a residence.
func (w WorkStarted) Respelled(store, bead string, gen int64) (out WorkStarted, changed bool) {
	out = WorkStarted{V: workCellVersion, Claims: append([]WorkStartedEntry(nil), w.Claims...)}
	if strings.TrimSpace(store) == "" {
		return out, false
	}
	for i, c := range out.Claims {
		if c.Resident || !c.NamesAt(store, bead, gen) {
			continue
		}
		out.Claims[i].Store, out.Claims[i].Resident = store, true
		changed = true
	}
	return out, changed
}

// DecodeWorkLadderRelease reads the one field of a work_ladder cell the hook
// needs: the claim the controller is releasing. ok is false for an absent
// cell, one with no pending release, and one it cannot read: the ladder never
// writes a release into a cell the hook cannot read without first having
// proved, by the same row CAS, that work_started does not name it.
func DecodeWorkLadderRelease(raw string) (store, bead string, ok bool) {
	if strings.TrimSpace(raw) == "" {
		return "", "", false
	}
	var cell struct {
		V       int `json:"v"`
		Release *struct {
			Store string `json:"store"`
			Bead  string `json:"bead"`
		} `json:"release"`
	}
	if err := json.Unmarshal([]byte(raw), &cell); err != nil || cell.V != workCellVersion || cell.Release == nil {
		return "", "", false
	}
	if bead = strings.TrimSpace(cell.Release.Bead); bead == "" {
		return "", "", false
	}
	return strings.TrimSpace(cell.Release.Store), bead, true
}

// WorkLadderReleases reports whether a work_ladder cell's pending release
// names the claim (store, bead).
func WorkLadderReleases(raw, store, bead string) bool {
	rs, rb, ok := DecodeWorkLadderRelease(raw)
	return ok && sameClaim(rs, rb, store, bead)
}
