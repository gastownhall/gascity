package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestValidateBeadsSnapshotRequestRequiresExactStoreAndJSON(t *testing.T) {
	t.Parallel()

	valid := beadsSnapshotRequest{
		storeRef:    "rig:tributary",
		format:      "json",
		storeRefSet: true,
	}
	if err := validateBeadsSnapshotRequest(valid); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*beadsSnapshotRequest)
		want   string
	}{
		{name: "missing store", mutate: func(r *beadsSnapshotRequest) { r.storeRefSet = false }, want: "--store-ref is required"},
		{name: "federated store", mutate: func(r *beadsSnapshotRequest) { r.storeRef = "all:*" }, want: "invalid --store-ref"},
		{name: "text output", mutate: func(r *beadsSnapshotRequest) { r.format = "text" }, want: "requires JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := valid
			tc.mutate(&request)
			err := validateBeadsSnapshotRequest(request)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestCollectBeadsSnapshotIncludesRevisionTypedDependenciesAndClosedRows(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	priority := 1
	blocker, err := store.Create(beads.Bead{Title: "blocker", Priority: &priority})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:              "private title",
		Description:        "private body",
		AcceptanceCriteria: "acceptance",
		Type:               "feature",
		Priority:           &priority,
		Metadata: beads.StringMap{
			"lifecycle_phase": "development",
			"evidence":        `{"commit":"abc123"}`,
		},
	})
	if err != nil {
		t.Fatalf("Create work: %v", err)
	}
	if err := store.DepAdd(work.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if err := store.Close(blocker.ID); err != nil {
		t.Fatalf("Close blocker: %v", err)
	}

	result, err := collectBeadsSnapshot(store, "rig:tributary")
	if err != nil {
		t.Fatalf("collectBeadsSnapshot: %v", err)
	}
	if !result.OK || result.StoreRef != "rig:tributary" || len(result.Beads) != 2 {
		t.Fatalf("result = %+v", result)
	}
	var got beadsSnapshotRow
	for _, row := range result.Beads {
		if row.ID == work.ID {
			got = row
		}
	}
	if got.Revision == 0 || got.DependencyCount != 1 {
		t.Fatalf("work row = %+v", got)
	}
	if len(got.Dependencies) != 1 || got.Dependencies[0].ID != blocker.ID || got.Dependencies[0].DependencyType != "blocks" {
		t.Fatalf("dependencies = %+v", got.Dependencies)
	}
	if got.AcceptanceCriteria != "acceptance" || got.Metadata["evidence"] != `{"commit":"abc123"}` {
		t.Fatalf("row lost canonical fields: %+v", got)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, field := range []string{`"revision"`, `"dependency_count"`, `"acceptance_criteria"`, `"dependency_type"`} {
		if !bytes.Contains(raw, []byte(field)) {
			t.Fatalf("snapshot missing %s: %s", field, raw)
		}
	}
}

func TestCollectBeadsSnapshotRequiresOneAtomicIssueGraphRead(t *testing.T) {
	t.Parallel()

	priority := 1
	store := &atomicSnapshotStore{
		Store: beads.NewMemStore(),
		rows: []beads.Bead{{
			ID:       "ga-1",
			Title:    "atomic",
			Priority: &priority,
			Revision: 7,
		}},
		deps: map[string][]beads.Dep{
			"ga-1": {{IssueID: "ga-1", DependsOnID: "ga-2", Type: "blocks"}},
		},
	}

	result, err := collectBeadsSnapshot(store, "rig:tributary")
	if err != nil {
		t.Fatalf("collectBeadsSnapshot: %v", err)
	}
	if store.snapshotCalls != 1 {
		t.Fatalf("atomic snapshot calls = %d, want 1", store.snapshotCalls)
	}
	if store.legacyReads != 0 {
		t.Fatalf("legacy List/DepList reads = %d, want 0", store.legacyReads)
	}
	if len(result.Beads) != 1 || result.Beads[0].DependencyCount != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestCollectBeadsSnapshotFailsClosedWithoutAtomicIssueGraphReader(t *testing.T) {
	t.Parallel()

	store := &legacyOnlySnapshotStore{Store: beads.NewMemStore()}
	if _, err := store.Create(beads.Bead{Title: "legacy"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := collectBeadsSnapshot(store, "rig:tributary")
	if err == nil || !strings.Contains(err.Error(), "atomic") {
		t.Fatalf("error = %v, want atomic snapshot requirement", err)
	}
	if store.listCalls != 0 || store.depListCalls != 0 {
		t.Fatalf("legacy reads = List:%d DepList:%d, want none", store.listCalls, store.depListCalls)
	}
}

func TestCollectBeadsSnapshotFailsClosedOnZeroRevisionOrDependencyError(t *testing.T) {
	t.Parallel()

	for name, revision := range map[string]int64{"zero": 0} {
		t.Run(name, func(t *testing.T) {
			store := &snapshotFailureStore{
				Store: beads.NewMemStore(),
				rows:  []beads.Bead{{ID: "ga-1", Title: "x", Revision: revision}},
			}
			if _, err := collectBeadsSnapshot(store, "rig:tributary"); err == nil || !strings.Contains(err.Error(), "revision") {
				t.Fatalf("revision %d error = %v", revision, err)
			}
		})
	}

	priority := 1
	broken := &snapshotFailureStore{
		Store:  beads.NewMemStore(),
		rows:   []beads.Bead{{ID: "ga-1", Title: "x", Priority: &priority, Revision: 7}},
		depErr: errors.New("dependency backend unavailable"),
	}
	if _, err := collectBeadsSnapshot(broken, "rig:tributary"); err == nil || !strings.Contains(err.Error(), "dependency") {
		t.Fatalf("dependency error = %v", err)
	}
}

func TestCollectBeadsSnapshotAcceptsSignedOpaqueRevision(t *testing.T) {
	t.Parallel()
	priority := 1
	store := &atomicSnapshotStore{
		Store: beads.NewMemStore(),
		rows:  []beads.Bead{{ID: "ga-1", Title: "work", Type: "task", Status: "open", Priority: &priority, Revision: -1}},
		deps:  map[string][]beads.Dep{"ga-1": {}},
	}
	result, err := collectBeadsSnapshot(store, "rig:tributary")
	if err != nil || len(result.Beads) != 1 || result.Beads[0].Revision != -1 {
		t.Fatalf("snapshot=%+v err=%v, want opaque revision -1 unchanged", result, err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	validateJSONAgainstResultSchema(t, []string{"beads", "snapshot"}, raw)
	result.Beads[0].Revision = 0
	raw, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateJSONAgainstResultSchemaE([]string{"beads", "snapshot"}, raw); err == nil {
		t.Fatal("snapshot schema accepted the zero revision sentinel")
	}
}

func TestBeadsSnapshotCommandReadsOnlyResolvedRigStore(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)
	cityPath := writeMetadataCASTestCity(t)
	priority := 1
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{Title: "private", Type: "task", Priority: &priority})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	previousOpen := openBeadsSnapshotStore
	previousClose := closeBeadsSnapshotStore
	openBeadsSnapshotStore = func(scopeRoot, _ string) (beads.Store, error) {
		if !strings.HasSuffix(scopeRoot, "tributary") {
			t.Fatalf("scopeRoot = %q, want tributary", scopeRoot)
		}
		return store, nil
	}
	closeBeadsSnapshotStore = func(beads.Store) error { return nil }
	t.Cleanup(func() {
		openBeadsSnapshotStore = previousOpen
		closeBeadsSnapshotStore = previousClose
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--city", cityPath,
		"beads", "snapshot",
		"--store-ref=rig:tributary",
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	var result beadsSnapshotResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, stdout.String())
	}
	if len(result.Beads) != 1 || result.Beads[0].ID != bead.ID || result.Beads[0].Revision == 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestBeadsSnapshotJSONSchemaMatchesRuntimeOutput(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)

	var manifestStdout, manifestStderr bytes.Buffer
	if code := run([]string{"beads", "snapshot", "--json-schema"}, &manifestStdout, &manifestStderr); code != 0 {
		t.Fatalf("manifest code=%d stderr=%q stdout=%q", code, manifestStderr.String(), manifestStdout.String())
	}
	var manifest jsonSchemaManifest
	if err := json.Unmarshal(manifestStdout.Bytes(), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest: %v\n%s", err, manifestStdout.String())
	}
	if !manifest.JSONSupported || strings.Join(manifest.Command, " ") != "beads snapshot" {
		t.Fatalf("manifest = %+v", manifest)
	}
	resultSchema := compileJSONSchema(t, "gc://schemas/beads/snapshot/result.schema.json", manifest.Schemas[jsonSchemaResultRole])

	cityPath := writeMetadataCASTestCity(t)
	store := beads.NewMemStore()
	priority := 1
	if _, err := store.Create(beads.Bead{Title: "private", Type: "task", Priority: &priority}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	previousOpen := openBeadsSnapshotStore
	previousClose := closeBeadsSnapshotStore
	openBeadsSnapshotStore = func(_, _ string) (beads.Store, error) { return store, nil }
	closeBeadsSnapshotStore = func(beads.Store) error { return nil }
	t.Cleanup(func() {
		openBeadsSnapshotStore = previousOpen
		closeBeadsSnapshotStore = previousClose
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--city", cityPath,
		"beads", "snapshot",
		"--store-ref=rig:tributary",
		"--json",
	}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	var payload any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("Unmarshal result: %v\n%s", err, stdout.String())
	}
	if err := resultSchema.Validate(payload); err != nil {
		t.Fatalf("result does not match schema: %v\n%s", err, stdout.String())
	}
}

type snapshotFailureStore struct {
	beads.Store
	rows    []beads.Bead
	listErr error
	depErr  error
}

func (s *snapshotFailureStore) IssueGraphSnapshot(beads.ListQuery) ([]beads.Bead, map[string][]beads.Dep, error) {
	if s.listErr != nil {
		return nil, nil, s.listErr
	}
	if s.depErr != nil {
		return nil, nil, s.depErr
	}
	deps := make(map[string][]beads.Dep, len(s.rows))
	for _, row := range s.rows {
		deps[row.ID] = []beads.Dep{}
	}
	return append([]beads.Bead(nil), s.rows...), deps, nil
}

func (s *snapshotFailureStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return append([]beads.Bead(nil), s.rows...), s.listErr
}

func (s *snapshotFailureStore) DepList(string, string) ([]beads.Dep, error) {
	return nil, s.depErr
}

type atomicSnapshotStore struct {
	beads.Store
	rows          []beads.Bead
	deps          map[string][]beads.Dep
	snapshotErr   error
	snapshotCalls int
	legacyReads   int
}

func (s *atomicSnapshotStore) IssueGraphSnapshot(beads.ListQuery) ([]beads.Bead, map[string][]beads.Dep, error) {
	s.snapshotCalls++
	return append([]beads.Bead(nil), s.rows...), s.deps, s.snapshotErr
}

func (s *atomicSnapshotStore) List(beads.ListQuery) ([]beads.Bead, error) {
	s.legacyReads++
	return nil, errors.New("legacy List path invoked")
}

func (s *atomicSnapshotStore) DepList(string, string) ([]beads.Dep, error) {
	s.legacyReads++
	return nil, errors.New("legacy DepList path invoked")
}

type legacyOnlySnapshotStore struct {
	beads.Store
	listCalls    int
	depListCalls int
}

func (s *legacyOnlySnapshotStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	return s.Store.List(query)
}

func (s *legacyOnlySnapshotStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.depListCalls++
	return s.Store.DepList(id, direction)
}
