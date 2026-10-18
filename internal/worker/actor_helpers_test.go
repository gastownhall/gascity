package worker

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// testActor is an Actor of kind on a fresh t.TempDir() city.
func testActor(t testing.TB, kind sessionpkg.ActorKind) sessionpkg.Actor {
	t.Helper()
	city, err := sessionpkg.NewCityDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return sessionpkg.Actor{Kind: kind, City: city}
}

func testOperator(t testing.TB) sessionpkg.Actor {
	t.Helper()
	return testActor(t, sessionpkg.ActorOperator)
}

// handleActor is an Actor of kind on h's Manager's city, when h is a session
// handle, and on a fresh city otherwise (a runtime-only handle has no
// Manager).
func handleActor(t testing.TB, h any, kind sessionpkg.ActorKind) sessionpkg.Actor {
	t.Helper()
	if sh, ok := h.(*SessionHandle); ok {
		if city, err := sh.manager.CityDir(); err == nil {
			return sessionpkg.Actor{Kind: kind, City: city}
		}
	}
	return testActor(t, kind)
}

// startLeaseActor is handleActor's controller for h's runtime-only start
// (StartResolved), carrying the lease on h's runtime that the legacy start
// holds across it. A runtime-only handle takes the name's flock itself.
func startLeaseActor(t testing.TB, h any) sessionpkg.Actor {
	t.Helper()
	by := handleActor(t, h, sessionpkg.ActorController)
	sh, ok := h.(*SessionHandle)
	if !ok {
		return by
	}
	id, err := sh.ensureSessionID()
	if err != nil {
		t.Fatal(err)
	}
	info, err := sh.manager.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	l, err := sessionpkg.TryRuntimeLease(nil, sessionpkg.RuntimeLeaseRequest{City: by.City.Path(), Name: info.SessionName})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Release)
	by.Lease = l
	return by
}

// managerActor is an Actor of kind on m's city.
func managerActor(t testing.TB, m *sessionpkg.Manager, kind sessionpkg.ActorKind) sessionpkg.Actor {
	t.Helper()
	city, err := m.CityDir()
	if err != nil {
		t.Fatal(err)
	}
	return sessionpkg.Actor{Kind: kind, City: city}
}

// startResolved is the legacy start's runtime-only bring-up through h: the
// controller under the lease it holds across the start (startLeaseActor),
// running the incarnation its PreWake minted under that lease. A
// runtime-only handle has no row, so no incarnation.
func startResolved(t testing.TB, h interface {
	StartResolved(context.Context, sessionpkg.Actor, sessionpkg.Incarnation, string, runtime.Config) error
}, startCommand string, cfg runtime.Config,
) error {
	t.Helper()
	by := startLeaseActor(t, h)
	var inc sessionpkg.Incarnation
	if sh, ok := h.(*SessionHandle); ok {
		info, err := sh.manager.Get(sh.currentSessionID())
		if err != nil {
			t.Fatal(err)
		}
		if inc, err = sh.manager.PersistedStore().MintIncarnation(context.Background(), by.Lease, sessionpkg.MintRequest{Expect: info, By: sessionpkg.ActorController, Now: time.Now()}); err != nil {
			t.Fatalf("minting %s: %v", info.ID, err)
		}
	}
	return h.StartResolved(context.Background(), by, inc, startCommand, cfg)
}
