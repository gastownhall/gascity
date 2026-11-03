package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

type publicLifecycleTestState struct {
	*publicAdmissionTestState
	ctx context.Context
}

func (s *publicLifecycleTestState) ExtMsgPublicContext() context.Context { return s.ctx }

// publicNoHTTPListener exercises Server.Serve/Shutdown without allocating a
// socket or accepting a connection. Only Close releases the blocked Accept.
type publicNoHTTPListener struct {
	closed    chan struct{}
	accepting chan struct{}
	closeOnce sync.Once
	startOnce sync.Once
}

func (l *publicNoHTTPListener) Accept() (net.Conn, error) {
	l.startOnce.Do(func() { close(l.accepting) })
	<-l.closed
	return nil, net.ErrClosed
}

func (l *publicNoHTTPListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *publicNoHTTPListener) Addr() net.Addr { return &net.UnixAddr{Net: "unix"} }

func TestPublicOutboxRestartsWithoutAnyCityHTTPClient(t *testing.T) {
	e, state, message, targets := newPublicAdmissionEnv(t)
	// First process durably accepts but exits before relay. It offered no worker
	// lifecycle. The new process must continue solely from its persisted outbox.
	rec := e.post(t, "/extmsg/inbound", "", publicAdmissionBody(t, message, "before-crash", targets))
	if rec.Code != 200 {
		t.Fatalf("admission=%d %s", rec.Code, rec.Body.String())
	}
	if len(e.opRows(t)) != 0 {
		t.Fatal("fixture already relayed")
	}
	cityCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restored := &publicLifecycleTestState{publicAdmissionTestState: state, ctx: cityCtx}
	resolver := &stateCityResolver{state: restored}
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())
	listener := &publicNoHTTPListener{closed: make(chan struct{}), accepting: make(chan struct{})}
	served := make(chan error, 1)
	go func() { served <- sm.Serve(listener) }()
	t.Cleanup(func() {
		cancel()
		if err := sm.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve after Shutdown: %v", err)
		}
		select {
		case <-listener.closed:
		default:
			t.Error("Shutdown did not close the controlled listener")
		}
	})
	// No HTTP request, getCityServer(), handler lookup, or API helper here.
	// Only the ledger and event provider are observed.
	waitFor(t, 5*time.Second, "public lane relay after restart", func() bool {
		select {
		case <-listener.accepting:
			return len(e.opRows(t)) == 1
		default:
			return false
		}
	})
	found, err := state.EventProvider().List(events.Filter{Type: events.ExtMsgInbound})
	if err != nil || len(found) != 1 {
		t.Fatalf("recovered invalidation=%v err=%v", found, err)
	}
	if len(e.opRows(t)) != 1 {
		t.Fatal("restart dispatched twice")
	}
}

func TestReadOnlySupervisorDoesNotStartPublicRelay(t *testing.T) {
	e, state, message, targets := newPublicAdmissionEnv(t)
	rec := e.post(t, "/extmsg/inbound", "", publicAdmissionBody(t, message, "read-only-pending", targets))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restored := &publicLifecycleTestState{publicAdmissionTestState: state, ctx: ctx}
	sm := NewSupervisorMux(&stateCityResolver{state: restored}, nil, true, "test", "", time.Now())
	sm.maintainPublicRelays(ctx)
	if len(e.opRows(t)) != 0 {
		t.Fatal("read-only supervisor admitted a native op")
	}
	found, err := state.EventProvider().List(events.Filter{Type: events.ExtMsgInbound})
	if err != nil || len(found) != 0 {
		t.Fatalf("read-only invalidations=%v err=%v", found, err)
	}
}
