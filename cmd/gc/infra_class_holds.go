package main

// Holds the copy must carry (ga-1hncv0, ga-uv5rch).
//
// The copy re-adds an infrastructure bead's edges to other infrastructure beads
// and nothing else, so two things that keep a bead from being ready in its
// source store do not reach the binding on their own:
//
//   - a blocking edge onto an unfinished WORK bead. A dependency row can only
//     name an id its own store holds, and the work bead stays in the work
//     store. Copied as a bare edge, SQLite would strand the bead forever and
//     bd would read the edge as external and never block.
//   - a hold bd keeps in the bead's status that the binding cannot record:
//     bd's undated deferral (status=deferred with no defer_until), or any
//     other status bd parks a bead with (blocked, the sanctioned "I cannot
//     proceed" mark; pinned; hooked; review; testing; a custom status). A
//     bd-backed read folds each to "open" and keeps the raw status beside it
//     (Bead.SourceStatus, Bead.IndefinitelyDeferred); Gas City's demand reads
//     and gc hook honor it, so the bead is parked in its source store, while
//     the binding stores the folded "open".
//
// Either way the bead an operator or agent parked would arrive open and ready
// in the binding, and be dispatched. The rehearsals found both on every
// unsplit city.
//
// The edge holds are the shared cross-boundary plan's (cross_boundary_plan.go,
// S5 §8.1): a `blocks` edge onto unfinished work crosses as a cross-store gate
// in the binding, and another blocking type onto unfinished work is refused by
// id. This file carries the status holds (owner decision D-a): a parked bead
// crosses with its defer_until set to infraParkedDeferUntil, which keeps it
// out of every ready and claimable view the binding serves, and with
// beadmeta.MigratedParkedHoldMetadataKey recording the status and the
// defer_until it had, so nothing about the hold is lost. Clearing the
// deferral in the split city releases the bead; `gc storage migrate
// --to-work` restores the recorded status and defer_until from the stamp
// (reverseParkedHold). A dated deferral is not parked: it crosses as its
// date, which the binding honors, and one already due wakes there as bd's
// own sweep would wake it.
//
// The carry is a pure function of the row, with no clock, so every stage that
// compares a source row with the row the copy wrote (the equality stage, a
// resume's keep, the rig import's proof before its clear) reaches the same
// form whenever it runs.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// infraParkedDeferUntil is the defer_until a parked bead carries in a class
// binding: far enough ahead that no clock reaches it, so the bead stays
// deferred until someone releases it.
var infraParkedDeferUntil = time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC)

// infraParkedHold is the content of beadmeta.MigratedParkedHoldMetadataKey:
// the hold a bead had in its source store.
type infraParkedHold struct {
	// Status is bd's raw status in the source store.
	Status string `json:"status"`
	// DeferUntil is the source row's defer_until in RFC 3339, or "".
	DeferUntil string `json:"defer_until,omitempty"`
}

// infraParkedStatus reports the raw status that parks b in its source store
// in a way a class binding cannot record, or "" when nothing does. A closed
// bead is never parked; a deferral with a date crosses as that date.
func infraParkedStatus(b beads.Bead) string {
	if b.Status == "closed" {
		return ""
	}
	if b.IndefinitelyDeferred || b.SourceStatus == "deferred" {
		if b.DeferUntil != nil {
			return ""
		}
		return "deferred"
	}
	switch b.SourceStatus {
	case "", "open", "in_progress", "closed":
		return ""
	}
	return b.SourceStatus
}

// infraCarryParkedHold returns b as a class binding holds it when its source
// store parks it (infraParkedStatus): deferred to infraParkedDeferUntil, the
// hold recorded in beadmeta.MigratedParkedHoldMetadataKey. Any other b is
// returned unchanged. It does not mutate the caller's metadata map.
func infraCarryParkedHold(b beads.Bead) beads.Bead {
	status := infraParkedStatus(b)
	if status == "" {
		return b
	}
	hold := infraParkedHold{Status: status}
	if b.DeferUntil != nil {
		hold.DeferUntil = b.DeferUntil.UTC().Format(time.RFC3339Nano)
	}
	raw, _ := json.Marshal(hold) // a struct of strings always encodes
	metadata := make(beads.StringMap, len(b.Metadata)+1)
	for key, value := range b.Metadata {
		metadata[key] = value
	}
	metadata[beadmeta.MigratedParkedHoldMetadataKey] = string(raw)
	b.Metadata = metadata
	until := infraParkedDeferUntil
	b.DeferUntil = &until
	b.IndefinitelyDeferred = false
	b.SourceStatus = ""
	return b
}

// parseInfraParkedHold reads a beadmeta.MigratedParkedHoldMetadataKey value.
func parseInfraParkedHold(raw string) (infraParkedHold, error) {
	var hold infraParkedHold
	if err := json.Unmarshal([]byte(raw), &hold); err != nil {
		return infraParkedHold{}, err
	}
	if strings.TrimSpace(hold.Status) == "" {
		return infraParkedHold{}, fmt.Errorf("it names no status")
	}
	return hold, nil
}

// deferUntil is the recorded defer_until, or nil.
func (h infraParkedHold) deferUntil() (*time.Time, error) {
	if h.DeferUntil == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, h.DeferUntil)
	if err != nil {
		return nil, fmt.Errorf("its defer_until %q: %w", h.DeferUntil, err)
	}
	return &t, nil
}

// reverseParkedHold returns b, a binding row on its way back to its source
// store, with the hold its stamp records restored: the raw status and the
// defer_until it had. The stamp is removed either way. A bead released in the
// split city (no longer open, or its deferral changed from
// infraParkedDeferUntil) keeps what the split city made of it, except a
// deferral still at infraParkedDeferUntil: that is the copy's, not the
// row's, and a row reopened later in its source store would stay hidden
// behind it, so it goes back as the defer_until the row had. It refuses a
// stamp that does not parse.
func reverseParkedHold(b beads.Bead) (beads.Bead, error) {
	raw, ok := b.Metadata[beadmeta.MigratedParkedHoldMetadataKey]
	if !ok {
		return b, nil
	}
	hold, err := parseInfraParkedHold(raw)
	if err != nil {
		return b, err
	}
	until, err := hold.deferUntil()
	if err != nil {
		return b, err
	}
	metadata := make(beads.StringMap, len(b.Metadata))
	for key, value := range b.Metadata {
		metadata[key] = value
	}
	delete(metadata, beadmeta.MigratedParkedHoldMetadataKey)
	b.Metadata = metadata
	sentinel := b.DeferUntil != nil && b.DeferUntil.Equal(infraParkedDeferUntil)
	if sentinel {
		b.DeferUntil = until
	}
	if b.Status == "open" && sentinel {
		b.Status = hold.Status
	}
	return b, nil
}

// infraParkedRows names, for preflight, each open row of rows the copy
// carries as parked, with its status.
func infraParkedRows(rows []beads.Bead) []string {
	var out []string
	for _, b := range rows {
		if status := infraParkedStatus(b); status != "" {
			out = append(out, fmt.Sprintf("%s (%s)", b.ID, status))
		}
	}
	return out
}
