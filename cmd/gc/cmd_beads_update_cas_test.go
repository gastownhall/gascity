package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/rogpeppe/go-internal/testscript"
)

func TestBeadsUpdateCASScript(t *testing.T) {
	testscript.Run(t, newTestscriptParams(t, filepath.Join("testdata", "beads-update-cas.txtar")))
}

func TestDecodeBeadsUpdateCASPatchIsStrictAndBounded(t *testing.T) {
	t.Parallel()

	patch, err := decodeBeadsUpdateCASPatch(strings.NewReader(`{
  "title":"human title",
  "description":"human description",
  "status":"in_progress",
  "priority":1,
  "type":"feature",
  "metadata":{"github.projection_hash":"sha256:abc"}
}`))
	if err != nil {
		t.Fatalf("decode valid patch: %v", err)
	}
	if patch.Title == nil || *patch.Title != "human title" ||
		patch.Description == nil || *patch.Description != "human description" ||
		patch.Status == nil || *patch.Status != "in_progress" ||
		patch.Priority == nil || *patch.Priority != 1 ||
		patch.Type == nil || *patch.Type != "feature" ||
		patch.Metadata["github.projection_hash"] != "sha256:abc" {
		t.Fatalf("decoded patch = %+v", patch)
	}

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "empty", body: `{}`, want: "at least one"},
		{name: "only null", body: `{"title":null}`, want: "at least one"},
		{name: "unknown field", body: `{"title":"x","labels":["unsafe"]}`, want: "unknown field"},
		{name: "trailing value", body: `{"title":"x"} {"title":"y"}`, want: "single JSON object"},
		{name: "oversized", body: `{"description":"` + strings.Repeat("x", beadsUpdateCASMaxPatchBytes) + `"}`, want: "exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeBeadsUpdateCASPatch(strings.NewReader(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestApplyBeadsUpdateCASReturnsOnlyOutcomeAndRevisions(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{Title: "old", Description: "private old"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	title := "new"
	description := "private new"
	request := beadsUpdateCASRequest{
		beadID:           bead.ID,
		storeRef:         "rig:tributary",
		expectedRevision: bead.Revision,
	}
	patch := beadsUpdateCASPatch{Title: &title, Description: &description}

	result, err := applyBeadsUpdateCAS(store, request, patch)
	if err != nil {
		t.Fatalf("applyBeadsUpdateCAS: %v", err)
	}
	if result.Outcome != beads.UpdateCASUpdated || result.Revision == bead.Revision {
		t.Fatalf("result = %+v", result)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, content := range []string{"old", "new", "private"} {
		if bytes.Contains(raw, []byte(content)) {
			t.Fatalf("result leaked patch content %q: %s", content, raw)
		}
	}
}

func TestValidateBeadsUpdateCASRequest(t *testing.T) {
	t.Parallel()

	valid := beadsUpdateCASRequest{
		beadID:              "ga-1",
		storeRef:            "rig:tributary",
		expectedRevision:    1,
		requestFile:         "-",
		format:              "json",
		storeRefSet:         true,
		expectedRevisionSet: true,
		requestFileSet:      true,
	}
	if err := validateBeadsUpdateCASRequest(valid); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*beadsUpdateCASRequest)
		want   string
	}{
		{name: "missing store", mutate: func(r *beadsUpdateCASRequest) { r.storeRefSet = false }, want: "--store-ref is required"},
		{name: "missing revision", mutate: func(r *beadsUpdateCASRequest) { r.expectedRevisionSet = false }, want: "--expected-revision is required"},
		{name: "zero revision", mutate: func(r *beadsUpdateCASRequest) { r.expectedRevision = 0 }, want: "nonzero opaque token"},
		{name: "missing request file", mutate: func(r *beadsUpdateCASRequest) { r.requestFileSet = false }, want: "--request-file is required"},
		{name: "unsafe id", mutate: func(r *beadsUpdateCASRequest) { r.beadID = "../ga-1" }, want: "invalid bead id"},
		{name: "bad store", mutate: func(r *beadsUpdateCASRequest) { r.storeRef = "all:*" }, want: "invalid --store-ref"},
		{name: "empty request file", mutate: func(r *beadsUpdateCASRequest) { r.requestFile = "" }, want: "--request-file"},
		{name: "bad format", mutate: func(r *beadsUpdateCASRequest) { r.format = "yaml" }, want: "invalid --format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := valid
			tc.mutate(&request)
			err := validateBeadsUpdateCASRequest(request)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestBeadsUpdateCASCanonicalJSONOutcomesDoNotLeakPatchContent(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)

	cityPath := writeMetadataCASTestCity(t)
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{Title: "old title", Description: "old private body"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	installUpdateCASStoreSeams(t, func(_, _ string) (beads.Store, error) {
		return wrapStoreWithBeadPolicies(store, nil), nil
	}, func(beads.Store) error { return nil })

	writePatch := func(name, title, description string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name+".json")
		raw, err := json.Marshal(map[string]string{"title": title, "description": description})
		if err != nil {
			t.Fatalf("Marshal patch: %v", err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("WriteFile patch: %v", err)
		}
		return path
	}
	winningPatch := writePatch("winning", "new title", "new private body")
	conflictingPatch := writePatch("conflicting", "other title", "other private body")

	tests := []struct {
		name    string
		patch   string
		outcome beads.UpdateCASOutcome
	}{
		{name: "updated", patch: winningPatch, outcome: beads.UpdateCASUpdated},
		{name: "replay", patch: winningPatch, outcome: beads.UpdateCASAlreadyApplied},
		{name: "conflict", patch: conflictingPatch, outcome: beads.UpdateCASConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runUpdateCASTestCommand(cityPath, bead.ID, bead.Revision, tc.patch)
			if code != 0 || stderr != "" {
				t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr, stdout)
			}
			if strings.Count(stdout, "\n") != 1 {
				t.Fatalf("stdout is not one JSON line: %q", stdout)
			}
			var result beadsUpdateCASResult
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatalf("Unmarshal result: %v\n%s", err, stdout)
			}
			if !result.OK || result.Outcome != tc.outcome || result.BeadID != bead.ID || result.StoreRef != "city:demo" {
				t.Fatalf("result = %+v, want outcome %q", result, tc.outcome)
			}
			for _, secret := range []string{"old title", "new title", "other title", "private body"} {
				if strings.Contains(stdout, secret) {
					t.Fatalf("stdout leaked patch content %q: %s", secret, stdout)
				}
			}
		})
	}
}

