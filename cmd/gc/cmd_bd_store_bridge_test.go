package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

type blockedCloseBridgeStore struct {
	*beads.MemStore
	combinedErr error
	closeErr    error
	combined    int
	retried     int
	closed      int
}

func (s *blockedCloseBridgeStore) Update(id string, opts beads.UpdateOpts) error {
	if opts.Status != nil && *opts.Status == "closed" {
		s.combined++
		return s.combinedErr
	}
	s.retried++
	return s.MemStore.Update(id, opts)
}

func (s *blockedCloseBridgeStore) Close(id string) error {
	s.closed++
	return s.MemStore.Close(id)
}

func (s *blockedCloseBridgeStore) AtomicTx() bool { return true }

func (s *blockedCloseBridgeStore) Tx(_ string, fn func(beads.Tx) error) error {
	tx := &blockedCloseBridgeTx{store: s}
	if err := fn(tx); err != nil {
		return err
	}
	for _, update := range tx.updates {
		if err := s.MemStore.Update(update.id, update.opts); err != nil {
			return err
		}
	}
	if tx.closeID != "" {
		return s.MemStore.Close(tx.closeID)
	}
	return nil
}

type blockedCloseBridgeUpdate struct {
	id   string
	opts beads.UpdateOpts
}

type blockedCloseBridgeTx struct {
	store   *blockedCloseBridgeStore
	updates []blockedCloseBridgeUpdate
	closeID string
}

func (tx *blockedCloseBridgeTx) Create(beads.Bead) (beads.Bead, error) {
	return beads.Bead{}, errors.New("unexpected Create in close compatibility transaction")
}

func (tx *blockedCloseBridgeTx) Update(id string, opts beads.UpdateOpts) error {
	tx.store.retried++
	tx.updates = append(tx.updates, blockedCloseBridgeUpdate{id: id, opts: opts})
	return nil
}

func (tx *blockedCloseBridgeTx) SetMetadataBatch(id string, kvs map[string]string) error {
	return tx.Update(id, beads.UpdateOpts{Metadata: kvs})
}

func (tx *blockedCloseBridgeTx) Close(id string) error {
	tx.store.closed++
	if tx.store.closeErr != nil {
		return tx.store.closeErr
	}
	tx.closeID = id
	return nil
}

func TestBdStoreBridgeUpdateConvergesTypedClosePolicyBeforeExecBoundary(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		combinedErr  error
		wantErr      error
		wantRetry    int
		wantClose    int
		wantStatus   string
		wantTitle    string
		wantMetadata string
		statusOnly   bool
		closeErr     error
	}{
		{
			name:         "typed_blocked_close_retries_fields_then_force_closes",
			combinedErr:  fmt.Errorf("bridge update: %w", beads.ErrCloseBlocked),
			wantRetry:    1,
			wantClose:    1,
			wantStatus:   "closed",
			wantTitle:    "updated before forced close",
			wantMetadata: "pass",
		},
		{
			name:         "typed_open_children_retries_fields_then_force_closes",
			combinedErr:  fmt.Errorf("bridge update: %w", beads.ErrCloseOpenChildren),
			wantRetry:    1,
			wantClose:    1,
			wantStatus:   "closed",
			wantTitle:    "updated before forced close",
			wantMetadata: "pass",
		},
		{
			name:        "typed_open_children_status_only_force_closes_without_empty_update",
			combinedErr: fmt.Errorf("bridge update: %w", beads.ErrCloseOpenChildren),
			wantClose:   1,
			wantStatus:  "closed",
			wantTitle:   "original",
			statusOnly:  true,
		},
		{
			name:        "atomic_force_close_failure_rolls_back_sibling_fields",
			combinedErr: fmt.Errorf("bridge update: %w", beads.ErrCloseBlocked),
			closeErr:    errors.New("injected close failure"),
			wantErr:     errors.New("injected close failure"),
			wantRetry:   1,
			wantClose:   1,
			wantStatus:  "open",
			wantTitle:   "original",
		},
		{
			name:        "generic_failure_never_retries_or_force_closes",
			combinedErr: errors.New("transport failed"),
			wantErr:     errors.New("transport failed"),
			wantStatus:  "open",
			wantTitle:   "original",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			created, err := mem.Create(beads.Bead{Title: "original", Status: "open"})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			store := &blockedCloseBridgeStore{MemStore: mem, combinedErr: tc.combinedErr, closeErr: tc.closeErr}
			status := "closed"
			title := "updated before forced close"
			opts := beads.UpdateOpts{
				Status:   &status,
				Title:    &title,
				Metadata: map[string]string{"outcome": "pass"},
			}
			if tc.statusOnly {
				opts = beads.UpdateOpts{Status: &status}
			}
			err = updateBdStoreBridge(store, created.ID, opts)
			if tc.wantErr != nil {
				if err == nil || !errors.Is(err, tc.wantErr) && !strings.Contains(err.Error(), tc.wantErr.Error()) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("updateBdStoreBridge: %v", err)
			}
			if store.combined != 1 || store.retried != tc.wantRetry || store.closed != tc.wantClose {
				t.Fatalf("calls combined/retried/closed = %d/%d/%d, want 1/%d/%d", store.combined, store.retried, store.closed, tc.wantRetry, tc.wantClose)
			}
			got, getErr := mem.Get(created.ID)
			if getErr != nil {
				t.Fatalf("get: %v", getErr)
			}
			if got.Status != tc.wantStatus || got.Title != tc.wantTitle || got.Metadata["outcome"] != tc.wantMetadata {
				t.Fatalf("bead after bridge update = %+v, want status=%q title=%q outcome=%q", got, tc.wantStatus, tc.wantTitle, tc.wantMetadata)
			}
		})
	}
}

