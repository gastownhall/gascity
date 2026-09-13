package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestValidateBeadsMetadataCASRequest(t *testing.T) {
	t.Parallel()

	valid := beadsMetadataCASRequest{
		beadID:      "ga-review_1.2",
		storeRef:    "rig:tributary",
		key:         "review_sha",
		expected:    "",
		next:        "",
		format:      "json",
		storeRefSet: true,
		keySet:      true,
		expectedSet: true,
		nextSet:     true,
	}
	if err := validateBeadsMetadataCASRequest(valid); err != nil {
		t.Fatalf("explicit empty expected/next rejected: %v", err)
	}
	for _, boundary := range []struct {
		name   string
		mutate func(*beadsMetadataCASRequest)
	}{
		{"bead id", func(r *beadsMetadataCASRequest) { r.beadID = strings.Repeat("i", metadataCASMaxBeadIDBytes) }},
		{"metadata key", func(r *beadsMetadataCASRequest) { r.key = strings.Repeat("k", metadataCASMaxKeyBytes) }},
		{"store name", func(r *beadsMetadataCASRequest) {
			r.storeRef = "rig:" + strings.Repeat("r", metadataCASMaxStoreNameBytes)
		}},
		{"expected value", func(r *beadsMetadataCASRequest) { r.expected = strings.Repeat("e", metadataCASMaxValueBytes) }},
		{"next value", func(r *beadsMetadataCASRequest) { r.next = strings.Repeat("n", metadataCASMaxValueBytes) }},
	} {
		t.Run("accepts maximum "+boundary.name, func(t *testing.T) {
			request := valid
			boundary.mutate(&request)
			if err := validateBeadsMetadataCASRequest(request); err != nil {
				t.Fatalf("boundary request rejected: %v", err)
			}
		})
	}

	tests := []struct {
		name   string
		mutate func(*beadsMetadataCASRequest)
		want   string
	}{
		{"missing store ref flag", func(r *beadsMetadataCASRequest) { r.storeRefSet = false }, "--store-ref is required"},
		{"missing key flag", func(r *beadsMetadataCASRequest) { r.keySet = false }, "--key is required"},
		{"missing expected flag", func(r *beadsMetadataCASRequest) { r.expectedSet = false }, "--expected is required"},
		{"missing next flag", func(r *beadsMetadataCASRequest) { r.nextSet = false }, "--next is required"},
		{"empty id", func(r *beadsMetadataCASRequest) { r.beadID = "" }, "invalid bead id"},
		{"unsafe id", func(r *beadsMetadataCASRequest) { r.beadID = "../other" }, "invalid bead id"},
		{"oversized id", func(r *beadsMetadataCASRequest) { r.beadID = strings.Repeat("i", metadataCASMaxBeadIDBytes+1) }, "invalid bead id"},
		{"unsafe key", func(r *beadsMetadataCASRequest) { r.key = "review/sha" }, "invalid metadata key"},
		{"oversized key", func(r *beadsMetadataCASRequest) { r.key = strings.Repeat("k", metadataCASMaxKeyBytes+1) }, "invalid metadata key"},
		{"bad scope", func(r *beadsMetadataCASRequest) { r.storeRef = "all:*" }, "invalid --store-ref"},
		{"unsafe scope name", func(r *beadsMetadataCASRequest) { r.storeRef = "rig:../other" }, "invalid --store-ref"},
		{"oversized scope name", func(r *beadsMetadataCASRequest) {
			r.storeRef = "rig:" + strings.Repeat("r", metadataCASMaxStoreNameBytes+1)
		}, "invalid --store-ref"},
		{"bad format", func(r *beadsMetadataCASRequest) { r.format = "yaml" }, "invalid --format"},
		{"oversized expected", func(r *beadsMetadataCASRequest) { r.expected = strings.Repeat("x", metadataCASMaxValueBytes+1) }, "--expected exceeds"},
		{"oversized next", func(r *beadsMetadataCASRequest) { r.next = strings.Repeat("x", metadataCASMaxValueBytes+1) }, "--next exceeds"},
		{"invalid utf8 expected", func(r *beadsMetadataCASRequest) { r.expected = string([]byte{0xff}) }, "--expected must be valid UTF-8"},
		{"invalid utf8 next", func(r *beadsMetadataCASRequest) { r.next = string([]byte{0xff}) }, "--next must be valid UTF-8"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := valid
			tc.mutate(&request)
			err := validateBeadsMetadataCASRequest(request)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestResolveBeadsMetadataCASOutputMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		request    beadsMetadataCASRequest
		wantFormat string
		wantErr    string
	}{
		{
			name:       "default text",
			request:    beadsMetadataCASRequest{format: "text"},
			wantFormat: "text",
		},
		{
			name:       "format json compatibility",
			request:    beadsMetadataCASRequest{format: "json", formatSet: true},
			wantFormat: "json",
		},
		{
			name:       "canonical json default",
			request:    beadsMetadataCASRequest{format: "text", jsonOut: true},
			wantFormat: "json",
		},
		{
			name:       "canonical and compatible json",
			request:    beadsMetadataCASRequest{format: "json", formatSet: true, jsonOut: true},
			wantFormat: "json",
		},
		{
			name:    "canonical conflicts with explicit text",
			request: beadsMetadataCASRequest{format: "text", formatSet: true, jsonOut: true},
			wantErr: "--json cannot be combined with --format=text",
		},
		{
			name:    "canonical does not hide invalid format",
			request: beadsMetadataCASRequest{format: "yaml", formatSet: true, jsonOut: true},
			wantErr: "invalid --format",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := tc.request
			err := resolveBeadsMetadataCASOutputMode(&request)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve output mode: %v", err)
			}
			if request.format != tc.wantFormat {
				t.Fatalf("format = %q, want %q", request.format, tc.wantFormat)
			}
		})
	}
}

