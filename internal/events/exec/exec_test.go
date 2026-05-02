package exec //nolint:revive // internal package, always imported with alias

import (
	"context"
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/events/eventstest"
)

// writeScript creates an executable shell script in dir and returns its path.
func writeScript(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "events-provider")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// allOpsScript returns a script body that handles all events operations.
func allOpsScript() string {
	return `
op="$1"

case "$op" in
  ensure-running) ;; # no-op, stateless
  record)
    cat > /dev/null  # consume stdin
    ;;
  list)
    cat > /dev/null  # consume stdin (filter)
    echo '[{"seq":1,"type":"bead.created","ts":"2025-06-15T10:30:00Z","actor":"human","subject":"gc-1"},{"seq":2,"type":"bead.closed","ts":"2025-06-15T11:00:00Z","actor":"human","subject":"gc-1"}]'
    ;;
  latest-seq)
    echo '2'
    ;;
  watch)
    echo '{"seq":3,"type":"bead.created","ts":"2025-06-15T12:00:00Z","actor":"human","subject":"gc-2"}'
    ;;
  *) exit 2 ;; # unknown operation
esac
`
}

func TestRecord(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "stdin.json")

	script := writeScript(t, dir, `
case "$1" in
  ensure-running) exit 2 ;;
  record) cat > "`+outFile+`" ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script, os.Stderr)

	p.Record(events.Event{
		Type:    events.BeadCreated,
		Actor:   "human",
		Subject: "gc-1",
		Message: "Build Tower of Hanoi",
	})

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}
	var e events.Event
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("unmarshal stdin: %v", err)
	}
	if e.Type != events.BeadCreated {
		t.Errorf("Type = %q, want %q", e.Type, events.BeadCreated)
	}
	if e.Actor != "human" {
		t.Errorf("Actor = %q, want %q", e.Actor, "human")
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script, os.Stderr)

	evts, err := p.List(events.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(evts) != 2 {
		t.Fatalf("List = %d events, want 2", len(evts))
	}
	if evts[0].Seq != 1 {
		t.Errorf("evts[0].Seq = %d, want 1", evts[0].Seq)
	}
	if evts[1].Type != events.BeadClosed {
		t.Errorf("evts[1].Type = %q, want %q", evts[1].Type, events.BeadClosed)
	}
}

func TestListEmpty(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  ensure-running) exit 2 ;;
  list) cat > /dev/null ;; # empty stdout = no events
  *) exit 2 ;;
esac
`)
	p := NewProvider(script, os.Stderr)

	evts, err := p.List(events.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(evts) != 0 {
		t.Errorf("List = %d events, want 0", len(evts))
	}
}

func TestListSendsFilter(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "stdin.json")

	script := writeScript(t, dir, `
case "$1" in
  ensure-running) exit 2 ;;
  list) cat > "`+outFile+`"
    echo '[]'
    ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script, os.Stderr)

	_, err := p.List(events.Filter{Type: events.BeadCreated, AfterSeq: 5})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read filter: %v", err)
	}
	var f events.Filter
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal filter: %v", err)
	}
	if f.Type != events.BeadCreated {
		t.Errorf("filter.Type = %q, want %q", f.Type, events.BeadCreated)
	}
	if f.AfterSeq != 5 {
		t.Errorf("filter.AfterSeq = %d, want 5", f.AfterSeq)
	}
}

func TestListAppliesSDKFilterAndStripsScriptLimit(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "stdin.json")
	script := writeScript(t, dir, `
case "$1" in
  ensure-running) exit 2 ;;
  list) cat > "`+outFile+`"
    echo '[{"seq":1,"type":"bead.created","ts":"2025-06-15T10:30:00Z","actor":"actor-a","subject":"gc-1"},{"seq":2,"type":"bead.created","ts":"2025-06-15T10:31:00Z","actor":"actor-a","subject":"gc-1"},{"seq":3,"type":"bead.created","ts":"2025-06-15T10:32:00Z","actor":"actor-a","subject":"gc-2"}]'
    ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script, os.Stderr)
	until := time.Date(2025, 6, 15, 10, 31, 0, 0, time.UTC)

	evts, err := p.List(events.Filter{Subject: "gc-1", Until: until, Limit: 1})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("List returned %d events, want 1", len(evts))
	}
	if evts[0].Seq != 1 {
		t.Errorf("Seq = %d, want 1", evts[0].Seq)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read filter: %v", err)
	}
	var f events.Filter
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal filter: %v", err)
	}
	if f.Subject != "" {
		t.Errorf("script filter Subject = %q, want empty legacy value", f.Subject)
	}
	if !f.Until.IsZero() {
		t.Errorf("script filter Until = %v, want zero legacy value", f.Until)
	}
	if f.Limit != 0 {
		t.Errorf("script filter Limit = %d, want 0", f.Limit)
	}
}