type nonAtomicBlockedCloseBridgeStore struct{ *blockedCloseBridgeStore }

func (*nonAtomicBlockedCloseBridgeStore) AtomicTx() bool { return false }

func TestBdStoreBridgeUpdateFailsClosedWithoutAtomicFallback(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	created, err := mem.Create(beads.Bead{Title: "original", Status: "open"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	base := &blockedCloseBridgeStore{
		MemStore:    mem,
		combinedErr: fmt.Errorf("bridge update: %w", beads.ErrCloseOpenChildren),
	}
	store := &nonAtomicBlockedCloseBridgeStore{blockedCloseBridgeStore: base}
	closed := "closed"
	title := "must not persist"
	err = updateBdStoreBridge(store, created.ID, beads.UpdateOpts{Status: &closed, Title: &title})
	if !errors.Is(err, beads.ErrCloseOpenChildren) {
		t.Fatalf("error = %v, want original typed refusal", err)
	}
	if base.retried != 0 || base.closed != 0 {
		t.Fatalf("non-atomic fallback attempted retry/close = %d/%d", base.retried, base.closed)
	}
	got, getErr := mem.Get(created.ID)
	if getErr != nil {
		t.Fatalf("get: %v", getErr)
	}
	if got.Status != "open" || got.Title != "original" {
		t.Fatalf("non-atomic refusal mutated bead: %+v", got)
	}
}

func withTestStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	defer func() {
		os.Stdin = old
		_ = r.Close()
	}()
	fn()
}

func writeFakeBdBridgeScript(t *testing.T, binDir, envFile, argsFile string) {
	t.Helper()
	path := filepath.Join(binDir, "bd")
	script := `#!/bin/sh
set -eu
printf 'BEADS_DIR=%s
GC_DOLT_HOST=%s
GC_DOLT_PORT=%s
GC_DOLT_USER=%s
GC_DOLT_PASSWORD=%s
BEADS_DOLT_SERVER_HOST=%s
BEADS_DOLT_SERVER_PORT=%s
BEADS_DOLT_SERVER_USER=%s
BEADS_DOLT_PASSWORD=%s
BEADS_DOLT_SERVER_DATABASE=%s
BEADS_CREDENTIALS_FILE=%s
GC_BEADS=%s
GC_BEADS_BACKEND=%s
BEADS_BACKEND=%s
GC_BEADS_PREFIX=%s
BD_EXPORT_AUTO=%s
' \
  "${BEADS_DIR:-}" "${GC_DOLT_HOST:-}" "${GC_DOLT_PORT:-}" "${GC_DOLT_USER:-}" "${GC_DOLT_PASSWORD:-}" \
  "${BEADS_DOLT_SERVER_HOST:-}" "${BEADS_DOLT_SERVER_PORT:-}" "${BEADS_DOLT_SERVER_USER:-}" "${BEADS_DOLT_PASSWORD:-}" \
  "${BEADS_DOLT_SERVER_DATABASE:-}" "${BEADS_CREDENTIALS_FILE:-}" "${GC_BEADS:-}" "${GC_BEADS_BACKEND:-}" "${BEADS_BACKEND:-}" "${GC_BEADS_PREFIX:-}" \
  "${BD_EXPORT_AUTO:-}" > "` + envFile + `"
printf '%s
' "$*" > "` + argsFile + `"
if [ "${1:-}" = "--dolt-auto-commit" ]; then
  shift 2
fi
case "${1:-}" in
  create)
    cat <<'JSON'
{"id":"BD-1","title":"captured","status":"open","issue_type":"task","created_at":"2026-02-27T10:00:00Z"}
JSON
    ;;
  show)
    cat <<'JSON'
[{"id":"BD-1","title":"captured","status":"open","issue_type":"task","created_at":"2026-02-27T10:00:00Z"}]
JSON
    ;;
  list)
    cat <<'JSON'
[{"id":"BD-1","title":"captured","status":"open","issue_type":"message","assignee":"mayor","created_at":"2026-02-27T10:00:00Z"}]
JSON
    ;;
  update)
    exit 0
    ;;
  dep)
    if [ "${2:-}" = "list" ]; then
      cat <<'JSON'
[{"id":"BD-2","dependency_type":"blocks"}]
JSON
      exit 0
    fi
    exit 2
    ;;
  *)
    exit 2
    ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestBdStoreBridgeCreateCmdProjectsCanonicalEnvAndClearsAmbientAuthority(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "bridge.env")
	argsFile := filepath.Join(t.TempDir(), "bridge.args")
	writeFakeBdBridgeScript(t, binDir, envFile, argsFile)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "wrong-db")
	t.Setenv("BEADS_CREDENTIALS_FILE", "/tmp/stale-creds")
	t.Setenv("GC_BEADS", "ambient-bd")
	t.Setenv("GC_BEADS_PREFIX", "ambient-prefix")
	t.Setenv("GC_DOLT_PASSWORD", "secret")
	var stdout, stderr bytes.Buffer

	withTestStdin(t, `{"title":"captured","type":"task","labels":["triage"]}`+"\n", func() {
		code := run([]string{
			"bd-store-bridge",
			"--dir", scopeDir,
			"--host", "db.example.internal",
			"--port", "3317",
			"--user", "root",
			"create",
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
		}
	})

	var bead map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &bead); err != nil {
		t.Fatalf("stdout JSON: %v\n%s", err, stdout.String())
	}
	if bead["id"] != "BD-1" || bead["type"] != "task" {
		t.Fatalf("unexpected bead payload: %#v", bead)
	}

	envText, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("ReadFile(env): %v", err)
	}
	envMap := readExecCaptureEnv(t, envFile)
	if got := envMap["BEADS_DIR"]; got != filepath.Join(scopeDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got, filepath.Join(scopeDir, ".beads"))
	}
	if got := envMap["GC_DOLT_HOST"]; got != "db.example.internal" {
		t.Fatalf("GC_DOLT_HOST = %q, want db.example.internal", got)
	}
	if got := envMap["GC_DOLT_PORT"]; got != "3317" {
		t.Fatalf("GC_DOLT_PORT = %q, want 3317", got)
	}
	if got := envMap["BEADS_DOLT_SERVER_DATABASE"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_DATABASE = %q, want empty after sanitization\n%s", got, string(envText))
	}
	if got := envMap["BEADS_CREDENTIALS_FILE"]; got != "" {
		t.Fatalf("BEADS_CREDENTIALS_FILE = %q, want empty after sanitization\n%s", got, string(envText))
	}
	if got := envMap["GC_BEADS"]; got != "" {
		t.Fatalf("GC_BEADS = %q, want empty after sanitization\n%s", got, string(envText))
	}
	if got := envMap["GC_BEADS_PREFIX"]; got != "" {
		t.Fatalf("GC_BEADS_PREFIX = %q, want empty after sanitization\n%s", got, string(envText))
	}
	if got := envMap["BD_EXPORT_AUTO"]; got != "false" {
		t.Fatalf("BD_EXPORT_AUTO = %q, want false to suppress bridge auto-export\n%s", got, string(envText))
	}

	argsText, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile(args): %v", err)
	}
	for _, want := range []string{"create", "--json", "captured", "-t", "task", "--labels", "triage"} {
		if !strings.Contains(string(argsText), want) {
			t.Fatalf("bd args missing %q: %s", want, string(argsText))
		}
	}
}

func TestBdStoreBridgeGetCmdReturnsBead(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "bridge.env")
	argsFile := filepath.Join(t.TempDir(), "bridge.args")
	writeFakeBdBridgeScript(t, binDir, envFile, argsFile)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"bd-store-bridge",
		"--dir", scopeDir,
		"--host", "db.example.internal",
		"--port", "3317",
		"get",
		"BD-1",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}

	var bead bdStoreBridgeBead
	if err := json.Unmarshal(stdout.Bytes(), &bead); err != nil {
		t.Fatalf("stdout JSON: %v\n%s", err, stdout.String())
	}
	if bead.ID != "BD-1" || bead.Title != "captured" || bead.Type != "task" {
		t.Fatalf("unexpected bead payload: %#v", bead)
	}

	argsText, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile(args): %v", err)
	}
	if got := strings.TrimSpace(string(argsText)); got != "show --json BD-1" {
		t.Fatalf("get args = %q, want %q", got, "show --json BD-1")
	}
}

func TestBdStoreBridgeDoltliteClearsDoltServerEnv(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeDir, ".beads", "metadata.json"), []byte(`{"backend":"doltlite","database":"doltlite"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "bridge.env")
	argsFile := filepath.Join(t.TempDir(), "bridge.args")
	writeFakeBdBridgeScript(t, binDir, envFile, argsFile)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_BEADS_BACKEND", "doltlite")
	var stdout, stderr bytes.Buffer
	withTestStdin(t, `{"title":"captured","type":"task"}`+"\n", func() {
		code := run([]string{
			"bd-store-bridge",
			"--dir", scopeDir,
			"--host", "db.example.internal",
			"--port", "3317",
			"--user", "root",
			"create",
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
		}
	})

	envMap := readExecCaptureEnv(t, envFile)
	if got := envMap["GC_BEADS_BACKEND"]; got != "doltlite" {
		t.Fatalf("GC_BEADS_BACKEND = %q, want doltlite", got)
	}
	if got := envMap["BEADS_BACKEND"]; got != "doltlite" {
		t.Fatalf("BEADS_BACKEND = %q, want doltlite", got)
	}
	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_AUTO_START"} {
		if got := envMap[key]; got != "" {
			t.Fatalf("%s = %q, want empty for doltlite bridge", key, got)
		}
	}
}

func TestBdStoreBridgeDepListCmdReturnsJSON(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "bridge.env")
	argsFile := filepath.Join(t.TempDir(), "bridge.args")
	writeFakeBdBridgeScript(t, binDir, envFile, argsFile)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"bd-store-bridge",
		"--dir", scopeDir,
		"--host", "db.example.internal",
		"--port", "3317",
		"dep-list",
		"BD-1",
		"up",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != `[{"issue_id":"BD-2","depends_on_id":"BD-1","type":"blocks"}]` {
		t.Fatalf("stdout = %q", got)
	}
	argsText, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile(args): %v", err)
	}
	if !strings.Contains(string(argsText), "dep list BD-1 --json --direction=up") {
		t.Fatalf("dep-list args = %q", string(argsText))
	}
}

