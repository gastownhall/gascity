package main

import (
	"bytes"
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

// hookClaimLandingTier is one claiming tier reached through doHookClaim: the
// work query output that routes the candidates to it and the actor its CAS runs
// as.
type hookClaimLandingTier struct {
	name   string
	output string
}

var hookClaimLandingTiers = []hookClaimLandingTier{
	{
		name:   "fresh",
		output: `[{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker"}},{"id":"work-2","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
	},
	{
		name:   "ready_assignment",
		output: `[{"id":"work-1","status":"open","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}},{"id":"work-2","status":"open","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`,
	},
}

// hookClaimLandingStore is a fake bd behind the real hookClaimWithBdStore
// adapter: the CAS moves the controlled clock to casAt (an invocation age), and
// the canonical show advances it by showTakes before answering (or failing with
// showErr).
type hookClaimLandingStore struct {
	updates []string
	shows   []string
}

func (s *hookClaimLandingStore) install(t *testing.T, clock *time.Time, invokedAt time.Time, casAt, showTakes time.Duration, showErr error) {
	t.Helper()
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })
	hookClaimCommandRunnerWithEnvContext = func(context.Context, map[string]string) beads.CommandRunner {
		return func(_ string, name string, args ...string) ([]byte, error) {
			if name != "bd" || len(args) < 3 {
				t.Fatalf("unexpected command %s %v", name, args)
			}
			row := `[{"id":"` + args[1] + `","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`
			switch {
			case args[0] == "update" && args[2] == "--claim":
				s.updates = append(s.updates, args[1])
				*clock = invokedAt.Add(casAt)
				return []byte(row), nil
			case args[0] == "show" && args[1] == "--json":
				s.shows = append(s.shows, args[2])
				*clock = clock.Add(showTakes)
				if showErr != nil {
					return nil, showErr
				}
				return []byte(`[{"id":"` + args[2] + `","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`), nil
			default:
				t.Fatalf("unexpected bd args: %#v", args)
				return nil, nil
			}
		}
	}
}

// TestHookClaimTiersKeepTheCASLandingAcrossCanonicalReadback pins that both
// claiming tiers fence a minted claim on the instant its CAS returned, not on
// when the canonical readback inside the claim op finished. A CAS that lands
// inside the claim window plus its mutation reserve is delivered even when the
// readback crosses that boundary.
func TestHookClaimTiersKeepTheCASLandingAcrossCanonicalReadback(t *testing.T) {
	const window = 40 * time.Second
	fence := window + hookClaimMutationTimeout
	for _, tier := range hookClaimLandingTiers {
		for _, tc := range []struct {
			name  string
			casAt time.Duration
		}{
			{"timely CAS", fence - time.Second},
			{"CAS exactly at the fence", fence},
		} {
			t.Run(tier.name+"/"+tc.name, func(t *testing.T) {
				rec := &turnBoundClaimRecorder{}
				ops := rec.ops(t, tier.output)
				invokedAt := time.Now()
				clock := invokedAt
				ops.Now = func() time.Time { return clock }
				ops.InvokedAt = invokedAt
				ops.ClaimWindow = window
				ops.Claim = hookClaimWithBdStore
				store := &hookClaimLandingStore{}
				store.install(t, &clock, invokedAt, tc.casAt, 5*time.Second, nil)

				var stdout, stderr bytes.Buffer
				code := doHookClaim("query", "/rig", hookClaimOptions{
					Assignee:     "worker-1",
					RouteTargets: []string{"worker"},
					JSON:         true,
					DrainAck:     true,
				}, ops, &stdout, &stderr)

				if code != 0 {
					t.Fatalf("code = %d, want 0; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
				}
				if result := decodeTurnBoundResult(t, stdout.String()); result.Action != "work" || result.BeadID != "work-1" {
					t.Fatalf("result = %+v, want the work receipt for work-1", result)
				}
				if len(rec.releases) != 0 || len(rec.claimReleased) != 0 {
					t.Fatalf("releases = %v events = %+v, want none: the CAS landed inside window + reserve", rec.releases, rec.claimReleased)
				}
				if want := []string{"work-1"}; !reflect.DeepEqual(store.updates, want) || !reflect.DeepEqual(store.shows, want) {
					t.Fatalf("updates = %v shows = %v, want one CAS and one canonical readback of work-1", store.updates, store.shows)
				}
				if rec.drainAcked {
					t.Fatal("a delivered claim acknowledged the drain")
				}
			})
		}
	}
}

// TestHookClaimLandingRecorder pins the recorder contract the tiers' straddle
// fence rests on: the shared classifier records a successful CAS before any
// read or ownership screen, records nothing for a failed or lost CAS, and each
// candidate's recorder is its own.
func TestHookClaimLandingRecorder(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	casReturn := start.Add(7 * time.Second)

	classify := func(t *testing.T, claimOK bool, claimErr error, projected, canonical string, readErr error) (*hookClaimLanding, *time.Time, int) {
		t.Helper()
		clock := start
		reloads := 0
		ctx, landing := withHookClaimLanding(context.Background(), func() time.Time { return clock })
		_, _, _ = hookClaimThroughStore(ctx, "work-1", "worker-1",
			func() (beads.Bead, bool, error) {
				clock = casReturn
				return beads.Bead{ID: "work-1", Status: "in_progress", Assignee: projected}, claimOK, claimErr
			},
			func(string) (beads.Bead, error) {
				clock = clock.Add(time.Minute)
				return beads.Bead{ID: "work-1", Assignee: "other-seat"}, nil
			},
			func(string) (beads.Bead, error) {
				reloads++
				clock = clock.Add(time.Minute)
				return beads.Bead{ID: "work-1", Status: "in_progress", Assignee: canonical}, readErr
			})
		return landing, &clock, reloads
	}

	for _, tc := range []struct {
		name       string
		projected  string
		canonical  string
		readErr    error
		wantReload int
	}{
		{"claimed", "worker-1", "worker-1", nil, 1},
		{"canonical readback fails", "worker-1", "worker-1", errors.New("store unavailable"), 1},
		{"canonical owner is another seat", "worker-1", "other-seat", nil, 1},
		{"mutation projection names another seat", "other-seat", "worker-1", nil, 0},
	} {
		t.Run("committed CAS records before reads/"+tc.name, func(t *testing.T) {
			landing, _, reloads := classify(t, true, nil, tc.projected, tc.canonical, tc.readErr)
			if reloads != tc.wantReload {
				t.Fatalf("canonical readbacks = %d, want %d", reloads, tc.wantReload)
			}
			if got := landing.landedAt(); !got.Equal(casReturn) {
				t.Fatalf("landedAt = %v, want the CAS return %v", got, casReturn)
			}
		})
	}

	for _, tc := range []struct {
		name     string
		claimOK  bool
		claimErr error
	}{
		{"failed CAS", false, errors.New("write failed")},
		{"lost race", false, nil},
	} {
		t.Run(tc.name+" records nothing", func(t *testing.T) {
			landing, clock, reloads := classify(t, tc.claimOK, tc.claimErr, "worker-1", "worker-1", nil)
			if reloads != 0 {
				t.Fatalf("canonical readbacks = %d, want none without a committed CAS", reloads)
			}
			if !landing.at.IsZero() {
				t.Fatalf("recorded landing = %v, want none", landing.at)
			}
			if got := landing.landedAt(); !got.Equal(*clock) {
				t.Fatalf("landedAt = %v, want the post-return fallback %v", got, *clock)
			}
		})
	}

	t.Run("a context without a recorder is safe", func(t *testing.T) {
		recordHookClaimLanding(context.Background())
		claimed, ok, err := hookClaimThroughStore(context.Background(), "work-1", "worker-1",
			func() (beads.Bead, bool, error) {
				return beads.Bead{ID: "work-1", Assignee: "worker-1"}, true, nil
			},
			func(string) (beads.Bead, error) {
				return beads.Bead{}, errors.New("conflict read on a committed claim")
			},
			func(string) (beads.Bead, error) { return beads.Bead{ID: "work-1", Assignee: "worker-1"}, nil })
		if err != nil || !ok || claimed.ID != "work-1" {
			t.Fatalf("hookClaimThroughStore = (%+v, %v, %v), want the claim", claimed, ok, err)
		}
	})

	t.Run("an unopened invocation window never trips the fence", func(t *testing.T) {
		var ops hookClaimOps
		ops.Now = func() time.Time { return casReturn.Add(time.Hour) }
		_, landing := withHookClaimLanding(context.Background(), ops.nowOrWallClock)
		if ops.claimLandingWindowSpent(landing.landedAt()) {
			t.Fatal("a zero InvokedAt tripped the straddle fence")
		}
	})

	// Candidate one records an early landing and is then rejected by its
	// canonical owner; candidate two's op records nothing and returns late. A
	// recorder shared across candidates would hand candidate two the early
	// landing and keep a straddling claim.
	t.Run("each candidate gets its own recorder", func(t *testing.T) {
		rec := &turnBoundClaimRecorder{}
		ops := rec.ops(t, `[{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker"}},{"id":"work-2","status":"open","metadata":{"gc.routed_to":"worker"}}]`)
		clock := start
		ops.Now = func() time.Time { return clock }
		ops.InvokedAt = start
		ops.ClaimWindow = 40 * time.Second
		ops.EmitClaimRejected = func(string, string, string) {}
		ops.Claim = func(ctx context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			rec.claims = append(rec.claims, beadID)
			if beadID == "work-1" {
				return hookClaimThroughStore(ctx, beadID, assignee,
					func() (beads.Bead, bool, error) {
						clock = start.Add(time.Second)
						return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, true, nil
					},
					func(string) (beads.Bead, error) {
						return beads.Bead{}, errors.New("conflict read on a committed claim")
					},
					func(string) (beads.Bead, error) {
						return beads.Bead{ID: beadID, Status: "in_progress", Assignee: "other-seat"}, nil
					})
			}
			clock = start.Add(ops.ClaimWindow + hookClaimMutationTimeout + time.Second)
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, true, nil
		}

		var stdout, stderr bytes.Buffer
		code := doHookClaim("query", "/rig", hookClaimOptions{
			Assignee:     "worker-1",
			RouteTargets: []string{"worker"},
			JSON:         true,
		}, ops, &stdout, &stderr)

		if code != 1 {
			t.Fatalf("code = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
		}
		if want := []string{"work-1", "work-2"}; !reflect.DeepEqual(rec.claims, want) {
			t.Fatalf("claims = %v, want %v", rec.claims, want)
		}
		if want := []string{"work-2"}; !reflect.DeepEqual(rec.releases, want) {
			t.Fatalf("releases = %v, want %v: candidate two landed late and must unwind", rec.releases, want)
		}
	})
}