func TestResolveBeadsMetadataCASStoreIsExact(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "tributary", Path: "tributary"},
			{Name: "unbound"},
		},
	}

	root, ref, err := resolveBeadsMetadataCASStore(cfg, cityPath, "city:demo")
	if err != nil || root != cityPath || ref != "city:demo" {
		t.Fatalf("city resolution = (%q, %q, %v)", root, ref, err)
	}
	root, ref, err = resolveBeadsMetadataCASStore(cfg, cityPath, "rig:tributary")
	if err != nil || root != filepath.Join(cityPath, "tributary") || ref != "rig:tributary" {
		t.Fatalf("rig resolution = (%q, %q, %v)", root, ref, err)
	}

	for _, storeRef := range []string{
		"city:other",
		"rig:missing",
		"rig:unbound",
		"city",
		"tributary",
		"all:*",
	} {
		if _, _, err := resolveBeadsMetadataCASStore(cfg, cityPath, storeRef); err == nil {
			t.Errorf("resolveBeadsMetadataCASStore(%q) succeeded, want error", storeRef)
		}
	}
}

func TestBeadsMetadataCASCanonicalJSONOutcomes(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)

	cityPath := writeMetadataCASTestCity(t)
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:    "review receipt",
		Metadata: beads.StringMap{"review_sha": "old"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) {
		return store, nil
	}, func(beads.Store) error {
		return nil
	})

	tests := []struct {
		name     string
		expected string
		next     string
		outcome  beads.MetadataCASOutcome
	}{
		{name: "swapped", expected: "old", next: "new", outcome: beads.MetadataCASSwapped},
		{name: "already next", expected: "old", next: "new", outcome: beads.MetadataCASAlreadyNext},
		{name: "conflict", expected: "old", next: "other", outcome: beads.MetadataCASConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runMetadataCASTestCommand(
				cityPath,
				bead.ID,
				"--expected="+tc.expected,
				"--next="+tc.next,
				"--json",
			)
			if code != 0 {
				t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr, stdout)
			}
			if stderr != "" {
				t.Fatalf("stderr=%q, want empty", stderr)
			}
			if strings.Count(stdout, "\n") != 1 {
				t.Fatalf("stdout is not exactly one JSON line: %q", stdout)
			}
			var result beadsMetadataCASResult
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatalf("Unmarshal: %v\n%s", err, stdout)
			}
			if result.Outcome != tc.outcome || !result.OK {
				t.Fatalf("result=%+v, want outcome=%q and ok", result, tc.outcome)
			}
		})
	}
}

