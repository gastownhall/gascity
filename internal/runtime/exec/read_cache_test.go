package exec //nolint:revive // internal package, always imported with alias

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// countingScript returns a script body that appends each op it runs, with
// its arguments, to logFile, then answers like allOpsScript. is-running
// answers the contents of answerFile when that file exists ("true" if not),
// so a test can change the box's state between reads.
func countingScript(logFile, answerFile string) string {
	return `
echo "$*" >> "` + logFile + `"
op="$1"
case "$op" in
  is-running)
    if [ -f "` + answerFile + `" ]; then cat "` + answerFile + `"; else echo "true"; fi ;;
  process-alive) cat > /dev/null; echo "true" ;;
  get-meta)    echo "meta-$3" ;;
  get-last-activity) echo "2026-11-15T04:00:00Z" ;;
  set-meta|start|nudge) cat > /dev/null ;;
  stop|interrupt|remove-meta|send-keys|clear-scrollback) ;;
  exec) cat > /dev/null ;;
  protocol) echo '{"version":0,"capabilities":["carrier.ops"]}' ;;
  *) exit 2 ;;
esac
`
}

// opCount counts the lines of the op log that start with prefix.
func opCount(t *testing.T, logFile, prefix string) int {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if line == prefix || strings.HasPrefix(line, prefix+" ") {
			n++
		}
	}
	return n
}

// fakeClock is a settable clock for the read cache.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// newCountingProvider returns a provider over countingScript with the
// default read-cache TTL and a fake clock, plus the op log path and the
// is-running answer file.
func newCountingProvider(t *testing.T) (*Provider, *fakeClock, string, string) {
	t.Helper()
	t.Setenv(readCacheTTLEnv, "")
	dir := t.TempDir()
	logFile := filepath.Join(dir, "ops.log")
	answerFile := filepath.Join(dir, "is-running.answer")
	p := NewProvider(writeScript(t, dir, countingScript(logFile, answerFile)))
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	p.reads.now = clock.Now
	return p, clock, logFile, answerFile
}

func TestReadCache_RepeatedReadsWithinTTLRunOneOp(t *testing.T) {
	p, _, logFile, _ := newCountingProvider(t)

	for i := 0; i < 3; i++ {
		if !p.IsRunning("s1") {
			t.Fatalf("IsRunning #%d = false, want true", i+1)
		}
		if !p.ProcessAlive("s1", []string{"claude"}) {
			t.Fatalf("ProcessAlive #%d = false, want true", i+1)
		}
		if v, err := p.GetMeta("s1", "GC_SESSION_ID"); err != nil || v != "meta-GC_SESSION_ID" {
			t.Fatalf("GetMeta #%d = %q, %v; want meta-GC_SESSION_ID, nil", i+1, v, err)
		}
		if at, err := p.GetLastActivity("s1"); err != nil || at.IsZero() {
			t.Fatalf("GetLastActivity #%d = %v, %v; want a time, nil", i+1, at, err)
		}
	}

	for _, op := range []string{"is-running", "process-alive", "get-meta", "get-last-activity"} {
		if got := opCount(t, logFile, op); got != 1 {
			t.Errorf("%s ran %d times for 3 reads in one TTL, want 1", op, got)
		}
	}
}

