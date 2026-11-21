package storeclass_test

import (
	_ "embed"
	"path"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/storeclass"
)

//go:embed ceiling.txt
var ceilingText string

//go:embed baseline.txt
var baselineText string

//go:embed allowed.txt
var allowedText string

// baseline is baseline.txt as the analyzer loads it.
func baseline(t *testing.T) map[storeclass.Key]int {
	t.Helper()
	rows, err := storeclass.ParseBaseline(baselineText)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// allowedRows is allowed.txt as the generator reads it.
func allowedRows(t *testing.T) map[storeclass.Key]int {
	t.Helper()
	rows, err := storeclass.ParseBaseline(allowedText)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// sealedFloorText is every package ever sealed (sealed_floor.txt, one per
// line). Default.Sealed must keep each: a sealed package never reopens. Seal a
// package by adding it to both; CODEOWNERS gates both files.
//
//go:embed sealed_floor.txt
var sealedFloorText string

// TestBaselineIsCanonical: the embedded baseline and allowed.txt are exactly
// what the generator writes, so a hand edit that reorders, duplicates or
// zeroes a row fails here before review has to spot it.
func TestBaselineIsCanonical(t *testing.T) {
	if got, want := baselineText, storeclass.FormatBaseline(baseline(t)); got != want {
		t.Fatal("baseline.txt is not canonical: regenerate it with go run ./analyzers/storeclass/cmd/storeclass-baseline -write (from tools/nogo)")
	}
	if got, want := allowedText, storeclass.FormatAllowed(allowedRows(t)); got != want {
		t.Fatal("allowed.txt is not canonical: regenerate it with go run ./analyzers/storeclass/cmd/storeclass-baseline -write (from tools/nogo)")
	}
	ceiling, err := storeclass.ParseCeiling(ceilingText)
	if err != nil {
		t.Fatal(err)
	}
	if storeclass.FormatCeiling(ceiling) != ceilingText {
		t.Fatal("ceiling.txt is not canonical")
	}
}

// TestBaselineOnlyShrinks holds the baseline's total per package and rule to
// ceiling.txt exactly: above it is growth, which is refused; below it is a
// shrink whose ceiling must come down in the same commit, so that the room it
// frees cannot be spent later.
func TestBaselineOnlyShrinks(t *testing.T) {
	ceiling, err := storeclass.ParseCeiling(ceilingText)
	if err != nil {
		t.Fatal(err)
	}
	totals := storeclass.Totals(baseline(t))
	var keys []storeclass.CeilingKey
	for k := range totals {
		keys = append(keys, k)
	}
	for k := range ceiling {
		if _, ok := totals[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Package != keys[j].Package {
			return keys[i].Package < keys[j].Package
		}
		return keys[i].Rule < keys[j].Rule
	})
	for _, k := range keys {
		have, pinned := totals[k], ceiling[k]
		switch {
		case have > pinned:
			t.Errorf("%s %s: the baseline grew to %d, past its ceiling %d: new raw-store debt is refused, not baselined", k.Package, k.Rule, have, pinned)
		case have < pinned:
			t.Errorf("%s %s: the baseline shrank to %d: lower ceiling.txt from %d in this commit (the generator's -write does it)", k.Package, k.Rule, have, pinned)
		}
	}
}

// TestSealedOnlyGrows: no package leaves the sealed list, and a sealed
// package has no baseline row.
func TestSealedOnlyGrows(t *testing.T) {
	sealed := storeclass.Default.Sealed
	for _, p := range strings.Fields(sealedFloorText) {
		if !slices.Contains(sealed, p) {
			t.Errorf("%s was sealed and is no longer: the sealed list only grows", p)
		}
	}
	if !slices.IsSorted(sealed) || len(slices.Compact(slices.Clone(sealed))) != len(sealed) {
		t.Errorf("Default.Sealed is not sorted and unique: %v", sealed)
	}
	for k := range baseline(t) {
		if slices.Contains(sealed, k.Package()) {
			t.Errorf("sealed package %s has a baseline row: %s", k.Package(), k)
		}
	}
}

// TestBaselineHasNoAllowlistedRows: a row the allowlist covers is dead
// weight that would hide a later removal of the allowlist entry.
func TestBaselineHasNoAllowlistedRows(t *testing.T) {
	for k := range baseline(t) {
		for _, a := range storeclass.Default.Allow {
			if allowCovers(a, k) {
				t.Errorf("baseline row %s is allowlisted (%s %s)", k, a.Package, a.File)
			}
		}
	}
}

// TestAllowedRowsAreFileScoped: every allowed.txt row is a finding an Allow
// entry for its file exempts, and no entry for its whole package: the
// generator counts nothing else there.
func TestAllowedRowsAreFileScoped(t *testing.T) {
	rows := allowedRows(t)
	if len(rows) == 0 {
		t.Fatal("allowed.txt has no rows: the allowlisted cmd/gc files hold raw stores, so the census counts some")
	}
	for k := range rows {
		var byFile, byPackage bool
		for _, a := range storeclass.Default.Allow {
			if allowCovers(a, k) {
				byFile = byFile || a.File != ""
				byPackage = byPackage || a.File == ""
			}
		}
		if !byFile || byPackage {
			t.Errorf("allowed.txt row %s: an entry for its file covers it = %v, one for its package = %v; want true and false", k, byFile, byPackage)
		}
	}
}

func allowCovers(a storeclass.Allow, k storeclass.Key) bool {
	if k.Package() != a.Package {
		return false
	}
	if a.File != "" {
		if ok, _ := path.Match(a.File, path.Base(k.Path)); !ok {
			return false
		}
	}
	return a.Rules == nil || slices.Contains(a.Rules, k.Rule)
}

// TestAllowlistReasons: every exemption says why.
func TestAllowlistReasons(t *testing.T) {
	for _, a := range storeclass.Default.Allow {
		if a.Reason == "" {
			t.Errorf("allowlist entry %s %s has no reason", a.Package, a.File)
		}
	}
}
