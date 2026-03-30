package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

type prefixedAliasStore struct {
	prefix        string
	base          *beads.MemStore
	getCalls      int
	updateCalls   int
	closeCalls    int
	childrenCalls int
}

func newPrefixedAliasStore(prefix string) *prefixedAliasStore {
	return &prefixedAliasStore{
		prefix: prefix,
		base:   beads.NewMemStore(),
	}
}

func (s *prefixedAliasStore) aliasToBase(id string) string {
	if strings.HasPrefix(id, s.prefix) {
		return "gc" + strings.TrimPrefix(id, s.prefix)
	}
	return id
}

func (s *prefixedAliasStore) baseToAlias(id string) string {
	if strings.HasPrefix(id, "gc") {
		return s.prefix + strings.TrimPrefix(id, "gc")
	}
	return id
}

func (s *prefixedAliasStore) beadToAlias(b beads.Bead) beads.Bead {
	b.ID = s.baseToAlias(b.ID)
	if b.ParentID != "" {
		b.ParentID = s.baseToAlias(b.ParentID)
	}
	if len(b.Needs) > 0 {
		needs := make([]string, 0, len(b.Needs))
		for _, need := range b.Needs {
			depType, depID, ok := strings.Cut(need, ":")
			if ok && depType != "" && depID != "" {
				needs = append(needs, depType+":"+s.baseToAlias(depID))
				continue
			}
			needs = append(needs, s.baseToAlias(need))
		}
		b.Needs = needs
	}
	return b
}

func (s *prefixedAliasStore) depToAlias(dep beads.Dep) beads.Dep {
	dep.IssueID = s.baseToAlias(dep.IssueID)
	dep.DependsOnID = s.baseToAlias(dep.DependsOnID)
	return dep
}

func (s *prefixedAliasStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.ParentID != "" {
		b.ParentID = s.aliasToBase(b.ParentID)
	}
	if len(b.Needs) > 0 {
		needs := make([]string, 0, len(b.Needs))
		for _, need := range b.Needs {
			depType, depID, ok := strings.Cut(need, ":")
			if ok && depType != "" && depID != "" {
				needs = append(needs, depType+":"+s.aliasToBase(depID))
				continue
			}
			needs = append(needs, s.aliasToBase(need))
		}
		b.Needs = needs
	}
	created, err := s.base.Create(b)
	if err != nil {
		return beads.Bead{}, err
	}
	return s.beadToAlias(created), nil
}

func (s *prefixedAliasStore) Get(id string) (beads.Bead, error) {
	s.getCalls++
	b, err := s.base.Get(s.aliasToBase(id))
	if err != nil {
		return beads.Bead{}, err
	}
	return s.beadToAlias(b), nil
}

func (s *prefixedAliasStore) Update(id string, opts beads.UpdateOpts) error {
	s.updateCalls++
	if opts.ParentID != nil {
		parentID := s.aliasToBase(*opts.ParentID)
		opts.ParentID = &parentID
	}
	return s.base.Update(s.aliasToBase(id), opts)
}

func (s *prefixedAliasStore) Close(id string) error {
	s.closeCalls++
	return s.base.Close(s.aliasToBase(id))
}

func (s *prefixedAliasStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	mapped := make([]string, 0, len(ids))
	for _, id := range ids {
		mapped = append(mapped, s.aliasToBase(id))
	}
	return s.base.CloseAll(mapped, metadata)
}

func (s *prefixedAliasStore) List(status ...string) ([]beads.Bead, error) {
	items, err := s.base.List(status...)
	if err != nil {
		return nil, err
	}
	out := make([]beads.Bead, 0, len(items))
	for _, item := range items {
		out = append(out, s.beadToAlias(item))
	}
	return out, nil
}

func (s *prefixedAliasStore) Ready() ([]beads.Bead, error) {
	items, err := s.base.Ready()
	if err != nil {
		return nil, err
	}
	out := make([]beads.Bead, 0, len(items))
	for _, item := range items {
		out = append(out, s.beadToAlias(item))
	}
	return out, nil
}

func (s *prefixedAliasStore) Children(parentID string) ([]beads.Bead, error) {
	s.childrenCalls++
	items, err := s.base.Children(s.aliasToBase(parentID))
	if err != nil {
		return nil, err
	}
	out := make([]beads.Bead, 0, len(items))
	for _, item := range items {
		out = append(out, s.beadToAlias(item))
	}
	return out, nil
}

