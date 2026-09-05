package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
	"golang.org/x/mod/semver"
)

type waitErrorStore struct {
	*beads.MemStore
}

type waitNudgeMetadataFailStore struct {
	*beads.MemStore
}

type waitGetSpyStore struct {
	beads.Store
	getIDs []string
}

type waitPrefixedStore struct {
	beads.Store
	prefix string
}

func (s waitPrefixedStore) IDPrefix() string { return s.prefix }

// waitDependencyReaderOver is the unsuspended, binding-free frame most wait
// tests want: the city work store leading and the named rigs behind it, which is
// the leg set these tests read before the reader planned through storeref.
//
// The config is synthesized from the prefixes the stores themselves declare
// because the by-id plan's shadow rule is CONFIGURED-prefix gated: a rig leg
// whose Prefix is empty covers no id and is out of every plan, so a topology
// assembled with a nil config would drop the rig these rows are about. A real
// city always supplies them (config.Rig.EffectivePrefix derives one from the rig
// name when none is set), and cr.residencyTopology reads them off cr.cfg.
func waitDependencyReaderOver(cityStore beads.Store, rigStores map[string]beads.Store) waitDependencyReader {
	cfg := &config.City{}
	cfg.Workspace.Prefix = declaredStoreIDPrefix(cityStore)
	for _, name := range sortedStoreNames(rigStores) {
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: name, Prefix: declaredStoreIDPrefix(rigStores[name])})
	}
	return newWaitDependencyPlanReader(assembleResidencyTopology(cfg, cityStore, rigStores, nil, nil), false)
}

// declaredStoreIDPrefix reads the prefix a test store declares, which is what
// the city's config would have declared for it.
func declaredStoreIDPrefix(store beads.Store) string {
	if p, ok := store.(storeref.HasIDPrefix); ok {
		return p.IDPrefix()
	}
	return ""
}

