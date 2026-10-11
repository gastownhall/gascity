package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// updateNotification builds a session/update notification carrying one agent
// message chunk.
func updateNotification(t *testing.T, text string) JSONRPCMessage {
	t.Helper()
	content, err := json.Marshal(ContentBlock{Type: "text", Text: text})
	if err != nil {
		t.Fatalf("marshal content block: %v", err)
	}
	params, err := json.Marshal(SessionUpdateParams{
		Update: SessionUpdateContent{Type: "agent_message_chunk", Content: content},
	})
	if err != nil {
		t.Fatalf("marshal update params: %v", err)
	}
	return JSONRPCMessage{JSONRPC: "2.0", Method: "session/update", Params: params}
}

func waitForActivityTest(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestGetLastActivityIsReadableFromAnotherProvider(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "acp")
	owner := NewProviderWithDir(dir, Config{})
	name := testName()

	stamp := time.Now().Add(-42 * time.Minute).UTC().Truncate(time.Millisecond)
	if err := owner.publishActivity(name, stamp); err != nil {
		t.Fatalf("publishActivity: %v", err)
	}

	// A second Provider over the same directory models any process that did
	// not start and therefore does not own the in-memory connection.
	reader := NewProviderWithDir(dir, Config{})
	got, err := reader.GetLastActivity(name)
	if err != nil {
		t.Fatalf("GetLastActivity: %v", err)
	}
	if !got.Equal(stamp) {
		t.Fatalf("GetLastActivity = %s, want %s", got.Format(time.RFC3339Nano), stamp.Format(time.RFC3339Nano))
	}
}

func TestActivityPublicationDoesNotBlockJSONRPCDispatch(t *testing.T) {
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	publisher := newActivityPublisher(
		time.Millisecond,
		time.Time{},
		func(time.Time) error {
			close(writeStarted)
			<-releaseWrite
			return nil
		},
		nil,
	)
	t.Cleanup(func() {
		select {
		case <-releaseWrite:
		default:
			close(releaseWrite)
		}
		publisher.close()
	})

	sc := newSessionConn(nil, nil, nil, 100, nil)
	if err := sc.installActivityPublisher(publisher, time.Time{}); err != nil {
		t.Fatalf("installActivityPublisher: %v", err)
	}
	sc.handleUpdate(updateNotification(t, "streaming"))
	waitForActivityTest(t, writeStarted, "blocked durable write")

	id := int64(17)
	response := make(chan JSONRPCMessage, 1)
	sc.mu.Lock()
	sc.pending[id] = response
	sc.mu.Unlock()

	dispatched := make(chan struct{})
	go func() {
		sc.dispatch(JSONRPCMessage{JSONRPC: "2.0", ID: &id})
		close(dispatched)
	}()
	waitForActivityTest(t, dispatched, "JSON-RPC response dispatch")
	select {
	case <-response:
	default:
		t.Fatal("response was not routed while activity write was blocked")
	}
	close(releaseWrite)
}

func TestActivityPublisherSerializesCoalescesAndOrdersWrites(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	twoWrites := make(chan struct{})

	var (
		mu     sync.Mutex
		writes []time.Time
	)
	publisher := newActivityPublisher(
		time.Millisecond,
		time.Time{},
		func(stamp time.Time) error {
			mu.Lock()
			writes = append(writes, stamp)
			count := len(writes)
			mu.Unlock()
			if count == 1 {
				close(firstStarted)
				<-releaseFirst
			}
			if count == 2 {
				close(twoWrites)
			}
			return nil
		},
		nil,
	)
	t.Cleanup(publisher.close)

	base := time.Now()
	publisher.offer(base)
	waitForActivityTest(t, firstStarted, "first write")
	publisher.offer(base.Add(time.Second))
	publisher.offer(base.Add(2 * time.Second))
	close(releaseFirst)
	waitForActivityTest(t, twoWrites, "coalesced trailing write")

	mu.Lock()
	defer mu.Unlock()
	if len(writes) != 2 {
		t.Fatalf("writes = %v, want exactly two", writes)
	}
	if !writes[0].Equal(base) || !writes[1].Equal(base.Add(2*time.Second)) {
		t.Fatalf("writes = %v, want [%s %s]", writes, base, base.Add(2*time.Second))
	}
}

func TestActivityPublisherRetriesAndReportsFailure(t *testing.T) {
	succeeded := make(chan struct{})
	var attempts atomic.Int32
	var reports atomic.Int32
	publisher := newActivityPublisher(
		time.Millisecond,
		time.Time{},
		func(time.Time) error {
			switch attempts.Add(1) {
			case 1, 2:
				return errors.New("injected sidecar failure")
			default:
				close(succeeded)
				return nil
			}
		},
		func(error) { reports.Add(1) },
	)
	t.Cleanup(publisher.close)

	publisher.offer(time.Now())
	waitForActivityTest(t, succeeded, "activity publication retry")
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if got := reports.Load(); got != 1 {
		t.Fatalf("error reports = %d, want one report for the failure streak", got)
	}
}

func TestActivityPublisherUpdatesDoNotPostponeRetry(t *testing.T) {
	succeeded := make(chan struct{})
	var attempts atomic.Int32
	publisher := newActivityPublisher(
		10*time.Millisecond,
		time.Time{},
		func(time.Time) error {
			switch attempts.Add(1) {
			case 1:
				return errors.New("injected sidecar failure")
			case 2:
				close(succeeded)
			}
			return nil
		},
		nil,
	)
	t.Cleanup(publisher.close)

	base := time.Now()
	publisher.offer(base)
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for i := 1; ; i++ {
		select {
		case <-succeeded:
			if got := attempts.Load(); got != 2 {
				t.Fatalf("attempts = %d, want 2", got)
			}
			return
		case <-ticker.C:
			publisher.offer(base.Add(time.Duration(i) * time.Millisecond))
		case <-deadline.C:
			t.Fatal("continuous updates postponed activity publication retry")
		}
	}
}

