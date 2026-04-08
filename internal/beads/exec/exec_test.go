package exec //nolint:revive // internal package, always imported with alias

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// writeScript creates an executable shell script in dir and returns its path.
func writeScript(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "beads-provider")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// allOpsScript returns a script body that handles all bead store operations
// with simple, predictable responses.
func allOpsScript() string {
	return `
op="$1"; shift

case "$op" in
  create)
    cat > /dev/null  # consume stdin
    echo '{"id":"EX-1","title":"test","status":"open","type":"task","created_at":"2026-02-27T10:00:00Z"}'
    ;;
  get)
    echo '{"id":"'"$1"'","title":"found","status":"open","type":"task","created_at":"2026-02-27T10:00:00Z"}'
    ;;
  update)
    cat > /dev/null  # consume stdin
    ;;
  close)
    ;;
  list)
    echo '[{"id":"EX-1","title":"alpha","status":"open","type":"task","created_at":"2026-02-27T10:00:00Z"},{"id":"EX-2","title":"beta","status":"closed","type":"bug","created_at":"2026-02-27T11:00:00Z"}]'
    ;;
  ready)
    echo '[{"id":"EX-1","title":"alpha","status":"open","type":"task","created_at":"2026-02-27T10:00:00Z"}]'
    ;;
  children)
    echo '[{"id":"EX-3","title":"child","status":"open","type":"task","created_at":"2026-02-27T10:00:00Z","parent_id":"'"$1"'"}]'
    ;;
  list-by-label)
    echo '[{"id":"EX-5","title":"labeled","status":"open","type":"task","created_at":"2026-02-27T10:00:00Z","labels":["'"$1"'"]}]'
    ;;
  set-metadata)
    cat > /dev/null  # consume stdin
    ;;
  *) exit 2 ;;  # unknown operation
esac
`
}

