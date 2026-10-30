package session

import (
	"encoding/json"
	"fmt"
	"slices"
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
// Resident marks an entry whose Store is the store the claim RESIDES in, as
// the hook proved it (the store its claim compare-and-swap ran against or its
// ownership readback found the bead in, or the class binding its route proved
// holds the bead), spelled as the census spells it, which is how every reader
// keys the claim. A compare-and-swap's or a readback's store is the residence
// only while bd resolves the bead in the store it ran against, not in one a
// prefix route names (cmd/gc hookStartedKeyFor). An unmarked entry's Store is
// the work LEG the hook ran in, which it could not prove the bead resides in:
// every entry an older hook wrote, and this hook's for an adoption on a city
// that relocates classes whose residence it could not prove. Leg and
// residence agree except for a bead resident in a relocated class binding
// reached through a rig seat's leg (a graph step: the leg says rig:<rig>, its
// residence city), so an unmarked entry also names its bead under any store,
// at its own generation only (NamesAt).
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
// generation dropped (NamesAt), and the oldest beyond the cap dropped. An
// unmarked e proves no residence, so it drops no resident entry recorded at
// its generation: that entry, which does, stays beside it.
func (w WorkStarted) With(e WorkStartedEntry) WorkStarted {
	out := WorkStarted{V: workCellVersion, Claims: []WorkStartedEntry{e}}
	for _, c := range w.Claims {
		if c.NamesAt(e.Store, e.Bead, e.Gen) && (e.Resident || !c.Resident || c.Gen != e.Gen) {
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

// Respelled is w with every unmarked entry recorded at generation gen that
// names bead under a store (NamesAt) re-spelled as resident in store, keeping
// its generation, time, way and place in the cell: the hook's migration of a
// claim it recorded under its leg before it could prove the residence (an
// older hook's, or this one's unproven adoption). An entry of another
// generation, and a storeless one (which already names the bead in every
// store), are left as they are; an unknown store ("") is never written as a
// residence. A resident entry already naming the claim at gen makes a
// re-spelled duplicate redundant, so the leg spelling is dropped instead (one
// of another generation does not: it is no record of this incarnation, and
// the #8133 backstop needs one at gen). changed reports whether the cell
// changed.
func (w WorkStarted) Respelled(store, bead string, gen int64) (out WorkStarted, changed bool) {
	out = WorkStarted{V: workCellVersion, Claims: append([]WorkStartedEntry(nil), w.Claims...)}
	if strings.TrimSpace(store) == "" {
		return out, false
	}
	resident := slices.ContainsFunc(w.Claims, func(c WorkStartedEntry) bool {
		return c.Resident && c.Gen == gen && sameClaim(c.Store, c.Bead, store, bead)
	})
	kept := out.Claims[:0]
	for _, c := range out.Claims {
		if !c.Resident && c.Store != "" && c.Gen == gen && c.NamesAt(store, bead, gen) {
			changed = true
			if resident {
				continue
			}
			c.Store, c.Resident = store, true
			resident = true
		}
		kept = append(kept, c)
	}
	out.Claims = kept
	return out, changed
}

// RecordsBead reports whether w holds an entry naming bead under every store
// on a row at generation gen (NamesAt): a storeless one, or an unmarked one
// recorded at gen. A resident entry is not one: it names its bead in its own
// store only, and a reader keying the claim by another store (where another
// copy of the id may be the claim) reads it unstarted. A hook whose key is not
// a proven residence asks it before writing an entry that would only duplicate
// one every reader already matches.
func (w WorkStarted) RecordsBead(bead string, gen int64) bool {
	return slices.ContainsFunc(w.Claims, func(c WorkStartedEntry) bool {
		return c.Bead != "" && c.Bead == bead && (c.Store == "" || (!c.Resident && gen > 0 && c.Gen == gen))
	})
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
