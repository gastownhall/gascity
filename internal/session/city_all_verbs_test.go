package session

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestEveryVerbRefusesAnActorOnAnotherCity: every lifecycle verb
// refuses an Actor whose City is not its Manager's, before any provider call.
func TestEveryVerbRefusesAnActorOnAnotherCity(t *testing.T) {
	type verb func(m managerLeaseFixture, by Actor) error
	hints := func(m managerLeaseFixture) runtime.Config { return runtime.Config{WorkDir: m.info.WorkDir} }
	starts := map[string]verb{
		"Start": func(m managerLeaseFixture, by Actor) error { return m.start(by) },
		"Attach": func(m managerLeaseFixture, by Actor) error {
			return m.mgr.Attach(context.Background(), by, m.info.ID, BuildResumeCommand(m.info), hints(m))
		},
		"Send": func(m managerLeaseFixture, by Actor) error {
			_, err := m.mgr.Send(context.Background(), by, m.info.ID, "hi", BuildResumeCommand(m.info), hints(m))
			return err
		},
		"SendImmediate": func(m managerLeaseFixture, by Actor) error {
			_, err := m.mgr.SendImmediate(context.Background(), by, m.info.ID, "hi", BuildResumeCommand(m.info), hints(m))
			return err
		},
		"Submit": func(m managerLeaseFixture, by Actor) error {
			_, err := m.mgr.Submit(context.Background(), by, m.info.ID, "hi", BuildResumeCommand(m.info), hints(m), SubmitIntentDefault)
			return err
		},
		"TryWaitIdleNudge": func(m managerLeaseFixture, by Actor) error {
			_, err := m.mgr.TryWaitIdleNudge(context.Background(), by, m.info.ID, "test", "hi", BuildResumeCommand(m.info), hints(m))
			return err
		},
	}
	stops := map[string]verb{
		"Kill": func(m managerLeaseFixture, by Actor) error { return m.mgr.Kill(context.Background(), by, m.info.ID) },
		"Suspend": func(m managerLeaseFixture, by Actor) error {
			return m.mgr.Suspend(context.Background(), by, m.info.ID, false)
		},
		"Suspend soft": func(m managerLeaseFixture, by Actor) error {
			return m.mgr.Suspend(context.Background(), by, m.info.ID, true)
		},
		"Close": func(m managerLeaseFixture, by Actor) error { return m.mgr.Close(context.Background(), by, m.info.ID) },
	}
	for _, set := range []struct {
		verbs   map[string]verb
		running bool
	}{{starts, false}, {stops, true}} {
		for name, do := range set.verbs {
			m := newManagerLeaseFixture(t)
			if set.running {
				if err := m.start(m.operator(nil)); err != nil {
					t.Fatal(err)
				}
			}
			elsewhere, err := NewCityDir(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			startsBefore, stopsBefore := len(m.sp.starts), len(m.sp.stops)
			if err := do(m, Actor{Kind: ActorOperator, City: elsewhere}); err == nil || !strings.Contains(err.Error(), "is not the manager's") {
				t.Errorf("%s by an actor on another city = %v, want refused", name, err)
			}
			if len(m.sp.starts) != startsBefore || len(m.sp.stops) != stopsBefore {
				t.Errorf("%s: the refused actor reached the provider (starts %q, stops %q)", name, m.sp.starts, m.sp.stops)
			}
		}
	}
}
