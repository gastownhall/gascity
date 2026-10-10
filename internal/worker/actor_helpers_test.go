package worker

import (
	"testing"

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
