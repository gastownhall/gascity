package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// demandServableForTemplate is the single-template form of the shared
// demand/serve agreement predicate (demand_serve_predicate.go), for the keyed
// readers.
//
// The fleet demand loop asks "which of these templates is this row demand for?"
// across a whole store group. Every keyed reader asks a narrower question: it
// already holds one exact (workID, poolTarget, sourceStore) key and only needs
// to know whether that key is still live. This wrapper is that question, and it
// keeps the upstream predicate the single definition of the serving rules
// instead of letting the keyed path grow its own copy — which is precisely how
// the demand and claim readers drifted apart in the first place (#5250).
//
// The empty-template guard is not decoration. A caller holding an empty
// PoolTarget would otherwise ask a one-key set keyed by "", and a route
// candidate that normalizes to "" would be told yes.
func demandServableForTemplate(cfg *config.City, b beads.Bead, template string) bool {
	template = strings.TrimSpace(template)
	if template == "" {
		return false
	}
	target, servable := demandServableForTemplates(cfg, b, map[string]struct{}{template: {}})
	return servable && target == template
}