func TestBeadsMetadataCASCanonicalJSONFailuresUseSharedContract(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)

	cityPath := writeMetadataCASTestCity(t)
	failureSchemaStdout, failureSchemaStderr := bytes.Buffer{}, bytes.Buffer{}
	if code := run([]string{"beads", "metadata-cas", "--json-schema=failure"}, &failureSchemaStdout, &failureSchemaStderr); code != 0 {
		t.Fatalf("failure schema code=%d stderr=%q stdout=%q", code, failureSchemaStderr.String(), failureSchemaStdout.String())
	}
	failureSchema := compileJSONSchema(t, "gc://schemas/failure.schema.json", failureSchemaStdout.Bytes())

	tests := []struct {
		name       string
		store      func(t *testing.T) (beads.Store, string)
		closeStore func(beads.Store) error
		extraArgs  []string
	}{
		{
			name: "validation",
			store: func(t *testing.T) (beads.Store, string) {
				t.Helper()
				return beads.NewMemStore(), "gc-1"
			},
			extraArgs: []string{"--next=new", "--json"},
		},
		{
			name: "unsupported capability",
			store: func(t *testing.T) (beads.Store, string) {
				t.Helper()
				backing, id := newMetadataCASTestMemStore(t)
				return metadataCASUnsupportedCommandStore{Store: backing}, id
			},
		},
		{
			name: "transport",
			store: func(t *testing.T) (beads.Store, string) {
				t.Helper()
				backing, id := newMetadataCASTestMemStore(t)
				return &metadataCASCommandFailureStore{
					Store:  backing,
					casErr: errors.New("metadata CAS transport failed"),
				}, id
			},
		},
		{
			name: "readback",
			store: func(t *testing.T) (beads.Store, string) {
				t.Helper()
				backing, id := newMetadataCASTestMemStore(t)
				return &metadataCASCommandFailureStore{
					Store:  backing,
					getErr: errors.New("metadata CAS readback failed"),
				}, id
			},
		},
		{
			name: "close",
			store: func(t *testing.T) (beads.Store, string) {
				t.Helper()
				return newMetadataCASTestMemStore(t)
			},
			closeStore: func(beads.Store) error {
				return errors.New("metadata CAS close failed")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, id := tc.store(t)
			closeStore := tc.closeStore
			if closeStore == nil {
				closeStore = func(beads.Store) error { return nil }
			}
			installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) {
				return store, nil
			}, closeStore)

			extraArgs := tc.extraArgs
			if extraArgs == nil {
				extraArgs = []string{"--expected=old", "--next=new", "--json"}
			}
			stdout, stderr, code := runMetadataCASTestCommand(cityPath, id, extraArgs...)
			if code == 0 {
				t.Fatalf("code=0, want nonzero; stderr=%q stdout=%q", stderr, stdout)
			}
			if strings.Count(stdout, "\n") != 1 {
				t.Fatalf("stdout is not exactly one shared failure JSON line: %q", stdout)
			}
			if strings.Contains(stdout, `"outcome"`) || strings.Contains(stdout, `"ok":true`) {
				t.Fatalf("success payload leaked before failure: %q", stdout)
			}
			var payload any
			if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
				t.Fatalf("failure stdout is not JSON: %v\n%s", err, stdout)
			}
			if err := failureSchema.Validate(payload); err != nil {
				t.Fatalf("failure payload does not match shared schema: %v\npayload=%s", err, stdout)
			}
		})
	}
}

