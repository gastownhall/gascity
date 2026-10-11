package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
	bdhttp "github.com/steveyegge/beads/backend/http"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// The http leg end to end: the real beads http client, registered the way
// gc's composition root registers it, against a STATEFUL in-process stand-in
// for `bd serve`. The stand-in keeps rows, labels, metadata, edges and a
// per-row revision, and answers only the role-surface routes the gc store
// reaches. Every request it sees is recorded, and any route outside the role
// surface (a raw SQL path, a transaction, anything it does not implement) is
// recorded as unknown and fails the test.

// remoteHTTPStatefulIssue is one stored row of the stand-in.
type remoteHTTPStatefulIssue struct {
	ID          string
	Title       string
	Description string
	Status      string
	IssueType   string
	Assignee    string
	Priority    int
	Labels      []string
	Metadata    map[string]json.RawMessage
	Revision    int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// remoteHTTPStatefulEdge is one stored dependency edge: IssueID depends on
// DependsOnID.
type remoteHTTPStatefulEdge struct {
	IssueID     string
	DependsOnID string
	Type        string
}

type remoteHTTPStatefulServer struct {
	mu       sync.Mutex
	issues   map[string]*remoteHTTPStatefulIssue
	order    []string
	edges    []remoteHTTPStatefulEdge
	nextID   int
	nextRev  int64
	requests []string
	unknown  []string
	now      time.Time
}

func newRemoteHTTPStatefulServer(t *testing.T) *remoteHTTPStatefulServer {
	t.Helper()
	s := &remoteHTTPStatefulServer{
		issues:  map[string]*remoteHTTPStatefulIssue{},
		nextRev: 100,
		now:     time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
	}
	client := &http.Client{Transport: s}
	previous := nativeOpenOptions
	nativeOpenOptions = func(string) beadslib.OpenOptions { return beadslib.OpenOptions{HTTPClient: client} }
	t.Cleanup(func() { nativeOpenOptions = previous })
	return s
}

// RoundTrip serves one request in memory.
func (s *remoteHTTPStatefulServer) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	s.serve(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

func (s *remoteHTTPStatefulServer) snapshot() (requests, unknown []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...), append([]string(nil), s.unknown...)
}

func (s *remoteHTTPStatefulServer) writeJSON(w http.ResponseWriter, body any) {
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *remoteHTTPStatefulServer) problem(w http.ResponseWriter, status int, code, param, detail string) {
	body := map[string]any{"status": status, "code": code, "detail": detail}
	if param != "" {
		body["param"] = param
	}
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// route names a request for the record: the method and the path with the
// issue id collapsed, so assertions read the role surface, not the data.
func (s *remoteHTTPStatefulServer) route(r *http.Request) string {
	path := r.URL.Path
	if rest, ok := strings.CutPrefix(path, "/v0/beads/issues/"); ok {
		suffix := ""
		if i := strings.IndexAny(rest, ":/"); i >= 0 {
			suffix = rest[i:]
		}
		path = "/v0/beads/issues/{id}" + suffix
	}
	return r.Method + " " + path
}

func (s *remoteHTTPStatefulServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	route := s.route(r)
	s.requests = append(s.requests, route)
	var body map[string]json.RawMessage
	if r.Body != nil {
		data, _ := io.ReadAll(r.Body)
		if len(data) > 0 {
			if err := json.Unmarshal(data, &body); err != nil {
				s.problem(w, http.StatusBadRequest, "invalid_argument", "", "body is not a JSON object")
				return
			}
		}
	}
	path := r.URL.Path
	switch route {
	case "GET /v0/beads/context":
		s.writeJSON(w, map[string]any{
			"api_version": "v0", "backend": "dolt", "bd_version": "1.3.1",
			"beads_dir": "/srv/.beads", "capabilities": fullRemoteCapabilities(),
			"database": "beads", "dolt_mode": "server", "project_id": remoteHTTPProjectID,
			"repo_root": "/srv", "schema_version": 1, "wire_revision": 2, "min_client_wire_revision": 0,
		})
	case "GET /v0/beads/config/issue_prefix":
		s.writeJSON(w, map[string]any{"key": "issue_prefix", "value": "gr"})
	case "POST /v0/beads/issues":
		s.create(w, body)
	case "GET /v0/beads/issues":
		s.list(w)
	case "GET /v0/beads/issues/{id}":
		s.get(w, issueIDFromPath(path, ""))
	case "PATCH /v0/beads/issues/{id}":
		s.patch(w, issueIDFromPath(path, ""), body)
	case "POST /v0/beads/issues/{id}:claim":
		s.claim(w, issueIDFromPath(path, ":claim"), body)
	case "POST /v0/beads/issues:batchApply":
		s.batchApply(w, body)
	case "GET /v0/beads/dependencies":
		s.dependencies(w, r.URL.Query())
	default:
		s.unknown = append(s.unknown, route)
		s.problem(w, http.StatusNotFound, "not_found", "", "the stand-in does not serve "+route)
	}
}

func issueIDFromPath(path, method string) string {
	id := strings.TrimPrefix(path, "/v0/beads/issues/")
	id = strings.TrimSuffix(id, method)
	if unescaped, err := url.PathUnescape(id); err == nil {
		return unescaped
	}
	return id
}

func (s *remoteHTTPStatefulServer) bump(issue *remoteHTTPStatefulIssue) {
	s.nextRev += 7
	issue.Revision = s.nextRev
	s.now = s.now.Add(time.Second)
	issue.UpdatedAt = s.now
}

func (s *remoteHTTPStatefulServer) render(issue *remoteHTTPStatefulIssue) map[string]any {
	out := map[string]any{
		"id": issue.ID, "title": issue.Title, "description": issue.Description,
		"status": issue.Status, "issue_type": issue.IssueType, "priority": issue.Priority,
		"created_at": issue.CreatedAt, "updated_at": issue.UpdatedAt,
		"revision": strconv.FormatInt(issue.Revision, 10),
	}
	if issue.Assignee != "" {
		out["assignee"] = issue.Assignee
	}
	if len(issue.Labels) > 0 {
		out["labels"] = slices.Clone(issue.Labels)
	}
	if len(issue.Metadata) > 0 {
		out["metadata"] = issue.Metadata
	}
	return out
}

func decodeString(raw json.RawMessage) string {
	var v string
	_ = json.Unmarshal(raw, &v)
	return v
}

// newIssue builds a row from create-shaped members, shared by createIssue and
// the batchApply create item.
func (s *remoteHTTPStatefulServer) newIssue(body map[string]json.RawMessage) (*remoteHTTPStatefulIssue, error) {
	title := decodeString(body["title"])
	if strings.TrimSpace(title) == "" {
		return nil, errors.New("title is required")
	}
	issue := &remoteHTTPStatefulIssue{
		Title: title, Description: decodeString(body["description"]),
		Status: decodeString(body["status"]), IssueType: decodeString(body["issue_type"]),
		Assignee: decodeString(body["assignee"]), Metadata: map[string]json.RawMessage{},
	}
	if raw, ok := body["priority"]; ok {
		_ = json.Unmarshal(raw, &issue.Priority)
	}
	if raw, ok := body["labels"]; ok {
		_ = json.Unmarshal(raw, &issue.Labels)
	}
	if raw, ok := body["metadata"]; ok && len(raw) > 0 && string(raw) != "null" {
		doc := raw
		var asString string
		if json.Unmarshal(raw, &asString) == nil {
			doc = json.RawMessage(asString)
		}
		if err := json.Unmarshal(doc, &issue.Metadata); err != nil {
			return nil, fmt.Errorf("metadata: %w", err)
		}
	}
	if issue.Status == "" {
		issue.Status = "open"
	}
	if issue.IssueType == "" {
		issue.IssueType = "task"
	}
	id := decodeString(body["id"])
	if id == "" {
		s.nextID++
		id = fmt.Sprintf("gr-%d", s.nextID)
	}
	if _, exists := s.issues[id]; exists {
		return nil, fmt.Errorf("id %s already exists", id)
	}
	issue.ID = id
	s.now = s.now.Add(time.Second)
	issue.CreatedAt = s.now
	s.bump(issue)
	s.issues[id] = issue
	s.order = append(s.order, id)
	return issue, nil
}

func (s *remoteHTTPStatefulServer) create(w http.ResponseWriter, body map[string]json.RawMessage) {
	issue, err := s.newIssue(body)
	if err != nil {
		s.problem(w, http.StatusBadRequest, "invalid_argument", "", err.Error())
		return
	}
	if raw, ok := body["dependencies"]; ok {
		var deps []struct {
			TargetID string `json:"target_id"`
			Type     string `json:"type"`
			Reverse  bool   `json:"reverse"`
		}
		_ = json.Unmarshal(raw, &deps)
		for _, dep := range deps {
			edge := remoteHTTPStatefulEdge{IssueID: issue.ID, DependsOnID: dep.TargetID, Type: dep.Type}
			if dep.Reverse {
				edge.IssueID, edge.DependsOnID = dep.TargetID, issue.ID
			}
			s.edges = append(s.edges, edge)
		}
	}
	s.writeJSON(w, s.render(issue))
}

func (s *remoteHTTPStatefulServer) list(w http.ResponseWriter) {
	items := make([]map[string]any, 0, len(s.order))
	for _, id := range s.order {
		items = append(items, s.render(s.issues[id]))
	}
	s.writeJSON(w, map[string]any{"items": items, "has_more": false})
}

func (s *remoteHTTPStatefulServer) get(w http.ResponseWriter, id string) {
	issue, ok := s.issues[id]
	if !ok {
		s.problem(w, http.StatusNotFound, "not_found", "", "no issue or wisp with that id")
		return
	}
	s.writeJSON(w, s.render(issue))
}

// applyPatch applies an update patch in either spelling the client sends:
// updateIssue's flat labels (`add_labels`/`remove_labels`, `labels` as a
// replace array) and batchApply's nested `labels` object.
func (s *remoteHTTPStatefulServer) applyPatch(issue *remoteHTTPStatefulIssue, patch map[string]json.RawMessage) error {
	for key, raw := range patch {
		switch key {
		case "title":
			issue.Title = decodeString(raw)
		case "description":
			issue.Description = decodeString(raw)
		case "status":
			issue.Status = decodeString(raw)
		case "issue_type":
			issue.IssueType = decodeString(raw)
		case "assignee":
			issue.Assignee = decodeString(raw)
		case "priority":
			_ = json.Unmarshal(raw, &issue.Priority)
		case "add_labels":
			var add []string
			_ = json.Unmarshal(raw, &add)
			issue.Labels = addLabels(issue.Labels, add)
		case "remove_labels":
			var remove []string
			_ = json.Unmarshal(raw, &remove)
			issue.Labels = removeLabels(issue.Labels, remove)
		case "labels":
			var replace []string
			if json.Unmarshal(raw, &replace) == nil {
				issue.Labels = replace
				continue
			}
			var nested struct {
				Replace *[]string `json:"replace"`
				Add     []string  `json:"add"`
				Remove  []string  `json:"remove"`
			}
			if err := json.Unmarshal(raw, &nested); err != nil {
				return fmt.Errorf("labels: %w", err)
			}
			if nested.Replace != nil {
				issue.Labels = *nested.Replace
			}
			issue.Labels = removeLabels(addLabels(issue.Labels, nested.Add), nested.Remove)
		case "metadata":
			var edit struct {
				Set   map[string]json.RawMessage `json:"set"`
				Unset []string                   `json:"unset"`
			}
			if err := json.Unmarshal(raw, &edit); err != nil {
				return fmt.Errorf("metadata: %w", err)
			}
			for k, v := range edit.Set {
				issue.Metadata[k] = v
			}
			for _, k := range edit.Unset {
				delete(issue.Metadata, k)
			}
		default:
			return fmt.Errorf("patch member %q is not served by the stand-in", key)
		}
	}
	return nil
}

func addLabels(labels, add []string) []string {
	for _, label := range add {
		if !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	sort.Strings(labels)
	return labels
}

func removeLabels(labels, remove []string) []string {
	return slices.DeleteFunc(slices.Clone(labels), func(l string) bool { return slices.Contains(remove, l) })
}

// guardVersion answers whether expected (absent = no guard) matches issue.
func guardVersion(issue *remoteHTTPStatefulIssue, expected json.RawMessage) bool {
	if len(expected) == 0 {
		return true
	}
	return decodeString(expected) == strconv.FormatInt(issue.Revision, 10)
}

func (s *remoteHTTPStatefulServer) patch(w http.ResponseWriter, id string, body map[string]json.RawMessage) {
	issue, ok := s.issues[id]
	if !ok {
		s.problem(w, http.StatusNotFound, "not_found", "", "no issue with that id")
		return
	}
	if !guardVersion(issue, body["expected_version"]) {
		s.problem(w, http.StatusConflict, "precondition_failed", "expected_version", "revision moved")
		return
	}
	var patch map[string]json.RawMessage
	_ = json.Unmarshal(body["patch"], &patch)
	if err := s.applyPatch(issue, patch); err != nil {
		s.problem(w, http.StatusBadRequest, "invalid_argument", "", err.Error())
		return
	}
	s.bump(issue)
	s.writeJSON(w, map[string]any{
		"changed": true, "issue": s.render(issue), "revision": strconv.FormatInt(issue.Revision, 10),
	})
}

func (s *remoteHTTPStatefulServer) claim(w http.ResponseWriter, id string, body map[string]json.RawMessage) {
	issue, ok := s.issues[id]
	if !ok {
		s.problem(w, http.StatusNotFound, "not_found", "", "no issue with that id")
		return
	}
	actor := decodeString(body["actor"])
	switch {
	case issue.Status == "in_progress" && issue.Assignee == actor:
		s.writeJSON(w, map[string]any{"already_claimed": true, "issue": s.render(issue)})
		return
	case issue.Assignee != "" && issue.Assignee != actor:
		s.problem(w, http.StatusConflict, "already_claimed", "", "claimed by "+issue.Assignee)
		return
	case issue.Status != "open":
		s.problem(w, http.StatusConflict, "not_claimable", "", "status "+issue.Status)
		return
	}
	issue.Assignee, issue.Status = actor, "in_progress"
	s.bump(issue)
	s.writeJSON(w, map[string]any{"already_claimed": false, "issue": s.render(issue)})
}

// batchApply serves issues:batchApply all or nothing: every item is validated
// and applied against a copy, and the copy replaces the store only when the
// whole request succeeded.
func (s *remoteHTTPStatefulServer) batchApply(w http.ResponseWriter, body map[string]json.RawMessage) {
	var items []struct {
		Kind   string                     `json:"kind"`
		Create map[string]json.RawMessage `json:"create"`
		Update *struct {
			Target          struct{ ID, Key string } `json:"target"`
			Patch           map[string]json.RawMessage
			ExpectedVersion json.RawMessage `json:"expected_version"`
		} `json:"update"`
		DepAdd *struct {
			Source struct{ ID, Key string } `json:"source"`
			Target struct{ ID, Key string } `json:"target"`
			Type   string                   `json:"type"`
		} `json:"dep_add"`
	}
	if err := json.Unmarshal(body["items"], &items); err != nil {
		s.problem(w, http.StatusBadRequest, "invalid_argument", "items", err.Error())
		return
	}
	// Snapshot for rollback.
	savedIssues := make(map[string]*remoteHTTPStatefulIssue, len(s.issues))
	for id, issue := range s.issues {
		cp := *issue
		cp.Labels = slices.Clone(issue.Labels)
		cp.Metadata = make(map[string]json.RawMessage, len(issue.Metadata))
		for k, v := range issue.Metadata {
			cp.Metadata[k] = v
		}
		savedIssues[id] = &cp
	}
	savedOrder, savedEdges, savedNextID, savedRev := slices.Clone(s.order), slices.Clone(s.edges), s.nextID, s.nextRev
	rollback := func() {
		s.issues, s.order, s.edges, s.nextID, s.nextRev = savedIssues, savedOrder, savedEdges, savedNextID, savedRev
	}
	keys := map[string]string{}
	resolve := func(ref struct{ ID, Key string }) string {
		if ref.ID != "" {
			return ref.ID
		}
		return keys[ref.Key]
	}
	results := make([]map[string]any, 0, len(items))
	for i, item := range items {
		switch item.Kind {
		case "create":
			issue, err := s.newIssue(item.Create)
			if err != nil {
				rollback()
				s.problem(w, http.StatusBadRequest, "invalid_argument", fmt.Sprintf("items[%d].create", i), err.Error())
				return
			}
			if key := decodeString(item.Create["key"]); key != "" {
				keys[key] = issue.ID
			}
			results = append(results, map[string]any{"kind": "create", "issue_id": issue.ID, "changed": true, "revision": strconv.FormatInt(issue.Revision, 10)})
		case "update":
			id := resolve(item.Update.Target)
			issue, ok := s.issues[id]
			if !ok {
				rollback()
				s.problem(w, http.StatusNotFound, "not_found", fmt.Sprintf("items[%d].update.target", i), "no issue with that id")
				return
			}
			if !guardVersion(issue, item.Update.ExpectedVersion) {
				rollback()
				s.problem(w, http.StatusConflict, "precondition_failed", fmt.Sprintf("items[%d].update.expected_version", i), "revision moved")
				return
			}
			if err := s.applyPatch(issue, item.Update.Patch); err != nil {
				rollback()
				s.problem(w, http.StatusBadRequest, "invalid_argument", fmt.Sprintf("items[%d].update.patch", i), err.Error())
				return
			}
			s.bump(issue)
			results = append(results, map[string]any{"kind": "update", "issue_id": id, "changed": true, "revision": strconv.FormatInt(issue.Revision, 10)})
		case "dep_add":
			from, to := resolve(item.DepAdd.Source), resolve(item.DepAdd.Target)
			if s.issues[from] == nil || s.issues[to] == nil {
				rollback()
				s.problem(w, http.StatusBadRequest, "invalid_argument", fmt.Sprintf("items[%d].dep_add", i), "edge endpoint names nothing")
				return
			}
			s.edges = append(s.edges, remoteHTTPStatefulEdge{IssueID: from, DependsOnID: to, Type: item.DepAdd.Type})
			results = append(results, map[string]any{"kind": "dep_add", "issue_id": from, "depends_on_id": to, "changed": true, "revision": strconv.FormatInt(s.issues[from].Revision, 10)})
		default:
			rollback()
			s.problem(w, http.StatusBadRequest, "invalid_argument", fmt.Sprintf("items[%d].kind", i), "the stand-in does not serve item kind "+item.Kind)
			return
		}
	}
	s.writeJSON(w, map[string]any{"items": results, "keys": keys})
}

func (s *remoteHTTPStatefulServer) dependencies(w http.ResponseWriter, query url.Values) {
	items := []map[string]any{}
	missing := []string{}
	for _, id := range query["issue_id"] {
		if s.issues[id] == nil {
			missing = append(missing, id)
			continue
		}
		for _, edge := range s.edges {
			if edge.IssueID == id {
				items = append(items, map[string]any{
					"issue_id": edge.IssueID, "depends_on_id": edge.DependsOnID,
					"type": edge.Type, "created_at": s.now,
				})
			}
		}
	}
	s.writeJSON(w, map[string]any{"items": items, "missing": missing})
}

// remoteHTTPE2ERoleSurface is every route this leg may reach. Each is an
// issueops role the http client serves; none is a raw SQL, transaction or
// blocked-projection path.
var remoteHTTPE2ERoleSurface = map[string]bool{
	"GET /v0/beads/context":             true,
	"GET /v0/beads/config/issue_prefix": true,
	"POST /v0/beads/issues":             true,
	"GET /v0/beads/issues":              true,
	"GET /v0/beads/issues/{id}":         true,
	"PATCH /v0/beads/issues/{id}":       true,
	"POST /v0/beads/issues/{id}:claim":  true,
	"POST /v0/beads/issues:batchApply":  true,
	"GET /v0/beads/dependencies":        true,
}

// TestNativeStoreRemoteHTTPEndToEndThroughTheRoleSurface drives the gc store
// over the real http client: create, read, update, list, claim, a fenced
// labeled conditional update (and its stale-revision refusal), and a graph
// apply with an edge, then proves every request stayed on the role surface.
func TestNativeStoreRemoteHTTPEndToEndThroughTheRoleSurface(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPStatefulServer(t)
	scope := writeRemoteTestScope(t, `{"backend": "http"}`)
	base, err := url.Parse("http://127.0.0.1:9")
	if err != nil {
		t.Fatal(err)
	}
	if err := bdhttp.SaveTarget(filepath.Join(scope, ".beads"), bdhttp.Target{BaseURL: base, ExpectProjectID: remoteHTTPProjectID}); err != nil {
		t.Fatalf("SaveTarget: %v", err)
	}

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		NativeTransport:  NativeTransportAuto,
		PreflightChecker: noDoltPreflight(t),
		OpenBdStore: func() (Store, error) {
			t.Fatal("OpenBdStore called for an http scope under auto")
			return nil, nil
		},
		OpenNativeStore: func() (Store, error) {
			t.Fatal("the Dolt native opener was used for an http scope")
			return nil, nil
		},
	})
	if err != nil {
		requests, _ := server.snapshot()
		t.Fatalf("OpenStoreAtForCity() error = %v (requests %v)", err, requests)
	}
	store, ok := result.Store.(*NativeDoltStore)
	if !ok {
		t.Fatalf("store = %T, want *NativeDoltStore", result.Store)
	}
	t.Cleanup(func() { _ = store.CloseStore() })

	routesSince := func(mark int) []string {
		requests, _ := server.snapshot()
		return requests[mark:]
	}
	mark := func() int {
		requests, _ := server.snapshot()
		return len(requests)
	}
	logRoutes := func(op string, from int) {
		t.Logf("%s reached: %v", op, routesSince(from))
	}

	// Create.
	m := mark()
	created, err := store.Create(Bead{Title: "first", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	logRoutes("Create", m)
	if created.ID == "" || created.Title != "first" {
		t.Fatalf("Create returned %+v", created)
	}

	// Get.
	m = mark()
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	logRoutes("Get", m)
	if got.Title != "first" || got.Status != "open" {
		t.Fatalf("Get = %+v", got)
	}

	// Update.
	m = mark()
	title := "first, retitled"
	if err := store.Update(created.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	logRoutes("Update", m)
	if got, err = store.Get(created.ID); err != nil || got.Title != title {
		t.Fatalf("Get after Update = %+v, %v; want title %q", got, err, title)
	}

	// List.
	m = mark()
	listed, err := store.List(ListQuery{Type: "task"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	logRoutes("List", m)
	if !slices.ContainsFunc(listed, func(b Bead) bool { return b.ID == created.ID && b.Title == title }) {
		t.Fatalf("List = %+v, want %s", listed, created.ID)
	}

	// Claim.
	m = mark()
	claimed, won, err := store.Claim(created.ID, "worker-1")
	if err != nil || !won {
		t.Fatalf("Claim = (%+v, %v, %v), want won", claimed, won, err)
	}
	logRoutes("Claim", m)
	if got, err = store.Get(created.ID); err != nil || got.Assignee != "worker-1" || got.Status != "in_progress" {
		t.Fatalf("Get after Claim = %+v, %v", got, err)
	}
	if _, won, err := store.Claim(created.ID, "worker-2"); err != nil || won {
		t.Fatalf("second Claim by another worker = (won %v, %v), want lost without error", won, err)
	}

	// Conditional update carrying labels.
	before, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	m = mark()
	if err := store.UpdateIfMatch(created.ID, before.Revision, UpdateOpts{Labels: []string{"hot"}}); err != nil {
		t.Fatalf("UpdateIfMatch with labels: %v (requests %v)", err, routesSince(m))
	}
	logRoutes("UpdateIfMatch(labels)", m)
	after, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(after.Labels, "hot") {
		t.Fatalf("labels after UpdateIfMatch = %v, want hot", after.Labels)
	}
	if after.Revision == before.Revision {
		t.Fatalf("revision stayed %d after a labeled conditional update", after.Revision)
	}
	if after.Metadata[beadmeta.LabelRevisionMetadataKey] == "" {
		t.Fatalf("metadata %q not advanced: %v", beadmeta.LabelRevisionMetadataKey, after.Metadata)
	}
	m = mark()
	err = store.UpdateIfMatch(created.ID, before.Revision, UpdateOpts{Labels: []string{"cold"}})
	var precondition *PreconditionFailedError
	if !errors.As(err, &precondition) {
		t.Fatalf("stale UpdateIfMatch error = %v, want *PreconditionFailedError", err)
	}
	logRoutes("UpdateIfMatch(stale)", m)
	if final, _ := store.Get(created.ID); slices.Contains(final.Labels, "cold") {
		t.Fatalf("a stale conditional update wrote its labels: %v", final.Labels)
	}

	// Graph apply.
	applier, ok := GraphApplyFor(store)
	if !ok {
		t.Fatal("the remote native store offers no graph apply")
	}
	m = mark()
	applied, err := applier.ApplyGraphPlan(context.Background(), &GraphApplyPlan{
		CommitMessage: "gc: e2e graph",
		Nodes: []GraphApplyNode{
			{Key: "root", Title: "graph root", Type: "task"},
			{Key: "step", Title: "graph step", Type: "task", Labels: []string{"graph"}},
		},
		Edges: []GraphApplyEdge{{FromKey: "step", ToKey: "root", Type: "blocks"}},
	})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v (requests %v)", err, routesSince(m))
	}
	logRoutes("ApplyGraphPlan", m)
	rootID, stepID := applied.IDs["root"], applied.IDs["step"]
	if rootID == "" || stepID == "" {
		t.Fatalf("graph ids = %v", applied.IDs)
	}
	if root, err := store.Get(rootID); err != nil || root.Title != "graph root" {
		t.Fatalf("Get(graph root) = %+v, %v", root, err)
	}
	step, err := store.Get(stepID)
	if err != nil || step.Title != "graph step" || !slices.Contains(step.Labels, "graph") {
		t.Fatalf("Get(graph step) = %+v, %v", step, err)
	}
	deps, err := store.DepList(stepID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if !slices.ContainsFunc(deps, func(d Dep) bool { return d.DependsOnID == rootID && d.Type == "blocks" }) {
		t.Fatalf("DepList(step) = %+v, want a blocks edge on %s", deps, rootID)
	}

	requests, unknown := server.snapshot()
	if len(unknown) > 0 {
		t.Errorf("routes outside the stand-in's role surface were reached: %v", unknown)
	}
	for _, request := range requests {
		if !remoteHTTPE2ERoleSurface[request] {
			t.Errorf("request %q is outside the role surface", request)
		}
		lower := strings.ToLower(request)
		if strings.Contains(lower, "sql") || strings.Contains(lower, "blocked") || strings.Contains(lower, "transaction") {
			t.Errorf("request %q is a raw path", request)
		}
	}
}
