package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

func importStatusLockFixture(t *testing.T, dir string) string {
	t.Helper()
	fetched := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := packman.WriteLockfile(fsys.OSFS{}, dir, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			"https://example.com/tools.git":  {Version: "1.4.2", Commit: "aaaa", Fetched: fetched},
			"https://example.com/base.git":   {Version: "2.0.0", Commit: "bbbb", Fetched: fetched},
			"https://example.com/worker.git": {Version: "3.1.0", Commit: "cccc", Fetched: fetched},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, packman.LockfileName))
	if err != nil {
		t.Fatalf("ReadFile(packs.lock): %v", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestDoImportStatusJSONGolden pins the exact machine-readable document
// emitted by "gc import status --json": the declared import set with
// source paths and lock pins, plus the packs.lock closure and its
// content hash (ga-qcnpu1). Drift checkers consume this instead of
// parsing "gc import list" text.
func TestDoImportStatusJSONGolden(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"

[imports.local]
source = "./packs/local"

[defaults.rig.imports.worker]
source = "https://example.com/worker.git"
version = "^3.0"
`)
	lockSHA := importStatusLockFixture(t, dir)

	var stdout, stderr bytes.Buffer
	code := doImportStatus(dir, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	want := fmt.Sprintf(`{
  "schema_version": "1",
  "ok": true,
  "root": %[1]q,
  "packs_lock_path": %[2]q,
  "packs_lock_sha256": %[3]q,
  "imports": [
    {
      "name": "default-rig:worker",
      "source": "https://example.com/worker.git",
      "constraint": "^3.0",
      "kind": "remote",
      "pin": {
        "version": "3.1.0",
        "commit": "cccc",
        "fetched": "2026-01-02T03:04:05Z"
      }
    },
    {
      "name": "pack:local",
      "source": "./packs/local",
      "kind": "path",
      "path": %[4]q
    },
    {
      "name": "pack:tools",
      "source": "https://example.com/tools.git",
      "constraint": "^1.4",
      "kind": "remote",
      "pin": {
        "version": "1.4.2",
        "commit": "aaaa",
        "fetched": "2026-01-02T03:04:05Z"
      }
    }
  ],
  "locked_packs": [
    {
      "source": "https://example.com/base.git",
      "version": "2.0.0",
      "commit": "bbbb",
      "fetched": "2026-01-02T03:04:05Z"
    },
    {
      "source": "https://example.com/tools.git",
      "version": "1.4.2",
      "commit": "aaaa",
      "fetched": "2026-01-02T03:04:05Z"
    },
    {
      "source": "https://example.com/worker.git",
      "version": "3.1.0",
      "commit": "cccc",
      "fetched": "2026-01-02T03:04:05Z"
    }
  ]
}
`, dir, filepath.Join(dir, packman.LockfileName), lockSHA, filepath.Join(dir, "packs", "local"))

	if got := stdout.String(); got != want {
		t.Fatalf("gc import status --json output mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestDoImportStatusJSONIncludesRigScopedImports asserts the status
// document covers rig-scoped [rigs.imports.*] bindings — the drift
// surface that "gc import list" only shows with an explicit --rig flag.
func TestDoImportStatusJSONIncludesRigScopedImports(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, `
[workspace]
name = "demo"

[[rigs]]
name = "myrig"

[rigs.imports.extra]
source = "/opt/packs/extra"
`)
	writePackToml(t, dir, "[pack]\nname = \"demo\"\nschema = 1\n")

	var stdout, stderr bytes.Buffer
	code := doImportStatus(dir, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, `"name": "rig:myrig:extra"`) {
		t.Fatalf("missing rig-scoped import entry:\n%s", out)
	}
	if !strings.Contains(out, `"source": "/opt/packs/extra"`) {
		t.Fatalf("missing rig-scoped import source:\n%s", out)
	}
	if !strings.Contains(out, `"kind": "path"`) {
		t.Fatalf("missing path kind for rig-scoped import:\n%s", out)
	}
}

// TestDoImportStatusJSONOmitsLockHashWhenMissing confirms a city
// without packs.lock emits no packs_lock_sha256 and an empty (not
// null) locked_packs array.
func TestDoImportStatusJSONOmitsLockHashWhenMissing(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, "[pack]\nname = \"demo\"\nschema = 1\n")

	var stdout, stderr bytes.Buffer
	code := doImportStatus(dir, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "packs_lock_sha256") {
		t.Fatalf("packs_lock_sha256 present despite missing packs.lock:\n%s", out)
	}
	if !strings.Contains(out, `"locked_packs": []`) {
		t.Fatalf("locked_packs should be an empty array:\n%s", out)
	}
	if !strings.Contains(out, `"imports": []`) {
		t.Fatalf("imports should be an empty array:\n%s", out)
	}
}

// TestDoImportStatusTextShowsPins covers the human-readable default:
// one line per import with its lock pin, prefixed by the lock hash.
func TestDoImportStatusTextShowsPins(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"
`)
	lockSHA := importStatusLockFixture(t, dir)

	var stdout, stderr bytes.Buffer
	code := doImportStatus(dir, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "packs.lock sha256: "+lockSHA) {
		t.Fatalf("missing lock hash line:\n%s", out)
	}
	if !strings.Contains(out, "pack:tools\thttps://example.com/tools.git\t^1.4\tremote\t1.4.2\taaaa") {
		t.Fatalf("missing pinned import line:\n%s", out)
	}
}

// TestImportStatusJSONProductionRun exercises "gc import status --json"
// through the full production CLI path — including the JSON contract
// gate in run(), which rejects --json with json_unsupported unless the
// command declares an embedded result schema — and validates the
// emitted document against that schema. doImportStatus-level tests
// bypass this gate, so only this test proves the machine-readable
// drift surface is reachable in production. The fixture declares a
// pinned remote, an unlocked remote, and a path import so the schema
// validation covers every entry shape the command can emit.
func TestImportStatusJSONProductionRun(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"

[imports.local]
source = "./packs/local"

[imports.unlocked]
source = "https://example.com/unlocked.git"
version = "^9.9"
`)
	importStatusLockFixture(t, dir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", dir, "import", "status", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(import status --json) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if strings.Contains(stdout.String(), "json_unsupported") {
		t.Fatalf("production path rejected --json:\n%s", stdout.String())
	}

	var doc struct {
		Imports []struct {
			Kind string          `json:"kind"`
			Pin  json.RawMessage `json:"pin"`
		} `json:"imports"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("production document is not JSON: %v\n%s", err, stdout.String())
	}
	shapes := map[string]bool{}
	for _, imp := range doc.Imports {
		switch {
		case imp.Kind == "path":
			shapes["path"] = true
		case len(imp.Pin) > 0:
			shapes["pinned remote"] = true
		default:
			shapes["unlocked remote"] = true
		}
	}
	for _, shape := range []string{"pinned remote", "unlocked remote", "path"} {
		if !shapes[shape] {
			t.Fatalf("fixture emitted no %s import entry, so the schema validation no longer covers that shape:\n%s", shape, stdout.String())
		}
	}

	assertTopLevelOKTrue(t, stdout.Bytes())
	validateJSONAgainstResultSchema(t, []string{"import", "status"}, stdout.Bytes())
}

