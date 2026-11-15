package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// signedCityHandler is a single-city supervisor with write-auth on, plus a
// builder for requests carrying a valid grant signed by kid "k1".
type signedCityHandler struct {
	h    http.Handler
	sign func(method, path string, body []byte) *http.Request
}

func newSignedCityHandler(t *testing.T, state State, srv *Server) signedCityHandler {
	t.Helper()
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	sm := NewSupervisorMux(&stateCityResolver{state: state}, nil, false, "test", "", time.Now()).
		WithWriteAuth(newTestWriteVerifier(t, pub, now))
	if srv != nil {
		sm.cacheMu.Lock()
		sm.cache[state.CityName()] = cachedCityServer{state: state, srv: srv}
		sm.cacheMu.Unlock()
	}
	jti := 0
	return signedCityHandler{
		h: wrapTestSupervisorMiddleware(sm),
		sign: func(method, path string, body []byte) *http.Request {
			jti++
			tok := mintToken(t, priv, grantFor(now, state.CityName(), method, path, body, fmt.Sprintf("j%d", jti)))
			req := httptest.NewRequest(method, path, bytes.NewReader(body))
			req.Header.Set(writeAuthHeader, tok)
			req.Header.Set(csrfHeaderName, "true")
			return req
		},
	}
}

func (s signedCityHandler) post(path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, s.sign(http.MethodPost, path, []byte(body)))
	return rec
}

func beadInitiator(t *testing.T, store beads.Store, id string) (string, bool) {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	v, ok := b.Metadata[beadmeta.InitiatorMetadataKey]
	return v, ok
}

