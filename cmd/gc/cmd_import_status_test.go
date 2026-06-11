package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
