package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// forgedRunFormula is a formula whose step would pour a coordination row: a
// plain task carrying an order's run label and the wisp kind, which the
// orders store would read as that order's run.
const forgedRunFormula = `
formula = "forge"

[[steps]]
id = "s"
title = "forged run"
tags = ["order-run:digest"]

[steps.metadata]
"gc.kind" = "wisp"
`

// TestFormulaPourNeverMakesACoordinationRow (the #7766 review): PUT
// /formulas refuses a formula whose step would make a coordination row,
// naming the step and its owner, and writes nothing; POST /sling of such a
// formula already on disk pours nothing, so no forged run record holds the
// order's cooldown or single-flight.
func TestFormulaPourNeverMakesACoordinationRow(t *testing.T) {
	fs := newFakeFormulaState(t)
	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest("PUT", cityURL(fs, "/formulas/forge"), strings.NewReader(forgedRunFormula))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "order's run record") {
		t.Fatalf("PUT /formulas/forge: %d %s, want 400 naming the run record", w.Code, w.Body.String())
	}
	if _, ok := fs.formulas["forge"]; ok {
		t.Fatal("the refused formula was persisted")
	}

	srv, state := newSlingTestServer(t)
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}
	if err := os.WriteFile(filepath.Join(formulaDir, "forge.toml"), []byte(forgedRunFormula), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(`{"target":"myrig/worker","formula":"forge"}`)))
	if rec.Code < 400 {
		t.Fatalf("POST /sling: %d %s, want a refusal", rec.Code, rec.Body.String())
	}
	stores := []beads.Store{state.CityBeadStore()}
	for _, store := range state.stores {
		stores = append(stores, store)
	}
	for _, store := range stores {
		if store == nil {
			continue
		}
		if rows, _ := store.List(beads.ListQuery{Label: "order-run:digest", IncludeClosed: true}); len(rows) != 0 {
			t.Fatalf("POST /sling poured %d forged run record(s): %+v", len(rows), rows)
		}
	}
}