func sortedStoreNames(stores map[string]beads.Store) []string {
	names := make([]string, 0, len(stores))
	for name := range stores {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type waitDependencyGetErrorStore struct {
	beads.Store
	prefix string
	err    error
}

func (s waitDependencyGetErrorStore) IDPrefix() string { return s.prefix }

func (s waitDependencyGetErrorStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, s.err
}

type waitListQueryCaptureStore struct {
	beads.Store
	queries []beads.ListQuery
}

type waitGlobalListOmitStore struct {
	beads.Store
}

type waitGlobalListLimitStore struct {
	beads.Store
}

func TestWaitNudgePollerKeyFallbackOrder(t *testing.T) {
	cases := []struct {
		name string
		bead beads.Bead
		want string
	}{
		{
			name: "session id wins over agent name",
			bead: beads.Bead{
				ID:       "session-id",
				Metadata: map[string]string{"agent_name": "agent", "template": "template"},
			},
			want: "session-id",
		},
		{
			name: "agent name fallback",
			bead: beads.Bead{
				Metadata: map[string]string{"agent_name": "agent", "template": "template", "session_name": "s-test"},
			},
			want: "agent",
		},
		{
			name: "alias fallback",
			bead: beads.Bead{
				Metadata: map[string]string{"alias": "alias", "agent_name": "agent", "template": "template", "session_name": "s-test"},
				Title:    "title",
			},
			want: "alias",
		},
		{
			name: "agent name fallback after alias",
			bead: beads.Bead{
				Metadata: map[string]string{"agent_name": "agent", "template": "template"},
			},
			want: "agent",
		},
		{
			name: "template fallback",
			bead: beads.Bead{
				Metadata: map[string]string{"template": "template"},
			},
			want: "template",
		},
		{
			name: "session name fallback",
			bead: beads.Bead{
				Metadata: map[string]string{"session_name": "s-test"},
				Title:    "title",
			},
			want: "s-test",
		},
		{
			name: "title fallback",
			bead: beads.Bead{Title: "title"},
			want: "title",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := sessionpkg.Info{
				ID:                  tc.bead.ID,
				Alias:               tc.bead.Metadata["alias"],
				AgentName:           tc.bead.Metadata["agent_name"],
				Template:            tc.bead.Metadata["template"],
				SessionNameMetadata: tc.bead.Metadata["session_name"],
				Title:               tc.bead.Title,
			}
			if got := waitNudgePollerKey(info); got != tc.want {
				t.Fatalf("waitNudgePollerKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

type waitGlobalListErrorStore struct {
	beads.Store
}

type waitOneSessionListLimitStore struct {
	beads.Store
	sessionID string
}

type waitLookupLimitStore struct {
	beads.Store
}

func setWaitTestFileBeads(t *testing.T) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
}

func TestWaitListJSON(t *testing.T) {
	cityDir, store := setupWaitJSONTestCity(t)
	wait := createTestWaitBead(t, store)

	var stdout, stderr bytes.Buffer
	if code := cmdWaitList("", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdWaitList(--json) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var payload struct {
		SchemaVersion string `json:"schema_version"`
		CityPath      string `json:"city_path"`
		Waits         []struct {
			ID              string            `json:"id"`
			SessionID       string            `json:"session_id"`
			State           string            `json:"state"`
			DepIDs          []string          `json:"dep_ids"`
			RegisteredEpoch string            `json:"registered_epoch"`
			Metadata        map[string]string `json:"metadata"`
		} `json:"waits"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.SchemaVersion != "1" || payload.CityPath != cityDir || len(payload.Waits) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	if got := payload.Waits[0]; got.ID != wait.ID || got.SessionID != "session-1" || got.State != waitStatePending || len(got.DepIDs) != 2 {
		t.Fatalf("wait row = %+v, source=%+v", got, wait)
	}
	if got := payload.Waits[0].RegisteredEpoch; got != "1" {
		t.Fatalf("registered_epoch = %q, want 1", got)
	}
	if payload.Waits[0].Metadata != nil {
		t.Fatalf("metadata = %+v, want omitted", payload.Waits[0].Metadata)
	}
}

func TestWaitInspectJSON(t *testing.T) {
	_, store := setupWaitJSONTestCity(t)
	wait := createTestWaitBead(t, store)

	var stdout, stderr bytes.Buffer
	if code := cmdWaitInspect(wait.ID, true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdWaitInspect(--json) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var payload struct {
		SchemaVersion string `json:"schema_version"`
		Wait          struct {
			ID              string            `json:"id"`
			Kind            string            `json:"kind"`
			Note            string            `json:"note"`
			DepMode         string            `json:"dep_mode"`
			RegisteredEpoch string            `json:"registered_epoch"`
			Metadata        map[string]string `json:"metadata"`
		} `json:"wait"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.SchemaVersion != "1" || payload.Wait.ID != wait.ID || payload.Wait.Kind != "deps" || payload.Wait.Note != "wait for deps" {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Wait.DepMode != "all" || payload.Wait.RegisteredEpoch != "1" {
		t.Fatalf("wait = %+v", payload.Wait)
	}
	if payload.Wait.Metadata != nil {
		t.Fatalf("metadata = %+v, want omitted", payload.Wait.Metadata)
	}
}

func TestWaitListJSONFiltersState(t *testing.T) {
	_, store := setupWaitJSONTestCity(t)
	pending := createTestWaitBead(t, store)
	ready := createTestWaitBeadForSession(t, store, "session-2", waitStateReady)

	var stdout, stderr bytes.Buffer
	if code := cmdWaitList(waitStatePending, "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdWaitList(--json --state pending) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}

	payload := decodeWaitListJSON(t, stdout.Bytes())
	if len(payload.Waits) != 1 || payload.Waits[0].ID != pending.ID {
		t.Fatalf("waits = %+v, want only %s; filtered ready=%s", payload.Waits, pending.ID, ready.ID)
	}
}

func TestWaitListJSONSessionFilterWiresFileStore(t *testing.T) {
	_, store := setupWaitJSONTestCity(t)
	targetWait := createTestWaitBeadForSession(t, store, "target-session", waitStatePending)
	otherWait := createTestWaitBeadForSession(t, store, "other-session", waitStatePending)

	var stdout, stderr bytes.Buffer
	if code := cmdWaitList("", "target-session", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdWaitList(--json --session target-session) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}

	payload := decodeWaitListJSON(t, stdout.Bytes())
	if len(payload.Waits) != 1 || payload.Waits[0].ID != targetWait.ID || payload.Waits[0].SessionID != "target-session" {
		t.Fatalf("waits = %+v, want only target %s", payload.Waits, targetWait.ID)
	}
	if strings.Contains(stdout.String(), otherWait.ID) {
		t.Fatalf("wait list output included non-target wait %s: %s", otherWait.ID, stdout.String())
	}
}

func TestWaitListJSONEmptyListUsesArray(t *testing.T) {
	_, _ = setupWaitJSONTestCity(t)

	var stdout, stderr bytes.Buffer
	if code := cmdWaitList("", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdWaitList(--json empty) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}

	payload := decodeWaitListJSON(t, stdout.Bytes())
	if payload.Waits == nil {
		t.Fatalf("waits decoded as nil; stdout=%s", stdout.String())
	}
	if len(payload.Waits) != 0 {
		t.Fatalf("waits = %+v, want empty", payload.Waits)
	}
}

func TestWaitInspectJSONFailuresUseCommandFailureEnvelope(t *testing.T) {
	cases := []struct {
		name       string
		waitID     func(t *testing.T, store beads.Store) string
		stderrWant string
	}{
		{
			name:       "missing",
			waitID:     func(_ *testing.T, _ beads.Store) string { return "missing-wait" },
			stderrWant: "gc wait inspect:",
		},
		{
			name: "non_wait",
			waitID: func(t *testing.T, store beads.Store) string {
				t.Helper()
				b, err := store.Create(beads.Bead{Title: "not a wait", Type: "task"})
				if err != nil {
					t.Fatalf("Create(non-wait): %v", err)
				}
				return b.ID
			},
			stderrWant: "is not a wait",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, store := setupWaitJSONTestCity(t)
			waitID := tt.waitID(t, store)

			var stdout, stderr bytes.Buffer
			code := run([]string{"wait", "inspect", waitID, "--json"}, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("run(wait inspect %s --json) = 0, want failure; stdout=%q stderr=%q", waitID, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.stderrWant) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.stderrWant)
			}
			var failure jsonSchemaErrorPayload
			if err := json.Unmarshal(stdout.Bytes(), &failure); err != nil {
				t.Fatalf("stdout is not JSON failure: %v\n%s", err, stdout.String())
			}
			if failure.OK || failure.Error.Code != "command_failed" || failure.Error.ExitCode != code {
				t.Fatalf("failure = %+v, exit code %d", failure, code)
			}
		})
	}
}

func TestWaitJSONEncoderErrorsWriteDiagnostics(t *testing.T) {
	var stderr bytes.Buffer
	if code := writeWaitListJSON(failingWriter{}, &stderr, "/city", nil); code != 1 {
		t.Fatalf("writeWaitListJSON = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "gc wait list: encode JSON: write failed") {
		t.Fatalf("stderr = %q", stderr.String())
	}

	stderr.Reset()
	if code := writeWaitInspectJSON(failingWriter{}, &stderr, "/city", sessionpkg.WaitInfo{}); code != 1 {
		t.Fatalf("writeWaitInspectJSON = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "gc wait inspect: encode JSON: write failed") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// TestWaitJSONFromInfo_MatchesBeadProjection locks the schema_version-1 CLI JSON
// contract byte-for-byte across the WaitInfo refactor: a fully-populated wait
// bead projected through the session codec and mapped to waitJSON must equal the
// hand-written literal the inline waitJSONFromBead previously produced.
func TestWaitJSONFromInfo_MatchesBeadProjection(t *testing.T) {
	created := time.Date(2026, 5, 15, 9, 30, 0, 0, time.UTC)
	b := beads.Bead{
		ID:          "gc-wait-1",
		Type:        waitBeadType,
		Status:      "closed",
		Title:       "wait:worker",
		Description: "Continue after review closes.",
		CreatedAt:   created,
		Labels:      []string{waitBeadLabel, "session:gc-session"},
		Metadata: map[string]string{
			"session_id":       "gc-session",
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-1,gc-2",
			"dep_mode":         "all",
			"registered_epoch": "3",
			"delivery_attempt": "2",
			"nudge_id":         "wait-gc-wait-1-3-2",
		},
	}
	got := waitJSONFromInfo(sessionpkg.WaitInfoFromBead(b))
	want := waitJSON{
		ID:              "gc-wait-1",
		SessionID:       "gc-session",
		SessionName:     "worker",
		State:           waitStateReady,
		Kind:            "deps",
		DepIDs:          []string{"gc-1", "gc-2"},
		DepMode:         "all",
		RegisteredEpoch: "3",
		DeliveryAttempt: "2",
		NudgeID:         "wait-gc-wait-1-3-2",
		Note:            "Continue after review closes.",
		Status:          "closed",
		CreatedAt:       created.UTC().Format(time.RFC3339),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("waitJSONFromInfo = %#v, want %#v", got, want)
	}
}

// TestWriteWaitDetail_RendersWaitInfo pins the human wait-inspect render,
// including the comma-joined DepIDs on the Deps line.
func TestWriteWaitDetail_RendersWaitInfo(t *testing.T) {
	w := sessionpkg.WaitInfo{
		ID:              "gc-wait-1",
		SessionID:       "gc-session",
		State:           waitStateReady,
		Kind:            "deps",
		DepIDs:          []string{"a", "b"},
		DepMode:         "all",
		RegisteredEpoch: "3",
		DeliveryAttempt: "2",
		NudgeID:         "wait-gc-wait-1-3-2",
		Note:            "Continue after review closes.",
	}
	var buf bytes.Buffer
	writeWaitDetail(w, &buf)
	want := "Wait:       gc-wait-1\n" +
		"Session:    gc-session\n" +
		"State:      ready\n" +
		"Kind:       deps\n" +
		"Deps:       a,b (all)\n" +
		"Epoch:      3\n" +
		"Attempt:    2\n" +
		"Nudge:      wait-gc-wait-1-3-2\n" +
		"Note:       Continue after review closes.\n"
	if got := buf.String(); got != want {
		t.Fatalf("writeWaitDetail =\n%q\nwant\n%q", got, want)
	}
}

func TestWaitJSONSchemasDoNotExposeRawMetadata(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "schemas", "wait", "list", "result.schema.json"),
		filepath.Join("..", "..", "schemas", "wait", "inspect", "result.schema.json"),
	} {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", path, err)
			}
			if bytes.Contains(data, []byte(`"metadata"`)) {
				t.Fatalf("%s exposes raw metadata:\n%s", path, string(data))
			}
		})
	}
}

type waitListJSONTestPayload struct {
	Waits []struct {
		ID        string `json:"id"`
		SessionID string `json:"session_id"`
		State     string `json:"state"`
	} `json:"waits"`
}

func setupWaitJSONTestCity(t *testing.T) (string, beads.Store) {
	t.Helper()
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeCityToml(t, cityDir, "[workspace]\nname = \"wait-json\"\n")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	return cityDir, store
}

func decodeWaitListJSON(t *testing.T, data []byte) waitListJSONTestPayload {
	t.Helper()
	var payload waitListJSONTestPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, string(data))
	}
	return payload
}

func createTestWaitBead(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	return createTestWaitBeadForSession(t, store, "session-1", waitStatePending)
}

func createTestWaitBeadForSession(t *testing.T, store beads.Store, sessionID, state string) beads.Bead {
	t.Helper()
	wait, err := store.Create(beads.Bead{
		Title:       "wait:demo",
		Type:        waitBeadType,
		Status:      "open",
		Description: "wait for deps",
		Labels:      []string{waitBeadLabel, "session:" + sessionID},
		Metadata: map[string]string{
			"session_id":       sessionID,
			"session_name":     "demo",
			"kind":             "deps",
			"state":            state,
			"dep_ids":          "bead-1,bead-2",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
			"nudge_id":         "nudge-1",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(wait): %v", err)
	}
	return wait
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func (s waitNudgeMetadataFailStore) SetMetadata(id, key, value string) error {
	if key == "nudge_id" {
		return errors.New("set nudge id failed")
	}
	return s.MemStore.SetMetadata(id, key, value)
}

func (s *waitGetSpyStore) Get(id string) (beads.Bead, error) {
	s.getIDs = append(s.getIDs, id)
	return s.Store.Get(id)
}

func (s *waitListQueryCaptureStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, query)
	return s.Store.List(query)
}

func (s waitGlobalListOmitStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == waitBeadLabel {
		return nil, nil
	}
	return s.Store.List(query)
}

func (s waitGlobalListLimitStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == waitBeadLabel {
		return waitLookupLimitStore(s).List(query)
	}
	return s.Store.List(query)
}

func (s waitGlobalListErrorStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == waitBeadLabel {
		return nil, errors.New("global wait list failed")
	}
	return s.Store.List(query)
}

func (s waitOneSessionListLimitStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == waitBeadLabel {
		return nil, nil
	}
	if query.Label == "session:"+s.sessionID {
		return waitLookupLimitStore{Store: s.Store}.List(query)
	}
	return s.Store.List(query)
}

func (s waitLookupLimitStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	items := make([]beads.Bead, query.Limit)
	for i := range items {
		items[i] = beads.Bead{
			ID:     fmt.Sprintf("wait-%d", i),
			Type:   waitBeadType,
			Status: "open",
			Labels: []string{query.Label},
		}
	}
	if len(items) > 0 {
		items[0].Metadata = map[string]string{
			"session_id": "session-1",
			"state":      waitStateReady,
		}
	}
	return items, nil
}

var (
	waitTestRealBDPathOnce sync.Once
	waitTestRealBDCached   string
	waitTestRealBDErr      error

	managedBdWaitTemplateOnce sync.Once //nolint:unused // exercised by native_dolt_rebind_integration_test.go
	managedBdWaitTemplatePath string    //nolint:unused // exercised by native_dolt_rebind_integration_test.go
	managedBdWaitTemplateErr  error     //nolint:unused // exercised by native_dolt_rebind_integration_test.go
)

//nolint:unused // exercised by native_dolt_rebind_integration_test.go
func waitTestEnv(overrides map[string]string) []string {
	env := map[string]string{}
	for _, entry := range sanitizedBaseEnv() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		env[key] = value
	}
	for key, value := range overrides {
		env[key] = value
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

func waitTestRealBDPath(t *testing.T) string {
	t.Helper()
	skipSlowCmdGCTest(t, "requires a managed bd lifecycle city; run make test-cmd-gc-process for full coverage")
	waitTestRealBDPathOnce.Do(func() {
		waitTestRealBDCached, waitTestRealBDErr = buildPinnedBDBinaryForTests()
	})
	if waitTestRealBDErr != nil {
		t.Fatalf("build pinned bd test binary: %v", waitTestRealBDErr)
	}
	return waitTestRealBDCached
}

// buildPinnedBDBinaryForTests builds the bd CLI from the exact
// github.com/steveyegge/beads module version this repo's go.mod requires, so
// the binary's compiled-in schema/migration knowledge always matches
// gascity's own in-process beads code (internal/beads imports that same
// module directly). A bd resolved by searching PATH/home-dir locations
// instead (as findPreferredBinary does for callers that only need some bd
// present) carries no such guarantee: it can drift to a different schema
// version and fail deep inside a test with a cryptic mismatch error instead
// of cleanly at the point the drift actually originates (ga-r9cvmi).
//
// go install's "@version" form deliberately ignores any enclosing module's
// go.mod/go.sum and resolves the target module's own dependency closure in
// isolation, which is required here: cmd/bd's full dependency graph (CLI
// extras like AI-assisted duplicate detection, ADO rich-text rendering,
// telemetry exporters) is broader than what gascity's own go.sum carries,
// since gascity only imports internal/beads's storage packages.
func buildPinnedBDBinaryForTests() (string, error) {
	version, err := pinnedBeadsModuleVersion()
	if err != nil {
		return "", fmt.Errorf("resolve pinned beads module version: %w", err)
	}

	sweepOrphanPIDPrefixedDirs(os.TempDir(), testBDBinaryDirPrefix)
	buildDir, err := os.MkdirTemp("", pidPrefixedTempPattern(testBDBinaryDirPrefix))
	if err != nil {
		return "", fmt.Errorf("mktemp bd binary dir: %w", err)
	}

	cmd := exec.Command("go", "install", "-tags", "gms_pure_go",
		"github.com/steveyegge/beads/cmd/bd@"+version)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOBIN="+buildDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go install github.com/steveyegge/beads/cmd/bd@%s: %w\n%s", version, err, out)
	}
	return filepath.Join(buildDir, "bd"), nil
}

// pinnedBeadsModuleVersion reports the github.com/steveyegge/beads version
// this test binary was actually built against, read from this process's own
// embedded build info rather than a `go list -m` subprocess or a go.mod text
// scan: debug.ReadBuildInfo reflects the exact resolved dependency graph
// (including any replace/exclude directives) with zero process spawn, and it
// can never itself drift from go.mod the way a second hardcoded version
// string could, since the compiler stamps it in at build time.
func pinnedBeadsModuleVersion() (string, error) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "", fmt.Errorf("read build info: not available (binary not built with module support)")
	}
	for _, dep := range bi.Deps {
		if dep.Path != "github.com/steveyegge/beads" {
			continue
		}
		if dep.Replace != nil {
			return dep.Replace.Version, nil
		}
		return dep.Version, nil
	}
	return "", fmt.Errorf("github.com/steveyegge/beads not found in build info deps")
}

// TestBuildPinnedBDBinaryForTestsUsesGoModSource locks in the fix for
// ga-r9cvmi: a bd binary resolved by searching PATH/home-dir locations (the
// old waitTestRealBDPath behavior, still used elsewhere via
// findPreferredBinary) carries no guarantee of matching the schema/migration
// knowledge baked into gascity's own in-process beads code, which is compiled
// from the exact github.com/steveyegge/beads version go.mod pins. Confirmed
// live: the same ~/.local/bin/bd path reported two different version stamps
// across two consecutive invocations in this same fleet sandbox, and
// ga-r9cvmi's own notes captured a deterministic v49-vs-v53 schema mismatch
// from that ambient drift. buildPinnedBDBinaryForTests must instead build bd
// fresh from the pinned dependency, so its correctness never depends on
// whatever happens to be installed on the host.
func TestBuildPinnedBDBinaryForTestsUsesGoModSource(t *testing.T) {
	// Load-bearing for the census even though waitTestRealBDPath calls it
	// again: this is the cmd/gc+untagged slow_process_gate call site the
	// 57 -> 58 bump accounts for across census.go, test-resources.toml, and
	// TESTING.md. Deleting it as redundant fails the ledger gate.
	skipSlowCmdGCTest(t, "builds a real bd binary from source; run make test-cmd-gc-process for full coverage")

	// Route through waitTestRealBDPath so this shares waitTestRealBDPathOnce
	// with the other bd-consuming tests. Calling buildPinnedBDBinaryForTests
	// directly builds a second ~91 MB binary, and leaks a second temp dir, in
	// any shard that also holds a waitTestRealBDPath caller.
	bdPath := waitTestRealBDPath(t)

	pinned, err := pinnedBeadsModuleVersion()
	if err != nil {
		t.Fatalf("pinnedBeadsModuleVersion: %v", err)
	}
	out, err := exec.Command(bdPath, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s version: %v\n%s", bdPath, err, out)
	}
	versionLine := ""
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "bd version ") {
			versionLine = line
			break
		}
	}
	fields := strings.Fields(versionLine)
	if len(fields) < 3 || !semver.IsValid("v"+fields[2]) {
		t.Fatalf("%s version output %q does not report a declared Beads release version", bdPath, out)
	}
	metadata, err := exec.Command("go", "version", "-m", bdPath).CombinedOutput()
	if err != nil {
		t.Fatalf("go version -m %s: %v\n%s", bdPath, err, metadata)
	}
	foundPinnedModule := false
	for _, line := range strings.Split(string(metadata), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "mod" && fields[1] == "github.com/steveyegge/beads" && fields[2] == pinned {
			foundPinnedModule = true
			break
		}
	}
	if !foundPinnedModule {
		t.Fatalf("%s build metadata %q does not retain pinned Beads module version %q", bdPath, metadata, pinned)
	}
	// `bd version` reports the release variable declared by Beads source
	// (currently 1.1.0), not the Go module pseudo-version used to fetch that
	// source. The exact source guarantee is therefore checked through the
	// compiled binary's module metadata above.
}

func TestLoadWaitBeadsByLabelUsesBoundedLookup(t *testing.T) {
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{
		Title:  "wait",
		Type:   sessionpkg.LegacyWaitBeadType,
		Labels: []string{waitBeadLabel},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	store := &waitListQueryCaptureStore{Store: mem}

	waits, err := sessionFrontDoor(store).ListWaits("", "")
	if err != nil {
		t.Fatalf("loadWaitsByLabel: %v", err)
	}
	if len(waits) != 1 {
		t.Fatalf("wait count = %d, want 1", len(waits))
	}
	if len(store.queries) != 1 {
		t.Fatalf("List calls = %d, want 1", len(store.queries))
	}
	if got := store.queries[0].Limit; got != waitLookupLimit+1 {
		t.Fatalf("List limit = %d, want %d", got, waitLookupLimit+1)
	}
	if got := store.queries[0].Sort; got != beads.SortCreatedDesc {
		t.Fatalf("List sort = %q, want %q", got, beads.SortCreatedDesc)
	}
}

func TestLoadWaitBeadsByLabelAllowsExactLookupLimit(t *testing.T) {
	mem := beads.NewMemStore()
	for i := 0; i < waitLookupLimit; i++ {
		if _, err := mem.Create(beads.Bead{
			Title:  fmt.Sprintf("wait-%d", i),
			Type:   waitBeadType,
			Labels: []string{waitBeadLabel},
		}); err != nil {
			t.Fatalf("create wait bead %d: %v", i, err)
		}
	}

	waits, err := sessionFrontDoor(mem).ListWaits("", "")
	if err != nil {
		t.Fatalf("loadWaitsByLabel: %v", err)
	}
	if len(waits) != waitLookupLimit {
		t.Fatalf("wait count = %d, want %d", len(waits), waitLookupLimit)
	}
}

func TestLoadWaitBeadsByLabelReportsLookupLimit(t *testing.T) {
	_, err := sessionFrontDoor(waitLookupLimitStore{Store: beads.NewMemStore()}).ListWaits("", "")
	if err == nil || !strings.Contains(err.Error(), "wait lookup hit limit") {
		t.Fatalf("loadWaitsByLabel error = %v, want wait lookup limit", err)
	}
}

func TestDoWaitListFromSessionStoreUsesSessionScopedLookup(t *testing.T) {
	mem := beads.NewMemStore()
	targetWait := createTestWaitBeadForSession(t, mem, "target-session", waitStatePending)
	otherWait := createTestWaitBeadForSession(t, mem, "other-session", waitStatePending)
	store := &waitListQueryCaptureStore{Store: mem}

	var stdout, stderr bytes.Buffer
	code := doWaitListFromSessionStore(sessionFrontDoor(store), "/test/city", "", "target-session", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doWaitListFromSessionStore = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), targetWait.ID) {
		t.Fatalf("wait list output missing target wait %s:\nstdout=%s\nstderr=%s", targetWait.ID, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), otherWait.ID) {
		t.Fatalf("wait list output included non-target wait %s:\n%s", otherWait.ID, stdout.String())
	}
	if len(store.queries) != 1 {
		t.Fatalf("List calls = %d, want 1; queries=%#v", len(store.queries), store.queries)
	}
	wantQuery := beads.ListQuery{
		Status: "open",
		Label:  "session:target-session",
		Limit:  waitLookupLimit + 1,
		Sort:   beads.SortCreatedDesc,
	}
	if !reflect.DeepEqual(store.queries[0], wantQuery) {
		t.Fatalf("List query = %#v, want %#v", store.queries[0], wantQuery)
	}
}

func TestReadyWaitSetForList_ReturnsSetAndCapError(t *testing.T) {
	ready, err := readyWaitSetForList(sessionFrontDoor(waitGlobalListLimitStore{Store: beads.NewMemStore()}))
	if err == nil || !strings.Contains(err.Error(), "wait lookup hit limit") {
		t.Fatalf("readyWaitSetForList error = %v, want wait lookup limit", err)
	}
	if !ready["session-1"] {
		t.Fatalf("readyWaitSetForList ready = %#v, want session-1 despite cap warning", ready)
	}
}

func writeWaitTestDoltIdentity(homeDir string) error {
	if err := os.MkdirAll(filepath.Join(homeDir, ".dolt"), 0o755); err != nil {
		return err
	}
	doltConfig := `{"user.name":"gc-test","user.email":"gc-test@example.com"}`
	return os.WriteFile(filepath.Join(homeDir, ".dolt", "config_global.json"), []byte(doltConfig), 0o644)
}

func writeManagedBdWaitTestCityScaffold(cityPath string) (string, error) {
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		return "", err
	}
	cityToml := `[workspace]
name = "gascity"
prefix = "gc"

[beads]
provider = "bd"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		return "", err
	}
	return rigPath, nil
}

//nolint:unused // exercised by native_dolt_rebind_integration_test.go
func managedBdWaitTestTemplate(t *testing.T, bdPath, doltPath string) string {
	t.Helper()
	managedBdWaitTemplateOnce.Do(func() {
		cityPath, err := os.MkdirTemp("/tmp", "gc-bd-template-city-")
		if err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("MkdirTemp(template city): %w", err)
			return
		}
		rigPath, err := writeManagedBdWaitTestCityScaffold(cityPath)
		if err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("write template scaffold: %w", err)
			return
		}
		if err := EnsureBuiltinRuntimeAssets(cityPath, io.Discard); err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("EnsureBuiltinRuntimeAssets(template): %w", err)
			return
		}
		script := gcBeadsBdScriptPath(cityPath)
		homeDir, err := os.MkdirTemp("/tmp", "gc-bd-template-home-")
		if err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("MkdirTemp(template home): %w", err)
			return
		}
		if err := writeWaitTestDoltIdentity(homeDir); err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("write template dolt identity: %w", err)
			return
		}
		env := waitTestEnv(map[string]string{
			"GC_BEADS":       "bd",
			"GC_DOLT":        "",
			"GC_BIN":         currentGCBinaryForTests(t),
			"GC_CITY":        cityPath,
			"GC_CITY_PATH":   cityPath,
			"HOME":           homeDir,
			"DOLT_ROOT_PATH": homeDir,
			"PATH":           strings.Join([]string{filepath.Dir(bdPath), filepath.Dir(doltPath), os.Getenv("PATH")}, string(os.PathListSeparator)),
		})
		runScript := func(args ...string) error {
			cmd := exec.Command(script, args...)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %w\n%s", strings.Join(args, " "), err, out)
			}
			return nil
		}
		if err := runScript("start"); err != nil {
			managedBdWaitTemplateErr = err
			return
		}
		if err := runScript("init", cityPath, "gc", "hq"); err != nil {
			managedBdWaitTemplateErr = err
			return
		}
		if err := runScript("init", rigPath, "fe", "fe"); err != nil {
			managedBdWaitTemplateErr = err
			return
		}
		stopCmd := exec.Command(script, "stop")
		stopCmd.Env = env
		if out, err := stopCmd.CombinedOutput(); err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("stop template city: %w\n%s", err, out)
			return
		}
		if err := clearManagedDoltRuntimeState(cityPath); err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("clear published dolt runtime state: %w", err)
			return
		}
		if err := removeDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath)); err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("remove provider dolt runtime state: %w", err)
			return
		}
		if err := os.RemoveAll(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")); err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("remove template runtime pack state: %w", err)
			return
		}
		removeDoltPortFile(cityPath)
		removeDoltPortFile(rigPath)
		managedBdWaitTemplatePath = cityPath
	})
	if managedBdWaitTemplateErr != nil {
		t.Fatal(managedBdWaitTemplateErr)
	}
	return managedBdWaitTemplatePath
}

func (s waitErrorStore) ListByLabel(label string, limit int, _ ...beads.QueryOpt) ([]beads.Bead, error) {
	if label == waitBeadLabel {
		return nil, errors.New("wait list failed")
	}
	return s.MemStore.ListByLabel(label, limit)
}

func (s waitErrorStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == waitBeadLabel || strings.HasPrefix(query.Label, "session:") {
		return nil, errors.New("wait list failed")
	}
	return s.MemStore.List(query)
}

func TestPrepareWaitWakeState_MarksDepsReady(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"provider":           "codex",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	dep, err := store.Create(beads.Bead{Title: "dep"})
	if err != nil {
		t.Fatalf("create dep bead: %v", err)
	}
	if err := store.Close(dep.ID); err != nil {
		t.Fatalf("close dep bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStatePending,
			"dep_ids":          dep.ID,
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if !readyWaitSet[sessionBead.ID] {
		t.Fatalf("readyWaitSet missing session %s", sessionBead.ID)
	}
	updated, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updated.Metadata["state"]; got != waitStateReady {
		t.Fatalf("wait state = %q, want %q", got, waitStateReady)
	}
	if updated.Metadata["ready_at"] == "" {
		t.Fatal("ready_at was not recorded")
	}
}

func TestPrepareWaitWakeState_CancelsWaitForClosedSession(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"provider":           "codex",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := store.Close(sessionBead.ID); err != nil {
		t.Fatalf("close session bead: %v", err)
	}
	dep, err := store.Create(beads.Bead{Title: "dep"})
	if err != nil {
		t.Fatalf("create dep bead: %v", err)
	}
	if err := store.Close(dep.ID); err != nil {
		t.Fatalf("close dep bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          dep.ID,
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if readyWaitSet[sessionBead.ID] {
		t.Fatalf("readyWaitSet unexpectedly contains closed session %s", sessionBead.ID)
	}
	updated, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if updated.Status != "closed" {
		t.Fatalf("wait status = %q, want closed", updated.Status)
	}
	if got := updated.Metadata["state"]; got != waitStateCanceled {
		t.Fatalf("wait state = %q, want %q", got, waitStateCanceled)
	}
	if got := updated.Metadata["last_error"]; got != "session-closed" {
		t.Fatalf("wait last_error = %q, want session-closed", got)
	}
}

func TestPrepareWaitWakeState_FailsMissingDependencyWait(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"wait_hold":          "true",
			"sleep_reason":       "wait-hold",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStatePending,
			"dep_ids":          "gc-missing",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if readyWaitSet[sessionBead.ID] {
		t.Fatalf("readyWaitSet unexpectedly contains session %s", sessionBead.ID)
	}

	updatedWait, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updatedWait.Metadata["state"]; got != waitStateFailed {
		t.Fatalf("wait state = %q, want %q", got, waitStateFailed)
	}
	if updatedWait.Status != "closed" {
		t.Fatalf("wait status = %q, want closed", updatedWait.Status)
	}
	if updatedWait.Metadata["failed_at"] == "" {
		t.Fatal("failed_at was not recorded")
	}
	if updatedWait.Metadata["last_error"] == "" {
		t.Fatal("last_error was not recorded")
	}

	updatedSession, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("store.Get(session): %v", err)
	}
	if updatedSession.Metadata["wait_hold"] != "" {
		t.Fatalf("wait_hold = %q, want cleared", updatedSession.Metadata["wait_hold"])
	}
	if updatedSession.Metadata["sleep_reason"] != "" {
		t.Fatalf("sleep_reason = %q, want cleared", updatedSession.Metadata["sleep_reason"])
	}
}

func TestPrepareWaitWakeState_FinalizesFromNudge(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-1",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	nudgeID := waitNudgeID(sessionpkg.WaitInfoFromBead(waitBead))
	nudge, err := store.Create(beads.Bead{
		Type:   nudgeBeadType,
		Title:  "nudge:" + nudgeID,
		Labels: []string{nudgeBeadLabel, "nudge:" + nudgeID},
		Metadata: map[string]string{
			"nudge_id":           nudgeID,
			"state":              "injected",
			"commit_boundary":    "provider-nudge-return",
			"terminal_reason":    "",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	if err := store.Close(nudge.ID); err != nil {
		t.Fatalf("close nudge bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if readyWaitSet[sessionBead.ID] {
		t.Fatalf("session %s should not remain in ready set after terminal nudge", sessionBead.ID)
	}
	updated, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updated.Metadata["state"]; got != waitStateClosed {
		t.Fatalf("wait state = %q, want %q", got, waitStateClosed)
	}
	if updated.Status != "closed" {
		t.Fatalf("wait status = %q, want closed", updated.Status)
	}
}

func TestPrepareWaitWakeState_UsesTargetedLookupForMissingSessionEpoch(t *testing.T) {
	base := beads.NewMemStore()
	store := &waitGetSpyStore{Store: base}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"state":              string(sessionpkg.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := store.Close(sessionBead.ID); err != nil {
		t.Fatalf("close session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"registered_epoch": "1",
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if len(readyWaitSet) != 0 {
		t.Fatalf("readyWaitSet = %#v, want empty for non-open session", readyWaitSet)
	}
	if len(store.getIDs) != 1 || store.getIDs[0] != sessionBead.ID {
		t.Fatalf("Get IDs = %v, want targeted lookup for %s", store.getIDs, sessionBead.ID)
	}
}

func TestPrepareWaitWakeState_SkipsMissingOpenSessionWithoutEpochLookup(t *testing.T) {
	base := beads.NewMemStore()
	store := &waitGetSpyStore{Store: base}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker",
			"agent_name":   "worker",
			"state":        string(sessionpkg.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := store.Close(sessionBead.ID); err != nil {
		t.Fatalf("close session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":   sessionBead.ID,
			"session_name": "worker",
			"kind":         "deps",
			"state":        waitStateReady,
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if len(readyWaitSet) != 0 {
		t.Fatalf("readyWaitSet = %#v, want empty for non-open session", readyWaitSet)
	}
	if len(store.getIDs) != 0 {
		t.Fatalf("Get IDs = %v, want no closed-session lookup without an epoch", store.getIDs)
	}
}

func TestPrepareWaitWakeState_CancelsStaleEpochWaitForClosedSession(t *testing.T) {
	base := beads.NewMemStore()
	store := &waitGetSpyStore{Store: base}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "2",
			"state":              string(sessionpkg.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := store.Close(sessionBead.ID); err != nil {
		t.Fatalf("close session bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"registered_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if len(readyWaitSet) != 0 {
		t.Fatalf("readyWaitSet = %#v, want empty after stale wait cancellation", readyWaitSet)
	}
	updated, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updated.Metadata["state"]; got != waitStateCanceled {
		t.Fatalf("wait state = %q, want %q", got, waitStateCanceled)
	}
	if got := updated.Metadata["last_error"]; got != "continuation-stale" {
		t.Fatalf("last_error = %q, want continuation-stale", got)
	}
	if updated.Status != "closed" {
		t.Fatalf("wait status = %q, want closed", updated.Status)
	}
}

func TestPrepareWaitWakeState_ProcessesOpenSessionWaitsWithoutGlobalWaitList(t *testing.T) {
	base := beads.NewMemStore()
	store := waitGlobalListOmitStore{Store: base}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	dep, err := store.Create(beads.Bead{Title: "dep"})
	if err != nil {
		t.Fatalf("create dep bead: %v", err)
	}
	if err := store.Close(dep.ID); err != nil {
		t.Fatalf("close dep bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStatePending,
			"dep_ids":          dep.ID,
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if !readyWaitSet[sessionBead.ID] {
		t.Fatalf("readyWaitSet missing session %s", sessionBead.ID)
	}
	updated, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updated.Metadata["state"]; got != waitStateReady {
		t.Fatalf("wait state = %q, want %q", got, waitStateReady)
	}
}

func TestPrepareWaitWakeState_ContinuesWhenGlobalListCaps(t *testing.T) {
	base := beads.NewMemStore()
	store := waitGlobalListLimitStore{Store: base}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"state":              string(sessionpkg.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	dep, err := store.Create(beads.Bead{Title: "dep"})
	if err != nil {
		t.Fatalf("create dep bead: %v", err)
	}
	if err := store.Close(dep.ID); err != nil {
		t.Fatalf("close dep bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStatePending,
			"dep_ids":          dep.ID,
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if !readyWaitSet[sessionBead.ID] {
		t.Fatalf("readyWaitSet missing session %s", sessionBead.ID)
	}
	updated, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updated.Metadata["state"]; got != waitStateReady {
		t.Fatalf("wait state = %q, want %q", got, waitStateReady)
	}
	updatedSession, err := base.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("store.Get(session): %v", err)
	}
	if got := updatedSession.Metadata["wait_lookup_capped_label"]; got != waitBeadLabel {
		t.Fatalf("wait_lookup_capped_label = %q, want %q", got, waitBeadLabel)
	}
	if got := updatedSession.Metadata["wait_lookup_capped_limit"]; got != fmt.Sprint(waitLookupLimit) {
		t.Fatalf("wait_lookup_capped_limit = %q, want %d", got, waitLookupLimit)
	}
	if got := updatedSession.Metadata["wait_lookup_capped_source"]; got != "wake-state-global" {
		t.Fatalf("wait_lookup_capped_source = %q, want wake-state-global", got)
	}
	if got := updatedSession.Metadata["wait_lookup_capped_at"]; got == "" {
		t.Fatal("wait_lookup_capped_at empty, want structured global cap diagnostic timestamp")
	}
}

func TestPrepareWaitWakeState_ContinuesWhenOneSessionLookupCaps(t *testing.T) {
	base := beads.NewMemStore()
	cappedSession, err := base.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "capped",
			"agent_name":         "capped",
			"continuation_epoch": "1",
			"state":              string(sessionpkg.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("create capped session bead: %v", err)
	}
	sessionBead, err := base.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"state":              string(sessionpkg.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	dep, err := base.Create(beads.Bead{Title: "dep"})
	if err != nil {
		t.Fatalf("create dep bead: %v", err)
	}
	if err := base.Close(dep.ID); err != nil {
		t.Fatalf("close dep bead: %v", err)
	}
	waitBead, err := base.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStatePending,
			"dep_ids":          dep.ID,
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	store := waitOneSessionListLimitStore{Store: base, sessionID: cappedSession.ID}

	readyWaitSet, err := prepareWaitWakeState(store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if !readyWaitSet[sessionBead.ID] {
		t.Fatalf("readyWaitSet missing session %s after capped session %s", sessionBead.ID, cappedSession.ID)
	}
	updated, err := base.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updated.Metadata["state"]; got != waitStateReady {
		t.Fatalf("wait state = %q, want %q", got, waitStateReady)
	}
	updatedCapped, err := base.Get(cappedSession.ID)
	if err != nil {
		t.Fatalf("store.Get(capped session): %v", err)
	}
	if got := updatedCapped.Metadata["wait_lookup_capped_label"]; got != "session:"+cappedSession.ID {
		t.Fatalf("wait_lookup_capped_label = %q, want session label", got)
	}
	if got := updatedCapped.Metadata["wait_lookup_capped_limit"]; got != "1000" {
		t.Fatalf("wait_lookup_capped_limit = %q, want 1000", got)
	}
	if got := updatedCapped.Metadata["wait_lookup_capped_source"]; got != "wake-state-session" {
		t.Fatalf("wait_lookup_capped_source = %q, want wake-state-session", got)
	}
	if got := updatedCapped.Metadata["wait_lookup_capped_at"]; got == "" {
		t.Fatal("wait_lookup_capped_at empty, want structured cap diagnostic timestamp")
	}
}

func TestPrepareWaitWakeState_PropagatesGlobalListError(t *testing.T) {
	base := beads.NewMemStore()
	store := waitGlobalListErrorStore{Store: base}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"state":              string(sessionpkg.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStatePending,
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	_, err = prepareWaitWakeState(store, time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "global wait list failed") {
		t.Fatalf("prepareWaitWakeState error = %v, want global wait list failed", err)
	}
}

func TestDepsWaitReady_IgnoresEmptyDependencyEntries(t *testing.T) {
	store := beads.NewMemStore()
	dep, err := store.Create(beads.Bead{Title: "dep"})
	if err != nil {
		t.Fatalf("create dep bead: %v", err)
	}
	if err := store.Close(dep.ID); err != nil {
		t.Fatalf("close dep bead: %v", err)
	}

	ready := depsWaitReady(store, sessionpkg.WaitInfoFromBead(beads.Bead{
		Metadata: map[string]string{
			"dep_ids":  dep.ID + ", ,",
			"dep_mode": "all",
		},
	}))
	if !ready {
		t.Fatal("depsWaitReady = false, want true with only one real closed dependency")
	}
}

func TestNextWaitDeliveryAttempt_IncrementsAfterTerminalNudge(t *testing.T) {
	store := beads.NewMemStore()
	wait, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel},
		Metadata: map[string]string{
			"state":            waitStateFailed,
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	nudgeID := waitNudgeID(sessionpkg.WaitInfoFromBead(wait))
	nudge, err := store.Create(beads.Bead{
		Type:   nudgeBeadType,
		Title:  "nudge:" + nudgeID,
		Labels: []string{nudgeBeadLabel, "nudge:" + nudgeID},
		Metadata: map[string]string{
			"nudge_id": nudgeID,
			"state":    "failed",
		},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	if err := store.Close(nudge.ID); err != nil {
		t.Fatalf("close nudge bead: %v", err)
	}

	next, err := nextWaitDeliveryAttempt(nudgeFrontDoor(beads.NudgesStore{Store: store}), sessionpkg.WaitInfoFromBead(wait))
	if err != nil {
		t.Fatalf("nextWaitDeliveryAttempt: %v", err)
	}
	if next != "2" {
		t.Fatalf("nextWaitDeliveryAttempt = %q, want 2", next)
	}
}

func TestDispatchReadyWaitNudges_EnqueuesDeterministicNudge(t *testing.T) {
	setWaitTestFileBeads(t)
	dir := t.TempDir()
	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:        waitBeadType,
		Labels:      []string{waitBeadLabel, "session:" + sessionBead.ID},
		Description: "Continue after review closes.",
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-1",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC()); err != nil {
		t.Fatalf("dispatchReadyWaitNudges: %v", err)
	}
	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now().UTC())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending=%d inFlight=%d dead=%d, want 1/0/0", len(pending), len(inFlight), len(dead))
	}
	wantID := waitNudgeID(sessionpkg.WaitInfoFromBead(waitBead))
	if pending[0].ID != wantID {
		t.Fatalf("queued nudge id = %q, want %q", pending[0].ID, wantID)
	}
	if pending[0].SessionID != sessionBead.ID {
		t.Fatalf("queued nudge session_id = %q, want %q", pending[0].SessionID, sessionBead.ID)
	}
	if pending[0].Reference == nil || pending[0].Reference.ID != waitBead.ID {
		t.Fatalf("queued nudge reference = %#v, want wait bead %s", pending[0].Reference, waitBead.ID)
	}
	if pending[0].BeadID == "" {
		t.Fatal("queued nudge bead_id is empty")
	}
	refreshedStore, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatalf("openCityStoreAt(refresh): %v", err)
	}
	if _, err := refreshedStore.Get(pending[0].BeadID); err != nil {
		t.Fatalf("refreshedStore.Get(%s): %v", pending[0].BeadID, err)
	}
}

func TestDispatchReadyWaitNudges_UsesOpenSessionSnapshotInsteadOfWorkerRunningCheck(t *testing.T) {
	setWaitTestFileBeads(t)
	dir := t.TempDir()
	base := beads.NewMemStore()
	store := &waitGetSpyStore{Store: base}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"template":           "worker",
			"continuation_epoch": "1",
			"state":              string(sessionpkg.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:        waitBeadType,
		Labels:      []string{waitBeadLabel, "session:" + sessionBead.ID},
		Description: "Continue after review closes.",
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-1",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	sp := runtime.NewFake()

	if err := dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC()); err != nil {
		t.Fatalf("dispatchReadyWaitNudges: %v", err)
	}
	for _, id := range store.getIDs {
		if id == sessionBead.ID {
			t.Fatalf("dispatch used Get for session %s instead of the open-session snapshot; getIDs=%v", sessionBead.ID, store.getIDs)
		}
	}
	for _, call := range sp.Calls {
		switch call.Method {
		case "IsRunning", "ProcessAlive", "IsAttached", "GetLastActivity", "GetMeta":
			t.Fatalf("dispatch should trust cached session state, saw provider call %#v", call)
		}
	}
}

func TestDispatchReadyWaitNudges_ProcessesOpenSessionWaitsWithoutGlobalWaitList(t *testing.T) {
	setWaitTestFileBeads(t)
	dir := t.TempDir()
	base := beads.NewMemStore()
	store := waitGlobalListOmitStore{Store: base}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"state":              string(sessionpkg.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:        waitBeadType,
		Labels:      []string{waitBeadLabel, "session:" + sessionBead.ID},
		Description: "Continue after review closes.",
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	sp := runtime.NewFake()

	if err := dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC()); err != nil {
		t.Fatalf("dispatchReadyWaitNudges: %v", err)
	}
	pending, _, _, err := listQueuedNudges(dir, "worker", time.Now().UTC())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != waitNudgeID(sessionpkg.WaitInfoFromBead(waitBead)) {
		t.Fatalf("pending nudges = %#v, want one wait nudge %q", pending, waitNudgeID(sessionpkg.WaitInfoFromBead(waitBead)))
	}
}

func TestDispatchReadyWaitNudges_ContinuesWhenOneSessionLookupCaps(t *testing.T) {
	setWaitTestFileBeads(t)
	dir := t.TempDir()
	base := beads.NewMemStore()
	cappedSession, err := base.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "capped",
			"agent_name":         "capped",
			"continuation_epoch": "1",
			"state":              string(sessionpkg.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("create capped session bead: %v", err)
	}
	sessionBead, err := base.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"state":              string(sessionpkg.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	waitBead, err := base.Create(beads.Bead{
		Type:        waitBeadType,
		Labels:      []string{waitBeadLabel, "session:" + sessionBead.ID},
		Description: "Continue after review closes.",
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	store := waitOneSessionListLimitStore{Store: base, sessionID: cappedSession.ID}

	if err := dispatchReadyWaitNudges(dir, store, runtime.NewFake(), time.Now().UTC()); err != nil {
		t.Fatalf("dispatchReadyWaitNudges: %v", err)
	}
	updated, err := base.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if updated.Metadata["nudge_id"] == "" {
		t.Fatal("wait nudge_id empty, want dispatch for uncapped session")
	}
}

func TestDispatchReadyWaitNudges_SkipsClosedSessionWithoutBackingGet(t *testing.T) {
	setWaitTestFileBeads(t)
	dir := t.TempDir()
	base := beads.NewMemStore()
	store := &waitGetSpyStore{Store: base}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"template":           "worker",
			"continuation_epoch": "1",
			"state":              string(sessionpkg.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := store.Close(sessionBead.ID); err != nil {
		t.Fatalf("close session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	sp := runtime.NewFake()

	if err := dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC()); err != nil {
		t.Fatalf("dispatchReadyWaitNudges: %v", err)
	}
	for _, id := range store.getIDs {
		if id == sessionBead.ID {
			t.Fatalf("dispatch used Get for closed session %s; getIDs=%v", sessionBead.ID, store.getIDs)
		}
	}
	if len(sp.Calls) != 0 {
		t.Fatalf("dispatch should not query provider for a session absent from the open-session snapshot, calls=%#v", sp.Calls)
	}
}

func TestDispatchReadyWaitNudges_StartsCodexPoller(t *testing.T) {
	setWaitTestFileBeads(t)
	dir := t.TempDir()
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"provider":           "codex",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-1",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	called := false
	prev := startNudgePoller
	startNudgePoller = func(cityPath, agentName, sessionName string) error {
		called = true
		if cityPath != dir || agentName != sessionBead.ID || sessionName != "worker" {
			t.Fatalf("unexpected poller args city=%q agent=%q session=%q", cityPath, agentName, sessionName)
		}
		return nil
	}
	t.Cleanup(func() { startNudgePoller = prev })

	if err := dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC()); err != nil {
		t.Fatalf("dispatchReadyWaitNudges: %v", err)
	}
	if !called {
		t.Fatal("startNudgePoller was not called")
	}
}

func TestDispatchReadyWaitNudges_StartsPiPoller(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"provider":           "pi",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-1",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	called := false
	prev := startNudgePoller
	startNudgePoller = func(cityPath, agentName, sessionName string) error {
		called = true
		if cityPath != dir || agentName != sessionBead.ID || sessionName != "worker" {
			t.Fatalf("unexpected poller args city=%q agent=%q session=%q", cityPath, agentName, sessionName)
		}
		return nil
	}
	t.Cleanup(func() { startNudgePoller = prev })

	if err := dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC()); err != nil {
		t.Fatalf("dispatchReadyWaitNudges: %v", err)
	}
	if !called {
		t.Fatal("startNudgePoller was not called")
	}
}

func TestDispatchReadyWaitNudges_PropagatesNudgeIDMetadataFailure(t *testing.T) {
	setWaitTestFileBeads(t)
	dir := t.TempDir()
	store := waitNudgeMetadataFailStore{MemStore: beads.NewMemStore()}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-1",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	err = dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "setting wait nudge_id") {
		t.Fatalf("dispatchReadyWaitNudges error = %v, want nudge_id failure", err)
	}
}

func TestDispatchReadyWaitNudges_PropagatesPollerFailure(t *testing.T) {
	setWaitTestFileBeads(t)
	dir := t.TempDir()
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"provider":           "codex",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-1",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	prev := startNudgePoller
	startNudgePoller = func(_, _, _ string) error {
		return errors.New("poller failed")
	}
	t.Cleanup(func() { startNudgePoller = prev })

	err = dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "starting wait nudge poller") {
		t.Fatalf("dispatchReadyWaitNudges error = %v, want poller failure", err)
	}
}

func TestWithdrawQueuedWaitNudges_RemovesQueuedNudge(t *testing.T) {
	setWaitTestFileBeads(t)
	dir := t.TempDir()
	item := newQueuedNudgeWithOptions("worker", "Wait satisfied.", "wait", time.Now().Add(-time.Minute), queuedNudgeOptions{
		ID:        "wait-gc-1-1-1",
		Reference: &nudgeReference{Kind: "bead", ID: "gc-1"},
	})
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	if err := withdrawQueuedWaitNudges(dir, []string{item.ID}); err != nil {
		t.Fatalf("withdrawQueuedWaitNudges: %v", err)
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending=%d inFlight=%d dead=%d, want all zero", len(pending), len(inFlight), len(dead))
	}

	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	nudge, ok, err := nudgeFrontDoor(beads.NudgesStore{Store: store}).FindIncludingTerminal(item.ID)
	if err != nil {
		t.Fatalf("nudgeFrontDoor.FindIncludingTerminal: %v", err)
	}
	if !ok {
		t.Fatal("nudgeFrontDoor.FindIncludingTerminal returned not found")
	}
	if nudge.Open {
		t.Fatalf("nudge open = true, want closed/terminal")
	}
	if nudge.TerminalReason != "wait-canceled" {
		t.Fatalf("terminal_reason = %q, want wait-canceled", nudge.TerminalReason)
	}
}

func TestCancelWaitsForSession(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id": sessionBead.ID,
			"state":      waitStatePending,
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	if err := cancelWaitsForSession(sessionFrontDoor(store), sessionBead.ID); err != nil {
		t.Fatalf("cancelWaitsForSession: %v", err)
	}
	updated, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updated.Metadata["state"]; got != waitStateCanceled {
		t.Fatalf("wait state = %q, want %q", got, waitStateCanceled)
	}
	if updated.Status != "closed" {
		t.Fatalf("wait status = %q, want closed", updated.Status)
	}
}

func TestCancelWaitsForSessionReturnsNilAfterCappedConvergence(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	waitIDs := make([]string, 0, sessionpkg.SessionWaitLookupLimit+1)
	for i := 0; i < sessionpkg.SessionWaitLookupLimit+1; i++ {
		waitBead, err := store.Create(beads.Bead{
			Type:   waitBeadType,
			Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
			Metadata: map[string]string{
				"session_id": sessionBead.ID,
				"state":      waitStatePending,
			},
		})
		if err != nil {
			t.Fatalf("create wait bead %d: %v", i, err)
		}
		waitIDs = append(waitIDs, waitBead.ID)
	}

	if err := cancelWaitsForSession(sessionFrontDoor(store), sessionBead.ID); err != nil {
		t.Fatalf("cancelWaitsForSession: %v", err)
	}
	for _, id := range waitIDs {
		updated, err := store.Get(id)
		if err != nil {
			t.Fatalf("store.Get(%s): %v", id, err)
		}
		if got := updated.Metadata["state"]; got != waitStateCanceled {
			t.Fatalf("wait %s state = %q, want %q", id, got, waitStateCanceled)
		}
		if updated.Status != "closed" {
			t.Fatalf("wait %s status = %q, want closed", id, updated.Status)
		}
	}
}

func TestLoadSessionWaitBeads_IncludesLegacyWaitType(t *testing.T) {
	store := beads.NewMemStore()
	sessionID := "gc-session"
	// loadSessionWaits returns session.WaitInfo, which omits the storage-level
	// bead Type. The legacy-type wait still flows through the lookup, so assert
	// the created legacy bead is returned by ID (the IsWaitBead legacy-type
	// coverage stays enforced by internal/session's IsWaitBead tests).
	legacy, err := store.Create(beads.Bead{
		Type:   sessionpkg.LegacyWaitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionID},
		Metadata: map[string]string{
			"session_id": sessionID,
			"state":      waitStatePending,
		},
	})
	if err != nil {
		t.Fatalf("create legacy wait bead: %v", err)
	}

	waits, err := sessionFrontDoor(store).WaitsForSession(sessionID)
	if err != nil {
		t.Fatalf("loadSessionWaits: %v", err)
	}
	if len(waits) != 1 {
		t.Fatalf("loadSessionWaits returned %d waits, want 1", len(waits))
	}
	if waits[0].ID != legacy.ID {
		t.Fatalf("wait ID = %q, want legacy wait %q", waits[0].ID, legacy.ID)
	}
}

func TestClearSessionWaitHoldIfIdle_UsesSessionWaitLookup(t *testing.T) {
	base := beads.NewMemStore()
	store := waitGlobalListOmitStore{Store: base}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"wait_hold":    "true",
			"sleep_intent": "wait-hold",
			"sleep_reason": "wait-hold",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id": sessionBead.ID,
			"state":      waitStatePending,
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	if err := clearSessionWaitHoldIfIdle(sessionFrontDoor(store), sessionBead.ID); err != nil {
		t.Fatalf("clearSessionWaitHoldIfIdle: %v", err)
	}

	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("store.Get(session): %v", err)
	}
	if updated.Metadata["wait_hold"] != "true" {
		t.Fatalf("wait_hold = %q, want preserved", updated.Metadata["wait_hold"])
	}
}

func TestClearSessionWaitHoldIfIdle_PropagatesWaitLoadError(t *testing.T) {
	store := waitErrorStore{MemStore: beads.NewMemStore()}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"wait_hold":    "true",
			"sleep_intent": "wait-hold",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	if err := clearSessionWaitHoldIfIdle(sessionFrontDoor(store), sessionBead.ID); err == nil {
		t.Fatal("expected clearSessionWaitHoldIfIdle to return load error")
	}

	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("store.Get(session): %v", err)
	}
	if updated.Metadata["wait_hold"] != "true" {
		t.Fatalf("wait_hold = %q, want true", updated.Metadata["wait_hold"])
	}
	if updated.Metadata["sleep_intent"] != "wait-hold" {
		t.Fatalf("sleep_intent = %q, want wait-hold", updated.Metadata["sleep_intent"])
	}
}

func TestCmdSessionWait_DoesNotMaterializeTemplateTarget(t *testing.T) {
	setWaitTestFileBeads(t)
	t.Setenv("GC_SESSION", "fake")

	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag = ""
	rigFlag = ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
	})

	cityPath := shortSocketTempDir(t, "gc-bd-city-")
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)

	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	dep, err := store.Create(beads.Bead{Title: "dep"})
	if err != nil {
		t.Fatalf("create dep bead: %v", err)
	}
	if err := store.Close(dep.ID); err != nil {
		t.Fatalf("close dep bead: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdSessionWait([]string{"worker"}, []string{dep.ID}, false, "block", false, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdSessionWait() = 0, want failure; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	sessions, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("session bead count = %d, want 0", len(sessions))
	}
}

func TestDoSessionWait_RegistersReadyWaitForRigDependency(t *testing.T) {
	const (
		sessionID = "gcg-session-1"
		depID     = "ga-dep-1"
		originID  = "gcg-origin-1"
	)
	now := time.Date(2026, time.July, 16, 6, 30, 0, 0, time.UTC)
	cityStore := waitPrefixedStore{
		Store: beads.NewMemStoreFrom(1, []beads.Bead{{
			ID:        sessionID,
			Title:     "worker session",
			Type:      sessionBeadType,
			Status:    "open",
			Labels:    []string{sessionBeadLabel},
			CreatedAt: now.Add(-time.Minute),
			UpdatedAt: now.Add(-time.Minute),
			Revision:  1,
			Metadata: map[string]string{
				"session_name":       "worker",
				"continuation_epoch": "1",
			},
		}}, nil),
		prefix: "gcg",
	}
	rigStore := waitPrefixedStore{
		Store: beads.NewMemStoreFrom(1, []beads.Bead{{
			ID:        depID,
			Title:     "rig dependency",
			Type:      "task",
			Status:    "closed",
			CreatedAt: now.Add(-time.Minute),
			UpdatedAt: now.Add(-time.Minute),
			Revision:  1,
		}}, nil),
		prefix: "ga",
	}

	var stdout, stderr bytes.Buffer
	code := doSessionWait(sessionID, []string{depID}, false, "block", false, &stdout, &stderr, sessionWaitDeps{
		sessions:         sessionFrontDoor(cityStore),
		dependencies:     waitDependencyReaderOver(cityStore, map[string]beads.Store{"frontend": rigStore}),
		now:              func() time.Time { return now },
		createdBySession: originID,
	})
	if code != 0 {
		t.Fatalf("doSessionWait() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "already ready") {
		t.Fatalf("stdout = %q, want already-ready result", got)
	}

	waits, err := cityStore.ListByLabel("session:"+sessionID, 0)
	if err != nil {
		t.Fatalf("ListByLabel(wait): %v", err)
	}
	if len(waits) != 1 {
		t.Fatalf("wait count = %d, want 1", len(waits))
	}
	wait := waits[0]
	if wait.Status != "open" {
		t.Fatalf("wait status = %q, want open", wait.Status)
	}
	for key, want := range map[string]string{
		"state":              waitStateReady,
		"created_at":         now.Format(time.RFC3339),
		"ready_at":           now.Format(time.RFC3339),
		"dep_ids":            depID,
		"dep_mode":           "all",
		"created_by_session": originID,
	} {
		if got := wait.Metadata[key]; got != want {
			t.Fatalf("wait metadata[%q] = %q, want %q", key, got, want)
		}
	}
	if wait.Description != "block" {
		t.Fatalf("wait description = %q, want block", wait.Description)
	}
}

func TestCmdSessionWait_AllowsRigDependencyBeads(t *testing.T) {
	setWaitTestFileBeads(t)
	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag = ""
	rigFlag = ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
	})

	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}
	cityToml := `[workspace]
name = "gascity"
prefix = "gc"

[beads]
provider = "file"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	cityFlag = cityPath
	if err := ensureScopedFileStoreLayout(cityPath); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityPath); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(city): %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigPath); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(rig): %v", err)
	}
	dep := beads.Bead{ID: "fe-1", Title: "rig dep", Status: "closed", Type: "task"}
	writeTestFileStoreBeads(t, rigPath, []beads.Bead{dep})

	cityStore, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	rigStore, err := openStoreAtForCity(rigPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	sessionBead, err := cityStore.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	gotDep, err := rigStore.Get(dep.ID)
	if err != nil {
		t.Fatalf("get rig dep bead: %v", err)
	}
	if gotDep.Status != "closed" {
		t.Fatalf("rig dep status = %q, want closed", gotDep.Status)
	}

	var stdout, stderr bytes.Buffer
	code := cmdSessionWait([]string{sessionBead.ID}, []string{dep.ID}, false, "block", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdSessionWait() = %d, want success; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	cityStore, err = openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt(reload): %v", err)
	}
	waits, err := cityStore.ListByLabel("session:"+sessionBead.ID, 0)
	if err != nil {
		t.Fatalf("ListByLabel(wait): %v", err)
	}
	if len(waits) != 1 {
		t.Fatalf("wait count = %d, want 1", len(waits))
	}
	if got := waits[0].Metadata["state"]; got != waitStateReady {
		t.Fatalf("wait state = %q, want %q", got, waitStateReady)
	}
	if waits[0].Metadata["ready_at"] == "" {
		t.Fatal("ready_at was not recorded")
	}
}