func TestBeadsUpdateCASOpensOnlyTheSelectedExactStore(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)

	cityPath := writeMetadataCASTestCity(t)
	rigPath := filepath.Join(cityPath, "tributary")
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{Title: "old"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var openedRoot string
	installUpdateCASStoreSeams(t, func(storeRoot, gotCityPath string) (beads.Store, error) {
		openedRoot = storeRoot
		if gotCityPath != cityPath {
			t.Fatalf("city path = %q, want %q", gotCityPath, cityPath)
		}
		return store, nil
	}, func(beads.Store) error { return nil })
	patchPath := filepath.Join(t.TempDir(), "patch.json")
	if err := os.WriteFile(patchPath, []byte(`{"title":"new"}`), 0o600); err != nil {
		t.Fatalf("WriteFile patch: %v", err)
	}

	stdout, stderr, code := runUpdateCASTestCommandForStore(
		cityPath, "rig:tributary", bead.ID, bead.Revision, patchPath,
	)
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	if openedRoot != rigPath {
		t.Fatalf("opened root = %q, want exact rig root %q", openedRoot, rigPath)
	}
}

func TestBeadsUpdateCASFailuresUseSharedJSONContract(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)

	cityPath := writeMetadataCASTestCity(t)
	backing := beads.NewMemStore()
	bead, err := backing.Create(beads.Bead{Title: "old"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	installUpdateCASStoreSeams(t, func(_, _ string) (beads.Store, error) {
		return updateCASUnsupportedCommandStore{Store: backing}, nil
	}, func(beads.Store) error { return nil })
	patchPath := filepath.Join(t.TempDir(), "patch.json")
	if err := os.WriteFile(patchPath, []byte(`{"title":"new"}`), 0o600); err != nil {
		t.Fatalf("WriteFile patch: %v", err)
	}

	stdout, _, code := runUpdateCASTestCommand(cityPath, bead.ID, bead.Revision, patchPath)
	if code == 0 {
		t.Fatalf("code=0, want nonzero; stdout=%q", stdout)
	}
	var payload jsonSchemaErrorPayload
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("failure is not JSON: %v\n%s", err, stdout)
	}
	if payload.OK || payload.Error.ExitCode == 0 || payload.Error.Code != "command_failed" {
		t.Fatalf("failure payload = %+v", payload)
	}
}

