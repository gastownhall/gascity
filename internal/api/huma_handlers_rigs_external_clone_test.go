package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func TestRigCreateAsyncAbsoluteCloneRequiresDurableRequestID(t *testing.T) {
	state := newFakeMutatorState(t)
	state.cityBeadStore = beads.NewMemStore()
	beforeRigs, err := json.Marshal(state.cfg.Rigs)
	if err != nil {
		t.Fatal(err)
	}
	h := newTestCityHandler(t, state)
	body, err := json.Marshal(RigCreateBody{Name: "external", Path: filepath.Join(t.TempDir(), "project"), GitURL: "https://example.com/repo.git"})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/rigs"), strings.NewReader(string(body))))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "request_id is required") {
		t.Fatalf("absolute clone without durable ID: status=%d body=%s", rec.Code, rec.Body.String())
	}
	state.provisionMu.Lock()
	calls := state.provisionCalls
	state.provisionMu.Unlock()
	afterRigs, err := json.Marshal(state.cfg.Rigs)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || string(afterRigs) != string(beforeRigs) {
		t.Fatalf("invalid admission changed existing rigs: calls=%d before=%s after=%s", calls, beforeRigs, afterRigs)
	}
	rows, err := state.cityBeadStore.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil || len(rows) != 0 {
		t.Fatalf("invalid admission wrote metadata: rows=%+v err=%v", rows, err)
	}
}

// This HTTP fixture uses the real async admission, durable callback and terminal
// event machinery. The cmd/gc fixtures separately prove actual physical identity
// acquisition; these values only test callback persistence at the API boundary.
type externalCloneManifestState struct {
	*fakeMutatorState
}

func (s *externalCloneManifestState) ProvisionRigFromGit(ctx context.Context, r config.Rig, gitURL string, onStep func(string, string, bool), onManifest func(RigProvisionManifest) error) (config.Rig, error) {
	manifest := RigProvisionManifest{RigName: r.Name, CreatedDir: r.Path}
	value := reflect.ValueOf(&manifest).Elem()
	required := value.FieldByName("CreatedDirIdentityRequired")
	identity := value.FieldByName("CreatedDirIdentity")
	if !required.IsValid() || !identity.IsValid() {
		return config.Rig{}, fmt.Errorf("clone ownership manifest has no required physical identity contract")
	}
	required.SetBool(true)
	if err := onManifest(manifest); err != nil {
		return config.Rig{}, err
	}
	record, err := lookupIdemRecord(s.cityBeadStore, filepath.Clean(s.CityPath()), "req-absolute-durable-1")
	if err != nil || record == nil || record.Metadata["gc.idem.created_dir_identity_required"] != "true" || record.Metadata[metaIdemCreatedDir] != r.Path {
		return config.Rig{}, fmt.Errorf("first absolute intent was not durable: record=%v err=%w", record, err)
	}
	identity.SetString("17:531")
	if err := onManifest(manifest); err != nil {
		return config.Rig{}, err
	}
	return s.fakeMutatorState.ProvisionRigFromGit(ctx, r, gitURL, onStep, nil)
}

func TestRigCreateAsyncAbsoluteClonePersistsRequiredOwnership(t *testing.T) {
	state := &externalCloneManifestState{fakeMutatorState: newFakeMutatorState(t)}
	state.cityBeadStore = beads.NewMemStore()
	h := newTestCityHandler(t, state)
	path := filepath.Join(t.TempDir(), "project")
	body, err := json.Marshal(RigCreateBody{Name: "external", Path: path, GitURL: "https://example.com/repo.git", RequestID: "req-absolute-durable-1"})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/rigs"), strings.NewReader(string(body))))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("absolute clone with durable ID: status=%d body=%s", rec.Code, rec.Body.String())
	}
	waitForEventType(t, state.eventProv, events.RequestResultRigCreate, 3*time.Second)
	record, err := lookupIdemRecord(state.cityBeadStore, filepath.Clean(state.CityPath()), "req-absolute-durable-1")
	if err != nil || record == nil {
		t.Fatalf("durable record missing: %v %v", record, err)
	}
	if record.Metadata[metaIdemState] != idemStateSucceeded || record.Metadata[metaIdemCreatedDir] != path || record.Metadata["gc.idem.created_dir_identity_required"] != "true" || record.Metadata["gc.idem.created_dir_identity"] != "17:531" {
		t.Fatalf("absolute clone ownership lost after terminal event: %+v", record.Metadata)
	}
}