// TestImportStatusJSONSchemaManifest asserts the --json-schema manifest
// reports JSON support for "gc import status" with a valid embedded
// result schema, so drift checkers can discover the contract.
func TestImportStatusJSONSchemaManifest(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"import", "status", "--json-schema"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(import status --json-schema) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	var manifest struct {
		Command       []string                   `json:"command"`
		JSONSupported bool                       `json:"json_supported"`
		Schemas       map[string]json.RawMessage `json:"schemas"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("manifest is not JSON: %v\n%s", err, stdout.String())
	}
	if got := strings.Join(manifest.Command, " "); got != "import status" {
		t.Fatalf("manifest command = %q, want \"import status\"", got)
	}
	if !manifest.JSONSupported {
		t.Fatalf("manifest json_supported = false, want true:\n%s", stdout.String())
	}
	if !json.Valid(manifest.Schemas["result"]) {
		t.Fatalf("result schema missing or invalid: %s", manifest.Schemas["result"])
	}
}

// TestImportStatusCommandRegistered asserts "gc import status" is wired
// into the import command tree with a --json flag.
func TestImportStatusCommandRegistered(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newImportCmd(&stdout, &stderr)
	for _, sub := range cmd.Commands() {
		if sub.Name() == "status" {
			if sub.Flags().Lookup("json") == nil {
				t.Fatal("gc import status is missing the --json flag")
			}
			return
		}
	}
	t.Fatal("gc import status subcommand not registered")
}