// The session and the wait are the same in every row below; only the dependency
// and the store frame vary, which is what those rows are about.
const (
	waitWakeSessionID = "gcg-session-1"
	waitWakeWaitID    = "gcg-wait-1"
)

// waitWakeCityStore builds the city work store every wake-state test starts
// from: one open session and one pending deps-wait on depID.
func waitWakeCityStore(now time.Time, depID string, extra ...beads.Bead) waitPrefixedStore {
	seed := []beads.Bead{
		{
			ID:        waitWakeSessionID,
			Title:     "worker session",
			Type:      sessionBeadType,
			Status:    "open",
			Labels:    []string{sessionBeadLabel},
			CreatedAt: now.Add(-time.Minute),
			UpdatedAt: now.Add(-time.Minute),
			Revision:  1,
			Metadata: map[string]string{
				"session_name":       "worker",
				"agent_name":         "worker",
				"continuation_epoch": "1",
			},
		},
		{
			ID:        waitWakeWaitID,
			Title:     "wait:worker session",
			Type:      waitBeadType,
			Status:    "open",
			Labels:    []string{waitBeadLabel, "session:" + waitWakeSessionID},
			CreatedAt: now.Add(-time.Minute),
			UpdatedAt: now.Add(-time.Minute),
			Revision:  1,
			Metadata: map[string]string{
				"session_id":       waitWakeSessionID,
				"session_name":     "worker",
				"kind":             "deps",
				"state":            waitStatePending,
				"dep_ids":          depID,
				"dep_mode":         "all",
				"registered_epoch": "1",
				"delivery_attempt": "1",
			},
		},
	}
	seed = append(seed, extra...)
	return waitPrefixedStore{Store: beads.NewMemStoreFrom(len(seed), seed, nil), prefix: "gcg"}
}

