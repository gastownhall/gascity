package session

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
)

// lookupSession is a manager over fake profiled as profile, with one
// session created on it.
func lookupSession(t *testing.T, profile runtime.Profile) (*Manager, *runtime.Fake, Info) {
	t.Helper()
	fake := runtime.NewFake()
	mgr := newTestManager(t, beads.NewMemStore(), runtime.NewFakeProfile(profile, runtime.FullFake{Fake: fake}))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "codex", WorkDir: "/tmp", Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	fake.Calls = nil
	return mgr, fake, info
}

// LookupDelivery passes the runtime's answer through for the row's runtime
// name and sends nothing.
func TestManagerLookupDelivery(t *testing.T) {
	mgr, fake, info := lookupSession(t, runtime.ProfileT3Bridge)
	held := runtime.DeliveryReceipt{MessageID: "m-1"}
	fake.KeyedDeliveries = map[string]map[string]runtime.DeliveryReceipt{info.SessionName: {"gc:op:1": held}}

	r, found, err := mgr.LookupDelivery(context.Background(), info.ID, "gc:op:1")
	if err != nil || !found || r != held {
		t.Fatalf("LookupDelivery(held) = %+v, %v, %v; want %+v", r, found, err, held)
	}
	if _, found, err := mgr.LookupDelivery(context.Background(), info.ID, "gc:op:2"); err != nil || found {
		t.Fatalf("LookupDelivery(not held) = %v, %v; want not found", found, err)
	}
	var calls []runtime.Call
	for _, c := range fake.SnapshotCalls() {
		if c.Method == "LookupDelivery" {
			calls = append(calls, c)
		}
	}
	if len(calls) != 2 || calls[0].Method != "LookupDelivery" || calls[0].Name != info.SessionName || calls[0].DeliveryKey != "gc:op:1" {
		t.Fatalf("calls = %+v, want two lookups naming the runtime %s", calls, info.SessionName)
	}

	fake.LookupDeliveryErrors = map[string]error{info.SessionName: runtime.ErrRuntimeUnavailable}
	if _, found, err := mgr.LookupDelivery(context.Background(), info.ID, "gc:op:1"); !errors.Is(err, runtime.ErrRuntimeUnavailable) || found {
		t.Fatalf("LookupDelivery(unavailable) = %v, %v; want ErrRuntimeUnavailable", found, err)
	}
}

// A runtime that keeps no keyed deliveries is not found, and nothing is
// called; a closed row is ErrSessionClosed.
func TestManagerLookupDeliveryNotKeptAndClosed(t *testing.T) {
	mgr, fake, info := lookupSession(t, runtime.ProfileTmux)
	if _, found, err := mgr.LookupDelivery(context.Background(), info.ID, "gc:op:1"); err != nil || found {
		t.Fatalf("LookupDelivery on tmux = %v, %v; want not found", found, err)
	}
	if n := fake.CountCalls("LookupDelivery", info.SessionName); n != 0 {
		t.Fatalf("tmux lookups = %d, want none", n)
	}
	if err := mgr.Close(context.Background(), testActor(mgr, ActorOperator), info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, err := mgr.LookupDelivery(context.Background(), info.ID, "gc:op:1"); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("LookupDelivery on a closed row = %v, want ErrSessionClosed", err)
	}
}

// The lookup reads the session's ACP route as its sends do: an ACP session
// behind the auto composite is looked up on the ACP backend, and the
// default backend is not asked, even in a process that has not routed the
// session's name yet.
func TestManagerLookupDeliveryFollowsTheACPRoute(t *testing.T) {
	plain := runtime.NewFake()
	keyed := runtime.NewFake()
	sp := auto.New(plain, runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: keyed}))
	mgr := newTestManager(t, beads.NewMemStore(), sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Transport: "acp", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	held := runtime.DeliveryReceipt{MessageID: "m-acp"}
	keyed.KeyedDeliveries = map[string]map[string]runtime.DeliveryReceipt{info.SessionName: {"gc:op:1": held}}
	// A fresh process has not routed the name yet: CreateSession's route
	// must not be what carries the lookup to the ACP backend.
	sp.Unroute(info.SessionName)

	r, found, err := mgr.LookupDelivery(context.Background(), info.ID, "gc:op:1")
	if err != nil || !found || r != held {
		t.Fatalf("LookupDelivery on an ACP session = %+v, %v, %v; want %+v from the ACP backend", r, found, err, held)
	}
	if n := keyed.CountCalls("LookupDelivery", info.SessionName); n != 1 {
		t.Fatalf("ACP backend lookups = %d, want 1", n)
	}
	if n := plain.CountCalls("LookupDelivery", info.SessionName); n != 0 {
		t.Fatalf("default backend lookups = %d, want none", n)
	}
}
