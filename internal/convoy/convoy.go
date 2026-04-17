package convoy

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// ConvoyDeps bundles dependencies for convoy operations.
type ConvoyDeps struct {
	Cfg       *config.City
	GetStore  func(rig string) (beads.Store, error)
	FindStore func(beadID string) (beads.Store, error)
	Recorder  events.Recorder
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
func ConvoyCreate(deps ConvoyDeps, store beads.Store, input ConvoyCreateInput) (ConvoyCreateResult, error) {
	b := beads.Bead{
		Title:  input.Title,
		Type:   "convoy",
		Labels: input.Labels,
	}
	ApplyConvoyFields(&b, input.Fields)

	convoy, err := store.Create(b)
	if err != nil {
		return ConvoyCreateResult{}, fmt.Errorf("creating convoy: %w", err)
	}

	linked := 0
	for _, itemID := range input.Items {
		pid := convoy.ID
		if err := store.Update(itemID, beads.UpdateOpts{ParentID: &pid}); err != nil {
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
func ConvoyProgress(_ ConvoyDeps, store beads.Store, id string) (ConvoyProgressResult, error) {
	b, err := store.Get(id)
	if err != nil {
		return ConvoyProgressResult{}, fmt.Errorf("getting convoy %s: %w", id, err)
	}
	if b.Type != "convoy" {
		return ConvoyProgressResult{}, fmt.Errorf("bead %s is not a convoy (type: %s)", id, b.Type)
	}

	children, err := store.List(beads.ListQuery{
		ParentID:      id,
		IncludeClosed: true,
		Sort:          beads.SortCreatedAsc,
	})
	if err != nil {
		return ConvoyProgressResult{}, fmt.Errorf("listing children of %s: %w", id, err)
	}

	total := len(children)
	closed := 0
	for _, c := range children {
		if c.Status == "closed" {
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
func ConvoyAddItems(_ ConvoyDeps, store beads.Store, convoyID string, items []string) error {
	b, err := store.Get(convoyID)
	if err != nil {
		return fmt.Errorf("getting convoy %s: %w", convoyID, err)
	}
	if b.Type != "convoy" {
		return fmt.Errorf("bead %s is not a convoy (type: %s)", convoyID, b.Type)
	}

	for _, itemID := range items {
		pid := convoyID
		if err := store.Update(itemID, beads.UpdateOpts{ParentID: &pid}); err != nil {
			return fmt.Errorf("linking item %s to convoy %s: %w", itemID, convoyID, err)
		}
	}
	return nil
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
