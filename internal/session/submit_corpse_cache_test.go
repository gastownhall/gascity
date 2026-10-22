package session

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

// corpseBehindCache models J26 C2's shape: the agent exited and left a
// remain-on-exit corpse, but the provider's cache (IsRunning, ProcessAlive)
// still holds the pre-crash snapshot. A fresh read sees the corpse; with
// refreshes it also refreshes the cache, as tmux's ObserveLivenessSince does
// today.
type corpseBehindCache struct {
	*runtime.Fake
	mu        *sync.Mutex
	stale     *bool
	refreshes bool
}

func (f corpseBehindCache) cached() bool { f.mu.Lock(); defer f.mu.Unlock(); return *f.stale }

func (f corpseBehindCache) IsRunning(name string) bool {
	return f.cached() || f.Fake.IsRunning(name)
}

func (f corpseBehindCache) ProcessAlive(name string, pn []string) bool {
	return f.cached() || f.Fake.ProcessAlive(name, pn)
}

func (f corpseBehindCache) ObserveLivenessSince(name string, _ []string, _ time.Time) (runtime.Liveness, error) {
	if f.refreshes {
		f.mu.Lock()
		*f.stale = false
		f.mu.Unlock()
	}
	if f.Fake.IsRunning(name) { // a runtime started since: up, its agent alive
		return runtime.Liveness{Running: true, Alive: true}, nil
	}
	return runtime.Liveness{Corpse: true}, nil
}

// TestSubmitOverACorpseBehindAStaleCache (J26 C2; mc-ir8qp, the #7779 r2
// review). Kills delivery deciding on a caching provider's IsRunning: a
// message to a crashed agent whose corpse a stale cache still shows running
// is never typed into the dead pane, whether or not the fresh read refreshes
// the cache. A background message queues for the controller's restart; an
// operator's starts a new agent and delivers to it.
func TestSubmitOverACorpseBehindAStaleCache(t *testing.T) {
	for _, refreshes := range []bool{true, false} {
		for _, kind := range []ActorKind{ActorBackground, ActorOperator} {
			store := beads.NewMemStore()
			fake := runtime.NewFake()
			stale := true
			sp := corpseBehindCache{Fake: fake, mu: &sync.Mutex{}, stale: &stale, refreshes: refreshes}
			cityPath := t.TempDir()
			mgr := NewManagerWithOptions(store, sp, WithCityPath(cityPath))
			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatal(err)
			}
			_ = fake.Stop(info.SessionName) // the truth: no live pane, only the corpse
			orig := startSessionSubmitPoller
			startSessionSubmitPoller = func(string, string, string) error { return nil }
			before := len(fake.Calls)
			out, err := mgr.Submit(context.Background(), testActor(mgr, kind), info.ID, "after the crash", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir, ProcessNames: []string{"claude"}}, SubmitIntentDefault)
			startSessionSubmitPoller = orig
			state, _ := nudgequeue.LoadState(cityPath)
			var methods []string
			for _, c := range fake.Calls[before:] {
				if c.Name == info.SessionName {
					methods = append(methods, c.Method)
				}
			}
			started := slices.Contains(methods, "Start")
			nudged := slices.Contains(methods, "Nudge")
			switch {
			case err != nil:
				t.Errorf("refreshes %t, actor %v: Submit: %v", refreshes, kind, err)
			case kind == ActorBackground && (!out.Queued || len(state.Pending) != 1 || nudged || started):
				t.Errorf("refreshes %t, background: outcome %+v, %d pending, calls %v; want it queued, nothing typed or started", refreshes, out, len(state.Pending), methods)
			case kind == ActorOperator && (!started || nudged && slices.Index(methods, "Nudge") < slices.Index(methods, "Start")):
				t.Errorf("refreshes %t, operator: outcome %+v, calls %v; want a new agent started before anything is delivered", refreshes, out, methods)
			}
		}
	}
}