func waitDepBead(now time.Time, depID, status string) beads.Bead {
	return beads.Bead{
		ID:        depID,
		Title:     "dependency",
		Type:      "task",
		Status:    status,
		CreatedAt: now.Add(-time.Minute),
		UpdatedAt: now.Add(-time.Minute),
		Revision:  1,
	}
}

// assertWaitStillOpen reads the wait back and checks the pair that decides
// whether a waiter survived the pass. The status is asserted open in every row
// because that is the outcome under test: a wait the pass failed is closed, so
// no row here may pass while the waiter was reaped.
func assertWaitStillOpen(t *testing.T, store beads.Store, wantState string) {
	t.Helper()
	wait, err := store.Get(waitWakeWaitID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := wait.Metadata["state"]; got != wantState {
		t.Fatalf("wait state = %q, want %q", got, wantState)
	}
	if wait.Status != "open" {
		t.Fatalf("wait status = %q, want open", wait.Status)
	}
}

// TestPrepareWaitWakeState_DarkCityWorkLegStillAbortsThePass is the CONTROL for
// the dark-rig row above. A rig leg degrades and the pass goes on; the city work
// leg is the authority and its going dark is a pass-level fault, so this row
// asserts the error IS returned. Without it, a reader that simply stopped
// reporting every leg failure would satisfy the dark-rig row.
func TestPrepareWaitWakeState_DarkCityWorkLegStillAbortsThePass(t *testing.T) {
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	const depID = "ga-dep-1"
	hardErr := errors.New("city work store unavailable")
	cityStore := waitWakeCityStore(now, depID)
	darkWork := waitDependencyGetErrorStore{Store: cityStore, prefix: "gcg", err: hardErr}

	_, err := prepareWaitWakeStateWithSnapshot(
		sessionFrontDoor(cityStore),
		waitDependencyReaderOver(darkWork, nil),
		beads.NudgesStore{Store: cityStore},
		now,
		nil,
	)
	if !errors.Is(err, hardErr) {
		t.Fatalf("prepareWaitWakeStateWithSnapshot error = %v, want %v", err, hardErr)
	}
	assertWaitStillOpen(t, cityStore, waitStatePending)
}

// TestPrepareWaitWakeState_SuspendedFrameRetainsAnUnprovedWait pins the edge
// that dropping suspended rigs opens: the frame is narrower than the city, so a
// dependency found nowhere in it is out of FRAME, not proved absent, and the
// waiter must survive to be re-read once the rig serves again.
func TestPrepareWaitWakeState_SuspendedFrameRetainsAnUnprovedWait(t *testing.T) {
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	const depID = "ga-dep-1"
	cityStore := waitWakeCityStore(now, depID)

	readyWaitSet, err := prepareWaitWakeStateWithSnapshot(
		sessionFrontDoor(cityStore),
		newWaitDependencyPlanReader(assembleResidencyTopology(nil, cityStore, nil, nil, nil), true),
		beads.NudgesStore{Store: cityStore},
		now,
		nil,
	)
	if err != nil {
		t.Fatalf("prepareWaitWakeStateWithSnapshot: %v", err)
	}
	if readyWaitSet[waitWakeSessionID] {
		t.Fatalf("readyWaitSet[%s] = true, want false", waitWakeSessionID)
	}
	assertWaitStillOpen(t, cityStore, waitStatePending)
}

// TestPrepareWaitWakeState_ResolvesBindingResidentDependency covers the leg the
// hand-rolled list did not have at all. Before the plan, a dependency relocated
// into a class binding resolved not-found on a split city and the wait was
// actively FAILED — worse than blindness.
func TestPrepareWaitWakeState_ResolvesBindingResidentDependency(t *testing.T) {
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	const depID = "gcb-dep-1"
	cityStore := waitWakeCityStore(now, depID)
	bindingStore := waitPrefixedStore{
		Store:  beads.NewMemStoreFrom(1, []beads.Bead{waitDepBead(now, depID, "closed")}, nil),
		prefix: "gcb",
	}
	bindings, refused := soleBindingResidency(bindingStore)
	if refused != nil {
		t.Fatalf("soleBindingResidency: %v", refused)
	}

	readyWaitSet, err := prepareWaitWakeStateWithSnapshot(
		sessionFrontDoor(cityStore),
		newWaitDependencyPlanReader(assembleResidencyTopology(nil, cityStore, nil, bindings, nil), false),
		beads.NudgesStore{Store: cityStore},
		now,
		nil,
	)
	if err != nil {
		t.Fatalf("prepareWaitWakeStateWithSnapshot: %v", err)
	}
	if !readyWaitSet[waitWakeSessionID] {
		t.Fatalf("readyWaitSet[%s] = false, want true", waitWakeSessionID)
	}
	assertWaitStillOpen(t, cityStore, waitStateReady)
}

// waitWakeMigratedCityStore is waitWakeCityStore under the WORK-ERA id prefix a
// city carried before its infrastructure classes were relocated. The prefix is
// what the retained-copy rows below turn on: storeref.Resolve consults the store
// whose self-declared IDPrefix owns the id first, so an id minted under this
// prefix and preserved across the cutover resolved to this store and not to the
// binding it was migrated into.
func waitWakeMigratedCityStore(now time.Time, depID string, extra ...beads.Bead) waitPrefixedStore {
	return waitPrefixedStore{Store: waitWakeCityStore(now, depID, extra...).Store, prefix: "hq"}
}

// TestPrepareWaitWakeState_MigrationPreservedDependencyAnswersFromTheBinding is
// the row ga-cu12x names: `gc storage migrate` COPIES AND RETAINS and it
// PRESERVES ids, so an infrastructure bead minted before the cutover keeps its
// work-era prefix and exists twice — a frozen row in the work ledger and the
// live row in the binding.
//
// The reader this replaced went through storeref.Resolve, whose PrefixOwner fast
// path consults the store whose declared IDPrefix owns the id FIRST. For
// "hq-dep-1" that is the city work store, so the wait read the frozen
// pre-migration copy — successfully, with no error to notice — no matter which
// leg the caller put first, which is why #5488's binding-first list did not
// close this one. The by-id plan has no such fast path: an id inside NO
// binding's reserved namespace is a migrate-preserved relic candidate, and every
// binding that has not retired its residence probe is asked BEFORE the work
// ledger.
func TestPrepareWaitWakeState_MigrationPreservedDependencyAnswersFromTheBinding(t *testing.T) {
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	// A work-era prefix, not a reserved one: no binding namespace claims it.
	const depID = "hq-dep-1"
	// The frozen twin the migration left behind, still OPEN as of the cutover.
	cityStore := waitWakeMigratedCityStore(now, depID, waitDepBead(now, depID, "open"))
	// The live row, closed after the cutover.
	binding := waitPrefixedStore{
		Store:  beads.NewMemStoreFrom(1, []beads.Bead{waitDepBead(now, depID, "closed")}, nil),
		prefix: "gcb",
	}
	bindings, refused := soleBindingResidency(binding)
	if refused != nil {
		t.Fatalf("soleBindingResidency: %v", refused)
	}

	readyWaitSet, err := prepareWaitWakeStateWithSnapshot(
		sessionFrontDoor(cityStore),
		newWaitDependencyPlanReader(assembleResidencyTopology(nil, cityStore, nil, bindings, nil), false),
		beads.NudgesStore{Store: cityStore},
		now,
		nil,
	)
	if err != nil {
		t.Fatalf("prepareWaitWakeStateWithSnapshot: %v", err)
	}
	if !readyWaitSet[waitWakeSessionID] {
		t.Fatalf("readyWaitSet[%s] = false, want true; the wait read the work ledger's frozen pre-migration copy instead of the binding's live row", waitWakeSessionID)
	}
	assertWaitStillOpen(t, cityStore, waitStateReady)
}

// TestWaitDependencyPlanReaderBindingFaultIsAnErrorNeverAbsence is the fault
// control for the row above. A binding that cannot be read has said NOTHING
// about the dependency, and the wake pass reaps a waiter on a proved absence —
// so the one thing this reader may never do is spell a fault as beads.ErrNotFound.
func TestWaitDependencyPlanReaderBindingFaultIsAnErrorNeverAbsence(t *testing.T) {
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	const depID = "hq-dep-1"
	hardErr := errors.New("binding store unavailable")
	cityStore := waitWakeMigratedCityStore(now, depID, waitDepBead(now, depID, "open"))
	binding := waitDependencyGetErrorStore{
		Store:  beads.NewMemStore(),
		prefix: "gcb",
		err:    hardErr,
	}
	bindings, refused := soleBindingResidency(binding)
	if refused != nil {
		t.Fatalf("soleBindingResidency: %v", refused)
	}
	reader := newWaitDependencyPlanReader(assembleResidencyTopology(nil, cityStore, nil, bindings, nil), false)

	_, err := reader.Get(depID)
	if !errors.Is(err, hardErr) {
		t.Fatalf("Get(%s) error = %v, want %v; the work ledger's frozen copy must not answer for a binding that could not be read", depID, err, hardErr)
	}
	if errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(%s) error = %v, must not wear beads.ErrNotFound: the wake pass fails a wait on a proved absence", depID, err)
	}
	if errors.Is(err, errWaitDependencyUnproven) {
		t.Fatalf("Get(%s) error = %v, must not be downgraded to an unproven absence: a fault the operator has to see would then only be logged", depID, err)
	}
}