func TestBdStoreBridgeUpdateCommandPassesType(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "bridge.env")
	argsFile := filepath.Join(t.TempDir(), "bridge.args")
	writeFakeBdBridgeScript(t, binDir, envFile, argsFile)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	withTestStdin(t, `{"type":"bug"}`+"\n", func() {
		code := run([]string{
			"bd-store-bridge",
			"--dir", scopeDir,
			"--host", "db.example.internal",
			"--port", "3317",
			"update",
			"BD-1",
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
		}
	})
	argsText, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile(args): %v", err)
	}
	for _, want := range []string{"update", "--json", "BD-1", "--type", "bug"} {
		if !strings.Contains(string(argsText), want) {
			t.Fatalf("update args missing %q: %s", want, string(argsText))
		}
	}
}

func TestBdStoreBridgeListCommandForwardsFilters(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "bridge.env")
	argsFile := filepath.Join(t.TempDir(), "bridge.args")
	writeFakeBdBridgeScript(t, binDir, envFile, argsFile)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"bd-store-bridge",
		"--dir", scopeDir,
		"--host", "db.example.internal",
		"--port", "3317",
		"list",
		"--status=open",
		"--assignee=mayor",
		"--type=message",
		"--limit=7",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); !strings.Contains(got, `"id":"BD-1"`) {
		t.Fatalf("stdout = %q", got)
	}
	argsText, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile(args): %v", err)
	}
	// BdStore may need to merge and filter policy tiers client-side, so it
	// requests all server-side candidates and applies the requested limit after
	// parsing.
	for _, want := range []string{"list", "--json", "--status=open", "--assignee=mayor", "--type=message", "--include-infra", "--include-gates", "--limit", "0"} {
		if !strings.Contains(string(argsText), want) {
			t.Fatalf("list args missing %q: %s", want, string(argsText))
		}
	}
}
