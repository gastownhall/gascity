package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The two live rows the ladder misread on 8d6r4 (maintainer-city,
// 2026-10-20 17:51Z and 18:10Z; /data/projects/reconciler-review/
// ladder-storeref-fixtures-20261020.jsonl, no secrets): rig-scoped seats
// whose hook claimed a graph step resident in the relocated binding and
// spelled it by its rig leg, while the ladder keyed it city, at the same
// generation. The cells are verbatim.
var claimStoreRefLiveRows = []struct {
	seat, row, gen, bead, workStarted, workLadder string
}{
	{
		seat: "gascity/gc.implementation-worker-9", row: "gcg-session-5029588e90ff014a9baff3c1990a4fba", gen: "2",
		bead:        "gcg--9223372036837583987",
		workStarted: `{"v":1,"claims":[{"store":"rig:gascity","bead":"gcg--9223372036837583987","gen":2,"at":"2026-10-20T18:10:13.755491787Z","via":"claim"}]}`,
		workLadder:  `{"v":1,"gen":2,"entries":[{"store":"city","bead":"gcg--9223372036837583987","kind":"claim","rung":"claimed","since":"2026-10-20T18:10:22.698935312Z","attempts":0,"paused":0,"last":"","next":"2026-10-20T18:11:52.698935312Z","recycles":0,"escalated":false,"seed":"","recycle_at":""}]}`,
	},
	{
		seat: "gascity/adopt-pr-review-claude-4", row: "gcg-session-2f610ced6ce6d57e9cf52df5c1178748", gen: "2",
		bead:        "gcg--9223372036837581966",
		workStarted: `{"v":1,"claims":[{"store":"rig:gascity","bead":"gcg--9223372036837581966","gen":2,"at":"2026-10-20T17:51:24.563313457Z","via":"claim"}]}`,
		workLadder:  `{"v":1,"gen":2,"entries":[{"store":"city","bead":"gcg--9223372036837581966","kind":"claim","rung":"claimed","since":"2026-10-20T17:51:28.084618899Z","attempts":0,"paused":0,"last":"","next":"2026-10-20T17:52:58.084618899Z","recycles":0,"escalated":false,"seed":"","recycle_at":""}]}`,
	},
}

// TestClaimStoreRefIsOneSpellingOfTheResidence pins claimStoreRef as the one
// spelling the three sides share. A binding-resident claim reaches it from the
// census as the binding's own ref, in either census vocabulary, and from the
// hook as the binding its class route took the claim through: all three are
// "city". A rig store is rig:<name> in every spelling, the city work store
// city in every spelling. Kills a hook that keys a binding claim by anything
// but the census's spelling of the same binding.
func TestClaimStoreRefIsOneSpellingOfTheResidence(t *testing.T) {
	route := newClaimRouteFor(t, newClaimRouteClassStore(t))
	binding, ok := route.topology.BindingFor(coordclass.ClassGraph)
	if !ok {
		t.Fatal("the route's frame names no graph binding")
	}
	cfg := &config.City{Workspace: config.Workspace{Name: "maintainer-city"}}
	hook := route.storeRef()
	for _, style := range []censusRefStyle{censusRefBare, censusRefScoped} {
		if census := claimStoreRef(censusRef(cfg, binding.Leg.Ref, style)); census != hook || census != "city" {
			t.Fatalf("binding %q: census spelling (style %d) folds to %q, the hook's to %q; want both city", binding.Leg.Ref, style, census, hook)
		}
	}
	for _, c := range []struct{ in, want string }{
		{"", "city"},
		{"city", "city"},
		{"city:maintainer-city", "city"},
		{string(storeref.ClassRef([]coordclass.Class{coordclass.ClassGraph})), "city"},
		{"gascity", "rig:gascity"},
		{"rig:gascity", "rig:gascity"},
		{censusRef(cfg, storeref.RigRef("gascity"), censusRefBare), "rig:gascity"},
		{censusRef(cfg, storeref.RigRef("gascity"), censusRefScoped), "rig:gascity"},
	} {
		if got := claimStoreRef(c.in); got != c.want {
			t.Errorf("claimStoreRef(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if (*hookClaimClassRoute)(nil).storeRef() != "" {
		t.Fatal("a nil route named a store")
	}
}
