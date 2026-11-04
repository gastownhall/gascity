package gastown_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeBindingSnapshot writes a binding-snapshot-shaped directory: a JSONL
// and, unless partial, a manifest recording sha (the JSONL's own when empty).
func writeBindingSnapshot(t *testing.T, archive, id, jsonl, sha string, partial bool) {
	t.Helper()
	dir := filepath.Join(archive, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "binding.jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}
	if partial {
		return
	}
	if sha == "" {
		sum := sha256.Sum256([]byte(jsonl))
		sha = hex.EncodeToString(sum[:])
	}
	manifest := fmt.Sprintf(`{"format":"gc.storage.snapshot.manifest/v1","id":%q,"stores":[{"name":"binding","jsonl":{"path":"binding.jsonl","bytes":%d,"sha256":%q}}]}`, id, len(jsonl), sha)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestJsonlExportArchivesTheNewestBindingSnapshot is the S6 hook: the newest
// complete binding snapshot's JSONL, proven against its manifest, is committed
// as gc-binding/issues.jsonl. A newer interrupted snapshot (no manifest) is
// not a snapshot and is skipped; a JSONL that does not match its manifest is
// never archived; a city with no binding archive archives nothing.
func TestJsonlExportArchivesTheNewestBindingSnapshot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	run := func(t *testing.T, setup func(archive string)) (archiveRepo string) {
		t.Helper()
		cityDir, binDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
		archiveRepo = filepath.Join(cityDir, "archive")
		writeMultiRecordDoltStub(t, binDir, 3)
		writeJsonlExportGCStub(t, binDir)
		if setup != nil {
			setup(filepath.Join(stateDir, "binding-archive"))
		}
		env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, filepath.Join(t.TempDir(), "gc.log"), filepath.Join(t.TempDir(), "mail.log"))
		runScript(t, coreScriptPath("jsonl-export.sh"), env)
		return archiveRepo
	}
	committed := func(t *testing.T, repo, path string) (string, bool) {
		t.Helper()
		out, err := exec.Command("git", "-C", repo, "show", "HEAD:"+path).CombinedOutput()
		return string(out), err == nil
	}

	t.Run("newest complete snapshot", func(t *testing.T) {
		repo := run(t, func(archive string) {
			writeBindingSnapshot(t, archive, "20261101T000000Z", "old\n", "", false)
			writeBindingSnapshot(t, archive, "20261102T000000Z", "new\n", "", false)
			writeBindingSnapshot(t, archive, "20261103T000000Z", "partial\n", "", true)
		})
		got, ok := committed(t, repo, "gc-binding/issues.jsonl")
		if !ok || got != "new\n" {
			t.Fatalf("archived binding JSONL = %q (%v), want the newest complete snapshot's", got, ok)
		}
		if id, _ := committed(t, repo, "gc-binding/snapshot-id"); strings.TrimSpace(id) != "20261102T000000Z" {
			t.Fatalf("snapshot-id = %q", id)
		}
	})
	t.Run("hash mismatch", func(t *testing.T) {
		repo := run(t, func(archive string) {
			writeBindingSnapshot(t, archive, "20261102T000000Z", "tampered\n", strings.Repeat("0", 64), false)
		})
		if _, ok := committed(t, repo, "gc-binding/issues.jsonl"); ok {
			t.Fatal("a JSONL that does not match its manifest was archived")
		}
	})
	t.Run("no binding", func(t *testing.T) {
		repo := run(t, nil)
		if _, err := os.Stat(filepath.Join(repo, "gc-binding")); !os.IsNotExist(err) {
			t.Fatalf("a city with no binding archive got gc-binding (%v)", err)
		}
	})
}