func TestActivityPublisherCloseFlushesPendingUpdate(t *testing.T) {
	var (
		mu     sync.Mutex
		writes []time.Time
	)
	publisher := newActivityPublisher(
		time.Hour,
		time.Now(),
		func(stamp time.Time) error {
			mu.Lock()
			writes = append(writes, stamp)
			mu.Unlock()
			return nil
		},
		nil,
	)
	stamp := time.Now().Add(time.Second)
	publisher.offer(stamp)
	publisher.close()

	mu.Lock()
	defer mu.Unlock()
	if len(writes) != 1 || !writes[0].Equal(stamp) {
		t.Fatalf("writes on close = %v, want [%s]", writes, stamp)
	}
}

func TestReadLoopDoneIncludesFinalActivityUpdate(t *testing.T) {
	var published time.Time
	publisher := newActivityPublisher(
		time.Hour,
		time.Now(),
		func(stamp time.Time) error {
			published = stamp
			return nil
		},
		nil,
	)
	sc := newSessionConn(nil, nil, nil, 100, nil)
	if err := sc.installActivityPublisher(publisher, time.Time{}); err != nil {
		t.Fatalf("installActivityPublisher: %v", err)
	}

	reader, writer := io.Pipe()
	go sc.readLoop(reader)
	encoded, err := json.Marshal(updateNotification(t, "final buffered update"))
	if err != nil {
		t.Fatalf("marshal update: %v", err)
	}
	if _, err := writer.Write(append(encoded, '\n')); err != nil {
		t.Fatalf("write update: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close update writer: %v", err)
	}
	waitForActivityTest(t, sc.readDone, "read loop completion")
	want := sc.getLastActivity()
	publisher.close()

	if want.IsZero() {
		t.Fatal("final buffered update did not advance in-memory activity")
	}
	if !published.Equal(want) {
		t.Fatalf("published on close = %s, want final activity %s", published, want)
	}
}

func TestPublishActivityIsAtomicForConcurrentReaders(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "acp")
	writer := NewProviderWithDir(dir, Config{})
	reader := NewProviderWithDir(dir, Config{})
	name := testName()
	first := time.Unix(1_700_000_000, 123).UTC()
	second := time.Unix(1_800_000_000, 456).UTC()
	if err := writer.publishActivity(name, first); err != nil {
		t.Fatalf("initial publishActivity: %v", err)
	}

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := range 200 {
			stamp := first
			if i%2 == 1 {
				stamp = second
			}
			if err := writer.publishActivity(name, stamp); err != nil {
				t.Errorf("publishActivity: %v", err)
				return
			}
		}
	}()

	for {
		got, err := reader.GetLastActivity(name)
		if err != nil {
			t.Fatalf("GetLastActivity observed a partial sidecar: %v", err)
		}
		if !got.Equal(first) && !got.Equal(second) {
			t.Fatalf("GetLastActivity = %s, want one complete published value", got)
		}
		select {
		case <-writerDone:
			return
		default:
		}
	}
}

func TestPersistedActivityRejectsCorruptStamp(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "acp")
	p := NewProviderWithDir(dir, Config{})
	name := testName()

	if err := p.SetMeta(name, lastActivityMetaKey, "not-a-timestamp"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if _, err := p.GetLastActivity(name); err == nil {
		t.Fatal("GetLastActivity accepted a corrupt stamp")
	}
}

func TestGetLastActivityUnknownSessionIsZero(t *testing.T) {
	p := NewProviderWithDir(filepath.Join(shortTempDir(t), "acp"), Config{})
	got, err := p.GetLastActivity("never-started")
	if err != nil {
		t.Fatalf("GetLastActivity: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("GetLastActivity = %s, want zero for an unknown session", got)
	}
}

func TestCapabilitiesDeclareActivity(t *testing.T) {
	caps := newTestProvider(t).Capabilities()
	if !caps.CanReportActivity {
		t.Fatal("CanReportActivity = false")
	}
	if caps.CanReportAttachment {
		t.Fatal("CanReportAttachment = true; ACP sessions are headless")
	}
}

func TestStartSeedsDurableActivity(t *testing.T) {
	p := newTestProvider(t)
	name := testName()

	before := time.Now()
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })

	reader := NewProviderWithDir(p.dir, Config{})
	got, err := reader.GetLastActivity(name)
	if err != nil {
		t.Fatalf("GetLastActivity: %v", err)
	}
	if got.IsZero() || got.Before(before.Add(-time.Second)) {
		t.Fatalf("seeded activity = %s, want a durable Start-time value", got)
	}
}

func TestStartFailsWhenInitialActivityCannotBePublished(t *testing.T) {
	p := newTestProvider(t)
	p.activityWrite = func(string, []byte) error {
		return errors.New("injected write failure")
	}
	name := testName()

	err := p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "publishing initial activity") {
		t.Fatalf("Start error = %v, want initial activity publication error", err)
	}
	if p.IsRunning(name) {
		t.Fatalf("session %q remained running after initial activity publication failed", name)
	}
}

func TestStopClearsDurableActivity(t *testing.T) {
	p := newTestProvider(t)
	name := testName()

	if err := p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if _, err := os.Stat(p.metaPath(name, lastActivityMetaKey)); !os.IsNotExist(err) {
		t.Fatalf("activity stamp survived Stop: stat err = %v", err)
	}
	reader := NewProviderWithDir(p.dir, Config{})
	got, err := reader.GetLastActivity(name)
	if err != nil {
		t.Fatalf("GetLastActivity: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("GetLastActivity = %s after Stop, want zero", got)
	}
}