func TestReadCache_ReadsAgainAfterTTL(t *testing.T) {
	p, clock, logFile, answerFile := newCountingProvider(t)

	if !p.IsRunning("s1") {
		t.Fatal("first IsRunning = false, want true")
	}
	if err := os.WriteFile(answerFile, []byte("false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clock.Advance(defaultReadCacheTTL - time.Millisecond)
	if !p.IsRunning("s1") {
		t.Fatal("IsRunning inside the TTL = false, want the cached true")
	}
	clock.Advance(time.Millisecond)
	if p.IsRunning("s1") {
		t.Fatal("IsRunning at the TTL = true, want the new false")
	}
	if got := opCount(t, logFile, "is-running"); got != 2 {
		t.Errorf("is-running ran %d times, want 2", got)
	}
}

func TestReadCache_KeysOnEveryArgumentAndStdin(t *testing.T) {
	p, _, logFile, _ := newCountingProvider(t)

	p.IsRunning("s1")
	p.IsRunning("s2")
	if v, _ := p.GetMeta("s1", "A"); v != "meta-A" {
		t.Fatalf("GetMeta(s1, A) = %q, want meta-A", v)
	}
	if v, _ := p.GetMeta("s1", "B"); v != "meta-B" {
		t.Fatalf("GetMeta(s1, B) = %q, want meta-B", v)
	}
	p.ProcessAlive("s1", []string{"claude"})
	p.ProcessAlive("s1", []string{"codex"})

	for op, want := range map[string]int{"is-running": 2, "get-meta": 2, "process-alive": 2} {
		if got := opCount(t, logFile, op); got != want {
			t.Errorf("%s ran %d times, want %d (one per distinct read)", op, got, want)
		}
	}
}

func TestReadCache_WritesClearIt(t *testing.T) {
	writes := map[string]func(p *Provider) error{
		"stop":        func(p *Provider) error { return p.Stop("s1") },
		"set-meta":    func(p *Provider) error { return p.SetMeta("s1", "K", "v") },
		"remove-meta": func(p *Provider) error { return p.RemoveMeta("s1", "K") },
		"nudge": func(p *Provider) error {
			return p.Nudge("s1", []runtime.ContentBlock{{Type: "text", Text: "hi"}})
		},
		"interrupt": func(p *Provider) error { return p.Interrupt("s1") },
		"send-keys": func(p *Provider) error { return p.SendKeys("s1", "Enter") },
		"exec": func(p *Provider) error {
			_, _, err := p.Exec(context.Background(), "s1", []string{"true"})
			return err
		},
	}
	for op, write := range writes {
		t.Run(op, func(t *testing.T) {
			p, _, logFile, _ := newCountingProvider(t)

			p.IsRunning("s1")
			if _, err := p.GetMeta("s1", "K"); err != nil {
				t.Fatalf("GetMeta before %s: %v", op, err)
			}
			if err := write(p); err != nil {
				t.Fatalf("%s: %v", op, err)
			}
			if got := opCount(t, logFile, op); got == 0 {
				t.Fatalf("%s never reached the script", op)
			}
			p.IsRunning("s1")
			if _, err := p.GetMeta("s1", "K"); err != nil {
				t.Fatalf("GetMeta after %s: %v", op, err)
			}

			if got := opCount(t, logFile, "is-running"); got != 2 {
				t.Errorf("is-running ran %d times around %s, want 2", got, op)
			}
			if got := opCount(t, logFile, "get-meta"); got != 2 {
				t.Errorf("get-meta ran %d times around %s, want 2", got, op)
			}
		})
	}
}

func TestReadCache_StartClearsIt(t *testing.T) {
	p, _, logFile, answerFile := newCountingProvider(t)
	if err := os.WriteFile(answerFile, []byte("false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p.IsRunning("s1") {
		t.Fatal("IsRunning before start = true, want false")
	}
	if err := os.Remove(answerFile); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background(), "s1", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !p.IsRunning("s1") {
		t.Fatal("IsRunning after start = false, want the fresh true")
	}
	if got := opCount(t, logFile, "start"); got != 1 {
		t.Errorf("start ran %d times, want 1", got)
	}
}

func TestReadCache_FailedReadsAreNotKept(t *testing.T) {
	t.Setenv(readCacheTTLEnv, "")
	dir := t.TempDir()
	logFile := filepath.Join(dir, "ops.log")
	failFile := filepath.Join(dir, "fail")
	if err := os.WriteFile(failFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewProvider(writeScript(t, dir, `
echo "$*" >> "`+logFile+`"
case "$1" in
  is-running)
    if [ -f "`+failFile+`" ]; then echo "boom" >&2; exit 1; fi
    echo "true" ;;
  *) exit 2 ;;
esac
`))

	if p.IsRunning("s1") {
		t.Fatal("IsRunning on a failing op = true, want false")
	}
	if err := os.Remove(failFile); err != nil {
		t.Fatal(err)
	}
	if !p.IsRunning("s1") {
		t.Fatal("IsRunning after the op recovered = false, want true: a failure must not be cached")
	}
	if got := opCount(t, logFile, "is-running"); got != 2 {
		t.Errorf("is-running ran %d times, want 2", got)
	}
}

func TestReadCache_ConcurrentReadsShareOneOp(t *testing.T) {
	t.Setenv(readCacheTTLEnv, "")
	dir := t.TempDir()
	logFile := filepath.Join(dir, "ops.log")
	p := NewProvider(writeScript(t, dir, `
echo "$*" >> "`+logFile+`"
case "$1" in
  is-running) sleep 0.3; echo "true" ;;
  *) exit 2 ;;
esac
`))

	var wg sync.WaitGroup
	results := make([]bool, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = p.IsRunning("s1")
		}()
	}
	wg.Wait()

	for i, ok := range results {
		if !ok {
			t.Errorf("reader %d got false, want true", i)
		}
	}
	if got := opCount(t, logFile, "is-running"); got != 1 {
		t.Errorf("is-running ran %d times for 8 concurrent readers, want 1", got)
	}
}

func TestReadCache_DecisionReadsAskTheScript(t *testing.T) {
	p, _, logFile, answerFile := newCountingProvider(t)

	if !p.IsRunning("s1") {
		t.Fatal("IsRunning = false, want true")
	}
	if err := os.WriteFile(answerFile, []byte("false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A start's vacancy check and a driving verb's presence check decide
	// what gc does to the box, so they never answer from the cache.
	if !p.holdsNoLiveBox("s1") {
		t.Error("holdsNoLiveBox = false, want the fresh vacant answer")
	}
	if err := p.requirePresent("s1"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Errorf("requirePresent = %v, want ErrSessionNotFound from a fresh read", err)
	}
	// A fresh read refreshes the cache for the plain reads after it.
	if p.IsRunning("s1") {
		t.Error("IsRunning after a fresh read = true, want the refreshed false")
	}
	if got := opCount(t, logFile, "is-running"); got != 3 {
		t.Errorf("is-running ran %d times, want 3", got)
	}
}

func TestReadCache_ZeroTTLTurnsItOff(t *testing.T) {
	t.Setenv(readCacheTTLEnv, "0")
	dir := t.TempDir()
	logFile := filepath.Join(dir, "ops.log")
	p := NewProvider(writeScript(t, dir, countingScript(logFile, filepath.Join(dir, "none"))))

	p.IsRunning("s1")
	p.IsRunning("s1")
	if got := opCount(t, logFile, "is-running"); got != 2 {
		t.Errorf("is-running ran %d times with %s=0, want 2", got, readCacheTTLEnv)
	}
}

func TestReadCacheTTLFromEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"", defaultReadCacheTTL},
		{"500ms", 500 * time.Millisecond},
		{"5s", 5 * time.Second},
		{"1500", 1500 * time.Millisecond},
		{"0", 0},
		{"-1s", 0},
		{"soon", defaultReadCacheTTL},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(readCacheTTLEnv, tc.value)
			if got := readCacheTTLFromEnv(); got != tc.want {
				t.Errorf("readCacheTTLFromEnv() with %q = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestReadCache_WriteDuringAReadDropsItsAnswer(t *testing.T) {
	c := newReadCache(time.Minute)
	release := make(chan struct{})
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.read("k", func() (string, error) {
			close(started)
			<-release
			return "before-write", nil
		})
	}()
	<-started
	c.invalidate()
	close(release)
	<-done

	fetched := false
	out, err := c.read("k", func() (string, error) {
		fetched = true
		return "after-write", nil
	})
	if err != nil || out != "after-write" || !fetched {
		t.Errorf("read after a write = %q, %v (fetched %v); want a new read, not the answer the write overtook", out, err, fetched)
	}
}

func TestReadCache_NilCacheReadsThrough(t *testing.T) {
	var c *readCache
	calls := 0
	for i := 0; i < 2; i++ {
		if _, err := c.read("k", func() (string, error) { calls++; return "x", nil }); err != nil {
			t.Fatal(err)
		}
	}
	c.invalidate()
	if calls != 2 {
		t.Errorf("nil cache fetched %d times, want 2", calls)
	}
}
