package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convoy"
)

// ConvoyFields is an alias for the shared convoy type.
type ConvoyFields = convoy.ConvoyFields

func applyConvoyFields(b *beads.Bead, fields ConvoyFields) {
	convoy.ApplyConvoyFields(b, fields)
}

func setConvoyFields(store beads.Store, id string, fields ConvoyFields) error {
	return convoy.SetConvoyFields(store, id, fields)
}

func getConvoyFields(b beads.Bead) ConvoyFields {
	return convoy.GetConvoyFields(b)
}