// The verified kid survives the supervisor's routing into a per-city Huma
// handler, which stamps it on the bead it creates.
func TestBeadCreateStampsVerifiedInitiator(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	s := newSignedCityHandler(t, state, nil)

	rec := s.post(cityURL(state, "/beads"), `{"rig":"myrig","title":"t","metadata":{"note":"x"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	var created beads.Bead
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := created.Metadata[beadmeta.InitiatorMetadataKey]; got != "k1" {
		t.Fatalf("response %s = %q, want k1", beadmeta.InitiatorMetadataKey, got)
	}
	if got, _ := beadInitiator(t, store, created.ID); got != "k1" {
		t.Fatalf("stored %s = %q, want k1", beadmeta.InitiatorMetadataKey, got)
	}
}

func TestBeadCreateWithoutPrincipalStampsNothing(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	rec := beadRequest(t, h, http.MethodPost, cityURL(state, "/beads"), `{"rig":"myrig","title":"t"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	var created beads.Bead
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, ok := beadInitiator(t, store, created.ID); ok {
		t.Fatalf("bead carries %s=%q without a verified principal", beadmeta.InitiatorMetadataKey, got)
	}
}

// gc.initiator is server-owned: a client may not supply it on create or
// update, with or without a verified principal, and nothing is written.
func TestBeadWriteRefusesClientInitiator(t *testing.T) {
	const wantLoc = "body.metadata." + beadmeta.InitiatorMetadataKey
	body := `{"rig":"myrig","title":"t","metadata":{"` + beadmeta.InitiatorMetadataKey + `":"mallory"}}`

	t.Run("create", func(t *testing.T) {
		state := newFakeState(t)
		store := state.stores["myrig"]
		before, _ := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
		for name, rec := range map[string]*httptest.ResponseRecorder{
			"unsigned": beadRequest(t, newTestCityHandler(t, state), http.MethodPost, cityURL(state, "/beads"), body),
			"signed":   newSignedCityHandler(t, state, nil).post(cityURL(state, "/beads"), body),
		} {
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s create status = %d, want 422; body = %s", name, rec.Code, rec.Body.String())
			}
			if locs := problemLocations(t, rec); len(locs) != 1 || locs[0] != wantLoc {
				t.Fatalf("%s create errors = %v, want [%s]", name, locs, wantLoc)
			}
		}
		after, _ := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
		if len(after) != len(before) {
			t.Fatalf("refused create wrote %d beads", len(after)-len(before))
		}
	})

	t.Run("update", func(t *testing.T) {
		state := newFakeState(t)
		store := state.stores["myrig"]
		row, err := store.Create(beads.Bead{Title: "w", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		update := `{"metadata":{"` + beadmeta.InitiatorMetadataKey + `":"mallory","note":"x"}}`
		for _, method := range []string{http.MethodPatch, http.MethodPost} {
			rec := beadUpdate(t, newTestCityHandler(t, state), state, method, row.ID, update)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s update status = %d, want 422; body = %s", method, rec.Code, rec.Body.String())
			}
			if locs := problemLocations(t, rec); len(locs) != 1 || locs[0] != wantLoc {
				t.Fatalf("%s update errors = %v, want [%s]", method, locs, wantLoc)
			}
		}
		got, err := store.Get(row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got.Metadata["note"]; ok {
			t.Fatalf("refused update wrote metadata %v", got.Metadata)
		}
		if _, ok := got.Metadata[beadmeta.InitiatorMetadataKey]; ok {
			t.Fatalf("refused update wrote %s", beadmeta.InitiatorMetadataKey)
		}
	})
}

func newInitiatorSlingServer(t *testing.T) (*fakeMutatorState, *Server) {
	t.Helper()
	state := newFakeMutatorState(t)
	state.cfg.Rigs[0].Prefix = "gc" // match MemStore's auto-generated prefix
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "attr-work.toml"), []byte(`
formula = "attr-work"
version = 1

[[steps]]
id = "work"
title = "Work"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	state.cfg.FormulaLayers.City = []string{formulaDir}
	srv := New(state)
	srv.SlingRunnerFunc = func(_ string, _ string, _ map[string]string) (string, error) {
		return "", nil
	}
	return state, srv
}

type slingInitiatorResponse struct {
	Bead       string `json:"bead"`
	MoleculeID string `json:"molecule_id"`
}

// The API sling stamps the verified kid on the routed bead and on the
// formula root it attaches, and a client var named initiator lands only in
// the formula's own var namespace, never on gc.initiator.
func TestSlingStampsVerifiedInitiatorAndIgnoresClientVar(t *testing.T) {
	state, srv := newInitiatorSlingServer(t)
	store := state.stores["myrig"]
	src, err := store.Create(beads.Bead{Title: "task", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	s := newSignedCityHandler(t, state, srv)

	body := `{"target":"myrig/worker","formula":"attr-work","attached_bead_id":"` + src.ID + `","vars":{"initiator":"mallory"}}`
	rec := s.post(cityURL(state, "/sling"), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("sling status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp slingInitiatorResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.MoleculeID == "" {
		t.Fatalf("response %s has no molecule_id", rec.Body.String())
	}
	if got, _ := beadInitiator(t, store, src.ID); got != "k1" {
		t.Fatalf("source %s = %q, want k1", beadmeta.InitiatorMetadataKey, got)
	}
	if got, _ := beadInitiator(t, store, resp.MoleculeID); got != "k1" {
		t.Fatalf("formula root %s = %q, want k1 (the client var must not win)", beadmeta.InitiatorMetadataKey, got)
	}
}

func TestSlingWithoutPrincipalIgnoresClientInitiatorVar(t *testing.T) {
	state, srv := newInitiatorSlingServer(t)
	store := state.stores["myrig"]
	src, err := store.Create(beads.Bead{Title: "task", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	h := newTestCityHandlerWith(t, state, srv)

	body := `{"target":"myrig/worker","formula":"attr-work","attached_bead_id":"` + src.ID + `","vars":{"initiator":"mallory"}}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), bytes.NewBufferString(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("sling status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp slingInitiatorResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, id := range []string{src.ID, resp.MoleculeID} {
		if got, ok := beadInitiator(t, store, id); ok {
			t.Fatalf("%s carries %s=%q without a verified principal", id, beadmeta.InitiatorMetadataKey, got)
		}
	}
}

// A signed POST /sling that resolves to a plain route (a bead on a target with
// no default formula, no_formula on a target that has one, or a plain convoy)
// stamps the verified kid on every routed bead before it is routed, under both
// the built-in router and a custom sling_query.
func TestPlainSlingStampsVerifiedInitiatorBeforeRouting(t *testing.T) {
	cases := []struct {
		name           string
		defaultFormula bool
		noFormula      bool
		convoy         bool
	}{
		{name: "plain bead"},
		{name: "no formula", defaultFormula: true, noFormula: true},
		{name: "plain convoy", convoy: true},
	}
	for _, tc := range cases {
		for _, routing := range []string{"builtin", "sling_query"} {
			t.Run(tc.name+"/"+routing, func(t *testing.T) {
				state, srv := newInitiatorSlingServer(t)
				store := state.stores["myrig"]
				if tc.defaultFormula {
					formula := "attr-work"
					state.cfg.Agents[0].DefaultSlingFormula = &formula
				}
				var ids []string
				slung := ""
				if tc.convoy {
					convoy, err := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
					if err != nil {
						t.Fatal(err)
					}
					for i := 0; i < 2; i++ {
						child, err := store.Create(beads.Bead{Title: "child", Type: "task", Status: "open"})
						if err != nil {
							t.Fatal(err)
						}
						if err := store.DepAdd(convoy.ID, child.ID, "tracks"); err != nil {
							t.Fatal(err)
						}
						ids = append(ids, child.ID)
					}
					slung = convoy.ID
				} else {
					b, err := store.Create(beads.Bead{Title: "task", Type: "task"})
					if err != nil {
						t.Fatal(err)
					}
					slung, ids = b.ID, []string{b.ID}
				}
				seen := map[string]string{}
				if routing == "sling_query" {
					state.cfg.Agents[0].SlingQuery = "custom-route {}"
					srv.SlingRunnerFunc = func(_ string, command string, _ map[string]string) (string, error) {
						for _, id := range ids {
							if strings.Contains(command, id) {
								got, _ := beadInitiator(t, store, id)
								seen[id] = got
							}
						}
						return "", nil
					}
				}
				s := newSignedCityHandler(t, state, srv)

				body := fmt.Sprintf(`{"target":"myrig/worker","bead":%q,"no_formula":%t}`, slung, tc.noFormula)
				rec := s.post(cityURL(state, "/sling"), body)
				if rec.Code != http.StatusOK {
					t.Fatalf("sling status = %d, want 200; body = %s", rec.Code, rec.Body.String())
				}
				for _, id := range ids {
					if got, _ := beadInitiator(t, store, id); got != "k1" {
						t.Fatalf("routed %s %s = %q, want k1", id, beadmeta.InitiatorMetadataKey, got)
					}
					if routing == "sling_query" {
						if got, ok := seen[id]; !ok || got != "k1" {
							t.Fatalf("sling_query saw %s %s = %q (ran=%v), want k1", id, beadmeta.InitiatorMetadataKey, got, ok)
						}
					}
				}
			})
		}
	}
}