func TestBeadsMetadataCASFormatCompatibilityAndConflict(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)

	cityPath := writeMetadataCASTestCity(t)
	store, id := newMetadataCASTestMemStore(t)
	installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) {
		return store, nil
	}, func(beads.Store) error {
		return nil
	})

	stdout, stderr, code := runMetadataCASTestCommand(
		cityPath, id, "--expected=old", "--next=new", "--format=json",
	)
	if code != 0 || stderr != "" {
		t.Fatalf("--format=json code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	var compatibility beadsMetadataCASResult
	if err := json.Unmarshal([]byte(stdout), &compatibility); err != nil || !compatibility.OK {
		t.Fatalf("--format=json payload=%q error=%v", stdout, err)
	}

	stdout, stderr, code = runMetadataCASTestCommand(
		cityPath, id, "--expected=old", "--next=new", "--json", "--format=json",
	)
	if code != 0 || stderr != "" {
		t.Fatalf("--json --format=json code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}

	stdout, stderr, code = runMetadataCASTestCommand(
		cityPath, id, "--expected=old", "--next=new", "--json", "--format=text",
	)
	if code == 0 {
		t.Fatalf("--json --format=text code=0; stderr=%q stdout=%q", stderr, stdout)
	}
	assertMetadataCASSharedFailureJSON(t, stdout)
	if !strings.Contains(stderr, "--json cannot be combined with --format=text") {
		t.Fatalf("stderr=%q, want format conflict diagnostic", stderr)
	}
}

func TestBeadsMetadataCASExactStoreWithDuplicateID(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)

	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "tributary")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "tributary"
path = "tributary"
prefix = "tr"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityPath); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	for _, root := range []string{cityPath, rigPath} {
		if err := ensurePersistedScopeLocalFileStore(root); err != nil {
			t.Fatalf("ensurePersistedScopeLocalFileStore(%q): %v", root, err)
		}
	}

	cityStore, err := openScopeLocalFileStore(cityPath)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	cityBead, err := cityStore.Create(beads.Bead{
		Title:    "duplicate city bead",
		Metadata: beads.StringMap{"review_sha": "city-old"},
	})
	if err != nil {
		t.Fatalf("Create city bead: %v", err)
	}
	rigStore, err := openScopeLocalFileStore(rigPath)
	if err != nil {
		t.Fatalf("open rig store: %v", err)
	}
	rigBead, err := rigStore.Create(beads.Bead{
		Title:    "duplicate rig bead",
		Metadata: beads.StringMap{"review_sha": "rig-old"},
	})
	if err != nil {
		t.Fatalf("Create rig bead: %v", err)
	}
	if cityBead.ID != rigBead.ID {
		t.Fatalf("fixture IDs differ: city=%q rig=%q", cityBead.ID, rigBead.ID)
	}

	runCAS := func(storeRef, id, expected, next string) beadsMetadataCASResult {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := run([]string{
			"--city", cityPath,
			"beads", "metadata-cas", id,
			"--store-ref", storeRef,
			"--key", "review_sha",
			"--expected=" + expected,
			"--next=" + next,
			"--json",
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("metadata-cas code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("metadata-cas stderr=%q", stderr.String())
		}
		var result beadsMetadataCASResult
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatalf("Unmarshal result: %v\n%s", err, stdout.String())
		}
		return result
	}

	if got := runCAS("rig:tributary", rigBead.ID, "rig-old", "rig-new"); got.Outcome != beads.MetadataCASSwapped {
		t.Fatalf("winning outcome=%q", got.Outcome)
	}

	cityCheck, err := openScopeLocalFileStore(cityPath)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	gotCity, err := cityCheck.Get(cityBead.ID)
	if err != nil {
		t.Fatalf("Get city bead: %v", err)
	}
	if gotCity.Metadata["review_sha"] != "city-old" {
		t.Fatalf("city duplicate mutated: metadata=%v", gotCity.Metadata)
	}
	rigCheck, err := openScopeLocalFileStore(rigPath)
	if err != nil {
		t.Fatalf("reopen rig store: %v", err)
	}
	gotRig, err := rigCheck.Get(rigBead.ID)
	if err != nil {
		t.Fatalf("Get rig bead: %v", err)
	}
	if gotRig.Metadata["review_sha"] != "rig-new" {
		t.Fatalf("rig bead metadata=%v, want review_sha=rig-new", gotRig.Metadata)
	}

	if got := runCAS("city:demo", cityBead.ID, "city-old", "city-new"); got.Outcome != beads.MetadataCASSwapped {
		t.Fatalf("city winning outcome=%q", got.Outcome)
	}
	cityCheck, err = openScopeLocalFileStore(cityPath)
	if err != nil {
		t.Fatalf("reopen city store after city CAS: %v", err)
	}
	gotCity, err = cityCheck.Get(cityBead.ID)
	if err != nil {
		t.Fatalf("Get city bead after city CAS: %v", err)
	}
	if gotCity.Metadata["review_sha"] != "city-new" {
		t.Fatalf("city bead metadata=%v, want review_sha=city-new", gotCity.Metadata)
	}
	rigCheck, err = openScopeLocalFileStore(rigPath)
	if err != nil {
		t.Fatalf("reopen rig store after city CAS: %v", err)
	}
	gotRig, err = rigCheck.Get(rigBead.ID)
	if err != nil {
		t.Fatalf("Get rig bead after city CAS: %v", err)
	}
	if gotRig.Metadata["review_sha"] != "rig-new" {
		t.Fatalf("rig duplicate mutated by city selection: metadata=%v", gotRig.Metadata)
	}
}

func TestBeadsMetadataCASMissingSelectedIDNeverSearchesOtherStore(t *testing.T) {
	tests := []struct {
		name         string
		selectedRef  string
		targetInCity bool
	}{
		{name: "city selected and id exists only in rig", selectedRef: "city:demo"},
		{name: "rig selected and id exists only in city", selectedRef: "rig:tributary", targetInCity: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			configureIsolatedRuntimeEnv(t)

			cityPath, rigPath := writeMetadataCASScopedFileCity(t)
			cityStore, err := openScopeLocalFileStore(cityPath)
			if err != nil {
				t.Fatalf("open city store: %v", err)
			}
			rigStore, err := openScopeLocalFileStore(rigPath)
			if err != nil {
				t.Fatalf("open rig store: %v", err)
			}

			create := func(store beads.Store, title string) beads.Bead {
				t.Helper()
				bead, err := store.Create(beads.Bead{
					Title:    title,
					Metadata: beads.StringMap{"review_sha": "old"},
				})
				if err != nil {
					t.Fatalf("Create(%s): %v", title, err)
				}
				return bead
			}

			var target beads.Bead
			if tc.targetInCity {
				_ = create(cityStore, "city overlap")
				target = create(cityStore, "city only")
				_ = create(rigStore, "rig overlap")
			} else {
				_ = create(cityStore, "city overlap")
				_ = create(rigStore, "rig overlap")
				target = create(rigStore, "rig only")
			}

			stdout, _, code := runMetadataCASTestCommandForStore(
				cityPath, tc.selectedRef, target.ID, "--expected=old", "--next=new", "--json",
			)
			if code == 0 {
				t.Fatalf("selected missing id unexpectedly succeeded: stdout=%q", stdout)
			}
			assertMetadataCASSharedFailureJSON(t, stdout)

			owner := rigStore
			if tc.targetInCity {
				owner = cityStore
			}
			got, err := owner.Get(target.ID)
			if err != nil {
				t.Fatalf("Get target from owning store: %v", err)
			}
			if got.Metadata["review_sha"] != "old" {
				t.Fatalf("target in unselected store was mutated: metadata=%v", got.Metadata)
			}
		})
	}
}