// TestWaitDependencyPlanReaderSingleStoreCityIsByteIdentical is the control that
// a city with nothing relocated pays nothing and answers exactly as its own
// store does — the same value on a hit, and a proved absence on a miss.
func TestWaitDependencyPlanReaderSingleStoreCityIsByteIdentical(t *testing.T) {
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	const depID = "hq-dep-1"
	cityStore := waitWakeMigratedCityStore(now, depID, waitDepBead(now, depID, "closed"))
	reader := newWaitDependencyPlanReader(assembleResidencyTopology(nil, cityStore, nil, nil, nil), false)

	got, err := reader.Get(depID)
	if err != nil {
		t.Fatalf("Get(%s): %v", depID, err)
	}
	want, err := cityStore.Get(depID)
	if err != nil {
		t.Fatalf("cityStore.Get(%s): %v", depID, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Get(%s) = %+v, want the store's own row %+v", depID, got, want)
	}

	_, missErr := reader.Get("hq-absent")
	if !errors.Is(missErr, beads.ErrNotFound) {
		t.Fatalf("Get(hq-absent) error = %v, want beads.ErrNotFound", missErr)
	}
	if errors.Is(missErr, errWaitDependencyUnproven) {
		t.Fatalf("Get(hq-absent) error = %v; a complete frame proves absence, and a wait whose dependency is gone must still fail", missErr)
	}
}

// TestPrepareWaitWakeState_CoResidentDependencyAnswersFromTheWorkStore pins the
// deliberate flip in delta (d): storeref.Resolve consulted the store whose
// self-declared IDPrefix owned the id FIRST, so a rig copy shadowed the work
// ledger's. The plan reads work first (#5148), which is the copy `gc ready`
// serves and the claim lands on, so the wait now agrees with them.
func TestPrepareWaitWakeState_CoResidentDependencyAnswersFromTheWorkStore(t *testing.T) {
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	const depID = "ga-dep-1"
	cityStore := waitWakeCityStore(now, depID, waitDepBead(now, depID, "closed"))
	rigStore := waitPrefixedStore{
		Store:  beads.NewMemStoreFrom(1, []beads.Bead{waitDepBead(now, depID, "open")}, nil),
		prefix: "ga",
	}

	readyWaitSet, err := prepareWaitWakeStateWithSnapshot(
		sessionFrontDoor(cityStore),
		waitDependencyReaderOver(cityStore, map[string]beads.Store{"frontend": rigStore}),
		beads.NudgesStore{Store: cityStore},
		now,
		nil,
	)
	if err != nil {
		t.Fatalf("prepareWaitWakeStateWithSnapshot: %v", err)
	}
	if !readyWaitSet[waitWakeSessionID] {
		t.Fatalf("readyWaitSet[%s] = false, want true; the rig's open copy shadowed the work store's closed one", waitWakeSessionID)
	}
	assertWaitStillOpen(t, cityStore, waitStateReady)
}

// TestDepsWaitReadyUnprovenDependencyNeitherFailsNorReadies pins the three-valued
// contract at the layer that consumes it: an unproved absence is not a not-found
// (which would fail the wait) and not a hit (which could ready it).
func TestDepsWaitReadyUnprovenDependencyNeitherFailsNorReadies(t *testing.T) {
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	unproven := fmt.Errorf("%w: rig frontend went dark", errWaitDependencyUnproven)

	t.Run("all mode reports unproven without failing", func(t *testing.T) {
		reader := waitDependencyReaderFunc(func(string) (beads.Bead, error) { return beads.Bead{}, unproven })
		ready, err := depsWaitReadyDetailedFrom(reader, sessionpkg.WaitInfo{DepIDs: []string{"ga-1"}, DepMode: "all"})
		if ready {
			t.Fatal("ready = true, want false")
		}
		if !errors.Is(err, errWaitDependencyUnproven) {
			t.Fatalf("err = %v, want an unproven error", err)
		}
		if errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("err = %v, must not wear beads.ErrNotFound: that would fail the wait", err)
		}
	})

	t.Run("any mode still readies on a closed sibling", func(t *testing.T) {
		reader := waitDependencyReaderFunc(func(id string) (beads.Bead, error) {
			if id == "ga-2" {
				return waitDepBead(now, id, "closed"), nil
			}
			return beads.Bead{}, unproven
		})
		ready, err := depsWaitReadyDetailedFrom(reader, sessionpkg.WaitInfo{DepIDs: []string{"ga-1", "ga-2"}, DepMode: "any"})
		if err != nil {
			t.Fatalf("depsWaitReadyDetailedFrom: %v", err)
		}
		if !ready {
			t.Fatal("ready = false, want true")
		}
	})

	t.Run("any mode does not fail when every dependency is unproven", func(t *testing.T) {
		reader := waitDependencyReaderFunc(func(string) (beads.Bead, error) { return beads.Bead{}, unproven })
		ready, err := depsWaitReadyDetailedFrom(reader, sessionpkg.WaitInfo{DepIDs: []string{"ga-1", "ga-2"}, DepMode: "any"})
		if ready {
			t.Fatal("ready = true, want false")
		}
		if !errors.Is(err, errWaitDependencyUnproven) {
			t.Fatalf("err = %v, want an unproven error", err)
		}
	})
}

