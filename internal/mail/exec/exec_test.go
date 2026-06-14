package exec //nolint:revive // internal package, always imported with alias

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/mail"
)

// writeScript creates an executable shell script in dir and returns its path.
func writeScript(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "mail-provider")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// allOpsScript returns a script body that handles all mail operations with
// simple, predictable responses.
func allOpsScript() string {
	return `
op="$1"

case "$op" in
  ensure-running) ;; # no-op, stateless
  send)
    # consume stdin, echo a JSON message
    cat > /dev/null
    echo '{"id":"m-1","from":"human","to":"'"$2"'","body":"test","created_at":"2025-06-15T10:30:00Z"}'
    ;;
  inbox|check)
    echo '[{"id":"m-1","from":"human","to":"'"$2"'","body":"hello","created_at":"2025-06-15T10:30:00Z"}]'
    ;;
  read)
    echo '{"id":"'"$2"'","from":"human","to":"mayor","body":"read me","created_at":"2025-06-15T10:30:00Z"}'
    ;;
  archive)
    ;; # success, no output
  *) exit 2 ;; # unknown operation
esac
`
}

func TestSend(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script)

	m, err := p.Send("human", "mayor", "", "hello")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if m.ID != "m-1" {
		t.Errorf("ID = %q, want %q", m.ID, "m-1")
	}
	if m.To != "mayor" {
		t.Errorf("To = %q, want %q", m.To, "mayor")
	}
}

func TestSend_stdinReachesScript(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "stdin.json")

	script := writeScript(t, dir, `
op="$1"
case "$op" in
  ensure-running) exit 2 ;; # stateless
  send) cat > "`+outFile+`"
    echo '{"id":"m-1","from":"x","to":"y","body":"z","created_at":"2025-06-15T10:30:00Z"}'
    ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	_, err := p.Send("alice", "bob", "", "test body")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}

	var input sendInput
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatalf("unmarshal stdin: %v", err)
	}
	if input.From != "alice" {
		t.Errorf("stdin From = %q, want %q", input.From, "alice")
	}
	if input.Body != "test body" {
		t.Errorf("stdin Body = %q, want %q", input.Body, "test body")
	}
}

func TestInbox(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script)

	msgs, err := p.Inbox("mayor")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Inbox = %d messages, want 1", len(msgs))
	}
	if msgs[0].ID != "m-1" {
		t.Errorf("ID = %q, want %q", msgs[0].ID, "m-1")
	}
}

func TestInbox_empty(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  ensure-running) exit 2 ;;
  inbox) ;; # empty stdout = no messages
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	msgs, err := p.Inbox("mayor")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("Inbox = %d messages, want 0", len(msgs))
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script)

	m, err := p.Read("m-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if m.ID != "m-1" {
		t.Errorf("ID = %q, want %q", m.ID, "m-1")
	}
	if m.Body != "read me" {
		t.Errorf("Body = %q, want %q", m.Body, "read me")
	}
}

func TestArchive(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script)

	if err := p.Archive("m-1"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
}

func TestArchiveDoesNotConsumeCallerStdin(t *testing.T) {
	dir := t.TempDir()
	stdinLog := filepath.Join(dir, "stdin.log")
	script := writeScript(t, dir, `
op="$1"
if IFS= read -r line; then
  printf '%s:%s\n' "$op" "$line" >> "`+stdinLog+`"
else
  printf '%s:EOF\n' "$op" >> "`+stdinLog+`"
fi

case "$op" in
  ensure-running|archive) ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	callerStdinPath := filepath.Join(dir, "caller-stdin")
	if err := os.WriteFile(callerStdinPath, []byte("caller-owned\nsecond-line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	callerStdin, err := os.Open(callerStdinPath)
	if err != nil {
		t.Fatal(err)
	}
	defer callerStdin.Close() //nolint:errcheck

	originalStdin := os.Stdin
	os.Stdin = callerStdin
	t.Cleanup(func() {
		os.Stdin = originalStdin
	})

	if err := p.Archive("m-1"); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	data, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("read stdin log: %v", err)
	}
	got := string(data)
	want := "ensure-running:EOF\narchive:EOF\n"
	if got != want {
		t.Fatalf("script stdin log = %q, want %q", got, want)
	}
	pos, err := callerStdin.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("caller stdin position: %v", err)
	}
	if pos != 0 {
		t.Fatalf("caller stdin offset = %d, want 0", pos)
	}
}

func TestCheck(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script)

	msgs, err := p.Check("mayor")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Check = %d messages, want 1", len(msgs))
	}
}