func (s *prefixedAliasStore) ListByLabel(label string, limit int) ([]beads.Bead, error) {
	items, err := s.base.ListByLabel(label, limit)
	if err != nil {
		return nil, err
	}
	out := make([]beads.Bead, 0, len(items))
	for _, item := range items {
		out = append(out, s.beadToAlias(item))
	}
	return out, nil
}

func (s *prefixedAliasStore) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	items, err := s.base.ListByAssignee(assignee, status, limit)
	if err != nil {
		return nil, err
	}
	out := make([]beads.Bead, 0, len(items))
	for _, item := range items {
		out = append(out, s.beadToAlias(item))
	}
	return out, nil
}

func (s *prefixedAliasStore) SetMetadata(id, key, value string) error {
	return s.base.SetMetadata(s.aliasToBase(id), key, value)
}

func (s *prefixedAliasStore) SetMetadataBatch(id string, kvs map[string]string) error {
	return s.base.SetMetadataBatch(s.aliasToBase(id), kvs)
}

func (s *prefixedAliasStore) Ping() error {
	return s.base.Ping()
}

func (s *prefixedAliasStore) DepAdd(issueID, dependsOnID, depType string) error {
	return s.base.DepAdd(s.aliasToBase(issueID), s.aliasToBase(dependsOnID), depType)
}

func (s *prefixedAliasStore) DepRemove(issueID, dependsOnID string) error {
	return s.base.DepRemove(s.aliasToBase(issueID), s.aliasToBase(dependsOnID))
}

func (s *prefixedAliasStore) DepList(id, direction string) ([]beads.Dep, error) {
	deps, err := s.base.DepList(s.aliasToBase(id), direction)
	if err != nil {
		return nil, err
	}
	out := make([]beads.Dep, 0, len(deps))
	for _, dep := range deps {
		out = append(out, s.depToAlias(dep))
	}
	return out, nil
}