func TestPrepareWaitWakeState_ResolvesRigDependencyBeads(t *testing.T) {
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	hardErr := errors.New("rig store unavailable")

	for _, tc := range []struct {
		name       string
		depStatus  string
		missing    bool
		readErr    error
		wantReady  bool
		wantState  string
		wantStatus string
	}{
		{name: "closed rig dependency becomes ready", depStatus: "closed", wantReady: true, wantState: waitStateReady, wantStatus: "open"},
		{name: "open rig dependency remains pending", depStatus: "open", wantState: waitStatePending, wantStatus: "open"},
		{name: "missing rig dependency fails the wait", missing: true, wantState: waitStateFailed, wantStatus: "closed"},
		// A dark SERVING rig leg is still a pass-level fault, and the wait is
		// retained pending rather than failed: the by-id plan makes every leg
		// fatal, so the fault surfaces as itself and is never flattened into the
		// proved absence that would reap the waiter. The suspended-rig freeze
		// this segment fixes is closed one level up, by narrowing the FRAME
		// (servingRigStores) so a suspended rig is out of the plan rather than
		// dark inside it — see
		// TestPrepareWaitWakeState_SuspendedFrameRetainsAnUnprovedWait.
		{name: "dark serving rig is a preserved fault, not a proved absence", readErr: hardErr, wantState: waitStatePending, wantStatus: "open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const (
				sessionID = "gcg-session-1"
				waitID    = "gcg-wait-1"
				depID     = "ga-dep-1"
			)
			cityStore := waitPrefixedStore{
				Store: beads.NewMemStoreFrom(2, []beads.Bead{
					{
						ID:        sessionID,
						Title:     "worker session",
						Type:      sessionBeadType,
						Status:    "open",
						Labels:    []string{sessionBeadLabel},
						CreatedAt: now.Add(-time.Minute),
						UpdatedAt: now.Add(-time.Minute),
						Revision:  1,
						Metadata: map[string]string{
							"session_name":       "worker",
							"agent_name":         "worker",
							"continuation_epoch": "1",
						},
					},
					{
						ID:        waitID,
						Title:     "wait:worker session",
						Type:      waitBeadType,
						Status:    "open",
						Labels:    []string{waitBeadLabel, "session:" + sessionID},
						CreatedAt: now.Add(-time.Minute),
						UpdatedAt: now.Add(-time.Minute),
						Revision:  1,
						Metadata: map[string]string{
							"session_id":       sessionID,
							"session_name":     "worker",
							"kind":             "deps",
							"state":            waitStatePending,
							"dep_ids":          depID,
							"dep_mode":         "all",
							"registered_epoch": "1",
							"delivery_attempt": "1",
						},
					},
				}, nil),
				prefix: "gcg",
			}

			var rigBeads []beads.Bead
			if !tc.missing {
				rigBeads = []beads.Bead{{
					ID:        depID,
					Title:     "rig dependency",
					Type:      "task",
					Status:    tc.depStatus,
					CreatedAt: now.Add(-time.Minute),
					UpdatedAt: now.Add(-time.Minute),
					Revision:  1,
				}}
			}
			var rigStore beads.Store = waitPrefixedStore{
				Store:  beads.NewMemStoreFrom(len(rigBeads), rigBeads, nil),
				prefix: "ga",
			}
			if tc.readErr != nil {
				rigStore = waitDependencyGetErrorStore{Store: rigStore, prefix: "ga", err: tc.readErr}
			}

			readyWaitSet, err := prepareWaitWakeStateWithSnapshot(
				sessionFrontDoor(cityStore),
				waitDependencyReaderOver(cityStore, map[string]beads.Store{"frontend": rigStore}),
				beads.NudgesStore{Store: cityStore},
				now,
				nil,
			)
			if tc.readErr != nil {
				if !errors.Is(err, tc.readErr) {
					t.Fatalf("prepareWaitWakeStateWithSnapshot error = %v, want %v; a leg that could not be read must never reach the pass as absence", err, tc.readErr)
				}
			} else if err != nil {
				t.Fatalf("prepareWaitWakeStateWithSnapshot: %v", err)
			}
			if got := readyWaitSet[sessionID]; got != tc.wantReady {
				t.Fatalf("readyWaitSet[%s] = %v, want %v", sessionID, got, tc.wantReady)
			}

			updatedWait, getErr := cityStore.Get(waitID)
			if getErr != nil {
				t.Fatalf("store.Get(wait): %v", getErr)
			}
			if got := updatedWait.Metadata["state"]; got != tc.wantState {
				t.Fatalf("wait state = %q, want %q", got, tc.wantState)
			}
			if updatedWait.Status != tc.wantStatus {
				t.Fatalf("wait status = %q, want %q", updatedWait.Status, tc.wantStatus)
			}
			if tc.wantState == waitStateReady && updatedWait.Metadata["ready_at"] == "" {
				t.Fatal("ready_at was not recorded")
			}
			if tc.wantState == waitStateFailed && updatedWait.Metadata["last_error"] == "" {
				t.Fatal("last_error was not recorded")
			}
		})
	}
}