func TestCheck_empty(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  ensure-running) exit 2 ;;
  check) ;; # empty stdout = no messages
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	msgs, err := p.Check("mayor")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("Check = %d messages, want 0", len(msgs))
	}
}

// --- ensure-running ---

func TestEnsureRunning_calledOnce(t *testing.T) {
	dir := t.TempDir()
	countFile := filepath.Join(dir, "count")
	os.WriteFile(countFile, []byte("0"), 0o644) //nolint:errcheck

	script := writeScript(t, dir, `
case "$1" in
  ensure-running)
    count=$(cat "`+countFile+`")
    echo $((count + 1)) > "`+countFile+`"
    ;;
  inbox) echo '[]' ;;
  check) echo '[]' ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	// Multiple operations should only call ensure-running once.
	p.Inbox("a") //nolint:errcheck
	p.Check("b") //nolint:errcheck
	p.Inbox("c") //nolint:errcheck

	data, _ := os.ReadFile(countFile)
	count := strings.TrimSpace(string(data))
	if count != "1" {
		t.Errorf("ensure-running called %s times, want 1", count)
	}
}

func TestEnsureRunning_exit2Stateless(t *testing.T) {
	dir := t.TempDir()
	// Script that exits 2 for ensure-running (stateless — no server needed).
	script := writeScript(t, dir, `
case "$1" in
  ensure-running) exit 2 ;;
  inbox) echo '[]' ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	// Should not fail even though ensure-running exits 2.
	msgs, err := p.Inbox("mayor")
	if err != nil {
		t.Fatalf("Inbox after ensure-running exit 2: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("Inbox = %d messages, want 0", len(msgs))
	}
}

// --- Error handling ---

func TestErrorPropagation(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  ensure-running) exit 2 ;;
  *)
    echo "something went wrong" >&2
    exit 1
    ;;
esac
`)
	p := NewProvider(script)

	_, err := p.Read("m-1")
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
	p := NewProvider(script)

	// Exit 2 for archive means "unknown operation" → treated as success.
	if err := p.Archive("m-1"); err != nil {
		t.Fatalf("exit 2 should be treated as success, got: %v", err)
	}
}

func TestTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("slow test")
	}

	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  ensure-running) exit 2 ;;
  *) sleep 60 ;;
esac
`)
	p := NewProvider(script)
	p.timeout = 500 * time.Millisecond

	start := time.Now()
	if err := p.Archive("m-1"); err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Errorf("timeout took %v, expected ~500ms", elapsed)
	}
}

// --- JSON wire format ---

func TestMarshalSendInput(t *testing.T) {
	data, err := marshalSendInput("alice", "", "hello world")
	if err != nil {
		t.Fatalf("marshalSendInput: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, `"from":"alice"`) {
		t.Errorf("missing from field: %s", s)
	}
	if !strings.Contains(s, `"body":"hello world"`) {
		t.Errorf("missing body field: %s", s)
	}
}

func TestUnmarshalMessage(t *testing.T) {
	input := `{"id":"m-1","from":"human","to":"mayor","body":"test","created_at":"2025-06-15T10:30:00Z"}`
	m, err := unmarshalMessage(input)
	if err != nil {
		t.Fatalf("unmarshalMessage: %v", err)
	}
	if m.ID != "m-1" {
		t.Errorf("ID = %q, want %q", m.ID, "m-1")
	}
	if m.From != "human" {
		t.Errorf("From = %q, want %q", m.From, "human")
	}
	if m.To != "mayor" {
		t.Errorf("To = %q, want %q", m.To, "mayor")
	}
	if m.Body != "test" {
		t.Errorf("Body = %q, want %q", m.Body, "test")
	}
	want := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
	if !m.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", m.CreatedAt, want)
	}
}

func TestUnmarshalMessages(t *testing.T) {
	input := `[{"id":"m-1","from":"a","to":"b","body":"hello","created_at":"2025-06-15T10:30:00Z"},{"id":"m-2","from":"c","to":"d","body":"world","created_at":"2025-06-15T11:00:00Z"}]`
	msgs, err := unmarshalMessages(input)
	if err != nil {
		t.Fatalf("unmarshalMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].ID != "m-1" {
		t.Errorf("msgs[0].ID = %q, want %q", msgs[0].ID, "m-1")
	}
	if msgs[1].ID != "m-2" {
		t.Errorf("msgs[1].ID = %q, want %q", msgs[1].ID, "m-2")
	}
}

// --- Compile-time interface check ---

var _ mail.Provider = (*Provider)(nil)