func configureBeadRouteState(t *testing.T) (*fakeState, *prefixedAliasStore, *prefixedAliasStore) {
	t.Helper()

	state := newFakeState(t)
	state.cityPath = t.TempDir()
	state.cfg.Rigs = []config.Rig{
		{Name: "alpha", Path: "rigs/alpha"},
		{Name: "beta", Path: "rigs/beta"},
	}

	alphaStore := newPrefixedAliasStore("ga")
	betaStore := newPrefixedAliasStore("gb")
	state.stores = map[string]beads.Store{
		"alpha": alphaStore,
		"beta":  betaStore,
	}

	alphaPath := filepath.Join(state.cityPath, "rigs", "alpha")
	betaPath := filepath.Join(state.cityPath, "rigs", "beta")
	if err := os.MkdirAll(filepath.Join(alphaPath, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(alpha .beads): %v", err)
	}
	if err := os.MkdirAll(betaPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(beta): %v", err)
	}
	routes := `{"prefix":"ga","path":"."}` + "\n" + `{"prefix":"gb","path":"../beta"}`
	if err := os.WriteFile(filepath.Join(alphaPath, ".beads", "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatalf("WriteFile(routes.jsonl): %v", err)
	}

	return state, alphaStore, betaStore
}

func TestBeadCRUD(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	// Create a bead.
	body := `{"rig":"myrig","title":"Fix login bug","type":"task"}`
	req := newPostRequest("/v0/beads", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var created beads.Bead
	json.NewDecoder(rec.Body).Decode(&created) //nolint:errcheck
	if created.Title != "Fix login bug" {
		t.Errorf("Title = %q, want %q", created.Title, "Fix login bug")
	}
	if created.ID == "" {
		t.Fatal("created bead has no ID")
	}

	// Get the bead.
	req = httptest.NewRequest("GET", "/v0/bead/"+created.ID, nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d", rec.Code, http.StatusOK)
	}

	var got beads.Bead
	json.NewDecoder(rec.Body).Decode(&got) //nolint:errcheck
	if got.Title != "Fix login bug" {
		t.Errorf("Title = %q, want %q", got.Title, "Fix login bug")
	}

	// Close the bead.
	req = newPostRequest("/v0/bead/"+created.ID+"/close", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Verify closed.
	req = httptest.NewRequest("GET", "/v0/bead/"+created.ID, nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	json.NewDecoder(rec.Body).Decode(&got) //nolint:errcheck
	if got.Status != "closed" {
		t.Errorf("Status = %q, want %q", got.Status, "closed")
	}
}

func TestBeadListFiltering(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	store.Create(beads.Bead{Title: "Open task", Type: "task"})                           //nolint:errcheck
	store.Create(beads.Bead{Title: "Message", Type: "message"})                          //nolint:errcheck
	store.Create(beads.Bead{Title: "Labeled", Type: "task", Labels: []string{"urgent"}}) //nolint:errcheck
	srv := New(state)

	// Filter by type.
	req := httptest.NewRequest("GET", "/v0/beads?type=message", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var resp struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("type filter: Total = %d, want 1", resp.Total)
	}

	// Filter by label.
	req = httptest.NewRequest("GET", "/v0/beads?label=urgent", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("label filter: Total = %d, want 1", resp.Total)
	}
}

func TestBeadListCrossRig(t *testing.T) {
	state := newFakeState(t)
	store2 := beads.NewMemStore()
	state.stores["rig2"] = store2

	state.stores["myrig"].Create(beads.Bead{Title: "Bead from rig1"}) //nolint:errcheck
	store2.Create(beads.Bead{Title: "Bead from rig2"})                //nolint:errcheck
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/beads", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var resp struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 2 {
		t.Errorf("cross-rig: Total = %d, want 2", resp.Total)
	}
}

func TestBeadGetNotFound(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/bead/nonexistent", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestBeadGetUsesRoutePrefixStore(t *testing.T) {
	state, alphaStore, betaStore := configureBeadRouteState(t)
	created, err := betaStore.Create(beads.Bead{Title: "Routed beta bead"})
	if err != nil {
		t.Fatalf("Create(beta): %v", err)
	}
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/bead/"+created.ID, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got beads.Bead
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode(): %v", err)
	}
	if got.Title != "Routed beta bead" {
		t.Fatalf("Title = %q, want %q", got.Title, "Routed beta bead")
	}
	if alphaStore.getCalls != 0 {
		t.Fatalf("alphaStore.getCalls = %d, want 0", alphaStore.getCalls)
	}
	if betaStore.getCalls != 1 {
		t.Fatalf("betaStore.getCalls = %d, want 1", betaStore.getCalls)
	}
}

func TestBeadReady(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	store.Create(beads.Bead{Title: "Open"}) //nolint:errcheck
	b2, _ := store.Create(beads.Bead{Title: "Closed"})
	store.Close(b2.ID) //nolint:errcheck
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/beads/ready", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var resp struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("ready: Total = %d, want 1", resp.Total)
	}
}

func TestBeadUpdate(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Test"})
	srv := New(state)

	desc := "updated description"
	body := `{"description":"` + desc + `","labels":["new-label"]}`
	req := newPostRequest("/v0/bead/"+b.ID+"/update", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Verify update.
	got, _ := store.Get(b.ID)
	if got.Description != desc {
		t.Errorf("Description = %q, want %q", got.Description, desc)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "new-label" {
		t.Errorf("Labels = %v, want [new-label]", got.Labels)
	}
}

func TestBeadUpdateUsesRoutePrefixStore(t *testing.T) {
	state, alphaStore, betaStore := configureBeadRouteState(t)
	created, err := betaStore.Create(beads.Bead{Title: "Routed beta bead"})
	if err != nil {
		t.Fatalf("Create(beta): %v", err)
	}
	srv := New(state)

	body := `{"description":"updated via route"}`
	req := newPostRequest("/v0/bead/"+created.ID+"/update", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	got, err := betaStore.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(beta): %v", err)
	}
	if got.Description != "updated via route" {
		t.Fatalf("Description = %q, want %q", got.Description, "updated via route")
	}
	if alphaStore.updateCalls != 0 {
		t.Fatalf("alphaStore.updateCalls = %d, want 0", alphaStore.updateCalls)
	}
	if betaStore.updateCalls != 1 {
		t.Fatalf("betaStore.updateCalls = %d, want 1", betaStore.updateCalls)
	}
}

func TestBeadDepsUsesRoutePrefixStore(t *testing.T) {
	state, alphaStore, betaStore := configureBeadRouteState(t)
	parent, err := betaStore.Create(beads.Bead{Title: "Parent"})
	if err != nil {
		t.Fatalf("Create(parent): %v", err)
	}
	child, err := betaStore.Create(beads.Bead{Title: "Child", ParentID: parent.ID})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/bead/"+parent.ID+"/deps", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Children []beads.Bead `json:"children"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode(): %v", err)
	}
	if len(resp.Children) != 1 || resp.Children[0].ID != child.ID {
		t.Fatalf("children = %#v, want [%s]", resp.Children, child.ID)
	}
	if alphaStore.childrenCalls != 0 {
		t.Fatalf("alphaStore.childrenCalls = %d, want 0", alphaStore.childrenCalls)
	}
	if betaStore.childrenCalls != 1 {
		t.Fatalf("betaStore.childrenCalls = %d, want 1", betaStore.childrenCalls)
	}
}

func TestBeadPatchAlias(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Test"})
	srv := New(state)

	desc := "patched"
	body := `{"description":"` + desc + `"}`
	req := httptest.NewRequest("PATCH", "/v0/bead/"+b.ID, bytes.NewBufferString(body))
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	got, _ := store.Get(b.ID)
	if got.Description != desc {
		t.Errorf("Description = %q, want %q", got.Description, desc)
	}
}

func TestBeadUpdatePriority(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Test"})
	srv := New(state)

	body := `{"priority":1}`
	req := newPostRequest("/v0/bead/"+b.ID+"/update", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want %d", rec.Code, http.StatusOK)
	}

	got, _ := store.Get(b.ID)
	if got.Priority == nil || *got.Priority != 1 {
		t.Fatalf("Priority = %v, want 1", got.Priority)
	}
}

func TestBeadUpdateRejectsNullPriority(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	priority := 1
	b, _ := store.Create(beads.Bead{Title: "Test", Priority: &priority})
	srv := New(state)

	body := `{"priority":null}`
	req := newPostRequest("/v0/bead/"+b.ID+"/update", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	got, _ := store.Get(b.ID)
	if got.Priority == nil || *got.Priority != 1 {
		t.Fatalf("Priority = %v, want unchanged 1", got.Priority)
	}
}

func TestBeadReopen(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Closed task"})
	store.Close(b.ID) //nolint:errcheck
	srv := New(state)

	// Reopen the closed bead.
	req := newPostRequest("/v0/bead/"+b.ID+"/reopen", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("reopen status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// Verify reopened.
	got, _ := store.Get(b.ID)
	if got.Status != "open" {
		t.Errorf("Status = %q, want %q", got.Status, "open")
	}
}

func TestBeadReopenNotClosed(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Open task"})
	srv := New(state)

	req := newPostRequest("/v0/bead/"+b.ID+"/reopen", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestBeadAssign(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Task"})
	srv := New(state)

	body := `{"assignee":"worker-1"}`
	req := newPostRequest("/v0/bead/"+b.ID+"/assign", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("assign status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	got, _ := store.Get(b.ID)
	if got.Assignee != "worker-1" {
		t.Errorf("Assignee = %q, want %q", got.Assignee, "worker-1")
	}
}

func TestBeadDelete(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "To delete"})
	srv := New(state)

	req := httptest.NewRequest("DELETE", "/v0/bead/"+b.ID, nil)
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// Verify closed (soft delete).
	got, _ := store.Get(b.ID)
	if got.Status != "closed" {
		t.Errorf("Status = %q, want %q", got.Status, "closed")
	}
}

func TestBeadDeleteNotFound(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("DELETE", "/v0/bead/nonexistent", nil)
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestBeadCreateValidation(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	// Missing title.
	req := newPostRequest("/v0/beads", bytes.NewBufferString(`{"rig":"myrig"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestPackList(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Packs = map[string]config.PackSource{
		"gastown": {
			Source: "https://github.com/example/gastown-pack",
			Ref:    "v1.0.0",
			Path:   "packs/gastown",
		},
	}
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/packs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Packs []packResponse `json:"packs"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if len(resp.Packs) != 1 {
		t.Fatalf("packs count = %d, want 1", len(resp.Packs))
	}
	if resp.Packs[0].Name != "gastown" {
		t.Errorf("Name = %q, want %q", resp.Packs[0].Name, "gastown")
	}
	if resp.Packs[0].Source != "https://github.com/example/gastown-pack" {
		t.Errorf("Source = %q", resp.Packs[0].Source)
	}
}

func TestPackListEmpty(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/packs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Packs []packResponse `json:"packs"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if len(resp.Packs) != 0 {
		t.Errorf("packs count = %d, want 0", len(resp.Packs))
	}
}
