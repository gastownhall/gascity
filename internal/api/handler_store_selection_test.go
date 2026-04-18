package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestBeadCreateUsesCityStoreWhenAvailableWithoutRig(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.stores["beta"] = beads.NewMemStore()
	state.cfg.Rigs = append(state.cfg.Rigs, config.Rig{Name: "beta", Path: "/tmp/beta"})
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(state, "/beads"), bytes.NewBufferString(`{"title":"city task","type":"task"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var created beads.Bead
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := state.cityBeadStore.Get(created.ID); err != nil {
		t.Fatalf("city store missing created bead %s: %v", created.ID, err)
	}
	if list, err := state.stores["myrig"].List(beads.ListQuery{AllowScan: true}); err != nil {
		t.Fatalf("myrig list: %v", err)
	} else if len(list) != 0 {
		t.Fatalf("myrig store should stay empty, got %#v", list)
	}
	if list, err := state.stores["beta"].List(beads.ListQuery{AllowScan: true}); err != nil {
		t.Fatalf("beta list: %v", err)
	} else if len(list) != 0 {
		t.Fatalf("beta store should stay empty, got %#v", list)
	}
}

func TestConvoyCreateUsesCityStoreWhenAvailableWithoutRig(t *testing.T) {
	state := newFakeMutatorState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.stores["beta"] = beads.NewMemStore()
	state.cfg.Rigs = append(state.cfg.Rigs, config.Rig{Name: "beta", Path: "/tmp/beta"})
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	item, err := state.cityBeadStore.Create(beads.Bead{Title: "city item", Type: "task"})
	if err != nil {
		t.Fatalf("create city item: %v", err)
	}
	body := `{"title":"city convoy","items":["` + item.ID + `"]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/convoys"), strings.NewReader(body)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var convoy beads.Bead
	if err := json.NewDecoder(rec.Body).Decode(&convoy); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := state.cityBeadStore.Get(convoy.ID); err != nil {
		t.Fatalf("city store missing convoy %s: %v", convoy.ID, err)
	}
	updatedItem, err := state.cityBeadStore.Get(item.ID)
	if err != nil {
		t.Fatalf("reload city item: %v", err)
	}
	if updatedItem.ParentID != convoy.ID {
		t.Fatalf("city item parent = %q, want %q", updatedItem.ParentID, convoy.ID)
	}
}
