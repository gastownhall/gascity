package storeref

import (
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// The Stores.Plan corpus (T7b): every T7 federation intent over every
// topology, planned over the minted handles, pinned as rows. "g" topologies
// are minted under a projection (taggedProjection, the split-strict gate's
// shape): they relocate nothing, yet their class handles read a class view of
// the work store, so the plans keep the split shape a gate fixture exercises.

// storesTopologies are the corpus's minted epochs, by name:
//
//	T0    single store              T0g   T0 under the gate's projection
//	T2u   single store, two rigs    T2    whole split, two rigs
//	T2g   T2u under the gate        T2rf  T2, alpha proven relic-free
//	T2s   T2, bravo suspended       T2wrf T2, the city leg marked relic-free
//	T2cr  T2, graph refused per class by the plane (a worker site)
//	T3    refused                   T3k   refused, relic-proven binding
//	T5    per-class split           Ts    sessions relocated alone
//	T5o   graph and orders on two different bindings
func storesTopologies() map[string]func() (topoFixture, StoresOptions) {
	none := StoresOptions{}
	gate := StoresOptions{Projection: taggedProjection{}}
	twoRigs := map[string]string{"alpha": alphaPrefix, "bravo": bravoPrefix}
	mark := func(f topoFixture, rig string, set func(*Leg)) topoFixture {
		for i := range f.topo.Rigs {
			if f.topo.Rigs[i].Ref == RigRef(rig) {
				set(&f.topo.Rigs[i])
			}
		}
		return f
	}
	return map[string]func() (topoFixture, StoresOptions){
		"T0":  func() (topoFixture, StoresOptions) { return newT0(), none },
		"T0g": func() (topoFixture, StoresOptions) { return newT0(), gate },
		"T2u": func() (topoFixture, StoresOptions) { return buildTopology("T2u", twoRigs, nil, nil), none },
		"T2":  func() (topoFixture, StoresOptions) { return newT2(), none },
		"T2g": func() (topoFixture, StoresOptions) { return buildTopology("T2g", twoRigs, nil, nil), gate },
		"T2rf": func() (topoFixture, StoresOptions) {
			return mark(newT2(), "alpha", func(l *Leg) { l.RelicFree = true }), none
		},
		"T2s": func() (topoFixture, StoresOptions) {
			return mark(newT2(), "bravo", func(l *Leg) { l.Suspended = true }), none
		},
		"T2wrf": func() (topoFixture, StoresOptions) {
			f := newT2()
			f.topo.Work.RelicFree = true
			return f, none
		},
		"T2cr": func() (topoFixture, StoresOptions) {
			return newT2(), StoresOptions{ClassRefusals: map[coordclass.Class]RefusingClassStore{coordclass.ClassGraph: refusingStore{}}}
		},
		"T3":  func() (topoFixture, StoresOptions) { return newT3(), none },
		"T3k": func() (topoFixture, StoresOptions) { return newT3Known(), none },
		"T5":  func() (topoFixture, StoresOptions) { return newT5(), none },
		"T5o": func() (topoFixture, StoresOptions) {
			return buildTopology("T5o", nil, []bindingSpec{
				{classes: []coordclass.Class{coordclass.ClassGraph}, prefixes: []string{"gcg"}, relics: true},
				{classes: []coordclass.Class{coordclass.ClassOrders}, prefixes: []string{"gco"}, relics: true},
			}, nil), none
		},
		"Ts": func() (topoFixture, StoresOptions) {
			sessions := []coordclass.Class{coordclass.ClassSessions}
			return buildTopology("Ts", nil, []bindingSpec{{classes: sessions, prefixes: []string{"gcs"}, relics: true}}, nil), none
		},
	}
}

// storesIntents are the T7 intents of the corpus, by printed name, over f's
// stores: Near is the city work store (the store a B2 walk, an attachment's
// parent or an agent most often sits in), the alpha rig's store, the binding
// itself (a co-resident Near), or none.
func storesIntents(f topoFixture) []struct {
	name   string
	intent Intent
} {
	work := beads.Store(f.work)
	var alpha beads.Store
	if s, ok := f.rigs["alpha"]; ok {
		alpha = s
	}
	var binding beads.Store
	for _, b := range f.topo.Bindings {
		binding = b.Leg.Store
	}
	alphaRef := RigRef("alpha")
	return []struct {
		name   string
		intent Intent
	}{
		{"Molecule(RootNear)", Molecule{Kind: MoleculeRootNear, Near: work, NearRef: WorkRef}},
		{"Molecule(RootNear,no near)", Molecule{Kind: MoleculeRootNear}},
		{"Molecule(ParentNear)", Molecule{Kind: MoleculeParentNear, Near: work, NearRef: WorkRef}},
		{"Molecule(ParentNear,near binding)", Molecule{Kind: MoleculeParentNear, Near: binding, NearRef: "near"}},
		{"Molecule(Attachment)", Molecule{Kind: MoleculeAttachment, Near: work, NearRef: WorkRef}},
		{"Molecule(Attachment,alpha)", Molecule{Kind: MoleculeAttachment, Scope: alphaRef, Near: alpha, NearRef: alphaRef}},
		{"Molecule(Attachment,no near)", Molecule{Kind: MoleculeAttachment}},
		{"Molecule(Roots)", Molecule{Kind: MoleculeRoots}},
		{"Molecule(Active)", Molecule{Kind: MoleculeActive, Near: work, NearRef: WorkRef}},
		{"Molecule(Active,alpha)", Molecule{Kind: MoleculeActive, Scope: alphaRef, Near: alpha, NearRef: alphaRef}},
		{"OrderRuns", OrderRuns{}},
		{"OrderRuns(pours)", OrderRuns{PoursWork: true}},
		{"OrderRuns(alpha)", OrderRuns{Scope: alphaRef}},
		{"OrderRuns(alpha,legacy)", OrderRuns{Scope: alphaRef, LegacyCity: true}},
		{"OrderRuns(alpha,pours,legacy)", OrderRuns{Scope: alphaRef, PoursWork: true, LegacyCity: true}},
		{"Workflow", Workflow{}},
		{"Workflow(retired)", Workflow{IncludeRetiredRigs: true}},
		{"Workflow(suspended)", Workflow{IncludeSuspendedRigs: true}},
	}
}

// storesCorpus is the pinned artifact, keyed "<intent> x <topology>". The
// rows to read first:
//
//   - T0, T2u: a single-store city collapses Roots and Active to the graph
//     placement (the GC and the step reminder read no work leg there), and
//     OrderRuns to the scope's work store (and the city's, for a legacy rig
//     order). No relic leg exists without a split.
//   - T2: OrderRuns reads [graph, orders] and the work store only when the
//     order pours work; every Workflow leg is Fatal, the scope legs relic.
//   - T0g, T2g: the gate's projection keeps the split shape.
//   - T2rf drops the relic-free rig (a federated tail for a retired-rig
//     reader); T2wrf never retires the city leg (OD-T7-11); T2s never reads
//     the suspended rig unless asked.
//   - T2cr: a plane's per-class refusal is the graph leg's store, refusing,
//     under the binding's ref; the topology is not refused.
//   - Attachment: [graph, Near], both FatalUnlessResolved (R2).
var storesCorpus = map[string]string{
	"Molecule(Active) x T0":                  `Union(first-leg-wins): ""[Authority,Fatal,federated]`,
	"Molecule(Active,alpha) x T0":            `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(Attachment) x T0":              `FirstOwner: ""[Authority,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T0":        `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(Attachment,no near) x T0":      `FirstOwner: ""[Authority,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear) x T0":              `FirstOwner: ""[Near,Fatal,federated]`,
	"Molecule(ParentNear,near binding) x T0": `FirstOwner: ""[Authority,Fatal,federated]`,
	"Molecule(RootNear) x T0":                `FirstOwner: ""[Authority,Fatal,federated]`,
	"Molecule(RootNear,no near) x T0":        `FirstOwner: ""[Authority,Fatal,federated]`,
	"Molecule(Roots) x T0":                   `Union(first-leg-wins): ""[Authority,Fatal,federated]`,
	"OrderRuns x T0":                         `Union(first-leg-wins): ""[Authority,Fatal,federated]`,
	"OrderRuns(alpha) x T0":                  `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(alpha,legacy) x T0":           `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(alpha,pours,legacy) x T0":     `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(pours) x T0":                  `Union(first-leg-wins): ""[Authority,Fatal,federated]`,
	"Workflow x T0":                          `Union(first-leg-wins): ""[FederationTail,Fatal,federated]`,
	"Workflow(retired) x T0":                 `Union(first-leg-wins): ""[FederationTail,Fatal,federated]`,
	"Workflow(suspended) x T0":               `Union(first-leg-wins): ""[FederationTail,Fatal,federated]`,

	"Molecule(Active) x T0g":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Molecule(Active,alpha) x T0g":            `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(Attachment) x T0g":              `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T0g":        `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(Attachment,no near) x T0g":      `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear) x T0g":              `FirstOwner: ""[Near,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"Molecule(ParentNear,near binding) x T0g": `FirstOwner: class:gmnos[Authority,Fatal,federated]`,
	"Molecule(RootNear) x T0g":                `FirstOwner: class:gmnos[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(RootNear,no near) x T0g":        `FirstOwner: class:gmnos[Authority,Fatal,federated]`,
	"Molecule(Roots) x T0g":                   `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated]`,
	"OrderRuns x T0g":                         `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha) x T0g":                  `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(alpha,legacy) x T0g":           `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(alpha,pours,legacy) x T0g":     `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(pours) x T0g":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Workflow x T0g":                          `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic]`,
	"Workflow(retired) x T0g":                 `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic]`,
	"Workflow(suspended) x T0g":               `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic]`,

	"Molecule(Active) x T2u":                  `Union(first-leg-wins): ""[Authority,Fatal,federated]`,
	"Molecule(Active,alpha) x T2u":            `Union(first-leg-wins): rig:alpha[Authority,Fatal,federated]`,
	"Molecule(Attachment) x T2u":              `FirstOwner: ""[Authority,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T2u":        `FirstOwner: rig:alpha[Authority,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,no near) x T2u":      `FirstOwner: ""[Authority,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear) x T2u":              `FirstOwner: ""[Near,Fatal,federated]`,
	"Molecule(ParentNear,near binding) x T2u": `FirstOwner: ""[Authority,Fatal,federated]`,
	"Molecule(RootNear) x T2u":                `FirstOwner: ""[Authority,Fatal,federated]`,
	"Molecule(RootNear,no near) x T2u":        `FirstOwner: ""[Authority,Fatal,federated]`,
	"Molecule(Roots) x T2u":                   `Union(first-leg-wins): ""[Authority,Fatal,federated]`,
	"OrderRuns x T2u":                         `Union(first-leg-wins): ""[Authority,Fatal,federated]`,
	"OrderRuns(alpha) x T2u":                  `Union(first-leg-wins): rig:alpha[Authority,Fatal,federated]`,
	"OrderRuns(alpha,legacy) x T2u":           `Union(first-leg-wins): rig:alpha[Authority,Fatal,federated] > ""[FederationTail,Fatal,federated]`,
	"OrderRuns(alpha,pours,legacy) x T2u":     `Union(first-leg-wins): rig:alpha[Authority,Fatal,federated] > ""[FederationTail,Fatal,federated]`,
	"OrderRuns(pours) x T2u":                  `Union(first-leg-wins): ""[Authority,Fatal,federated]`,
	"Workflow x T2u":                          `Union(first-leg-wins): ""[FederationTail,Fatal,federated] > rig:alpha[FederationTail,Fatal,federated] > rig:bravo[FederationTail,Fatal,federated]`,
	"Workflow(retired) x T2u":                 `Union(first-leg-wins): ""[FederationTail,Fatal,federated] > rig:alpha[FederationTail,Fatal,federated] > rig:bravo[FederationTail,Fatal,federated]`,
	"Workflow(suspended) x T2u":               `Union(first-leg-wins): ""[FederationTail,Fatal,federated] > rig:alpha[FederationTail,Fatal,federated] > rig:bravo[FederationTail,Fatal,federated]`,

	"Molecule(Active) x T2":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Molecule(Active,alpha) x T2":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > rig:alpha[V1,Fatal,federated]`,
	"Molecule(Attachment) x T2":              `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T2":        `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > rig:alpha[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,no near) x T2":      `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear) x T2":              `FirstOwner: ""[Near,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"Molecule(ParentNear,near binding) x T2": `FirstOwner: near[Near,Fatal,federated]`,
	"Molecule(RootNear) x T2":                `FirstOwner: class:gmnos[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(RootNear,no near) x T2":        `FirstOwner: class:gmnos[Authority,Fatal,federated]`,
	"Molecule(Roots) x T2":                   `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated] > rig:alpha[V1,PartialDegrade,federated] > rig:bravo[V1,PartialDegrade,federated]`,
	"OrderRuns x T2":                         `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha) x T2":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha,legacy) x T2":           `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha,pours,legacy) x T2":     `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > rig:alpha[V1,Fatal,federated]`,
	"OrderRuns(pours) x T2":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Workflow x T2":                          `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic]`,
	"Workflow(retired) x T2":                 `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic]`,
	"Workflow(suspended) x T2":               `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic]`,

	"Molecule(Active) x T2g":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Molecule(Active,alpha) x T2g":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > rig:alpha[V1,Fatal,federated]`,
	"Molecule(Attachment) x T2g":              `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T2g":        `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > rig:alpha[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,no near) x T2g":      `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear) x T2g":              `FirstOwner: ""[Near,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"Molecule(ParentNear,near binding) x T2g": `FirstOwner: class:gmnos[Authority,Fatal,federated]`,
	"Molecule(RootNear) x T2g":                `FirstOwner: class:gmnos[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(RootNear,no near) x T2g":        `FirstOwner: class:gmnos[Authority,Fatal,federated]`,
	"Molecule(Roots) x T2g":                   `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated] > rig:alpha[V1,PartialDegrade,federated] > rig:bravo[V1,PartialDegrade,federated]`,
	"OrderRuns x T2g":                         `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha) x T2g":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha,legacy) x T2g":           `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha,pours,legacy) x T2g":     `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > rig:alpha[V1,Fatal,federated]`,
	"OrderRuns(pours) x T2g":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Workflow x T2g":                          `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic]`,
	"Workflow(retired) x T2g":                 `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic]`,
	"Workflow(suspended) x T2g":               `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic]`,

	"Molecule(Active) x T2rf":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Molecule(Active,alpha) x T2rf":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > rig:alpha[V1,Fatal,federated]`,
	"Molecule(Attachment) x T2rf":              `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T2rf":        `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > rig:alpha[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,no near) x T2rf":      `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear) x T2rf":              `FirstOwner: ""[Near,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"Molecule(ParentNear,near binding) x T2rf": `FirstOwner: near[Near,Fatal,federated]`,
	"Molecule(RootNear) x T2rf":                `FirstOwner: class:gmnos[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(RootNear,no near) x T2rf":        `FirstOwner: class:gmnos[Authority,Fatal,federated]`,
	"Molecule(Roots) x T2rf":                   `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated] > rig:alpha[V1,PartialDegrade,federated] > rig:bravo[V1,PartialDegrade,federated]`,
	"OrderRuns x T2rf":                         `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha) x T2rf":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha,legacy) x T2rf":           `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha,pours,legacy) x T2rf":     `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > rig:alpha[V1,Fatal,federated]`,
	"OrderRuns(pours) x T2rf":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Workflow x T2rf":                          `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic]`,
	"Workflow(retired) x T2rf":                 `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[FederationTail,Fatal,federated] > rig:bravo[Relic,Fatal,relic]`,
	"Workflow(suspended) x T2rf":               `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic]`,

	"Molecule(Active) x T2s":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Molecule(Active,alpha) x T2s":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > rig:alpha[V1,Fatal,federated]`,
	"Molecule(Attachment) x T2s":              `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T2s":        `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > rig:alpha[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,no near) x T2s":      `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear) x T2s":              `FirstOwner: ""[Near,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"Molecule(ParentNear,near binding) x T2s": `FirstOwner: near[Near,Fatal,federated]`,
	"Molecule(RootNear) x T2s":                `FirstOwner: class:gmnos[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(RootNear,no near) x T2s":        `FirstOwner: class:gmnos[Authority,Fatal,federated]`,
	"Molecule(Roots) x T2s":                   `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated] > rig:alpha[V1,PartialDegrade,federated]`,
	"OrderRuns x T2s":                         `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha) x T2s":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha,legacy) x T2s":           `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha,pours,legacy) x T2s":     `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > rig:alpha[V1,Fatal,federated]`,
	"OrderRuns(pours) x T2s":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Workflow x T2s":                          `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic]`,
	"Workflow(retired) x T2s":                 `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic]`,
	"Workflow(suspended) x T2s":               `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic]`,

	"Molecule(Active) x T2wrf":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Molecule(Active,alpha) x T2wrf":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > rig:alpha[V1,Fatal,federated]`,
	"Molecule(Attachment) x T2wrf":              `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T2wrf":        `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > rig:alpha[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,no near) x T2wrf":      `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear) x T2wrf":              `FirstOwner: ""[Near,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"Molecule(ParentNear,near binding) x T2wrf": `FirstOwner: near[Near,Fatal,federated]`,
	"Molecule(RootNear) x T2wrf":                `FirstOwner: class:gmnos[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(RootNear,no near) x T2wrf":        `FirstOwner: class:gmnos[Authority,Fatal,federated]`,
	"Molecule(Roots) x T2wrf":                   `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated] > rig:alpha[V1,PartialDegrade,federated] > rig:bravo[V1,PartialDegrade,federated]`,
	"OrderRuns x T2wrf":                         `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha) x T2wrf":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha,legacy) x T2wrf":           `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha,pours,legacy) x T2wrf":     `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > rig:alpha[V1,Fatal,federated]`,
	"OrderRuns(pours) x T2wrf":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Workflow x T2wrf":                          `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic]`,
	"Workflow(retired) x T2wrf":                 `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic]`,
	"Workflow(suspended) x T2wrf":               `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic]`,

	"Molecule(Active) x T2cr":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Molecule(Active,alpha) x T2cr":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > rig:alpha[V1,Fatal,federated]`,
	"Molecule(Attachment) x T2cr":              `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T2cr":        `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > rig:alpha[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,no near) x T2cr":      `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear) x T2cr":              `FirstOwner: ""[Near,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"Molecule(ParentNear,near binding) x T2cr": `FirstOwner: near[Near,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"Molecule(RootNear) x T2cr":                `FirstOwner: class:gmnos[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(RootNear,no near) x T2cr":        `FirstOwner: class:gmnos[Authority,Fatal,federated]`,
	"Molecule(Roots) x T2cr":                   `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated] > rig:alpha[V1,PartialDegrade,federated] > rig:bravo[V1,PartialDegrade,federated]`,
	"OrderRuns x T2cr":                         `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha) x T2cr":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha,legacy) x T2cr":           `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha,pours,legacy) x T2cr":     `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > class:gmnos[Authority,Fatal,federated] > rig:alpha[V1,Fatal,federated]`,
	"OrderRuns(pours) x T2cr":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Workflow x T2cr":                          `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic]`,
	"Workflow(retired) x T2cr":                 `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic]`,
	"Workflow(suspended) x T2cr":               `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic]`,

	"Molecule(Active) x T3":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Molecule(Active,alpha) x T3":            `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(Attachment) x T3":              `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T3":        `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(Attachment,no near) x T3":      `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear) x T3":              `FirstOwner: ""[Near,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"Molecule(ParentNear,near binding) x T3": `FirstOwner: near[Near,Fatal,federated]`,
	"Molecule(RootNear) x T3":                `FirstOwner: class:gmnos[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(RootNear,no near) x T3":        `FirstOwner: class:gmnos[Authority,Fatal,federated]`,
	"Molecule(Roots) x T3":                   `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated]`,
	"OrderRuns x T3":                         `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha) x T3":                  `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(alpha,legacy) x T3":           `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(alpha,pours,legacy) x T3":     `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(pours) x T3":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Workflow x T3":                          `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic]`,
	"Workflow(retired) x T3":                 `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic]`,
	"Workflow(suspended) x T3":               `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic]`,

	"Molecule(Active) x T3k":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Molecule(Active,alpha) x T3k":            `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(Attachment) x T3k":              `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T3k":        `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(Attachment,no near) x T3k":      `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear) x T3k":              `FirstOwner: ""[Near,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"Molecule(ParentNear,near binding) x T3k": `FirstOwner: near[Near,Fatal,federated]`,
	"Molecule(RootNear) x T3k":                `FirstOwner: class:gmnos[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(RootNear,no near) x T3k":        `FirstOwner: class:gmnos[Authority,Fatal,federated]`,
	"Molecule(Roots) x T3k":                   `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated]`,
	"OrderRuns x T3k":                         `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated]`,
	"OrderRuns(alpha) x T3k":                  `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(alpha,legacy) x T3k":           `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(alpha,pours,legacy) x T3k":     `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(pours) x T3k":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Workflow x T3k":                          `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic]`,
	"Workflow(retired) x T3k":                 `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic]`,
	"Workflow(suspended) x T3k":               `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic]`,

	"Molecule(Active) x T5":                  `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Molecule(Active,alpha) x T5":            `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(Attachment) x T5":              `FirstOwner: class:g[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T5":        `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(Attachment,no near) x T5":      `FirstOwner: class:g[Authority,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear) x T5":              `FirstOwner: ""[Near,Fatal,federated] > class:g[Authority,Fatal,federated]`,
	"Molecule(ParentNear,near binding) x T5": `FirstOwner: near[Near,Fatal,federated] > class:g[Authority,Fatal,federated]`,
	"Molecule(RootNear) x T5":                `FirstOwner: class:g[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(RootNear,no near) x T5":        `FirstOwner: class:g[Authority,Fatal,federated]`,
	"Molecule(Roots) x T5":                   `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated]`,
	"OrderRuns x T5":                         `Union(first-leg-wins): class:g[Authority,Fatal,federated]`,
	"OrderRuns(alpha) x T5":                  `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(alpha,legacy) x T5":           `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(alpha,pours,legacy) x T5":     `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(pours) x T5":                  `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Workflow x T5":                          `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[Relic,Fatal,relic]`,
	"Workflow(retired) x T5":                 `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[Relic,Fatal,relic]`,
	"Workflow(suspended) x T5":               `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[Relic,Fatal,relic]`,

	"Molecule(Active,alpha) x T5o":            `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(Active) x T5o":                  `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"Molecule(Attachment,alpha) x T5o":        `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(Attachment,no near) x T5o":      `FirstOwner: class:g[Authority,FatalUnlessResolved,federated]`,
	"Molecule(Attachment) x T5o":              `FirstOwner: class:g[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear,near binding) x T5o": `FirstOwner: near[Near,Fatal,federated] > class:g[Authority,Fatal,federated]`,
	"Molecule(ParentNear) x T5o":              `FirstOwner: ""[Near,Fatal,federated] > class:g[Authority,Fatal,federated]`,
	"Molecule(RootNear,no near) x T5o":        `FirstOwner: class:g[Authority,Fatal,federated]`,
	"Molecule(RootNear) x T5o":                `FirstOwner: class:g[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(Roots) x T5o":                   `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated]`,
	"OrderRuns(alpha,legacy) x T5o":           `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(alpha,pours,legacy) x T5o":     `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(alpha) x T5o":                  `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(pours) x T5o":                  `Union(first-leg-wins): class:g[Authority,Fatal,federated] > class:o[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"OrderRuns x T5o":                         `Union(first-leg-wins): class:g[Authority,Fatal,federated] > class:o[Authority,Fatal,federated]`,
	"Workflow(retired) x T5o":                 `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[Relic,Fatal,relic]`,
	"Workflow(suspended) x T5o":               `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[Relic,Fatal,relic]`,
	"Workflow x T5o":                          `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[Relic,Fatal,relic]`,

	"Molecule(Active) x Ts":                  `Union(first-leg-wins): ""[Authority,Fatal,federated]`,
	"Molecule(Active,alpha) x Ts":            `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(Attachment) x Ts":              `FirstOwner: ""[Authority,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x Ts":        `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(Attachment,no near) x Ts":      `FirstOwner: ""[Authority,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear) x Ts":              `FirstOwner: ""[Near,Fatal,federated]`,
	"Molecule(ParentNear,near binding) x Ts": `FirstOwner: near[Near,Fatal,federated] > ""[Authority,Fatal,federated]`,
	"Molecule(RootNear) x Ts":                `FirstOwner: ""[Authority,Fatal,federated]`,
	"Molecule(RootNear,no near) x Ts":        `FirstOwner: ""[Authority,Fatal,federated]`,
	"Molecule(Roots) x Ts":                   `Union(first-leg-wins): ""[Authority,Fatal,federated]`,
	"OrderRuns x Ts":                         `Union(first-leg-wins): ""[Authority,Fatal,federated]`,
	"OrderRuns(alpha) x Ts":                  `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(alpha,legacy) x Ts":           `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(alpha,pours,legacy) x Ts":     `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"OrderRuns(pours) x Ts":                  `Union(first-leg-wins): ""[Authority,Fatal,federated]`,
	"Workflow x Ts":                          `Union(first-leg-wins): ""[FederationTail,Fatal,federated]`,
	"Workflow(retired) x Ts":                 `Union(first-leg-wins): ""[FederationTail,Fatal,federated]`,
	"Workflow(suspended) x Ts":               `Union(first-leg-wins): ""[FederationTail,Fatal,federated]`,
}

// storesPlanString renders a Stores.Plan row.
func storesPlanString(s Stores, i Intent) string {
	p, err := s.Plan(i)
	if err != nil {
		return "error: " + err.Error()
	}
	return p.String()
}

func TestStoresPlanCorpus(t *testing.T) {
	var got []string
	for name, build := range storesTopologies() {
		f, opts := build()
		s := NewStores(f.topo, opts)
		for _, in := range storesIntents(f) {
			key := in.name + " x " + name
			row := storesPlanString(s, in.intent)
			got = append(got, key+"\t"+row)
			if want, ok := storesCorpus[key]; !ok || want != row {
				t.Errorf("%s:\n got  %s\n want %s", key, row, want)
			}
		}
	}
	if len(got) != len(storesCorpus) {
		sort.Strings(got)
		t.Errorf("the corpus pins %d rows, the axes produce %d:\n%s", len(storesCorpus), len(got), strings.Join(got, "\n"))
	}
}

// newT2RelicFree is T2 with the alpha rig proven relic-free.
func newT2RelicFree() topoFixture {
	f := newT2()
	f.name = "T2rf"
	for i := range f.topo.Rigs {
		if f.topo.Rigs[i].Ref == RigRef("alpha") {
			f.topo.Rigs[i].RelicFree = true
		}
	}
	return f
}