func TestListUsesLegacyScriptFilterShape(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  ensure-running) exit 2 ;;
  list)
    input="$(cat)"
    case "$input" in
      *'"Subject"'*|*'"Until"'*|*'"Limit"'*)
        echo "unknown filter key in $input" >&2
        exit 1
        ;;
    esac
    echo '[{"seq":1,"type":"bead.created","ts":"2025-06-15T10:30:00Z","actor":"actor-a","subject":"gc-1"},{"seq":2,"type":"bead.created","ts":"2025-06-15T10:31:00Z","actor":"actor-a","subject":"gc-2"}]'
    ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script, os.Stderr)

	evts, err := p.List(events.Filter{Subject: "gc-1", Limit: 1})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("List returned %d events, want 1", len(evts))
	}
	if evts[0].Subject != "gc-1" {
		t.Fatalf("List returned subject %q, want gc-1", evts[0].Subject)
	}
}

func TestListInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  ensure-running) exit 2 ;;
  list) cat > /dev/null
    echo 'not-json'
    ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script, os.Stderr)

	_, err := p.List(events.Filter{})
	if err == nil {
		t.Fatal("List returned nil error, want unmarshal error")
	}
	if !strings.Contains(err.Error(), "unmarshal events") {
		t.Fatalf("List error = %q, want unmarshal context", err.Error())
	}
}

func TestLatestSeq(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script, os.Stderr)

	seq, err := p.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq: %v", err)
	}
	if seq != 2 {
		t.Errorf("LatestSeq = %d, want 2", seq)
	}
}

func TestLatestSeqEmpty(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  ensure-running) exit 2 ;;
  latest-seq) ;; # empty stdout = 0
  *) exit 2 ;;
esac
`)
	p := NewProvider(script, os.Stderr)

	seq, err := p.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq: %v", err)
	}
	if seq != 0 {
		t.Errorf("LatestSeq = %d, want 0", seq)
	}
}

func TestWatch(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script, os.Stderr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	w, err := p.Watch(ctx, 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	e, err := w.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if e.Seq != 3 {
		t.Errorf("Seq = %d, want 3", e.Seq)
	}
	if e.Type != events.BeadCreated {
		t.Errorf("Type = %q, want %q", e.Type, events.BeadCreated)
	}
}

// --- ensure-running ---

func TestEnsureRunningCalledOnce(t *testing.T) {
	dir := t.TempDir()
	countFile := filepath.Join(dir, "count")
	os.WriteFile(countFile, []byte("0"), 0o644) //nolint:errcheck

	script := writeScript(t, dir, `
case "$1" in
  ensure-running)
    count=$(cat "`+countFile+`")
    echo $((count + 1)) > "`+countFile+`"
    ;;
  list) cat > /dev/null; echo '[]' ;;
  latest-seq) echo '0' ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script, os.Stderr)

	// Multiple operations should only call ensure-running once.
	p.List(events.Filter{}) //nolint:errcheck
	p.LatestSeq()           //nolint:errcheck
	p.List(events.Filter{}) //nolint:errcheck

	data, _ := os.ReadFile(countFile)
	count := strings.TrimSpace(string(data))
	if count != "1" {
		t.Errorf("ensure-running called %s times, want 1", count)
	}
}

func TestEnsureRunningExit2Stateless(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  ensure-running) exit 2 ;;
  list) cat > /dev/null; echo '[]' ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script, os.Stderr)

	evts, err := p.List(events.Filter{})
	if err != nil {
		t.Fatalf("List after ensure-running exit 2: %v", err)
	}
	if len(evts) != 0 {
		t.Errorf("List = %d events, want 0", len(evts))
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
	p := NewProvider(script, os.Stderr)

	_, err := p.List(events.Filter{})
	if err == nil {
		t.Fatal("expected error from exit 1, got nil")
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Errorf("error = %q, want stderr content", err.Error())
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
	p := NewProvider(script, os.Stderr)
	p.timeout = 500 * time.Millisecond

	start := time.Now()
	_, err := p.List(events.Filter{})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Errorf("timeout took %v, expected ~500ms", elapsed)
	}
}

// --- Conformance suite ---

