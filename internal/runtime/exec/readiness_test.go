package exec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Kills (§13.5 1): readiness without observe.v1 or without
// report-attachment, or gaps out of their fixed order.
func TestV2ProfileGaps(t *testing.T) {
	const observe, attach = runtime.ProtocolCapabilityObserveV1, runtime.ProtocolCapabilityReportAttachment
	for _, tc := range []struct {
		caps []string
		want []string
	}{
		{nil, []string{observe, attach}},
		{[]string{"proc.exec", "observe.v2"}, []string{observe, attach}},
		{[]string{observe}, []string{attach}},
		{[]string{attach}, []string{observe}},
		{[]string{attach, "future.token", observe}, nil},
	} {
		if got := V2ProfileGaps(runtime.ProtocolInfo{Capabilities: tc.caps}); !slices.Equal(got, tc.want) {
			t.Errorf("V2ProfileGaps(%v) = %v, want %v", tc.caps, got, tc.want)
		}
	}
}

// Kills (§13.5 11, F7): a handshake error cached for the provider's life,
// or a success re-run.
func TestHandshakeErrorIsRetriedAfterTheTTL(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	old := handshakeNow
	handshakeNow = func() time.Time { return now }
	t.Cleanup(func() { handshakeNow = old })

	dir := t.TempDir()
	counter := filepath.Join(dir, "protocol-calls")
	script := writeScript(t, dir, fmt.Sprintf(`
case "$1" in
  protocol) echo run >> %q
            [ "$(grep -c run %q)" -eq 1 ] && { echo transient >&2; exit 1; }
            printf '%%s' '%s' ;;
  *) exit 2 ;;
esac
`, counter, counter, readyHandshake))
	sp := NewSeamBacked(script)
	reporter := sp.(runtime.V2ReadinessReporter)
	if _, err := reporter.V2Readiness(); err == nil {
		t.Fatal("first handshake succeeded, want the transient failure")
	}
	if _, err := reporter.V2Readiness(); err == nil || protocolCallCount(t, counter) != 1 {
		t.Fatalf("a handshake error inside the TTL re-ran (calls %d, err %v)", protocolCallCount(t, counter), err)
	}
	if runtime.FreshReadable(sp) {
		t.Fatal("FreshReadable during a cached handshake error")
	}
	now = now.Add(handshakeRetryAfter - time.Second)
	if _, err := reporter.V2Readiness(); err == nil || protocolCallCount(t, counter) != 1 {
		t.Fatalf("a handshake error re-ran before the TTL (calls %d, err %v)", protocolCallCount(t, counter), err)
	}
	now = now.Add(time.Second)
	if missing, err := reporter.V2Readiness(); err != nil || missing != nil {
		t.Fatalf("after the TTL: V2Readiness = %v, %v; want ready", missing, err)
	}
	if !runtime.FreshReadable(sp) {
		t.Fatal("FreshReadable = false after the handshake recovered")
	}
	now = now.Add(time.Hour)
	_, _ = reporter.V2Readiness()
	if n := protocolCallCount(t, counter); n != 2 {
		t.Fatalf("protocol ran %d times, want 2: a success is cached for good", n)
	}
}

func TestProbeV2Readiness(t *testing.T) {
	for _, tc := range []struct {
		name, handshake string
		missing         []string
		local, wantErr  bool
	}{
		{"ready, local", `{"version":0,"capabilities":["observe.v1","report-attachment"],"locality":"local"}`, nil, true, false},
		{"ready, remote", `{"version":0,"capabilities":["observe.v1","report-attachment"],"locality":"remote"}`, nil, false, false},
		{"ready, no locality", readyHandshake, nil, false, false},
		{"unknown locality", `{"version":0,"capabilities":["observe.v1"],"locality":"LOCAL"}`, []string{runtime.ProtocolCapabilityReportAttachment}, false, false},
		{"no handshake", "", []string{runtime.ProtocolCapabilityObserveV1, runtime.ProtocolCapabilityReportAttachment}, false, false},
		{"broken handshake", `{"version":`, nil, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script, _ := observePack(t, tc.handshake, nil)
			info, missing, err := ProbeV2Readiness(context.Background(), script, 5*time.Second)
			if tc.wantErr != (err != nil) || !slices.Equal(missing, tc.missing) || info.DeclaresLocal() != tc.local {
				t.Fatalf("ProbeV2Readiness = %+v, %v, %v; want missing %v, local %v, error %v", info, missing, err, tc.missing, tc.local, tc.wantErr)
			}
			p := NewProvider(script)
			if got, err := p.Protocol(); err == nil && got.DeclaresLocal() != tc.local {
				t.Errorf("provider handshake locality %q, want local %v", got.Locality, tc.local)
			}
		})
	}

	slow, _ := observePack(t, "", nil)
	writeScript(t, filepath.Dir(slow), `[ "$1" = protocol ] && { sleep 5 & wait $!; }; exit 2`)
	start := time.Now()
	if _, _, err := ProbeV2Readiness(context.Background(), slow, 200*time.Millisecond); err == nil {
		t.Error("a probe past its timeout succeeded")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("probe took %v, want it bounded by its timeout", elapsed)
	}
}

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
