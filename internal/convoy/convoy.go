package convoy

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// ConvoyDeps bundles the ambient dependencies for convoy operations. Store
// handles are not among them: an operation names the classes it spans in its
// MemberClasses argument, so there is no store lookup by rig name and no
// find-the-store-for-this-id callback to route through.
type ConvoyDeps struct {
	Cfg      *config.City
	Recorder events.Recorder
}

// ConvoyCreateInput holds the parameters for creating a convoy.
type ConvoyCreateInput struct {
	Title  string
	Items  []string
	Fields ConvoyFields
	Labels []string
}

// ConvoyCreateResult holds the result of creating a convoy.
type ConvoyCreateResult struct {
	Convoy      beads.Bead
	LinkedCount int
}

// ConvoyProgressResult holds the progress of a convoy.
type ConvoyProgressResult struct {
	ConvoyID string
	Total    int
	Closed   int
	Complete bool
}

// ConvoyCreate creates a convoy bead, applies metadata, links child items,
// and emits a ConvoyCreated event.
//
// The convoy bead and its tracks edges are created in classes.Convoy. The
// linked items are resolved across every class the caller named; a class it did
// not name is not searched, and an item owned by a class other than the
// convoy's own is refused before any edge is written.
func ConvoyCreate(deps ConvoyDeps, classes MemberClasses, input ConvoyCreateInput) (ConvoyCreateResult, error) {
	b := beads.Bead{
		Title:  input.Title,
		Type:   "convoy",
		Labels: input.Labels,
	}
	ApplyConvoyFields(&b, input.Fields)

	convoy, err := classes.Convoy.Create(b)
	if err != nil {
		return ConvoyCreateResult{}, fmt.Errorf("creating convoy: %w", err)
	}

	linked := 0
	for _, itemID := range input.Items {
		if err := TrackItemIn(classes, convoy.ID, itemID); err != nil {
			return ConvoyCreateResult{Convoy: convoy, LinkedCount: linked},
				fmt.Errorf("linking item %s: %w", itemID, err)
		}
		linked++
	}

	if deps.Recorder != nil {
		deps.Recorder.Record(events.Event{
			Type:    events.ConvoyCreated,
			Subject: convoy.ID,
		})
	}

	return ConvoyCreateResult{Convoy: convoy, LinkedCount: linked}, nil
}

// ConvoyProgress returns the completion progress of a convoy.
//
// The convoy bead is read from classes.Convoy and its members are materialized
// across the classes the caller named. A member owned by a class the caller did
// not name stays unresolved, and an unresolved member is never counted as
// closed — an unspanned class contributes an empty result, and an empty result
// must not read as completed work.
func ConvoyProgress(_ ConvoyDeps, classes MemberClasses, id string) (ConvoyProgressResult, error) {
	b, err := classes.Convoy.Get(id)
	if err != nil {
		return ConvoyProgressResult{}, fmt.Errorf("getting convoy %s: %w", id, err)
	}
	if b.Type != "convoy" {
		return ConvoyProgressResult{}, fmt.Errorf("bead %s is not a convoy (type: %s)", id, b.Type)
	}

	children, err := MembersIn(classes, id, true)
	if err != nil {
		return ConvoyProgressResult{}, fmt.Errorf("listing tracked items of %s: %w", id, err)
	}

	total := len(children)
	closed := 0
	for _, c := range children {
		if IsTerminalStatus(c.Status) {
			closed++
		}
	}

	return ConvoyProgressResult{
		ConvoyID: id,
		Total:    total,
		Closed:   closed,
		Complete: total > 0 && closed == total,
	}, nil
}

// ConvoyAddItems links beads to an existing convoy.
//
// The convoy bead and the new tracks edges are read from and written to
// classes.Convoy; each item is resolved across the classes the caller named.
func ConvoyAddItems(_ ConvoyDeps, classes MemberClasses, convoyID string, items []string) error {
	b, err := classes.Convoy.Get(convoyID)
	if err != nil {
		return fmt.Errorf("getting convoy %s: %w", convoyID, err)
	}
	if b.Type != "convoy" {
		return fmt.Errorf("bead %s is not a convoy (type: %s)", convoyID, b.Type)
	}

	for _, itemID := range items {
		if err := TrackItemIn(classes, convoyID, itemID); err != nil {
			return fmt.Errorf("linking item %s to convoy %s: %w", itemID, convoyID, err)
		}
	}
	return nil
}

// explicitReasonCloser is implemented by stores whose close path accepts a
// reason directly (BdStore maps it to `bd close --reason ...`).
type explicitReasonCloser interface {
	CloseWithReason(id, reason string) error
}

// CloseWithReason stamps a close_reason metadata key on a convoy bead before
// closing it. Stores that accept a reason on the close call receive the same
// text, which lets cities running with validation.on-close=error accept
// system-driven closes whose default reason ("Closed") would otherwise be
// rejected as terse. For stores whose Close path does not consult the reason,
// the metadata still serves as a permanent audit trail of why the convoy was
// closed.
//
// An empty or whitespace-only reason falls through to a plain Close rather
// than stamping a meaningless value that downstream validators would trip on.
func CloseWithReason(store beads.Store, id, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return store.Close(id)
	}
	if err := store.SetMetadata(id, "close_reason", reason); err != nil {
		return fmt.Errorf("stamping convoy %s close reason: %w", id, err)
	}
	if closer, ok := store.(explicitReasonCloser); ok {
		return closer.CloseWithReason(id, reason)
	}
	return store.Close(id)
}

// ConvoyClose closes a convoy bead and emits a ConvoyClosed event.
func ConvoyClose(deps ConvoyDeps, store beads.Store, id string) error {
	if _, err := store.Get(id); err != nil {
		return fmt.Errorf("getting convoy %s: %w", id, err)
	}

	if err := store.Close(id); err != nil {
		return fmt.Errorf("closing convoy %s: %w", id, err)
	}

	if deps.Recorder != nil {
		deps.Recorder.Record(events.Event{
			Type:    events.ConvoyClosed,
			Subject: id,
		})
	}

	return nil
}
