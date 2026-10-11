package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestSweepStartsNothing: a stop sweep takes no lease, so every start verb
// refuses it (ErrSweepStarts) before any provider call, even with no other
// holder; the runtime-only start and a runtime name lease included.
func TestSweepStartsNothing(t *testing.T) {
	m := newManagerLeaseFixture(t)
	held := m.hold(t)
	defer held.Release()
	sweep := m.as(ActorSweep)
	hints := runtime.Config{WorkDir: m.info.WorkDir}
	for name, err := range map[string]error{
		"Start":  m.start(sweep),
		"Attach": m.mgr.Attach(context.Background(), sweep, m.info.ID, BuildResumeCommand(m.info), hints),
		"Send": func() error {
			_, err := m.mgr.Send(context.Background(), sweep, m.info.ID, "hi", BuildResumeCommand(m.info), hints)
			return err
		}(),
		"StartRuntimeOnly": m.mgr.StartRuntimeOnly(context.Background(), Actor{Kind: ActorSweep, Lease: held}, m.info.ID, BuildResumeCommand(m.info), hints),
	} {
		if !errors.Is(err, ErrSweepStarts) {
			t.Errorf("%s by a sweep = %v, want ErrSweepStarts", name, err)
		}
	}
	if len(m.sp.starts) != 0 {
		t.Fatalf("a sweep started the runtime under another holder's lease: %q", m.sp.starts)
	}
	if err := (Actor{Kind: ActorSweep}).CheckStarts(); !errors.Is(err, ErrSweepStarts) {
		t.Fatalf("CheckStarts(sweep) = %v", err)
	}
	if err := m.as(ActorController).CheckStarts(); err != nil {
		t.Fatalf("CheckStarts(controller) = %v", err)
	}
}

// TestBorrowedLeaseIsChecked: a lease a caller hands a verb must be on the
// verb's runtime and session and not yet released, at every borrow point:
// the Manager's starts and stops, the runtime-only start, and a runtime name
// lease.
func TestBorrowedLeaseIsChecked(t *testing.T) {
	m := newManagerLeaseFixture(t)
	other, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: m.city, Name: "another-runtime"})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	released := m.hold(t)
	released.Release()
	hints := runtime.Config{WorkDir: m.info.WorkDir}
	for name, lease := range map[string]*RuntimeLease{"another runtime": other, "released": released} {
		by := m.operator(lease)
		if err := m.start(by); err == nil || !strings.Contains(err.Error(), "the caller's lease") {
			t.Errorf("%s: Start = %v, want the borrow refused", name, err)
		}
		ctl := m.as(ActorController)
		ctl.Lease = lease
		if err := m.mgr.StartRuntimeOnly(context.Background(), ctl, m.info.ID, BuildResumeCommand(m.info), hints); err == nil || !strings.Contains(err.Error(), "the caller's lease") {
			t.Errorf("%s: StartRuntimeOnly = %v, want the borrow refused", name, err)
		}
		if _, err := LeaseRuntimeName(context.Background(), by, m.info.SessionName); err == nil || !strings.Contains(err.Error(), "the caller's lease") {
			t.Errorf("%s: LeaseRuntimeName = %v, want the borrow refused", name, err)
		}
	}
	if len(m.sp.starts) != 0 {
		t.Fatalf("a refused borrow started the runtime: %q", m.sp.starts)
	}
	held := m.hold(t)
	defer held.Release()
	if release, err := LeaseRuntimeName(context.Background(), m.operator(held), m.info.SessionName); err != nil {
		t.Fatalf("LeaseRuntimeName under the caller's own lease: %v", err)
	} else {
		release()
	}
	if err := m.mgr.StartRuntimeOnly(context.Background(), m.as(ActorController), m.info.ID, BuildResumeCommand(m.info), hints); !errors.Is(err, ErrNoCallerLease) {
		t.Fatalf("StartRuntimeOnly with a City and no lease = %v, want ErrNoCallerLease", err)
	}
}

// TestActorOnAnotherCityIsRefused: a Manager refuses an Actor whose City is
// not its own, before any lease or provider call.
func TestActorOnAnotherCityIsRefused(t *testing.T) {
	m := newManagerLeaseFixture(t)
	elsewhere, err := NewCityDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	by := Actor{Kind: ActorOperator, City: elsewhere}
	if err := m.start(by); err == nil || !strings.Contains(err.Error(), "is not the manager's") {
		t.Fatalf("Start by an actor on another city = %v, want refused", err)
	}
	if err := m.mgr.Kill(context.Background(), by, m.info.ID); err == nil || !strings.Contains(err.Error(), "is not the manager's") {
		t.Fatalf("Kill by an actor on another city = %v, want refused", err)
	}
	if len(m.sp.starts) != 0 || len(m.sp.stops) != 0 {
		t.Fatalf("a refused actor reached the provider: starts %q stops %q", m.sp.starts, m.sp.stops)
	}
	if err := m.start(m.operator(nil)); err != nil {
		t.Fatalf("Start by an actor on the manager's city: %v", err)
	}
}

// TestRuntimeLeaseRequestNeedsAnAbsoluteCity: a lease request names an
// absolute city, whoever builds it.
func TestRuntimeLeaseRequestNeedsAnAbsoluteCity(t *testing.T) {
	for _, city := range []string{"", "city", "./city"} {
		if l, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: city, Name: "s"}); !errors.Is(err, ErrRuntimeLeaseNoCity) {
			l.Release()
			t.Errorf("TryRuntimeLease(city %q) = %v, want ErrRuntimeLeaseNoCity", city, err)
		}
	}
}

// TestRefusedCityDirKeepsItsPath: a path NewCityDir refuses names no city,
// but the refusal's message keeps it.
func TestRefusedCityDirKeepsItsPath(t *testing.T) {
	d, err := NewCityDir("relative/city")
	if err == nil || !d.IsZero() || d.Path() != "" || d.String() != "relative/city" {
		t.Fatalf("NewCityDir(relative) = %q (path %q), %v; want no city, keeping the refused path", d.String(), d.Path(), err)
	}
}
