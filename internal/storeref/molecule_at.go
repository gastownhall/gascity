package storeref

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// MoleculeAt is the molecule target over owner for the molecule rooted at
// root (typed-stores T7b.2, OD-T7-2): graph class when coordclass.Classify(root)
// is ClassGraph, else work class. A v1 poured molecule is work class; a root
// of any other class is no molecule root, and the target is the store that
// holds root either way, so it is typed as the work ledger's.
//
// It wraps owner itself, not the store beneath it: the target's reads keep
// owner's gate role (a MintResidentLeg owner reads in its leg's role), its
// fenced writes resolve to owner's store, and its handle providers forward,
// as the GraphMolecule(GraphStore{Store: owner}) relabels it replaces did. A
// zero owner is a zero target (Absent).
//
// The class a target carries is a type, read by no operation today
// (MoleculeTarget.IsGraph); the store every molecule operation reads and
// writes is owner's, whatever root classifies as.
func MoleculeAt(owner beads.ResidentStore, root beads.Bead) beads.MoleculeTarget {
	if beads.IsNilStore(owner) {
		return beads.MoleculeTarget{}
	}
	if coordclass.Classify(root) == coordclass.ClassGraph {
		return beads.GraphMolecule(beads.GraphStore{Store: owner})
	}
	return beads.WorkMolecule(beads.WorkStore{Store: owner})
}