func TestBeadsUpdateCASTransportAndCloseFailuresAreNonZero(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)

	cityPath := writeMetadataCASTestCity(t)
	patchPath := filepath.Join(t.TempDir(), "patch.json")
	if err := os.WriteFile(patchPath, []byte(`{"title":"must-not-leak"}`), 0o600); err != nil {
		t.Fatalf("WriteFile patch: %v", err)
	}

	for _, tc := range []struct {
		name       string
		open       func(store *beads.MemStore) beads.Store
		closeStore func(beads.Store) error
	}{
		{
			name: "transport",
			open: func(store *beads.MemStore) beads.Store {
				return &updateCASCommandFailureStore{MemStore: store, updateErr: errors.New("transport unavailable")}
			},
			closeStore: func(beads.Store) error { return nil },
		},
		{
			name:       "close",
			open:       func(store *beads.MemStore) beads.Store { return store },
			closeStore: func(beads.Store) error { return errors.New("close failed") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			bead, err := store.Create(beads.Bead{Title: "base"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			installUpdateCASStoreSeams(t, func(_, _ string) (beads.Store, error) {
				return tc.open(store), nil
			}, tc.closeStore)

			stdout, _, code := runUpdateCASTestCommand(cityPath, bead.ID, bead.Revision, patchPath)
			if code == 0 {
				t.Fatalf("code=0, want nonzero; stdout=%q", stdout)
			}
			assertUpdateCASSharedFailureJSON(t, stdout)
			if strings.Contains(stdout, "must-not-leak") {
				t.Fatalf("failure payload leaked patch content: %s", stdout)
			}
		})
	}
}

func TestBeadsUpdateCASManifestMatchesRuntimeResult(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)

	var manifestStdout, manifestStderr bytes.Buffer
	if code := run([]string{"beads", "update-cas", "--json-schema"}, &manifestStdout, &manifestStderr); code != 0 {
		t.Fatalf("manifest code=%d stderr=%q stdout=%q", code, manifestStderr.String(), manifestStdout.String())
	}
	var manifest jsonSchemaManifest
	if err := json.Unmarshal(manifestStdout.Bytes(), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest: %v\n%s", err, manifestStdout.String())
	}
	if !manifest.JSONSupported || strings.Join(manifest.Command, " ") != "beads update-cas" {
		t.Fatalf("manifest = %+v", manifest)
	}
	resultSchema := compileJSONSchema(t, "gc://schemas/beads/update-cas/result.schema.json", manifest.Schemas[jsonSchemaResultRole])

	cityPath := writeMetadataCASTestCity(t)
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{Title: "old"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	installUpdateCASStoreSeams(t, func(_, _ string) (beads.Store, error) { return store, nil }, func(beads.Store) error { return nil })
	patchPath := filepath.Join(t.TempDir(), "patch.json")
	if err := os.WriteFile(patchPath, []byte(`{"title":"new"}`), 0o600); err != nil {
		t.Fatalf("WriteFile patch: %v", err)
	}
	stdout, stderr, code := runUpdateCASTestCommand(cityPath, bead.ID, bead.Revision, patchPath)
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	var payload any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("Unmarshal result: %v\n%s", err, stdout)
	}
	if err := resultSchema.Validate(payload); err != nil {
		t.Fatalf("result does not match schema: %v\n%s", err, stdout)
	}
}

func installUpdateCASStoreSeams(
	t *testing.T,
	open func(storePath, cityPath string) (beads.Store, error),
	closeStore func(beads.Store) error,
) {
	t.Helper()
	previousOpen := openBeadsUpdateCASStore
	previousClose := closeBeadsUpdateCASStore
	openBeadsUpdateCASStore = open
	closeBeadsUpdateCASStore = closeStore
	t.Cleanup(func() {
		openBeadsUpdateCASStore = previousOpen
		closeBeadsUpdateCASStore = previousClose
	})
}

func runUpdateCASTestCommand(cityPath, beadID string, revision int64, patchPath string) (stdout, stderr string, code int) {
	return runUpdateCASTestCommandForStore(cityPath, "city:demo", beadID, revision, patchPath)
}

func runUpdateCASTestCommandForStore(cityPath, storeRef, beadID string, revision int64, patchPath string) (stdout, stderr string, code int) {
	args := []string{
		"--city", cityPath,
		"beads", "update-cas", beadID,
		"--store-ref", storeRef,
		"--expected-revision", strconv.FormatInt(revision, 10),
		"--request-file", patchPath,
		"--json",
	}
	var stdoutBuffer, stderrBuffer bytes.Buffer
	code = run(args, &stdoutBuffer, &stderrBuffer)
	return stdoutBuffer.String(), stderrBuffer.String(), code
}

type updateCASUnsupportedCommandStore struct {
	beads.Store
}

type updateCASCommandFailureStore struct {
	*beads.MemStore
	updateErr error
}

func (s *updateCASCommandFailureStore) UpdateIfMatch(string, int64, beads.UpdateOpts) error {
	return s.updateErr
}

func assertUpdateCASSharedFailureJSON(t *testing.T, stdout string) {
	t.Helper()
	if strings.Count(stdout, "\n") != 1 {
		t.Fatalf("stdout is not exactly one JSON line: %q", stdout)
	}
	var payload jsonSchemaErrorPayload
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("Unmarshal shared failure: %v\n%s", err, stdout)
	}
	if payload.OK || payload.SchemaVersion != "1" ||
		payload.Error.Code != "command_failed" || payload.Error.ExitCode == 0 {
		t.Fatalf("shared failure payload=%+v", payload)
	}
}