func setupFreshManagedBdWaitTestCity(t *testing.T) string {
	t.Helper()
	configureIsolatedRuntimeEnv(t)

	bdPath := waitTestRealBDPath(t)
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}

	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")

	homeDir := filepath.Join(shortSocketTempDir(t, "gc-bd-home-"), "home")
	if err := writeWaitTestDoltIdentity(homeDir); err != nil {
		t.Fatalf("writeWaitTestDoltIdentity: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("DOLT_ROOT_PATH", homeDir)
	t.Setenv("PATH", strings.Join([]string{filepath.Dir(bdPath), filepath.Dir(doltPath), os.Getenv("PATH")}, string(os.PathListSeparator)))

	reexecGC := reexecGCTestBinaryForTests(t)
	oldResolve := resolveProviderLifecycleGCBinary
	resolveProviderLifecycleGCBinary = func() string { return reexecGC }
	t.Cleanup(func() { resolveProviderLifecycleGCBinary = oldResolve })

	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag = ""
	rigFlag = ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
	})

	cityPath := shortSocketTempDir(t, "gc-bd-city-")
	if _, err := writeManagedBdWaitTestCityScaffold(cityPath); err != nil {
		t.Fatalf("writeManagedBdWaitTestCityScaffold: %v", err)
	}
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)
	materializeBuiltinPacksForTest(t, cityPath)
	if err := ensureBeadsProvider(cityPath); err != nil {
		t.Fatalf("ensureBeadsProvider: %v", err)
	}
	t.Cleanup(func() {
		_ = shutdownBeadsProvider(cityPath)
	})
	if err := initAndHookDir(cityPath, cityPath, "gc"); err != nil {
		t.Fatalf("initAndHookDir(city): %v", err)
	}
	if err := publishManagedDoltRuntimeState(cityPath); err != nil {
		t.Fatalf("publishManagedDoltRuntimeState: %v", err)
	}
	return cityPath
}

//nolint:unused // exercised by native_dolt_rebind_integration_test.go
func setupManagedBdWaitTestCity(t *testing.T) (string, string) {
	t.Helper()
	skipSlowCmdGCTest(t, "requires a managed bd/dolt lifecycle city; run make test-cmd-gc-process for full coverage")
	configureIsolatedRuntimeEnv(t)

	bdPath := waitTestRealBDPath(t)
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}

	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")

	homeDir := filepath.Join(shortSocketTempDir(t, "gc-bd-home-"), "home")
	if err := writeWaitTestDoltIdentity(homeDir); err != nil {
		t.Fatalf("writeWaitTestDoltIdentity: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("DOLT_ROOT_PATH", homeDir)
	t.Setenv("PATH", strings.Join([]string{filepath.Dir(bdPath), filepath.Dir(doltPath), os.Getenv("PATH")}, string(os.PathListSeparator)))

	oldResolve := resolveProviderLifecycleGCBinary
	resolveProviderLifecycleGCBinary = func() string { return currentGCBinaryForTests(t) }
	t.Cleanup(func() { resolveProviderLifecycleGCBinary = oldResolve })

	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag = ""
	rigFlag = ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
	})

	templatePath := managedBdWaitTestTemplate(t, bdPath, doltPath)
	cityPath := shortSocketTempDir(t, "gc-bd-city-")
	if err := overlay.CopyDir(templatePath, cityPath, io.Discard); err != nil {
		t.Fatalf("overlay.CopyDir(template city): %v", err)
	}
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.Chmod(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatalf("Chmod(city .beads): %v", err)
	}
	if err := os.Chmod(filepath.Join(rigPath, ".beads"), 0o700); err != nil {
		t.Fatalf("Chmod(rig .beads): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)

	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)
	poisonRuntimeDir := filepath.Join(t.TempDir(), "poison-runtime")
	poisonPackStateDir := filepath.Join(poisonRuntimeDir, "packs", "dolt")
	poisonStateFile := filepath.Join(poisonPackStateDir, "dolt-provider-state.json")
	t.Setenv("GC_CITY_RUNTIME_DIR", poisonRuntimeDir)
	t.Setenv("GC_PACK_STATE_DIR", poisonPackStateDir)
	t.Setenv("GC_DOLT_STATE_FILE", poisonStateFile)
	scriptEnv := sanitizedBaseEnv(
		"GC_CITY="+cityPath,
		"GC_CITY_PATH="+cityPath,
	)
	runScript := func(args ...string) {
		t.Helper()
		cmd := exec.Command(script, args...)
		cmd.Env = scriptEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	t.Cleanup(func() {
		cmd := exec.Command(script, "stop")
		cmd.Env = scriptEnv
		_, _ = cmd.CombinedOutput()
	})

	runScript("start")
	if _, err := os.Stat(poisonStateFile); !os.IsNotExist(err) {
		t.Fatalf("start leaked ambient GC_* state to %q, stat err = %v", poisonStateFile, err)
	}
	if err := publishManagedDoltRuntimeState(cityPath); err != nil {
		t.Fatalf("publishManagedDoltRuntimeState: %v", err)
	}
	return cityPath, rigPath
}

// ---------------------------------------------------------------------------
// Read-path routing matrix for `gc wait list` and `gc wait inspect`. Since
// WI-4 the CLI is a three-rung ladder: the typed /v0/waits endpoint (rung 1),
// the legacy gc:wait beads endpoint when an old server lacks that route
// (rung 2), and the local store leg (rung 3). The six canonical rows below
// (enforced by scripts/check-routed-test-rows.sh) cover rungs 1 and 3; the two
// route-missing rows cover rung 2's old-server fallback.
//
//   api-happy-path       typed /v0/waits 200            route=api, exit 0
//   api-cache-not-live   typed 503 cache_not_live       fallback, exit 0
//   api-500-fallback     typed generic 500              fallback (conn-refused)
//   api-404-error        typed 404 problem+json         no fallback, exit 1
//   controller-down      apiClient returns nil          fallback (controller-down)
//   escape-hatch         GC_NO_API truthy               fallback (escape-hatch)
//   route-missing-legacy typed plain 404 -> /beads 200  route=api-legacy, exit 0
//   route-missing-local  typed plain 404 -> /beads 500  fallback (conn-refused)
// ---------------------------------------------------------------------------

type waitMatrixHandler func(t *testing.T) http.Handler

// okWaitListHandler serves the typed /v0/waits endpoint with one wait.
func okWaitListHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/waits") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-GC-Cache-Age-S", "2")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"waits": []map[string]any{{
				"id":         "ga-wait-1",
				"session_id": "ga-sess-1",
				"kind":       "deps",
				"state":      waitStatePending,
				"status":     "open",
				"note":       "wait note",
			}},
			"capped": false,
		})
	})
}

// okWaitInspectHandler serves the typed /v0/wait/{id} endpoint.
func okWaitInspectHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/wait/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-GC-Cache-Age-S", "3")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":               "ga-wait-1",
			"session_id":       "ga-sess-1",
			"kind":             "deps",
			"state":            waitStatePending,
			"status":           "open",
			"dep_ids":          []string{"gc-1"},
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
			"note":             "wait note",
		})
	})
}

// legacyWaitBeadItem is the generic-beads projection of the sample wait, served
// by the rung-2 legacy leg.
func legacyWaitBeadItem() map[string]any {
	return map[string]any{
		"id":         "ga-wait-1",
		"title":      "wait:worker",
		"issue_type": sessionpkg.WaitBeadType,
		"status":     "open",
		"labels":     []string{sessionpkg.WaitBeadLabel, "session:ga-sess-1"},
		"metadata": map[string]string{
			"session_id": "ga-sess-1",
			"state":      waitStatePending,
			"kind":       "deps",
		},
		"description": "wait note",
	}
}

// waitRouteMissingListHandler emulates an OLD server: /v0/waits returns a
// plain-text 404 (no problem+json body), while the generic /beads endpoint still
// serves the label read. The plain 404 is what drives routeMissing classification.
func waitRouteMissingListHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/waits"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/beads"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{legacyWaitBeadItem()}, "total": 1})
		default:
			http.NotFound(w, r)
		}
	})
}

// waitRouteMissingListConnErrHandler is the old-server shape where the legacy
// /beads leg also fails (500), so the CLI drops to the local store leg.
func waitRouteMissingListConnErrHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/waits"):
			http.NotFound(w, r)
		default:
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 500, "title": "Internal Server Error", "detail": "explode"})
		}
	})
}

// waitRouteMissingInspectHandler is the inspect analog: /wait/{id} plain 404,
// /bead/{id} serves the wait bead.
func waitRouteMissingInspectHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/bead/"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(legacyWaitBeadItem())
		default:
			http.NotFound(w, r)
		}
	})
}

// waitRouteMissingInspectConnErrHandler: /wait/{id} plain 404, /bead/{id} 500.
func waitRouteMissingInspectConnErrHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/bead/"):
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 500, "title": "Internal Server Error", "detail": "explode"})
		default:
			http.NotFound(w, r)
		}
	})
}

func waitProblemHandler(status int, detail string) waitMatrixHandler {
	return func(_ *testing.T) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": status,
				"title":  http.StatusText(status),
				"detail": detail,
			})
		})
	}
}

// writeWaitTestCity prepares a file-provider city for the local fallback leg.
func writeWaitTestCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")
	return cityPath
}

func TestRouteWaitList_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      waitMatrixHandler
		useNilClient bool
		nilReason    string
		wantExit     int
		wantRoute    string
		wantReason   string
		wantStderr   string
		wantStdout   string
	}{
		{name: "api-happy-path", handler: okWaitListHandler, wantExit: 0, wantRoute: "api", wantStdout: "ga-wait-1"},
		{name: "api-cache-not-live", handler: waitProblemHandler(http.StatusServiceUnavailable, "cache_not_live: priming"), wantExit: 0, wantRoute: "fallback", wantReason: "cache-not-live", wantStdout: "WAIT"},
		{name: "api-500-fallback", handler: waitProblemHandler(http.StatusInternalServerError, "internal: explode"), wantExit: 0, wantRoute: "fallback", wantReason: "conn-refused", wantStdout: "WAIT"},
		{name: "api-404-error", handler: waitProblemHandler(http.StatusNotFound, "not_found: city missing"), wantExit: 1, wantStderr: "not_found"},
		{name: "route-missing-legacy", handler: waitRouteMissingListHandler, wantExit: 0, wantRoute: "api-legacy", wantReason: "route-missing", wantStdout: "ga-wait-1"},
		{name: "route-missing-local", handler: waitRouteMissingListConnErrHandler, wantExit: 0, wantRoute: "fallback", wantReason: "conn-refused", wantStdout: "WAIT"},
		{name: "controller-down", useNilClient: true, nilReason: "controller-down", wantExit: 0, wantRoute: "fallback", wantReason: "controller-down", wantStdout: "WAIT"},
		{name: "escape-hatch", useNilClient: true, nilReason: "escape-hatch", wantExit: 0, wantRoute: "fallback", wantReason: "escape-hatch", wantStdout: "WAIT"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DEBUG", "1")
			cityPath := writeWaitTestCity(t)

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			code := routeWaitList(cityPath, c, tc.nilReason, "", "", false, &stdout, &stderr)

			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, tc.wantExit, stderr.String(), stdout.String())
			}
			if tc.wantRoute != "" {
				want := "route=" + tc.wantRoute
				if tc.wantReason != "" {
					want += " reason=" + tc.wantReason
				}
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr missing %q:\n%s", want, stderr.String())
				}
				if n := strings.Count(stderr.String(), "route="); n != 1 {
					t.Errorf("route=... lines = %d, want 1:\n%s", n, stderr.String())
				}
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
		})
	}
}

func TestRouteWaitInspect_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      waitMatrixHandler
		useNilClient bool
		nilReason    string
		wantExit     int
		wantRoute    string
		wantReason   string
		wantStderr   string
		wantStdout   string
	}{
		{name: "api-happy-path", handler: okWaitInspectHandler, wantExit: 0, wantRoute: "api", wantStdout: "ga-wait-1"},
		{name: "api-cache-not-live", handler: waitProblemHandler(http.StatusServiceUnavailable, "cache_not_live: priming"), wantExit: 1, wantRoute: "fallback", wantReason: "cache-not-live", wantStderr: "not found"},
		{name: "api-500-fallback", handler: waitProblemHandler(http.StatusInternalServerError, "explode"), wantExit: 1, wantRoute: "fallback", wantReason: "conn-refused", wantStderr: "not found"},
		{name: "api-404-error", handler: waitProblemHandler(http.StatusNotFound, "not_found: bead missing"), wantExit: 1, wantStderr: "not_found"},
		{name: "route-missing-legacy", handler: waitRouteMissingInspectHandler, wantExit: 0, wantRoute: "api-legacy", wantReason: "route-missing", wantStdout: "ga-wait-1"},
		{name: "route-missing-local", handler: waitRouteMissingInspectConnErrHandler, wantExit: 1, wantRoute: "fallback", wantReason: "conn-refused", wantStderr: "not found"},
		{name: "controller-down", useNilClient: true, nilReason: "controller-down", wantExit: 1, wantRoute: "fallback", wantReason: "controller-down", wantStderr: "not found"},
		{name: "escape-hatch", useNilClient: true, nilReason: "escape-hatch", wantExit: 1, wantRoute: "fallback", wantReason: "escape-hatch", wantStderr: "not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DEBUG", "1")
			cityPath := writeWaitTestCity(t)

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			code := routeWaitInspect(cityPath, c, tc.nilReason, "ga-missing", false, &stdout, &stderr)

			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, tc.wantExit, stderr.String(), stdout.String())
			}
			if tc.wantRoute != "" {
				want := "route=" + tc.wantRoute
				if tc.wantReason != "" {
					want += " reason=" + tc.wantReason
				}
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr missing %q:\n%s", want, stderr.String())
				}
				if n := strings.Count(stderr.String(), "route="); n != 1 {
					t.Errorf("route=... lines = %d, want 1:\n%s", n, stderr.String())
				}
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
		})
	}
}

// TestRouteWaitList_PassesWaitBeadLabelConstant locks the locator contract for
// the rung-2 legacy leg: when the typed route is missing, the CLI must query the
// generic beads endpoint with sessionpkg.WaitBeadLabel.
func TestRouteWaitList_PassesWaitBeadLabelConstant(t *testing.T) {
	t.Setenv("GC_DEBUG", "0")
	cityPath := writeWaitTestCity(t)

	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/waits"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/beads"):
			gotQuery = r.URL.Query().Get("label")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}, "total": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	if code := routeWaitList(cityPath, c, "", "", "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if gotQuery != sessionpkg.WaitBeadLabel {
		t.Errorf("legacy leg label query = %q, want %q", gotQuery, sessionpkg.WaitBeadLabel)
	}
}

