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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
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

func TestWaitListJSONFiltersSessionWithSessionScopedLookup(t *testing.T) {
	_, store := setupWaitJSONTestCity(t)
	targetWait := createTestWaitBeadForSession(t, store, "target-session", waitStatePending)
	for i := 0; i < waitLookupLimit; i++ {
		createTestWaitBeadForSession(t, store, "other-session", waitStatePending)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdWaitList("", "target-session", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdWaitList(--json --session target-session) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}

	payload := decodeWaitListJSON(t, stdout.Bytes())
	if len(payload.Waits) != 1 || payload.Waits[0].ID != targetWait.ID || payload.Waits[0].SessionID != "target-session" {
		t.Fatalf("waits = %+v, want only target %s", payload.Waits, targetWait.ID)
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

	managedBdWaitTemplateOnce sync.Once
	managedBdWaitTemplatePath string
	managedBdWaitTemplateErr  error
)

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
		candidate, err := findPreferredBinary("bd")
		if err != nil {
			waitTestRealBDErr = errors.New("bd with init not installed")
			return
		}
		cmd := exec.Command(candidate, "init", "--help")
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), `unknown subcommand "init"`) {
			waitTestRealBDCached = candidate
			return
		}
		waitTestRealBDErr = errors.New("bd with init not installed")
	})
	if waitTestRealBDErr != nil {
		t.Skip(waitTestRealBDErr.Error())
	}
	return waitTestRealBDCached
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

func TestCmdWaitListSessionFilterUsesSessionScopedLookup(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"
`)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())
	t.Setenv("GC_BEADS", "file")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	targetWait, err := store.Create(beads.Bead{
		Title:  "target wait",
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:target-session"},
		Metadata: map[string]string{
			"session_id": "target-session",
			"state":      waitStatePending,
			"kind":       "manual",
		},
	})
	if err != nil {
		t.Fatalf("Create(target wait): %v", err)
	}
	for i := 0; i < waitLookupLimit; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("other wait %d", i),
			Type:   waitBeadType,
			Labels: []string{waitBeadLabel, "session:other-session"},
			Metadata: map[string]string{
				"session_id": "other-session",
				"state":      waitStatePending,
				"kind":       "manual",
			},
		}); err != nil {
			t.Fatalf("Create(other wait %d): %v", i, err)
		}
	}

	var stdout, stderr bytes.Buffer
	code := cmdWaitList("", "target-session", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdWaitList = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), targetWait.ID) {
		t.Fatalf("wait list output missing target wait %s:\nstdout=%s\nstderr=%s", targetWait.ID, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "other-session") {
		t.Fatalf("wait list output included non-target session:\n%s", stdout.String())
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

func TestCmdSessionWait_AllowsRigDependencyBeads(t *testing.T) {
	cityPath, rigPath := setupManagedBdWaitTestCity(t)

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
	dep, err := rigStore.Create(beads.Bead{Title: "rig dep"})
	if err != nil {
		t.Fatalf("create rig dep bead: %v", err)
	}
	if err := rigStore.Close(dep.ID); err != nil {
		t.Fatalf("close rig dep bead: %v", err)
	}
	if got := beadPrefix(nil, dep.ID); got != "fe" {
		t.Fatalf("rig dep prefix = %q, want %q", got, "fe")
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

func TestPrepareWaitWakeState_ResolvesRigDependencyBeads(t *testing.T) {
	cityPath, rigPath := setupManagedBdWaitTestCity(t)

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
	dep, err := rigStore.Create(beads.Bead{Title: "rig dep"})
	if err != nil {
		t.Fatalf("create rig dep bead: %v", err)
	}
	wait, err := cityStore.Create(beads.Bead{
		Title:  "wait:worker session",
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
	if err := rigStore.Close(dep.ID); err != nil {
		t.Fatalf("close rig dep bead: %v", err)
	}
	if got := beadPrefix(nil, dep.ID); got != "fe" {
		t.Fatalf("rig dep prefix = %q, want %q", got, "fe")
	}
	cityStore, err = openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt(reload): %v", err)
	}

	readyWaitSet, err := prepareWaitWakeStateForCity(cityPath, cityStore, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeStateForCity: %v", err)
	}
	if !readyWaitSet[sessionBead.ID] {
		t.Fatalf("readyWaitSet missing session %s", sessionBead.ID)
	}
	updatedWait, err := cityStore.Get(wait.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updatedWait.Metadata["state"]; got != waitStateReady {
		t.Fatalf("wait state = %q, want %q", got, waitStateReady)
	}
	if updatedWait.Metadata["ready_at"] == "" {
		t.Fatal("ready_at was not recorded")
	}
}

func setupFreshManagedBdWaitTestCity(t *testing.T) (string, string) {
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

	cityPath := shortSocketTempDir(t, "gc-bd-city-")
	rigPath, err := writeManagedBdWaitTestCityScaffold(cityPath)
	if err != nil {
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
	if err := initAndHookDir(cityPath, rigPath, "fe"); err != nil {
		t.Fatalf("initAndHookDir(rig): %v", err)
	}
	if err := publishManagedDoltRuntimeState(cityPath); err != nil {
		t.Fatalf("publishManagedDoltRuntimeState: %v", err)
	}
	return cityPath, rigPath
}

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