func TestExecConformance(t *testing.T) {
	// Check for jq — needed by the stateful mock script.
	if _, err := osexec.LookPath("jq"); err != nil {
		t.Skip("jq not found, skipping exec conformance tests")
	}

	factory := func(t *testing.T) (events.Provider, func()) {
		t.Helper()
		dir := t.TempDir()
		script := writeScript(t, dir, statefulMockScript(dir))
		p := NewProvider(script, os.Stderr)
		return p, func() { p.Close() } //nolint:errcheck // test cleanup
	}
	eventstest.RunProviderTests(t, factory)
}

// statefulMockScript returns a shell script body that implements a real
// stateful events backend using JSONL files. This tests the exec wire
// protocol end-to-end through an actual stateful backend.
//
// State is stored in dir/events.jsonl (event data) and dir/seq (counter).
func statefulMockScript(dir string) string {
	return `
DATAFILE="` + dir + `/events.jsonl"
SEQFILE="` + dir + `/seq"

op="$1"

case "$op" in
  ensure-running)
    # Initialize state files if needed.
    [ -f "$SEQFILE" ] || echo 0 > "$SEQFILE"
    touch "$DATAFILE"
    ;;
  record)
    # Read event from stdin, assign seq+ts, append to file.
    event=$(cat)
    seq=$(cat "$SEQFILE")
    seq=$((seq + 1))
    echo "$seq" > "$SEQFILE"
    # Assign seq. If ts is zero/missing, assign current time.
    ts=$(echo "$event" | jq -r '.ts // empty')
    if [ -z "$ts" ] || [ "$ts" = "0001-01-01T00:00:00Z" ]; then
      event=$(echo "$event" | jq -c --argjson s "$seq" '.seq = $s | .ts = (now | strftime("%Y-%m-%dT%H:%M:%SZ"))')
    else
      event=$(echo "$event" | jq -c --argjson s "$seq" '.seq = $s')
    fi
    echo "$event" >> "$DATAFILE"
    ;;
  list)
    # Read filter from stdin, apply filters, output JSON array.
    filter=$(cat)
    type_filter=$(echo "$filter" | jq -r '.Type // empty')
    actor_filter=$(echo "$filter" | jq -r '.Actor // empty')
    after_seq=$(echo "$filter" | jq -r '.AfterSeq // 0')
    since=$(echo "$filter" | jq -r '.Since // empty')

    if [ ! -s "$DATAFILE" ]; then
      echo '[]'
      exit 0
    fi

    # Build jq filter expression.
    jq_filter="."
    if [ -n "$type_filter" ]; then
      jq_filter="$jq_filter | select(.type == \"$type_filter\")"
    fi
    if [ -n "$actor_filter" ]; then
      jq_filter="$jq_filter | select(.actor == \"$actor_filter\")"
    fi
    if [ "$after_seq" != "0" ] && [ -n "$after_seq" ]; then
      jq_filter="$jq_filter | select(.seq > $after_seq)"
    fi
    if [ -n "$since" ] && [ "$since" != "0001-01-01T00:00:00Z" ]; then
      jq_filter="$jq_filter | select(.ts >= \"$since\")"
    fi

    jq -c -s "[ .[] | $jq_filter ]" "$DATAFILE"
    ;;
  latest-seq)
    if [ ! -s "$DATAFILE" ]; then
      echo '0'
    else
      cat "$SEQFILE"
    fi
    ;;
  watch)
    after_seq="${2:-0}"
    # Snapshot the current file before the handoff to polling so we do not
    # miss events appended during watch startup or emit them twice.
    last_lines=$(wc -l < "$DATAFILE" 2>/dev/null || echo 0)
    if [ "$last_lines" -gt 0 ]; then
      head -n "$last_lines" "$DATAFILE" | jq -c "select(.seq > $after_seq)"
    fi
    # Poll for new events (up to 3 seconds for tests).
    end=$(($(date +%s) + 3))
    while [ "$(date +%s)" -lt "$end" ]; do
      cur_lines=$(wc -l < "$DATAFILE" 2>/dev/null || echo 0)
      if [ "$cur_lines" -gt "$last_lines" ]; then
        tail -n +"$((last_lines + 1))" "$DATAFILE" | jq -c "select(.seq > $after_seq)"
        last_lines=$cur_lines
      fi
      sleep 0.1
    done
    ;;
  *) exit 2 ;;
esac
`
}

// Compile-time interface check.
var _ events.Provider = (*Provider)(nil)
