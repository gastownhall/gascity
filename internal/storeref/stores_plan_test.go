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

// storesTopologies are the corpus's minted epochs, by name.
func storesTopologies() map[string]func() (topoFixture, StoresOptions) {
	none := StoresOptions{}
	gate := StoresOptions{Projection: taggedProjection{}}
	return map[string]func() (topoFixture, StoresOptions){
		"T0":  func() (topoFixture, StoresOptions) { return newT0(), none },
		"T0g": func() (topoFixture, StoresOptions) { return newT0(), gate },
		"T1":  func() (topoFixture, StoresOptions) { return newT1(), none },
		"T2":  func() (topoFixture, StoresOptions) { return newT2(), none },
		"T2g": func() (topoFixture, StoresOptions) {
			f := buildTopology("T2g", map[string]string{"alpha": alphaPrefix, "bravo": bravoPrefix}, nil, nil)
			return f, gate
		},
		"T2rf": func() (topoFixture, StoresOptions) { return newT2RelicFree(), none },
		"T3":   func() (topoFixture, StoresOptions) { return newT3(), none },
		"T5":   func() (topoFixture, StoresOptions) { return newT5(), none },
		"Ts": func() (topoFixture, StoresOptions) {
			sessions := []coordclass.Class{coordclass.ClassSessions}
			return buildTopology("Ts", nil, []bindingSpec{{classes: sessions, prefixes: []string{"gcs"}, relics: true}}, nil), none
		},
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

// storesIntents are the T7 intents of the corpus, by printed name. near is the
// fixture's work store, the store a B2 walk most often starts from.
func storesIntents(f topoFixture) []struct {
	name   string
	intent Intent
} {
	near := beads.Store(f.work)
	return []struct {
		name   string
		intent Intent
	}{
		{"Molecule(RootNear)", Molecule{Kind: MoleculeRootNear, Near: near, NearRef: WorkRef}},
		{"Molecule(ParentNear)", Molecule{Kind: MoleculeParentNear, Near: near, NearRef: WorkRef}},
		{"Molecule(Attachment)", Molecule{Kind: MoleculeAttachment}},
		{"Molecule(Attachment,alpha)", Molecule{Kind: MoleculeAttachment, Scope: RigRef("alpha")}},
		{"Molecule(Roots)", Molecule{Kind: MoleculeRoots}},
		{"OrderRuns", OrderRuns{}},
		{"OrderRuns(alpha)", OrderRuns{Scope: RigRef("alpha")}},
		{"Workflow(read)", Workflow{}},
		{"Workflow(guard)", Workflow{Door: WorkflowDoorSourceGuard}},
		{"Workflow(retired)", Workflow{IncludeRetiredRigs: true}},
	}
}

// storesCorpus is the pinned artifact, keyed "<intent> x <topology>". The
// rows to read first: T0 collapses every intent to the one store (no relic
// leg on a single-store city); T0g keeps the split shape under the gate's
// projection, its city leg a relic; T2rf drops the relic-free rig (kept as a
// federated tail only for a reader of retired rigs); and the attachment rows
// carry FatalUnlessResolved on both legs (R2). Ts relocates sessions only:
// the graph class is not relocated, so every T7 intent collapses to the work
// store, as on T0.
var storesCorpus = map[string]string{
	"Molecule(Attachment) x T0":       `FirstOwner: ""[Authority,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T0": `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(ParentNear) x T0":       `FirstOwner: ""[Near,Fatal,federated]`,
	"Molecule(RootNear) x T0":         `FirstOwner: ""[Authority,Fatal,federated]`,
	"Molecule(Roots) x T0":            `Union(first-leg-wins): ""[Authority,Fatal,federated]`,
	"OrderRuns x T0":                  `Union(first-leg-wins): ""[Authority,Fatal,federated]`,
	"OrderRuns(alpha) x T0":           `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Workflow(guard) x T0":            `Union(first-leg-wins): ""[Authority,PartialDegrade,federated]`,
	"Workflow(read) x T0":             `Union(first-leg-wins): ""[Authority,PartialDegrade,federated]`,
	"Workflow(retired) x T0":          `Union(first-leg-wins): ""[Authority,PartialDegrade,federated]`,

	"Molecule(Attachment) x T0g":       `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T0g": `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(ParentNear) x T0g":       `FirstOwner: ""[Near,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"Molecule(RootNear) x T0g":         `FirstOwner: class:gmnos[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(Roots) x T0g":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated]`,
	"OrderRuns x T0g":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"OrderRuns(alpha) x T0g":           `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Workflow(guard) x T0g":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,PartialDegrade,relic]`,
	"Workflow(read) x T0g":             `Union(first-leg-wins): class:gmnos[Authority,PartialDegrade,federated] > ""[Relic,PartialDegrade,relic]`,
	"Workflow(retired) x T0g":          `Union(first-leg-wins): class:gmnos[Authority,PartialDegrade,federated] > ""[Relic,PartialDegrade,relic]`,

	"Molecule(Attachment) x T1":       `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T1": `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(ParentNear) x T1":       `FirstOwner: ""[Near,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"Molecule(RootNear) x T1":         `FirstOwner: class:gmnos[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(Roots) x T1":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated]`,
	"OrderRuns x T1":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"OrderRuns(alpha) x T1":           `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Workflow(guard) x T1":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,PartialDegrade,relic]`,
	"Workflow(read) x T1":             `Union(first-leg-wins): class:gmnos[Authority,PartialDegrade,federated] > ""[Relic,PartialDegrade,relic]`,
	"Workflow(retired) x T1":          `Union(first-leg-wins): class:gmnos[Authority,PartialDegrade,federated] > ""[Relic,PartialDegrade,relic]`,

	"Molecule(Attachment) x T2":       `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T2": `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > rig:alpha[V1,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear) x T2":       `FirstOwner: ""[Near,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"Molecule(RootNear) x T2":         `FirstOwner: class:gmnos[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(Roots) x T2":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated] > rig:alpha[V1,PartialDegrade,federated] > rig:bravo[V1,PartialDegrade,federated]`,
	"OrderRuns x T2":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"OrderRuns(alpha) x T2":           `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > rig:alpha[V1,Fatal,federated]`,
	"Workflow(guard) x T2":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,PartialDegrade,relic] > rig:alpha[Relic,PartialDegrade,relic] > rig:bravo[Relic,PartialDegrade,relic]`,
	"Workflow(read) x T2":             `Union(first-leg-wins): class:gmnos[Authority,PartialDegrade,federated] > ""[Relic,PartialDegrade,relic] > rig:alpha[Relic,PartialDegrade,relic] > rig:bravo[Relic,PartialDegrade,relic]`,
	"Workflow(retired) x T2":          `Union(first-leg-wins): class:gmnos[Authority,PartialDegrade,federated] > ""[Relic,PartialDegrade,relic] > rig:alpha[Relic,PartialDegrade,relic] > rig:bravo[Relic,PartialDegrade,relic]`,

	"Molecule(Attachment) x T2g":       `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T2g": `FirstOwner: rig:alpha[Authority,FatalUnlessResolved,federated] > rig:alpha[V1,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear) x T2g":       `FirstOwner: ""[Near,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"Molecule(RootNear) x T2g":         `FirstOwner: class:gmnos[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(Roots) x T2g":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated] > rig:alpha[V1,PartialDegrade,federated] > rig:bravo[V1,PartialDegrade,federated]`,
	"OrderRuns x T2g":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"OrderRuns(alpha) x T2g":           `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > rig:alpha[Authority,Fatal,federated] > rig:alpha[V1,Fatal,federated]`,
	"Workflow(guard) x T2g":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,PartialDegrade,relic] > rig:alpha[Relic,PartialDegrade,relic] > rig:bravo[Relic,PartialDegrade,relic]`,
	"Workflow(read) x T2g":             `Union(first-leg-wins): class:gmnos[Authority,PartialDegrade,federated] > ""[Relic,PartialDegrade,relic] > rig:alpha[Relic,PartialDegrade,relic] > rig:bravo[Relic,PartialDegrade,relic]`,
	"Workflow(retired) x T2g":          `Union(first-leg-wins): class:gmnos[Authority,PartialDegrade,federated] > ""[Relic,PartialDegrade,relic] > rig:alpha[Relic,PartialDegrade,relic] > rig:bravo[Relic,PartialDegrade,relic]`,

	"Molecule(Attachment) x T2rf":       `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T2rf": `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > rig:alpha[V1,FatalUnlessResolved,federated]`,
	"Molecule(ParentNear) x T2rf":       `FirstOwner: ""[Near,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"Molecule(RootNear) x T2rf":         `FirstOwner: class:gmnos[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(Roots) x T2rf":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated] > rig:alpha[V1,PartialDegrade,federated] > rig:bravo[V1,PartialDegrade,federated]`,
	"OrderRuns x T2rf":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"OrderRuns(alpha) x T2rf":           `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > rig:alpha[V1,Fatal,federated]`,
	"Workflow(guard) x T2rf":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,PartialDegrade,relic] > rig:bravo[Relic,PartialDegrade,relic]`,
	"Workflow(read) x T2rf":             `Union(first-leg-wins): class:gmnos[Authority,PartialDegrade,federated] > ""[Relic,PartialDegrade,relic] > rig:bravo[Relic,PartialDegrade,relic]`,
	"Workflow(retired) x T2rf":          `Union(first-leg-wins): class:gmnos[Authority,PartialDegrade,federated] > ""[Relic,PartialDegrade,relic] > rig:alpha[FederationTail,PartialDegrade,federated] > rig:bravo[Relic,PartialDegrade,relic]`,

	"Molecule(Attachment) x T3":       `FirstOwner: class:gmnos[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T3": `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(ParentNear) x T3":       `FirstOwner: ""[Near,Fatal,federated] > class:gmnos[Authority,Fatal,federated]`,
	"Molecule(RootNear) x T3":         `FirstOwner: class:gmnos[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(Roots) x T3":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated]`,
	"OrderRuns x T3":                  `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[V1,Fatal,federated]`,
	"OrderRuns(alpha) x T3":           `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Workflow(guard) x T3":            `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,PartialDegrade,relic]`,
	"Workflow(read) x T3":             `Union(first-leg-wins): class:gmnos[Authority,PartialDegrade,federated] > ""[Relic,PartialDegrade,relic]`,
	"Workflow(retired) x T3":          `Union(first-leg-wins): class:gmnos[Authority,PartialDegrade,federated] > ""[Relic,PartialDegrade,relic]`,

	"Molecule(Attachment) x T5":       `FirstOwner: class:g[Authority,FatalUnlessResolved,federated] > ""[V1,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x T5": `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(ParentNear) x T5":       `FirstOwner: ""[Near,Fatal,federated] > class:g[Authority,Fatal,federated]`,
	"Molecule(RootNear) x T5":         `FirstOwner: class:g[Authority,Fatal,federated] > ""[Near,Fatal,federated]`,
	"Molecule(Roots) x T5":            `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[V1,PartialDegrade,federated]`,
	"OrderRuns x T5":                  `Union(first-leg-wins): ""[Authority,Fatal,federated] > class:g[Authority,Fatal,federated]`,
	"OrderRuns(alpha) x T5":           `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Workflow(guard) x T5":            `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[Relic,PartialDegrade,relic]`,
	"Workflow(read) x T5":             `Union(first-leg-wins): class:g[Authority,PartialDegrade,federated] > ""[Relic,PartialDegrade,relic]`,
	"Workflow(retired) x T5":          `Union(first-leg-wins): class:g[Authority,PartialDegrade,federated] > ""[Relic,PartialDegrade,relic]`,
	"Molecule(Attachment) x Ts":       `FirstOwner: ""[Authority,FatalUnlessResolved,federated]`,
	"Molecule(Attachment,alpha) x Ts": `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Molecule(ParentNear) x Ts":       `FirstOwner: ""[Near,Fatal,federated]`,
	"Molecule(RootNear) x Ts":         `FirstOwner: ""[Authority,Fatal,federated]`,
	"Molecule(Roots) x Ts":            `Union(first-leg-wins): ""[Authority,Fatal,federated]`,
	"OrderRuns x Ts":                  `Union(first-leg-wins): ""[Authority,Fatal,federated]`,
	"OrderRuns(alpha) x Ts":           `error: storeref: no store frame for scope "rig:alpha" in this epoch`,
	"Workflow(guard) x Ts":            `Union(first-leg-wins): ""[Authority,PartialDegrade,federated]`,
	"Workflow(read) x Ts":             `Union(first-leg-wins): ""[Authority,PartialDegrade,federated]`,
	"Workflow(retired) x Ts":          `Union(first-leg-wins): ""[Authority,PartialDegrade,federated]`,
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