func TestCreate(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	s := NewStore(script)

	b, err := s.Create(beads.Bead{Title: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if b.ID != "EX-1" {
		t.Errorf("ID = %q, want %q", b.ID, "EX-1")
	}
	if b.Status != "open" {
		t.Errorf("Status = %q, want %q", b.Status, "open")
	}
	if b.Type != "task" {
		t.Errorf("Type = %q, want %q", b.Type, "task")
	}
}

func TestCreate_stdinReachesScript(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "stdin.json")

	script := writeScript(t, dir, `
op="$1"
case "$op" in
  create)
    cat > "`+outFile+`"
    echo '{"id":"EX-1","title":"test","status":"open","type":"bug","created_at":"2026-02-27T10:00:00Z"}'
    ;;
  *) exit 2 ;;
esac
`)
	s := NewStore(script)

	_, err := s.Create(beads.Bead{
		Title:    "my task",
		Type:     "bug",
		Labels:   []string{"pool:dog"},
		ParentID: "WP-1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}
	stdin := string(data)
	if !strings.Contains(stdin, `"title":"my task"`) {
		t.Errorf("stdin missing title, got: %s", stdin)
	}
	if !strings.Contains(stdin, `"type":"bug"`) {
		t.Errorf("stdin missing type, got: %s", stdin)
	}
	if !strings.Contains(stdin, `"pool:dog"`) {
		t.Errorf("stdin missing label, got: %s", stdin)
	}
	if !strings.Contains(stdin, `"parent_id":"WP-1"`) {
		t.Errorf("stdin missing parent_id, got: %s", stdin)
	}
}

func TestCreate_metadataReachesScript(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "stdin.json")

	script := writeScript(t, dir, `
op="$1"
case "$op" in
  create)
    cat > "`+outFile+`"
    echo '{"id":"EX-1","title":"test","status":"open","type":"session","created_at":"2026-02-27T10:00:00Z"}'
    ;;
  *) exit 2 ;;
esac
`)
	s := NewStore(script)

	_, err := s.Create(beads.Bead{
		Title:  "mayor",
		Type:   "session",
		Labels: []string{"gc:session"},
		Metadata: map[string]string{
			"session_name": "gascity-mayor",
			"agent_name":   "mayor",
			"state":        "stopped",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}
	stdin := string(data)
	if !strings.Contains(stdin, `"session_name":"gascity-mayor"`) {
		t.Errorf("stdin missing metadata session_name, got: %s", stdin)
	}
	if !strings.Contains(stdin, `"agent_name":"mayor"`) {
		t.Errorf("stdin missing metadata agent_name, got: %s", stdin)
	}
	if !strings.Contains(stdin, `"state":"stopped"`) {
		t.Errorf("stdin missing metadata state, got: %s", stdin)
	}
}

func TestCreate_metadataRoundTripsViaConformance(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}
	scriptPath, err := filepath.Abs(filepath.Join("testdata", "conformance.sh"))
	if err != nil {
		t.Fatal(err)
	}

	s := NewStore(scriptPath)
	s.SetEnv(map[string]string{"BEADS_DIR": t.TempDir()})

	created, err := s.Create(beads.Bead{
		Title:  "mayor",
		Type:   "session",
		Labels: []string{"gc:session"},
		Metadata: map[string]string{
			"session_name": "gascity-mayor",
			"agent_name":   "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify Create return value has normalized metadata and labels.
	if created.Metadata["session_name"] != "gascity-mayor" {
		t.Errorf("created.Metadata[session_name] = %q, want %q", created.Metadata["session_name"], "gascity-mayor")
	}
	if created.Metadata["agent_name"] != "mayor" {
		t.Errorf("created.Metadata[agent_name] = %q, want %q", created.Metadata["agent_name"], "mayor")
	}
	for _, l := range created.Labels {
		if strings.HasPrefix(l, "meta:") {
			t.Errorf("meta: label leaked into created.Labels: %s", l)
		}
	}

	// Verify Get returns the same normalized data.
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["session_name"] != "gascity-mayor" {
		t.Errorf("Metadata[session_name] = %q, want %q", got.Metadata["session_name"], "gascity-mayor")
	}
	if got.Metadata["agent_name"] != "mayor" {
		t.Errorf("Metadata[agent_name] = %q, want %q", got.Metadata["agent_name"], "mayor")
	}
	for _, l := range got.Labels {
		if strings.HasPrefix(l, "meta:") {
			t.Errorf("meta: label leaked into Labels: %s", l)
		}
	}
}

func TestUpdate_metadataRoundTripsViaConformance(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}
	scriptPath, err := filepath.Abs(filepath.Join("testdata", "conformance.sh"))
	if err != nil {
		t.Fatal(err)
	}

	s := NewStore(scriptPath)
	s.SetEnv(map[string]string{"BEADS_DIR": t.TempDir()})

	created, err := s.Create(beads.Bead{
		Title:  "mayor",
		Type:   "session",
		Labels: []string{"gc:session"},
		Metadata: map[string]string{
			"session_name": "gascity-mayor",
			"state":        "stopped",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Update metadata via Store.Update.
	if err := s.Update(created.ID, beads.UpdateOpts{
		Metadata: map[string]string{"state": "running"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["state"] != "running" {
		t.Errorf("Metadata[state] = %q after Update, want %q", got.Metadata["state"], "running")
	}
	// Original metadata should be preserved.
	if got.Metadata["session_name"] != "gascity-mayor" {
		t.Errorf("Metadata[session_name] = %q, want %q (should be preserved)", got.Metadata["session_name"], "gascity-mayor")
	}
	// No duplicate meta: labels should exist.
	for _, l := range got.Labels {
		if strings.HasPrefix(l, "meta:") {
			t.Errorf("meta: label leaked into Labels: %s", l)
		}
	}
}

func TestSetMetadata_deduplicatesViaConformance(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}
	scriptPath, err := filepath.Abs(filepath.Join("testdata", "conformance.sh"))
	if err != nil {
		t.Fatal(err)
	}

	s := NewStore(scriptPath)
	s.SetEnv(map[string]string{"BEADS_DIR": t.TempDir()})

	created, err := s.Create(beads.Bead{
		Title:  "test",
		Type:   "session",
		Labels: []string{"gc:session"},
		Metadata: map[string]string{
			"state": "stopped",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Overwrite via SetMetadata — should replace, not accumulate.
	if err := s.SetMetadata(created.ID, "state", "running"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["state"] != "running" {
		t.Errorf("Metadata[state] = %q, want %q", got.Metadata["state"], "running")
	}
}

// TestCreate_metadataWithSpecialCharsRoundTrips validates the exec.Store
// protocol contract: metadata values containing quotes and commas must
// round-trip correctly. This exercises conformance.sh (the reference
// provider), not gc-beads-k8s directly. The gc-beads-k8s fix was verified
// via K8s homelab deployment (see PR #367).
func TestCreate_metadataWithSpecialCharsRoundTrips(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}
	scriptPath, err := filepath.Abs(filepath.Join("testdata", "conformance.sh"))
	if err != nil {
		t.Fatal(err)
	}

	s := NewStore(scriptPath)
	s.SetEnv(map[string]string{"BEADS_DIR": t.TempDir()})

	created, err := s.Create(beads.Bead{
		Title:  "agent-session",
		Type:   "session",
		Labels: []string{"gc:session"},
		Metadata: map[string]string{
			"command":      `claude --settings "/city/.gc/settings.json"`,
			"csv_tricky":   `value,with,commas`,
			"session_name": "gascity-mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["command"] != `claude --settings "/city/.gc/settings.json"` {
		t.Errorf("Metadata[command] = %q, want value with quotes", got.Metadata["command"])
	}
	if got.Metadata["csv_tricky"] != "value,with,commas" {
		t.Errorf("Metadata[csv_tricky] = %q, want value with commas", got.Metadata["csv_tricky"])
	}
	if got.Metadata["session_name"] != "gascity-mayor" {
		t.Errorf("Metadata[session_name] = %q, want %q", got.Metadata["session_name"], "gascity-mayor")
	}
	for _, l := range got.Labels {
		if strings.HasPrefix(l, "meta:") {
			t.Errorf("meta: label leaked into Labels: %s", l)
		}
	}
}

// TestCreate_numericLookingMetadataStaysString validates the exec.Store
// protocol contract: numeric-looking metadata values must round-trip as
// strings. This exercises conformance.sh (the reference provider), not
// gc-beads-k8s directly.
func TestCreate_numericLookingMetadataStaysString(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}
	scriptPath, err := filepath.Abs(filepath.Join("testdata", "conformance.sh"))
	if err != nil {
		t.Fatal(err)
	}

	s := NewStore(scriptPath)
	s.SetEnv(map[string]string{"BEADS_DIR": t.TempDir()})

	created, err := s.Create(beads.Bead{
		Title: "quarantined-session",
		Type:  "session",
		Metadata: map[string]string{
			"wake_attempts":     "0",
			"quarantined_until": "",
			"churn_count":       "42",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Values that look numeric must round-trip as strings. bd's JSON column
	// can store 0 as a number; the script's jq must coerce with tostring.
	if got.Metadata["wake_attempts"] != "0" {
		t.Errorf("Metadata[wake_attempts] = %q, want %q", got.Metadata["wake_attempts"], "0")
	}
	if got.Metadata["quarantined_until"] != "" {
		t.Errorf("Metadata[quarantined_until] = %q, want empty string", got.Metadata["quarantined_until"])
	}
	if got.Metadata["churn_count"] != "42" {
		t.Errorf("Metadata[churn_count] = %q, want %q", got.Metadata["churn_count"], "42")
	}
}

func TestCreate_defaultsTypeToTask(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "stdin.json")

	script := writeScript(t, dir, `
case "$1" in
  create)
    cat > "`+outFile+`"
    echo '{"id":"EX-1","title":"test","status":"open","type":"task","created_at":"2026-02-27T10:00:00Z"}'
    ;;
  *) exit 2 ;;
esac
`)
	s := NewStore(script)
	_, err := s.Create(beads.Bead{Title: "test"})
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"type":"task"`) {
		t.Errorf("stdin should contain type=task, got: %s", string(data))
	}
}

func TestGet(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	s := NewStore(script)

	b, err := s.Get("EX-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b.ID != "EX-1" {
		t.Errorf("ID = %q, want %q", b.ID, "EX-1")
	}
	if b.Title != "found" {
		t.Errorf("Title = %q, want %q", b.Title, "found")
	}
}

func TestGet_notFound(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  get) echo "not found" >&2; exit 1 ;;
  *) exit 2 ;;
esac
`)
	s := NewStore(script)

	_, err := s.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestGet_notFound_noIssueFound(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  get) echo "no issue found matching \"$2\"" >&2; exit 1 ;;
  *) exit 2 ;;
esac
`)
	s := NewStore(script)

	_, err := s.Get("mayor")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestUpdate(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "stdin.json")

	script := writeScript(t, dir, `
case "$1" in
  update) cat > "`+outFile+`" ;;
  *) exit 2 ;;
esac
`)
	s := NewStore(script)

	desc := "new description"
	priority := 2
	err := s.Update("EX-1", beads.UpdateOpts{
		Description: &desc,
		Priority:    &priority,
		Labels:      []string{"extra"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	stdin := string(data)
	if !strings.Contains(stdin, `"description":"new description"`) {
		t.Errorf("stdin missing description, got: %s", stdin)
	}
	if !strings.Contains(stdin, `"priority":2`) {
		t.Errorf("stdin missing priority, got: %s", stdin)
	}
	if !strings.Contains(stdin, `"extra"`) {
		t.Errorf("stdin missing label, got: %s", stdin)
	}
}

func TestUpdate_assigneeRoundTripsThroughConformanceScript(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}
	scriptPath, err := filepath.Abs(filepath.Join("testdata", "conformance.sh"))
	if err != nil {
		t.Fatal(err)
	}

	s := NewStore(scriptPath)
	s.SetEnv(map[string]string{"BEADS_DIR": t.TempDir()})

	created, err := s.Create(beads.Bead{Title: "reassignable"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	assignee := "mayor"
	if err := s.Update(created.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Assignee != assignee {
		t.Errorf("Assignee = %q after Update, want %q", got.Assignee, assignee)
	}
}

func TestClose(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	s := NewStore(script)

	if err := s.Close("EX-1"); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	s := NewStore(script)

	got, err := s.ListOpen()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List returned %d beads, want 1 open bead", len(got))
	}
	if got[0].Title != "alpha" {
		t.Errorf("got[0].Title = %q, want %q", got[0].Title, "alpha")
	}
}

func TestList_statusFilter(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	s := NewStore(script)

	got, err := s.ListOpen("closed")
	if err != nil {
		t.Fatalf("List(closed): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List(closed) returned %d beads, want 1 closed bead", len(got))
	}
	if got[0].Title != "beta" {
		t.Errorf("got[0].Title = %q, want %q", got[0].Title, "beta")
	}
}

func TestList_empty(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  list) echo "[]" ;;
  *) exit 2 ;;
esac
`)
	s := NewStore(script)

	got, err := s.ListOpen()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List returned %d beads, want 0", len(got))
	}
}

func TestReady(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	s := NewStore(script)

	got, err := s.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Ready returned %d beads, want 1", len(got))
	}
	if got[0].Status != "open" {
		t.Errorf("got[0].Status = %q, want %q", got[0].Status, "open")
	}
}

func TestChildren(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	s := NewStore(script)

	got, err := s.Children("EX-1")
	if err != nil {
		t.Fatalf("Children: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Children returned %d beads, want 1", len(got))
	}
	if got[0].ParentID != "EX-1" {
		t.Errorf("got[0].ParentID = %q, want %q", got[0].ParentID, "EX-1")
	}
}

func TestListByLabel(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	s := NewStore(script)

	got, err := s.ListByLabel("order-run:lint", 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListByLabel returned %d beads, want 1", len(got))
	}
	if got[0].Title != "labeled" {
		t.Errorf("got[0].Title = %q, want %q", got[0].Title, "labeled")
	}
}

func TestSetMetadata(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "meta.txt")

	script := writeScript(t, dir, `
case "$1" in
  set-metadata) cat > "`+outFile+`" ;;
  *) exit 2 ;;
esac
`)
	s := NewStore(script)

	if err := s.SetMetadata("EX-1", "merge_strategy", "mr"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "mr" {
		t.Errorf("metadata value = %q, want %q", string(data), "mr")
	}
}

func TestGet_numericMetadataValuesCoercedToStrings(t *testing.T) {
	dir := t.TempDir()

	// Script returns metadata with non-string values — this is what bd does
	// in production. The Go domain model is map[string]string, so the parser
	// must coerce non-string JSON values to their string representation.
	script := writeScript(t, dir, `
case "$1" in
  get)
    echo '{"id":"EX-1","title":"test","status":"open","type":"task","created_at":"2026-01-01T00:00:00Z","metadata":{"retries":3,"score":1.5,"flag":true,"name":"ok"}}'
    ;;
  *) exit 2 ;;
esac
`)
	s := NewStore(script)

	got, err := s.Get("EX-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["retries"] != "3" {
		t.Errorf("Metadata[retries] = %q, want %q", got.Metadata["retries"], "3")
	}
	if got.Metadata["score"] != "1.5" {
		t.Errorf("Metadata[score] = %q, want %q", got.Metadata["score"], "1.5")
	}
	if got.Metadata["flag"] != "true" {
		t.Errorf("Metadata[flag] = %q, want %q", got.Metadata["flag"], "true")
	}
	if got.Metadata["name"] != "ok" {
		t.Errorf("Metadata[name] = %q, want %q", got.Metadata["name"], "ok")
	}
}

func TestList_numericMetadataValuesCoercedToStrings(t *testing.T) {
	dir := t.TempDir()

	script := writeScript(t, dir, `
case "$1" in
  list)
    echo '[{"id":"EX-1","title":"test","status":"open","type":"task","created_at":"2026-01-01T00:00:00Z","metadata":{"retries":3,"name":"ok"}}]'
    ;;
  *) exit 2 ;;
esac
`)
	s := NewStore(script)

	got, err := s.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListOpen returned %d beads, want 1", len(got))
	}
	if got[0].Metadata["retries"] != "3" {
		t.Errorf("Metadata[retries] = %q, want %q", got[0].Metadata["retries"], "3")
	}
	if got[0].Metadata["name"] != "ok" {
		t.Errorf("Metadata[name] = %q, want %q", got[0].Metadata["name"], "ok")
	}
}

// --- Error handling ---

func TestErrorPropagation(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
echo "something went wrong" >&2
exit 1
`)
	s := NewStore(script)

	_, err := s.ListOpen()
	if err == nil {
		t.Fatal("expected error from exit 1, got nil")
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Errorf("error = %q, want stderr content", err.Error())
	}
}

func TestUnknownOperation_exit2(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `exit 2`)
	s := NewStore(script)

	// Exit 2 → unknown operation → treated as success.
	// List returns empty because stdout is empty.
	got, err := s.ListOpen()
	if err != nil {
		t.Fatalf("exit 2 should be treated as success, got: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List returned %d beads on exit 2, want 0", len(got))
	}
}

func TestTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("slow test")
	}

	dir := t.TempDir()
	script := writeScript(t, dir, `sleep 60`)
	s := NewStore(script)
	s.timeout = 500 * time.Millisecond

	start := time.Now()
	_, err := s.ListOpen()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 5*time.Second {
		t.Errorf("timeout took %v, expected ~500ms", elapsed)
	}
}

func TestCreate_badJSON(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  create) cat > /dev/null; echo '{not json' ;;
  *) exit 2 ;;
esac
`)
	s := NewStore(script)

	_, err := s.Create(beads.Bead{Title: "test"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "parsing JSON") {
		t.Errorf("error = %q, want to contain 'parsing JSON'", err)
	}
}

// --- Conformance suite ---

func TestExecStoreConformance(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}
	scriptPath, err := filepath.Abs(filepath.Join("testdata", "conformance.sh"))
	if err != nil {
		t.Fatal(err)
	}
	beadstest.RunStoreTests(t, func() beads.Store {
		dir := t.TempDir()
		s := NewStore(scriptPath)
		s.SetEnv(map[string]string{"BEADS_DIR": dir})
		return s
	})
}

// --- Compile-time interface check ---

var _ beads.Store = (*Store)(nil)
