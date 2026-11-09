package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// hookClaimLandingCtxKey tags a test context so a runner factory can prove the
// canonical readback context still carries the caller's values.
type hookClaimLandingCtxKey struct{}

// hookClaimRunnerCall is one hookClaimCommandRunnerWithEnvContext construction:
// the context and exact environment a bd store was bound to.
type hookClaimRunnerCall struct {
	ctx context.Context
	env map[string]string
}

// TestHookClaimWithBdStoreReadbackHasItsOwnBudget pins #6984: a claim CAS that
// commits as its mutation context runs out must not leave the canonical
// readback on that exhausted context. The readback gets a fresh bounded context
// of its own, bound to the same directory, exact environment and actor, and it
// keeps the caller's context values.
func TestHookClaimWithBdStoreReadbackHasItsOwnBudget(t *testing.T) {
	const claimedRow = `[{"id":"work-1","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"rig/worker"}}]`
	const canonicalRow = `[{"id":"work-1","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"rig/worker","gc.root_bead_id":"root-1"}}]`

	// install replaces the runner factory. The update spends its own bound
	// context (cancel) before reporting the committed CAS; show answers through
	// showFn on whatever context its store was bound to.
	install := func(t *testing.T, cancelMutation context.CancelFunc, showFn func(ctx context.Context) ([]byte, error)) (*[]hookClaimRunnerCall, *[]string) {
		t.Helper()
		originalRunner := hookClaimCommandRunnerWithEnvContext
		t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })
		var calls []hookClaimRunnerCall
		var dirs []string
		hookClaimCommandRunnerWithEnvContext = func(ctx context.Context, env map[string]string) beads.CommandRunner {
			calls = append(calls, hookClaimRunnerCall{ctx: ctx, env: env})
			return func(dir, name string, args ...string) ([]byte, error) {
				if name != "bd" {
					t.Fatalf("command name = %q, want bd", name)
				}
				dirs = append(dirs, dir)
				switch {
				case reflect.DeepEqual(args, []string{"update", "work-1", "--claim", "--json"}):
					cancelMutation()
					return []byte(claimedRow), nil
				case reflect.DeepEqual(args, []string{"show", "--json", "work-1"}):
					return showFn(ctx)
				default:
					t.Fatalf("unexpected bd args: %#v", args)
					return nil, nil
				}
			}
		}
		return &calls, &dirs
	}
	liveShow := func(ctx context.Context) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return []byte(canonicalRow), nil
	}
	env := []string{"BEADS_DIR=/rig/.beads", "GC_HOOK_CLAIM_TEST=1"}

	t.Run("readback succeeds on a fresh bounded context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), hookClaimLandingCtxKey{}, "caller"))
		defer cancel()
		calls, dirs := install(t, cancel, liveShow)

		before := time.Now()
		claimed, ok, err := hookClaimWithBdStore(ctx, "/rig", env, "work-1", "worker-1")
		if err != nil {
			t.Fatalf("hookClaimWithBdStore after a CAS that spent its mutation context: %v", err)
		}
		if !ok || claimed.Assignee != "worker-1" || claimed.Metadata["gc.root_bead_id"] != "root-1" {
			t.Fatalf("hookClaimWithBdStore = (%+v, %v), want the canonical bead claimed by worker-1", claimed, ok)
		}
		if len(*calls) != 2 {
			t.Fatalf("runner constructions = %d, want one for the claim and one for the canonical readback", len(*calls))
		}
		mutation, readback := (*calls)[0], (*calls)[1]
		if mutation.ctx != ctx {
			t.Fatal("the claim CAS did not run on the caller's mutation context")
		}
		if readback.ctx == ctx {
			t.Fatal("the canonical readback reused the mutation context")
		}
		if got := readback.ctx.Value(hookClaimLandingCtxKey{}); got != "caller" {
			t.Fatalf("readback context value = %v, want the caller's value preserved", got)
		}
		deadline, hasDeadline := readback.ctx.Deadline()
		if !hasDeadline {
			t.Fatal("the canonical readback context has no deadline; it must be bounded")
		}
		if deadline.Before(before) || deadline.After(time.Now().Add(hookClaimReadbackTimeout)) {
			t.Fatalf("readback deadline = %v, want within %s of the readback", deadline, hookClaimReadbackTimeout)
		}
		if readback.ctx.Err() == nil {
			t.Fatal("the canonical readback context was not canceled once the readback returned")
		}
		if !reflect.DeepEqual(mutation.env, readback.env) {
			t.Fatalf("readback env = %#v, want the claim's exact env %#v", readback.env, mutation.env)
		}
		if readback.env["BEADS_ACTOR"] != "worker-1" {
			t.Fatalf("readback BEADS_ACTOR = %q, want worker-1", readback.env["BEADS_ACTOR"])
		}
		if want := []string{"/rig", "/rig"}; !reflect.DeepEqual(*dirs, want) {
			t.Fatalf("bd dirs = %v, want %v", *dirs, want)
		}
	})

	t.Run("readback deadline is a committed-claim failure", func(t *testing.T) {
		originalTimeout := hookClaimReadbackTimeout
		hookClaimReadbackTimeout = 10 * time.Millisecond
		t.Cleanup(func() { hookClaimReadbackTimeout = originalTimeout })

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		install(t, cancel, func(ctx context.Context) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})

		claimed, ok, err := hookClaimWithBdStore(ctx, "/rig", env, "work-1", "worker-1")
		if !ok || claimed.ID != "work-1" {
			t.Fatalf("hookClaimWithBdStore = (%+v, %v), want the committed claim reported with ok=true", claimed, ok)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("hookClaimWithBdStore err = %v, want the readback's own deadline", err)
		}
	})
}
