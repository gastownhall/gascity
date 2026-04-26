package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

type prefixedAliasStore struct {
	prefix        string
	base          *beads.MemStore
	getCalls      int
	updateCalls   int
	closeCalls    int
	reopenCalls   int
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

type sparseCreateStore struct {
	*beads.MemStore
}

func newSparseCreateStore() *sparseCreateStore {
	return &sparseCreateStore{MemStore: beads.NewMemStore()}
}

func (s *sparseCreateStore) Create(b beads.Bead) (beads.Bead, error) {
	created, err := s.MemStore.Create(b)
	if err != nil {
		return beads.Bead{}, err
	}
	return beads.Bead{
		ID:        created.ID,
		Title:     created.Title,
		Type:      created.Type,
		Status:    created.Status,
		CreatedAt: created.CreatedAt,
	}, nil
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

func (s *prefixedAliasStore) Reopen(id string) error {
	s.reopenCalls++
	return s.base.Reopen(s.aliasToBase(id))
}

func (s *prefixedAliasStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	mapped := make([]string, 0, len(ids))
	for _, id := range ids {
		mapped = append(mapped, s.aliasToBase(id))
	}
	return s.base.CloseAll(mapped, metadata)
}

func (s *prefixedAliasStore) ListOpen(status ...string) ([]beads.Bead, error) {
	items, err := s.base.ListOpen(status...)
	if err != nil {
		return nil, err
	}
	out := make([]beads.Bead, 0, len(items))
	for _, item := range items {
		out = append(out, s.beadToAlias(item))
	}
	return out, nil
}

func (s *prefixedAliasStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.ParentID != "" {
		s.childrenCalls++
		query.ParentID = s.aliasToBase(query.ParentID)
	}
	if len(query.Metadata) > 0 {
		filters := make(map[string]string, len(query.Metadata))
		for k, v := range query.Metadata {
			switch k {
			case "gc.root_bead_id", "gc.workflow_id", "gc.source_bead_id":
				filters[k] = s.aliasToBase(v)
			default:
				filters[k] = v
			}
		}
		query.Metadata = filters
	}
	items, err := s.base.List(query)
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

func (s *prefixedAliasStore) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.childrenCalls++
	items, err := s.base.Children(s.aliasToBase(parentID), opts...)
	if err != nil {
		return nil, err
	}
	out := make([]beads.Bead, 0, len(items))
	for _, item := range items {
		out = append(out, s.beadToAlias(item))
	}
	return out, nil
}

func (s *prefixedAliasStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	items, err := s.base.ListByLabel(label, limit, opts...)
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

func (s *prefixedAliasStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	result, err := s.base.ListByMetadata(filters, limit, opts...)
	if err != nil {
		return nil, err
	}
	out := make([]beads.Bead, 0, len(result))
	for _, b := range result {
		out = append(out, s.beadToAlias(b))
	}
	return out, nil
}

func (s *prefixedAliasStore) Delete(id string) error {
	return s.base.Delete(s.aliasToBase(id))
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
	if err := os.MkdirAll(filepath.Join(alphaPath, ".beads"), 0o700); err != nil {
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

func TestBeadPrefixAllowsAlphanumericPrefixes(t *testing.T) {
	if got := beadPrefix("mcdi3bsyeryols-yyn"); got != "mcdi3bsyeryols" {
		t.Fatalf("beadPrefix() = %q, want alphanumeric prefix", got)
	}
}

func TestBeadCloseVerifiesStoreContainsBeadBeforeClosing(t *testing.T) {
	rigStore := beads.NewMemStore()
	created, err := rigStore.Create(beads.Bead{Title: "close me"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	status := "in_progress"
	if err := rigStore.Update(created.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	misrouted := &closeSucceedsWithoutBeadStore{Store: beads.NewMemStore()}
	state := newFakeState(t)
	state.cityBeadStore = misrouted
	state.stores = map[string]beads.Store{"myrig": rigStore}

	s := New(state)
	if _, err := s.humaHandleBeadClose(context.Background(), &BeadCloseInput{ID: created.ID}); err != nil {
		t.Fatalf("humaHandleBeadClose: %v", err)
	}

	if misrouted.closeCalls != 0 {
		t.Fatalf("misrouted close calls = %d, want 0", misrouted.closeCalls)
	}
	got, err := rigStore.Get(created.ID)
	if err != nil {
		t.Fatalf("rig Get: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("rig status = %q, want closed", got.Status)
	}
}

func TestBeadStoresForIDUsesConfiguredRigPrefixBeforeFallback(t *testing.T) {
	state := newFakeState(t)
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	state.cityBeadStore = cityStore
	state.stores = map[string]beads.Store{"myrig": rigStore}
	state.cfg.Workspace.Prefix = "ct"
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: filepath.Join(state.cityPath, "rigs", "myrig"), Prefix: "rw"}}

	s := New(state)
	stores := s.beadStoresForID("rw-1")
	if len(stores) != 1 || stores[0] != rigStore {
		t.Fatalf("beadStoresForID(rw-1) = %#v, want only configured rig store", stores)
	}
}

func TestBeadStoresForIDUsesConfiguredHyphenatedRigPrefix(t *testing.T) {
	state := newFakeState(t)
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	state.cityBeadStore = cityStore
	state.stores = map[string]beads.Store{"myrig": rigStore}
	state.cfg.Workspace.Prefix = "mlcm"
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: filepath.Join(state.cityPath, "rigs", "myrig"), Prefix: "mc-mogbzvrs"}}

	s := New(state)
	stores := s.beadStoresForID("mc-mogbzvrs-hiv.1")
	if len(stores) != 1 || stores[0] != rigStore {
		t.Fatalf("beadStoresForID(hyphenated prefix) = %#v, want only configured rig store", stores)
	}
}

type closeSucceedsWithoutBeadStore struct {
	beads.Store
	closeCalls int
}

func (s *closeSucceedsWithoutBeadStore) Close(string) error {
	s.closeCalls++
	return nil
}

func TestBeadCRUD(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	// Create a bead.
	body := `{"rig":"myrig","title":"Fix login bug","type":"task"}`
	req := newPostRequest(cityURL(state, "/beads"), bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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
	req = httptest.NewRequest("GET", cityURL(state, "/bead/")+created.ID, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d", rec.Code, http.StatusOK)
	}

	var got beads.Bead
	json.NewDecoder(rec.Body).Decode(&got) //nolint:errcheck
	if got.Title != "Fix login bug" {
		t.Errorf("Title = %q, want %q", got.Title, "Fix login bug")
	}

	// Close the bead.
	req = newPostRequest(cityURL(state, "/bead/")+created.ID+"/close", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Verify closed.
	req = httptest.NewRequest("GET", cityURL(state, "/bead/")+created.ID, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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
	h := newTestCityHandler(t, state)

	// Filter by type.
	req := httptest.NewRequest("GET", cityURL(state, "/beads?type=message"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("type filter: Total = %d, want 1", resp.Total)
	}

	// Filter by label.
	req = httptest.NewRequest("GET", cityURL(state, "/beads?label=urgent"), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/beads"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/bead/nonexistent"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/bead/")+created.ID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/beads/ready"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("ready: Total = %d, want 1", resp.Total)
	}
}

func TestBeadListInProgressUsesLiveLookup(t *testing.T) {
	state := newFakeState(t)
	backing := beads.NewMemStore()
	work, err := backing.Create(beads.Bead{Title: "active work"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	state.stores["myrig"] = cache
	status := "in_progress"
	if err := backing.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest("GET", cityURL(state, "/beads?status=in_progress"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || len(resp.Items) != 1 || resp.Items[0].ID != work.ID {
		t.Fatalf("in_progress beads = %+v, want only %s", resp.Items, work.ID)
	}
}

func TestBeadReadyUsesLiveLookup(t *testing.T) {
	state := newFakeState(t)
	backing := beads.NewMemStore()
	blocker, err := backing.Create(beads.Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	ready, err := backing.Create(beads.Bead{Title: "ready"})
	if err != nil {
		t.Fatalf("Create(ready): %v", err)
	}
	if err := backing.DepAdd(ready.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	state.stores["myrig"] = cache
	if err := backing.Close(blocker.ID); err != nil {
		t.Fatalf("Close(blocker): %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest("GET", cityURL(state, "/beads/ready"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || len(resp.Items) != 1 || resp.Items[0].ID != ready.ID {
		t.Fatalf("ready beads = %+v, want only %s", resp.Items, ready.ID)
	}
}

func TestBeadUpdate(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Test"})
	h := newTestCityHandler(t, state)

	desc := "updated description"
	body := `{"description":"` + desc + `","labels":["new-label"]}`
	req := newPostRequest(cityURL(state, "/bead/")+b.ID+"/update", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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

func TestBeadUpdateStatusAndMetadata(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Test"})
	h := newTestCityHandler(t, state)

	body := `{"status":"in_progress","metadata":{"verified":"true"}}`
	req := newPostRequest(cityURL(state, "/bead/")+b.ID+"/update", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want %d", rec.Code, http.StatusOK)
	}

	got, _ := store.Get(b.ID)
	if got.Status != "in_progress" || got.Metadata["verified"] != "true" {
		t.Fatalf("bead = %+v, want in_progress plus metadata", got)
	}
}

func TestBeadCreatePersistsMetadataAndParent(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	parent, err := store.Create(beads.Bead{Title: "Parent"})
	if err != nil {
		t.Fatalf("Create(parent): %v", err)
	}
	h := newTestCityHandler(t, state)

	body := `{
		"rig":"myrig",
		"title":"Child",
		"type":"feature",
		"parent":"` + parent.ID + `",
		"metadata":{
			"real_world_app.contract.role":"child",
			"real_world_app.contract.run_id":"run-1"
		}
	}`
	req := newPostRequest(cityURL(state, "/beads"), bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var created beads.Bead
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode created bead: %v", err)
	}
	if created.ParentID != parent.ID {
		t.Fatalf("response parent = %q, want %q", created.ParentID, parent.ID)
	}
	if created.Metadata["real_world_app.contract.run_id"] != "run-1" {
		t.Fatalf("response metadata = %#v, want real_world_app.contract.run_id=run-1", created.Metadata)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(created): %v", err)
	}
	if got.ParentID != parent.ID {
		t.Fatalf("stored parent = %q, want %q", got.ParentID, parent.ID)
	}
	if got.Metadata["real_world_app.contract.role"] != "child" || got.Metadata["real_world_app.contract.run_id"] != "run-1" {
		t.Fatalf("stored metadata = %#v, want real-world app metadata", got.Metadata)
	}
}

func TestBeadCreateResponseUsesAuthoritativeStoredBead(t *testing.T) {
	state := newFakeState(t)
	store := newSparseCreateStore()
	state.stores["myrig"] = store
	parent, err := store.Create(beads.Bead{Title: "Parent"})
	if err != nil {
		t.Fatalf("Create(parent): %v", err)
	}
	h := newTestCityHandler(t, state)

	body := `{
		"rig":"myrig",
		"title":"Child",
		"type":"feature",
		"parent":"` + parent.ID + `",
		"labels":["urgent"],
		"metadata":{"real_world_app.contract.run_id":"run-1"}
	}`
	req := newPostRequest(cityURL(state, "/beads"), bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var created beads.Bead
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode created bead: %v", err)
	}
	if created.ParentID != parent.ID {
		t.Fatalf("response parent = %q, want %q", created.ParentID, parent.ID)
	}
	if len(created.Labels) != 1 || created.Labels[0] != "urgent" {
		t.Fatalf("response labels = %#v, want [urgent]", created.Labels)
	}
	if created.Metadata["real_world_app.contract.run_id"] != "run-1" {
		t.Fatalf("response metadata = %#v, want real_world_app.contract.run_id=run-1", created.Metadata)
	}
}

func TestBeadUpdateUsesRoutePrefixStore(t *testing.T) {
	state, alphaStore, betaStore := configureBeadRouteState(t)
	created, err := betaStore.Create(beads.Bead{Title: "Routed beta bead"})
	if err != nil {
		t.Fatalf("Create(beta): %v", err)
	}
	h := newTestCityHandler(t, state)

	body := `{"description":"updated via route"}`
	req := newPostRequest(cityURL(state, "/bead/")+created.ID+"/update", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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

func TestBeadStoresForIDUsesLongestConfiguredHyphenatedPrefix(t *testing.T) {
	state := newFakeState(t)
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	state.cityBeadStore = cityStore
	state.cfg.Workspace.Prefix = "mc"
	state.cfg.Rigs = []config.Rig{{
		Name:   "alpha",
		Path:   "/tmp/alpha",
		Prefix: "mc-alpha",
	}}
	state.stores = map[string]beads.Store{"alpha": rigStore}

	server := &Server{state: state}
	stores := server.beadStoresForID("mc-alpha-123")
	if len(stores) != 1 || stores[0] != rigStore {
		t.Fatalf("beadStoresForID returned %#v, want only authoritative rig store", stores)
	}
}

func TestBeadUpdateSetsAndClearsParent(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	parent, err := store.Create(beads.Bead{Title: "Parent"})
	if err != nil {
		t.Fatalf("Create(parent): %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "Child"})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	h := newTestCityHandler(t, state)

	body := `{"parent":"` + parent.ID + `"}`
	req := newPostRequest(cityURL(state, "/bead/")+child.ID+"/update", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("set parent status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get(child): %v", err)
	}
	if got.ParentID != parent.ID {
		t.Fatalf("parent after set = %q, want %q", got.ParentID, parent.ID)
	}

	req = newPostRequest(cityURL(state, "/bead/")+child.ID+"/update", bytes.NewBufferString(`{"parent":null}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("clear parent status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	got, err = store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get(child after clear): %v", err)
	}
	if got.ParentID != "" {
		t.Fatalf("parent after clear = %q, want empty", got.ParentID)
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
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/bead/")+parent.ID+"/deps", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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

func TestBeadDepsIncludesMetadataAttachments(t *testing.T) {
	state, _, betaStore := configureBeadRouteState(t)
	parent, err := betaStore.Create(beads.Bead{Title: "Parent"})
	if err != nil {
		t.Fatalf("Create(parent): %v", err)
	}
	attached, err := betaStore.Create(beads.Bead{Title: "Attached", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create(attached): %v", err)
	}
	if err := betaStore.SetMetadata(parent.ID, "molecule_id", attached.ID); err != nil {
		t.Fatalf("SetMetadata(molecule_id): %v", err)
	}
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/bead/")+parent.ID+"/deps", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Children []beads.Bead `json:"children"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode(): %v", err)
	}
	if len(resp.Children) != 1 || resp.Children[0].ID != attached.ID {
		t.Fatalf("children = %#v, want [%s]", resp.Children, attached.ID)
	}
	if betaStore.getCalls < 2 {
		t.Fatalf("betaStore.getCalls = %d, want at least 2 (parent + attachment)", betaStore.getCalls)
	}
}

func TestBeadPatchAlias(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Test"})
	h := newTestCityHandler(t, state)

	desc := "patched"
	body := `{"description":"` + desc + `"}`
	req := httptest.NewRequest("PATCH", cityURL(state, "/bead/")+b.ID, bytes.NewBufferString(body))
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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
	h := newTestCityHandler(t, state)

	body := `{"priority":1}`
	req := newPostRequest(cityURL(state, "/bead/")+b.ID+"/update", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want %d", rec.Code, http.StatusOK)
	}

	got, _ := store.Get(b.ID)
	if got.Priority == nil || *got.Priority != 1 {
		t.Fatalf("Priority = %v, want 1", got.Priority)
	}
}

// TestBeadUpdateNullPriorityRejected asserts that `priority: null` is
// rejected with a 4xx + migration-friendly error message, not silently
// ignored. An earlier revision removed the explicit null-vs-absent
// detection so clients that said "clear priority" via null got a 200
// with priority unchanged — a silent semantic shift. The rejection was
// reinstated via a custom UnmarshalJSON on beadUpdateBody. Clients that
// want to clear priority must use a dedicated endpoint (not exposed yet);
// callers who previously sent null by accident now see a clear error.
func TestBeadUpdateNullPriorityRejected(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	priority := 1
	b, _ := store.Create(beads.Bead{Title: "Test", Priority: &priority})
	h := newTestCityHandler(t, state)

	body := `{"priority":null}`
	req := newPostRequest(cityURL(state, "/bead/")+b.ID+"/update", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("update status = %d, want 4xx rejecting null priority: body=%s", rec.Code, rec.Body.String())
	}

	got, _ := store.Get(b.ID)
	if got.Priority == nil || *got.Priority != 1 {
		t.Fatalf("Priority = %v, want unchanged 1 (existing value preserved after 4xx)", got.Priority)
	}
}

func TestBeadReopen(t *testing.T) {
	state := newFakeState(t)
	store := newPrefixedAliasStore("myrig-")
	state.stores["myrig"] = store
	b, _ := store.Create(beads.Bead{Title: "Closed task"})
	store.Close(b.ID) //nolint:errcheck
	h := newTestCityHandler(t, state)

	// Reopen the closed bead.
	req := newPostRequest(cityURL(state, "/bead/")+b.ID+"/reopen", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("reopen status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// Verify reopened.
	got, _ := store.Get(b.ID)
	if got.Status != "open" {
		t.Errorf("Status = %q, want %q", got.Status, "open")
	}
	if store.reopenCalls != 1 {
		t.Fatalf("reopen calls = %d, want 1", store.reopenCalls)
	}
	if store.updateCalls != 0 {
		t.Fatalf("update calls = %d, want 0; reopen must not use generic update", store.updateCalls)
	}
}

func TestBeadReopenNotClosed(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Open task"})
	h := newTestCityHandler(t, state)

	req := newPostRequest(cityURL(state, "/bead/")+b.ID+"/reopen", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestBeadAssign(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "Task"})
	h := newTestCityHandler(t, state)

	body := `{"assignee":"worker-1"}`
	req := newPostRequest(cityURL(state, "/bead/")+b.ID+"/assign", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("assign status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	got, _ := store.Get(b.ID)
	if got.Assignee != "worker-1" {
		t.Errorf("Assignee = %q, want %q", got.Assignee, "worker-1")
	}
}

func TestPhase2BeadAssignNormalizesCurrentSessionAlias(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	store := state.stores["myrig"]
	_, _ = store.Create(beads.Bead{Title: "ID offset"})
	work, _ := store.Create(beads.Bead{Title: "Task"})
	sessionBead := createPhase2APISessionBead(t, state.cityBeadStore)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(state, "/bead/"+work.ID+"/assign"), bytes.NewBufferString(`{"assignee":"worker"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("assign alias status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	got, _ := store.Get(work.ID)
	if got.Assignee != sessionBead.ID {
		t.Fatalf("assignee = %q, want alias normalized to session bead ID %q", got.Assignee, sessionBead.ID)
	}

	listReq := httptest.NewRequest("GET", cityURL(state, "/beads?assignee=worker"), nil)
	listRec := httptest.NewRecorder()
	h.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list by alias status = %d, want %d; body: %s", listRec.Code, http.StatusOK, listRec.Body.String())
	}
	var listed struct {
		Items []beads.Bead `json:"items"`
	}
	if err := json.NewDecoder(listRec.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Items) != 1 || listed.Items[0].ID != work.ID {
		t.Fatalf("list by alias items = %#v, want only %s", listed.Items, work.ID)
	}
}

func TestPhase2BeadListAssigneeAliasKeepsCrossRigDuplicateIDs(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.stores["otherrig"] = beads.NewMemStore()
	state.cfg.Rigs = append(state.cfg.Rigs, config.Rig{Name: "otherrig", Path: "/tmp/otherrig"})
	sessionBead := createPhase2APISessionBead(t, state.cityBeadStore)
	workA, _ := state.stores["myrig"].Create(beads.Bead{Title: "Task A", Assignee: sessionBead.ID})
	workB, _ := state.stores["otherrig"].Create(beads.Bead{Title: "Task B", Assignee: sessionBead.ID})
	if workA.ID != workB.ID {
		t.Fatalf("test setup expected duplicate local IDs, got %q and %q", workA.ID, workB.ID)
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/beads?assignee=worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("list by alias status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var listed struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listed.Total != 2 || len(listed.Items) != 2 {
		t.Fatalf("list by alias total/items = %d/%d, want 2/2: %#v", listed.Total, len(listed.Items), listed.Items)
	}
}

func TestPhase2BeadAssignNormalizesCurrentSessionName(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	store := state.stores["myrig"]
	_, _ = store.Create(beads.Bead{Title: "ID offset"})
	work, _ := store.Create(beads.Bead{Title: "Task"})
	sessionBead := createPhase2APISessionBead(t, state.cityBeadStore)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(state, "/bead/"+work.ID+"/assign"), bytes.NewBufferString(`{"assignee":"test-city--worker"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("assign session_name status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	got, _ := store.Get(work.ID)
	if got.Assignee != sessionBead.ID {
		t.Fatalf("assignee = %q, want session_name normalized to session bead ID %q", got.Assignee, sessionBead.ID)
	}
}

func TestPhase2BeadAssignMaterializesExactConfiguredNamedIdentity(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	store := state.stores["myrig"]
	_, _ = store.Create(beads.Bead{Title: "ID offset"})
	work, _ := store.Create(beads.Bead{Title: "Task"})
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(state, "/bead/"+work.ID+"/assign"), bytes.NewBufferString(`{"assignee":"myrig/worker"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("assign configured named identity status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	got, _ := store.Get(work.ID)
	if got.Assignee == "" || got.Assignee == "myrig/worker" {
		t.Fatalf("assignee = %q, want materialized concrete session bead ID", got.Assignee)
	}
	gotSession, err := state.cityBeadStore.Get(got.Assignee)
	if err != nil {
		t.Fatalf("Get(materialized session %q): %v", got.Assignee, err)
	}
	if gotSession.Metadata[apiNamedSessionMetadataKey] != "true" {
		t.Fatalf("configured_named_session = %q, want true", gotSession.Metadata[apiNamedSessionMetadataKey])
	}
	if gotSession.Metadata[apiNamedSessionIdentityKey] != "myrig/worker" {
		t.Fatalf("configured_named_identity = %q, want myrig/worker", gotSession.Metadata[apiNamedSessionIdentityKey])
	}
}

func TestPhase2BeadAssignDoesNotMaterializeNamedSessionForMissingBead(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(state, "/bead/missing/assign"), bytes.NewBufferString(`{"assignee":"myrig/worker"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("assign missing bead status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	items, err := state.cityBeadStore.List(beads.ListQuery{Label: session.LabelSession})
	if err != nil {
		t.Fatalf("List(sessions): %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("materialized %d session(s) for missing bead, want 0: %#v", len(items), items)
	}
}

func TestPhase2BeadAssignRejectsUnknownAssigneeAlias(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	store := state.stores["myrig"]
	_, _ = store.Create(beads.Bead{Title: "ID offset"})
	work, _ := store.Create(beads.Bead{Title: "Task"})
	createPhase2APISessionBead(t, state.cityBeadStore)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(state, "/bead/"+work.ID+"/assign"), bytes.NewBufferString(`{"assignee":"missing-worker"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("assign unknown alias status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	got, _ := store.Get(work.ID)
	if got.Assignee != "" {
		t.Fatalf("assignee after rejected unknown alias = %q, want empty", got.Assignee)
	}
}

func TestPhase2BeadAssignRejectsClosedSessionBeadID(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	store := state.stores["myrig"]
	_, _ = store.Create(beads.Bead{Title: "ID offset"})
	work, _ := store.Create(beads.Bead{Title: "Task"})
	sessionBead := createPhase2APISessionBead(t, state.cityBeadStore)
	closed := "closed"
	if err := state.cityBeadStore.Update(sessionBead.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("Update(session status): %v", err)
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(state, "/bead/"+work.ID+"/assign"), bytes.NewBufferString(`{"assignee":"`+sessionBead.ID+`"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("assign closed session status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	got, _ := store.Get(work.ID)
	if got.Assignee != "" {
		t.Fatalf("assignee after rejected closed session = %q, want empty", got.Assignee)
	}
}

func TestPhase2BeadAssignAcceptsRepairableSessionBeadID(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	store := state.stores["myrig"]
	_, _ = store.Create(beads.Bead{Title: "ID offset"})
	work, _ := store.Create(beads.Bead{Title: "Task"})
	sessionBead := createPhase2APISessionBead(t, state.cityBeadStore)
	empty := ""
	if err := state.cityBeadStore.Update(sessionBead.ID, beads.UpdateOpts{Type: &empty}); err != nil {
		t.Fatalf("Update(session type): %v", err)
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(state, "/bead/"+work.ID+"/assign"), bytes.NewBufferString(`{"assignee":"`+sessionBead.ID+`"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("assign repairable session status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	got, _ := store.Get(work.ID)
	if got.Assignee != sessionBead.ID {
		t.Fatalf("assignee = %q, want repairable session bead ID %q", got.Assignee, sessionBead.ID)
	}
	gotSession, _ := state.cityBeadStore.Get(sessionBead.ID)
	if gotSession.Type != session.BeadType {
		t.Fatalf("repaired session type = %q, want %q", gotSession.Type, session.BeadType)
	}
}

func TestPhase2BeadUpdateNormalizesRawAssigneeAlias(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	store := state.stores["myrig"]
	_, _ = store.Create(beads.Bead{Title: "ID offset"})
	work, _ := store.Create(beads.Bead{Title: "Task"})
	sessionBead := createPhase2APISessionBead(t, state.cityBeadStore)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(state, "/bead/"+work.ID+"/update"), bytes.NewBufferString(`{"assignee":"worker"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("update alias status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	got, _ := store.Get(work.ID)
	if got.Assignee != sessionBead.ID {
		t.Fatalf("assignee = %q, want alias normalized to session bead ID %q", got.Assignee, sessionBead.ID)
	}
}

func TestPhase2BeadCreateNormalizesRawAssigneeAlias(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	sessionBead := createPhase2APISessionBead(t, state.cityBeadStore)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(state, "/beads"), bytes.NewBufferString(`{"rig":"myrig","title":"Task","assignee":"worker"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create alias status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	items, err := state.stores["myrig"].List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("created %d beads, want 1", len(items))
	}
	if items[0].Assignee != sessionBead.ID {
		t.Fatalf("created assignee = %q, want alias normalized to session bead ID %q", items[0].Assignee, sessionBead.ID)
	}
}

func createPhase2APISessionBead(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  "Worker session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "test-city--worker",
			"alias":        "worker",
			"template":     "myrig/worker",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	return b
}

func TestBeadDelete(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	b, _ := store.Create(beads.Bead{Title: "To delete"})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("DELETE", cityURL(state, "/bead/")+b.ID, nil)
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("DELETE", cityURL(state, "/bead/nonexistent"), nil)
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestBeadCreateValidation(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	// Missing title.
	req := newPostRequest(cityURL(state, "/beads"), bytes.NewBufferString(`{"rig":"myrig"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
}

func TestBeadUpdateParentOpenAPISchemaAllowsNull(t *testing.T) {
	data, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatalf("read openapi.json: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse openapi.json: %v", err)
	}
	components, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatal("openapi components missing")
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatal("openapi schemas missing")
	}
	beadUpdate, ok := schemas["BeadUpdateBody"].(map[string]any)
	if !ok {
		t.Fatal("BeadUpdateBody schema missing")
	}
	properties, ok := beadUpdate["properties"].(map[string]any)
	if !ok {
		t.Fatal("BeadUpdateBody properties missing")
	}
	parent, ok := properties["parent"].(map[string]any)
	if !ok {
		t.Fatal("BeadUpdateBody parent property missing")
	}
	typeValues, ok := parent["type"].([]any)
	if !ok {
		t.Fatalf("parent type = %#v, want [\"string\", \"null\"]", parent["type"])
	}
	seen := map[string]bool{}
	for _, value := range typeValues {
		if s, ok := value.(string); ok {
			seen[s] = true
		}
	}
	if !seen["string"] || !seen["null"] {
		t.Fatalf("parent type = %#v, want string and null", parent["type"])
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
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/packs"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/packs"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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
