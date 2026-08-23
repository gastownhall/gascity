package main

// The namespaces a binding declares, on the CLI plane, and the front door that
// decides an id is not bd's to answer for.
//
// Both read the reserved set, and both were reading only the prefixes each
// class MINTS under. The nudges store additionally holds the nudge queue's
// "gcnq-…" records, whose ids come from a content hash of the nudge they carry
// rather than from the store's sequence. Unlisted, such an id is planned as an
// ordinary work id: `gc bd show gcnq-…` goes to bd, which does not have it.

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

func TestReservedPrefixesForDeclaresHeldNamespaces(t *testing.T) {
	got := map[string]bool{}
	for _, p := range reservedPrefixesFor([]coordclass.Class{coordclass.ClassNudges}) {
		got[p] = true
	}
	for _, want := range config.ReservedClassPrefixesFor(config.BeadClassNudges) {
		if !got[want] {
			t.Errorf("the nudges binding does not declare %q; an id in that namespace resolves past the binding", want)
		}
	}
}

func TestBdByIDFrontDoorClaimsHeldNamespaces(t *testing.T) {
	for _, prefix := range config.AllReservedClassPrefixes() {
		id := prefix + "-abc"
		if !bdIDIsClassReserved(id) {
			t.Errorf("bdIDIsClassReserved(%q) = false; the by-id front door hands a class id to bd, which cannot answer for it", id)
		}
	}
	// The control: a work id must stay bd's to answer.
	if bdIDIsClassReserved("ga-abc") {
		t.Error("bdIDIsClassReserved claimed a work id; the front door would divert every ordinary read")
	}
}