// TestRouteWaitList_StaleBannerOver30s confirms the >30 s cache-age banner on
// the typed rung.
func TestRouteWaitList_StaleBannerOver30s(t *testing.T) {
	t.Setenv("GC_DEBUG", "0")
	cityPath := writeWaitTestCity(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/waits") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-GC-Cache-Age-S", "45")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"waits": []map[string]any{}, "capped": false})
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	if code := routeWaitList(cityPath, c, "", "", "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "cache age: 45s") {
		t.Errorf("stale banner missing from human output:\n%s", stdout.String())
	}
}

// TestRouteWaitList_ThreeRungByteIdentical is the cross-rung byte-identity pin
// for the CreatedAt-precision blocker: two waits created sub-second apart in the
// SAME second must render in the same --json row order on all three rungs. The
// typed and legacy mocks carry created_at at RFC3339Nano (as the real server and
// the bead encoder do), the local rung reads the persisted store; the CLI's
// ascending created-time sort must resolve the tie identically on every rung.
func TestRouteWaitList_ThreeRungByteIdentical(t *testing.T) {
	cityDir, store := setupWaitJSONTestCity(t)

	// The store assigns CreatedAt=now on Create, so two back-to-back creates land
	// sub-second apart in (almost always) the same second — the tie the
	// truncation bug broke. The skip guard below covers the rare second-straddle.
	seed := func() {
		if _, err := store.Create(beads.Bead{
			Title:       "wait:demo",
			Type:        waitBeadType,
			Status:      "open",
			Description: "wait for deps",
			Labels:      []string{waitBeadLabel, "session:s-1"},
			Metadata:    map[string]string{"session_id": "s-1", "session_name": "demo", "kind": "deps", "state": waitStateReady},
		}); err != nil {
			t.Fatalf("seed wait: %v", err)
		}
	}
	seed()
	seed()

	// Read the persisted waits back the way the local rung will (reopened store),
	// so the mock wire values match the local rung's CreatedAt exactly.
	reopened, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	persisted, err := reopened.List(beads.ListQuery{Label: waitBeadLabel, Sort: beads.SortCreatedDesc})
	if err != nil {
		t.Fatalf("list persisted waits: %v", err)
	}
	if len(persisted) != 2 {
		t.Fatalf("persisted wait count = %d, want 2", len(persisted))
	}
	// The file store must preserve sub-second precision for the tie to be
	// resolvable on every rung (the coordinator's nanosecond-backend premise).
	if persisted[0].CreatedAt.Truncate(time.Second) != persisted[1].CreatedAt.Truncate(time.Second) {
		t.Skipf("seeded waits landed in different seconds (%v vs %v); tie scenario not exercised", persisted[0].CreatedAt, persisted[1].CreatedAt)
	}
	if persisted[0].CreatedAt.Equal(persisted[1].CreatedAt) {
		t.Fatalf("file store truncated sub-second CreatedAt; both waits at %v — tie unresolvable on any rung", persisted[0].CreatedAt)
	}

	beadItem := func(b beads.Bead) map[string]any {
		return map[string]any{
			"id":          b.ID,
			"title":       b.Title,
			"issue_type":  b.Type,
			"status":      b.Status,
			"labels":      b.Labels,
			"metadata":    b.Metadata,
			"description": b.Description,
			"created_at":  b.CreatedAt.UTC().Format(time.RFC3339Nano),
		}
	}
	waitView := func(b beads.Bead) map[string]any {
		return map[string]any{
			"id":           b.ID,
			"session_id":   b.Metadata["session_id"],
			"session_name": b.Metadata["session_name"],
			"kind":         b.Metadata["kind"],
			"state":        b.Metadata["state"],
			"status":       b.Status,
			"note":         b.Description,
			"created_at":   b.CreatedAt.UTC().Format(time.RFC3339Nano),
		}
	}

	// Typed /v0/waits mock returns created-DESC (as the real server does).
	typedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/waits") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"waits":  []map[string]any{waitView(persisted[0]), waitView(persisted[1])},
			"capped": false,
		})
	}))
	defer typedSrv.Close()

	// Legacy mock: /waits plain-404 (route-missing) -> generic /beads.
	legacySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/waits"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/beads"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{beadItem(persisted[0]), beadItem(persisted[1])}, "total": 2})
		default:
			http.NotFound(w, r)
		}
	}))
	defer legacySrv.Close()

	run := func(c *api.Client, nilReason string) string {
		var stdout, stderr bytes.Buffer
		if code := routeWaitList(cityDir, c, nilReason, "", "", true, &stdout, &stderr); code != 0 {
			t.Fatalf("routeWaitList exit=%d stderr=%q", code, stderr.String())
		}
		return stdout.String()
	}

	typed := run(api.NewCityScopedClient(typedSrv.URL, "wait-json"), "")
	legacy := run(api.NewCityScopedClient(legacySrv.URL, "wait-json"), "")
	local := run(nil, "controller-down")

	if typed != legacy || typed != local {
		t.Fatalf("--json differs across rungs:\n typed=%s\n legacy=%s\n local=%s", typed, legacy, local)
	}
	// Sanity: the tie resolved chronologically (oldest wait first in the array).
	if !strings.Contains(typed, persisted[1].ID) || !strings.Contains(typed, persisted[0].ID) {
		t.Fatalf("both waits should render: %s", typed)
	}
}

// TestPrepareWaitWakeState_ResolvesGraphBindingDependencyBeads pins that a
// dependency living in the relocated infrastructure binding resolves. Without
// the binding leg it misses on every leg, and a clean miss is consumed as proof
// the dependency was deleted — a silent FailWait, with no event and no wake.
//
// Ported from #5488, which pinned the same property against the hand-rolled
// store list this reader replaced. The list put the binding FIRST and that leg
// order is preserved here by the resolver rather than by hand: `gcg-dep-1` is
// inside the graph class's reserved namespace, so the by-id plan makes the
// binding the AUTHORITY leg and nothing behind it is consulted.
func TestPrepareWaitWakeState_ResolvesGraphBindingDependencyBeads(t *testing.T) {
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name       string
		depStatus  string
		wantReady  bool
		wantState  string
		wantStatus string
	}{
		{name: "closed binding dependency becomes ready", depStatus: "closed", wantReady: true, wantState: waitStateReady, wantStatus: "open"},
		{name: "open binding dependency remains pending", depStatus: "open", wantState: waitStatePending, wantStatus: "open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const (
				sessionID = "hq-session-1"
				waitID    = "hq-wait-1"
				// A step bead minted by the graph class: the reserved prefix is
				// what makes it unreachable from every work scope.
				depID = "gcg-dep-1"
			)
			cityStore := waitPrefixedStore{
				Store: beads.NewMemStoreFrom(2, []beads.Bead{
					{
						ID:        sessionID,
						Title:     "worker session",
						Type:      sessionBeadType,
						Status:    "open",
						Labels:    []string{sessionBeadLabel},
						CreatedAt: now.Add(-time.Minute),
						UpdatedAt: now.Add(-time.Minute),
						Revision:  1,
						Metadata: map[string]string{
							"session_name":       "worker",
							"agent_name":         "worker",
							"continuation_epoch": "1",
						},
					},
					{
						ID:        waitID,
						Title:     "wait:worker session",
						Type:      waitBeadType,
						Status:    "open",
						Labels:    []string{waitBeadLabel, "session:" + sessionID},
						CreatedAt: now.Add(-time.Minute),
						UpdatedAt: now.Add(-time.Minute),
						Revision:  1,
						Metadata: map[string]string{
							"session_id":       sessionID,
							"session_name":     "worker",
							"kind":             "deps",
							"state":            waitStatePending,
							"dep_ids":          depID,
							"dep_mode":         "all",
							"registered_epoch": "1",
							"delivery_attempt": "1",
						},
					},
				}, nil),
				prefix: "hq",
			}
			binding := waitPrefixedStore{
				Store: beads.NewMemStoreFrom(1, []beads.Bead{{
					ID:        depID,
					Title:     "Step 1: implement",
					Type:      "step",
					Status:    tc.depStatus,
					CreatedAt: now.Add(-time.Minute),
					UpdatedAt: now.Add(-time.Minute),
					Revision:  1,
				}}, nil),
				prefix: "gcg",
			}
			rigStore := waitPrefixedStore{Store: beads.NewMemStore(), prefix: "ga"}

			bindings, refused := soleBindingResidency(binding)
			if refused != nil {
				t.Fatalf("soleBindingResidency: %v", refused)
			}
			readyWaitSet, err := prepareWaitWakeStateWithSnapshot(
				sessionFrontDoor(cityStore),
				newWaitDependencyPlanReader(
					assembleResidencyTopology(nil, cityStore, map[string]beads.Store{"frontend": rigStore}, bindings, nil),
					false,
				),
				beads.NudgesStore{Store: cityStore},
				now,
				nil,
			)
			if err != nil {
				t.Fatalf("prepareWaitWakeStateWithSnapshot: %v", err)
			}
			if got := readyWaitSet[sessionID]; got != tc.wantReady {
				t.Fatalf("readyWaitSet[%s] = %v, want %v", sessionID, got, tc.wantReady)
			}

			updatedWait, getErr := cityStore.Get(waitID)
			if getErr != nil {
				t.Fatalf("store.Get(wait): %v", getErr)
			}
			if got := updatedWait.Metadata["state"]; got != tc.wantState {
				t.Fatalf("wait state = %q, want %q; a dependency in the binding that no leg can read is indistinguishable from a deleted one, so the wait fails instead of waking", got, tc.wantState)
			}
			if updatedWait.Status != tc.wantStatus {
				t.Fatalf("wait status = %q, want %q", updatedWait.Status, tc.wantStatus)
			}
		})
	}
}

// TestLoadWaitDependencyBeadReadsTheBindingOnAMigratedCity is the one-shot CLI
// twin of the controller-tick case above: `gc session wait` resolves its
// dependency through loadWaitDependencyBead, whose scope candidates are all work
// roots, so a binding-resident dependency reads as a bead that does not exist.
func TestLoadWaitDependencyBeadReadsTheBindingOnAMigratedCity(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)

	binding := openMigratedDestination(t, mustResolveInfraTarget(t, cityPath, cfg))
	dep, err := binding.Create(beads.Bead{Title: "Step 1: implement", Type: "step"})
	if err != nil {
		t.Fatalf("creating the dependency in the binding: %v", err)
	}
	if err := closeBeadStoreHandle(binding); err != nil {
		t.Fatalf("closing the binding handle: %v", err)
	}

	cityStore, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening the work store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(cityStore) })

	got, err := loadWaitDependencyBead(cityPath, cityStore, dep.ID)
	if err != nil {
		t.Fatalf("loadWaitDependencyBead(%s): %v; a dependency the reader cannot see is consumed as a deleted one, which fails the wait", dep.ID, err)
	}
	if got.ID != dep.ID {
		t.Errorf("resolved %q, want the binding-resident dependency %q", got.ID, dep.ID)
	}
}

// seedRelocatedWaitDependency builds the shape a finished `gc storage migrate`
// leaves behind: one dependency id resident in BOTH the class binding and the
// city work store, where the binding copy is the live one and the work copy is
// the frozen row the migration retained.
//
// The two copies are given OPPOSITE statuses on purpose. A test that only
// checked which title came back would pass on a reader that resolved correctly
// by accident; asserting on status makes the fixture answer the operational
// question instead — does the wait fire — and the two answers cannot coincide.
func seedRelocatedWaitDependency(t *testing.T) (cityPath string, work beads.Store, depID string) {
	t.Helper()
	cityPath, _ = foreignProviderCity(t)
	work = workStoreFor(t, cityPath)

	frozen, err := work.Create(beads.Bead{Title: "the retained work copy", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the retained work copy: %v", err)
	}
	resident, classStore := classResidentWorkShapedBead(t, cityPath, frozen.ID, "the class-binding copy")

	// The binding's copy is the one that moved on. The work copy stays open
	// forever — nothing writes to it again after the migration.
	if err := classStore.Close(resident.ID); err != nil {
		t.Fatalf("closing the class-binding copy of %s: %v", resident.ID, err)
	}
	if reread, err := work.Get(frozen.ID); err != nil {
		t.Fatalf("re-reading the retained work copy: %v", err)
	} else if reread.Status == "closed" {
		t.Fatalf("fixture premise broken: closing the binding copy also closed the work copy, so no reader can tell the two apart")
	}
	return cityPath, work, resident.ID
}

// TestWaitDependencyReadServesTheBindingCopy is the one-shot twin of the
// controller-arm fix in ga-qdt5y.16 slice B, reached by a different code path.
//
// loadWaitDependencyBead resolved a dependency by scanning the city's store
// DIRECTORIES, and a relocated class binding is not one of them. So a dependency
// the migration moved was not merely unrouted: the scan answered, successfully,
// with the permanently-open copy retained in the work store. The wait's
// dependency then never reads closed and the waiter sleeps forever.
func TestWaitDependencyReadServesTheBindingCopy(t *testing.T) {
	cityPath, work, depID := seedRelocatedWaitDependency(t)

	dep, err := loadWaitDependencyBead(cityPath, work, depID)
	if err != nil {
		t.Fatalf("reading wait dependency %s: %v", depID, err)
	}
	if dep.Title != "the class-binding copy" {
		t.Errorf("wait dependency read served %q, want the class-binding copy — the scan answered from the frozen work copy", dep.Title)
	}
	if dep.Status != "closed" {
		t.Errorf("wait dependency %s read status %q, want closed; a waiter on this dependency never wakes", depID, dep.Status)
	}
}

// TestWaitReadiesOnADependencyTheMigrationRelocated asserts the consequence
// rather than the lookup: the wait itself must fire.
//
// The read above and this one fail together today, but they are not the same
// assertion — a reader could serve the right row and still be consumed by a
// readiness rule that ignores it. Pinning both means neither half can regress
// while the other keeps the suite green.
func TestWaitReadiesOnADependencyTheMigrationRelocated(t *testing.T) {
	cityPath, work, depID := seedRelocatedWaitDependency(t)

	dependencies := waitDependencyReaderFunc(func(id string) (beads.Bead, error) {
		return loadWaitDependencyBead(cityPath, work, id)
	})
	ready, err := depsWaitReadyDetailedFrom(dependencies, sessionpkg.WaitInfo{
		ID:      waitWakeWaitID,
		DepIDs:  []string{depID},
		DepMode: "all",
	})
	if err != nil {
		t.Fatalf("evaluating readiness against relocated dependency %s: %v", depID, err)
	}
	if !ready {
		t.Errorf("wait on relocated dependency %s is not ready; its only dependency is closed in the binding that owns it", depID)
	}
}