func TestBeadsMetadataCASLegacyFileLayoutAliasesRigToSharedCityStore(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)

	cityPath := writeMetadataCASTestCity(t)
	if fileStoreUsesScopedRoots(cityPath) {
		t.Fatal("legacy fixture unexpectedly has a scoped-layout marker")
	}
	cityStore, err := openScopeLocalFileStore(cityPath)
	if err != nil {
		t.Fatalf("open shared city store: %v", err)
	}
	bead, err := cityStore.Create(beads.Bead{
		Title:    "legacy shared review receipt",
		Metadata: beads.StringMap{"review_sha": "old"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stdout, stderr, code := runMetadataCASTestCommandForStore(
		cityPath, "rig:tributary", bead.ID, "--expected=old", "--next=new", "--json",
	)
	if code != 0 || stderr != "" {
		t.Fatalf("legacy rig alias code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	var result beadsMetadataCASResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, stdout)
	}
	if result.StoreRef != "rig:tributary" || result.Outcome != beads.MetadataCASSwapped {
		t.Fatalf("legacy alias result=%+v", result)
	}

	sharedCheck, err := openScopeLocalFileStore(cityPath)
	if err != nil {
		t.Fatalf("reopen shared city store: %v", err)
	}
	got, err := sharedCheck.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get shared bead: %v", err)
	}
	if got.Metadata["review_sha"] != "new" {
		t.Fatalf("shared city metadata=%v, want review_sha=new", got.Metadata)
	}
	if scopeUsesFileStoreContract(filepath.Join(cityPath, "tributary")) {
		t.Fatal("legacy rig unexpectedly gained its own file-store contract")
	}
}

func TestBeadsMetadataCASRejectsRemoteTarget(t *testing.T) {
	clearGCEnv(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--city-url", "https://example.invalid",
		"--city-name", "demo",
		"beads", "metadata-cas", "ga-1",
		"--store-ref", "city:demo",
		"--key", "review_sha",
		"--expected=",
		"--next=new",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("code=0, want nonzero; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "remote") || !strings.Contains(stderr.String(), "does not support") {
		t.Fatalf("stderr=%q, want remote unsupported diagnostic", stderr.String())
	}
}

func TestBeadsMetadataCASManifestAndRuntimePayloadsAreCoherent(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)

	var manifestStdout, manifestStderr bytes.Buffer
	code := run([]string{"beads", "metadata-cas", "--json-schema"}, &manifestStdout, &manifestStderr)
	if code != 0 {
		t.Fatalf("manifest code=%d stderr=%q stdout=%q", code, manifestStderr.String(), manifestStdout.String())
	}
	var manifest jsonSchemaManifest
	if err := json.Unmarshal(manifestStdout.Bytes(), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest: %v\n%s", err, manifestStdout.String())
	}
	if !manifest.JSONSupported {
		t.Fatalf("manifest does not declare JSON support: %+v", manifest)
	}
	if got := strings.Join(manifest.Command, " "); got != "beads metadata-cas" {
		t.Fatalf("manifest command=%q, want %q", got, "beads metadata-cas")
	}
	resultRaw := manifest.Schemas[jsonSchemaResultRole]
	failureRaw := manifest.Schemas[jsonSchemaFailureRole]
	if len(resultRaw) == 0 || len(failureRaw) == 0 {
		t.Fatalf("manifest schemas=%v, want result and failure", manifest.Schemas)
	}
	resultSchema := compileJSONSchema(t, "gc://schemas/beads/metadata-cas/result.schema.json", resultRaw)
	failureSchema := compileJSONSchema(t, "gc://schemas/failure.schema.json", failureRaw)

	cityPath := writeMetadataCASTestCity(t)
	store, id := newMetadataCASTestMemStore(t)
	installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) {
		return store, nil
	}, func(beads.Store) error {
		return nil
	})

	success, successStderr, successCode := runMetadataCASTestCommand(
		cityPath, id, "--expected=old", "--next=new", "--json",
	)
	if successCode != 0 || successStderr != "" {
		t.Fatalf("success code=%d stderr=%q stdout=%q", successCode, successStderr, success)
	}
	var successPayload any
	if err := json.Unmarshal([]byte(success), &successPayload); err != nil {
		t.Fatalf("Unmarshal success: %v\n%s", err, success)
	}
	if err := resultSchema.Validate(successPayload); err != nil {
		t.Fatalf("success payload does not match manifest result schema: %v\npayload=%s", err, success)
	}

	failure, _, failureCode := runMetadataCASTestCommand(
		cityPath, id, "--next=other", "--json",
	)
	if failureCode == 0 {
		t.Fatalf("validation failure code=0; stdout=%q", failure)
	}
	var failurePayload any
	if err := json.Unmarshal([]byte(failure), &failurePayload); err != nil {
		t.Fatalf("Unmarshal failure: %v\n%s", err, failure)
	}
	if err := failureSchema.Validate(failurePayload); err != nil {
		t.Fatalf("failure payload does not match manifest failure schema: %v\npayload=%s", err, failure)
	}
}

func writeMetadataCASTestCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "tributary"), 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "tributary"
path = "tributary"
prefix = "tr"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	return cityPath
}

func writeMetadataCASScopedFileCity(t *testing.T) (cityPath, rigPath string) {
	t.Helper()
	cityPath = writeMetadataCASTestCity(t)
	rigPath = filepath.Join(cityPath, "tributary")
	if err := ensureScopedFileStoreLayout(cityPath); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	for _, root := range []string{cityPath, rigPath} {
		if err := ensurePersistedScopeLocalFileStore(root); err != nil {
			t.Fatalf("ensurePersistedScopeLocalFileStore(%q): %v", root, err)
		}
	}
	return cityPath, rigPath
}

func newMetadataCASTestMemStore(t *testing.T) (*beads.MemStore, string) {
	t.Helper()
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:    "review receipt",
		Metadata: beads.StringMap{"review_sha": "old"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return store, bead.ID
}

func installMetadataCASStoreSeams(
	t *testing.T,
	open func(storePath, cityPath string) (beads.Store, error),
	closeStore func(beads.Store) error,
) {
	t.Helper()
	previousOpen := openBeadsMetadataCASStore
	previousClose := closeBeadsMetadataCASStore
	openBeadsMetadataCASStore = open
	closeBeadsMetadataCASStore = closeStore
	t.Cleanup(func() {
		openBeadsMetadataCASStore = previousOpen
		closeBeadsMetadataCASStore = previousClose
	})
}

func runMetadataCASTestCommand(cityPath, beadID string, extraArgs ...string) (stdout, stderr string, code int) {
	return runMetadataCASTestCommandForStore(cityPath, "city:demo", beadID, extraArgs...)
}

func runMetadataCASTestCommandForStore(cityPath, storeRef, beadID string, extraArgs ...string) (stdout, stderr string, code int) {
	args := []string{
		"--city", cityPath,
		"beads", "metadata-cas", beadID,
		"--store-ref", storeRef,
		"--key", "review_sha",
	}
	args = append(args, extraArgs...)
	var stdoutBuffer, stderrBuffer bytes.Buffer
	code = run(args, &stdoutBuffer, &stderrBuffer)
	return stdoutBuffer.String(), stderrBuffer.String(), code
}

func assertMetadataCASSharedFailureJSON(t *testing.T, stdout string) {
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

type metadataCASUnsupportedCommandStore struct {
	beads.Store
}

type metadataCASCommandFailureStore struct {
	beads.Store
	casErr error
	getErr error
}

func (s *metadataCASCommandFailureStore) CompareAndSetMetadataKey(_, _, _, _ string) (bool, error) {
	return false, s.casErr
}

func (s *metadataCASCommandFailureStore) Get(id string) (beads.Bead, error) {
	if s.getErr != nil {
		return beads.Bead{}, s.getErr
	}
	return s.Store.Get(id)
}
