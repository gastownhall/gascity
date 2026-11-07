//go:build !windows

package exec

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Kills (NIT 1): a handshake retry run under the lock, which stalls every
// capability probe behind a slow or hung `protocol` op. While the retry
// runs, other callers get the cached failure at once.
func TestHandshakeRetryDoesNotBlockOtherCallers(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	old := handshakeNow
	var mu sync.Mutex
	handshakeNow = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	t.Cleanup(func() { handshakeNow = old })

	dir := t.TempDir()
	counter := filepath.Join(dir, "protocol-calls")
	started := filepath.Join(dir, "retry-started")
	if err := syscall.Mkfifo(started, 0o600); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t, dir, fmt.Sprintf(`
case "$1" in
  protocol) echo run >> %q
            [ "$(grep -c run %q)" -ge 2 ] && { echo started > %q; sleep 2 & wait $!; }
            echo down >&2; exit 1 ;;
  *) exit 2 ;;
esac
`, counter, counter, started))
	p := NewProvider(script)
	if _, err := p.Protocol(); err == nil {
		t.Fatal("first handshake succeeded")
	}
	mu.Lock()
	now = now.Add(handshakeRetryAfter)
	mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := p.Protocol(); done <- err }()
	// Opening the fifo blocks until the retry's protocol op writes to it.
	if _, err := os.ReadFile(started); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if caps := p.Capabilities(); caps.CanReportAttachment {
		t.Errorf("Capabilities during a failing retry = %+v, want the zero floor", caps)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("Capabilities waited %v behind the retry, want the cached answer at once", elapsed)
	}
	if n := protocolCallCount(t, counter); n != 2 {
		t.Errorf("protocol ran %d times, want one retry", n)
	}
	if err := <-done; err == nil {
		t.Error("the retry succeeded, want its failure")
	}
}
