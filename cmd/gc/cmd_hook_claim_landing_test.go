package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
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

// TestHookClaimTiersUnwindALateCASAfterCanonicalReadback is the other side of
// the landing fence: a CAS that itself returns after the claim window plus its
// mutation reserve is a straddle in either claiming tier, even though its
// canonical readback succeeds, and takes the existing unwind.
func TestHookClaimTiersUnwindALateCASAfterCanonicalReadback(t *testing.T) {
	const window = 40 * time.Second
	for _, tier := range hookClaimLandingTiers {
		t.Run(tier.name, func(t *testing.T) {
			rec := &turnBoundClaimRecorder{}
			ops := rec.ops(t, tier.output)
			invokedAt := time.Now()
			clock := invokedAt
			ops.Now = func() time.Time { return clock }
			ops.InvokedAt = invokedAt
			ops.ClaimWindow = window
			ops.Claim = hookClaimWithBdStore
			store := &hookClaimLandingStore{}
			store.install(t, &clock, invokedAt, window+hookClaimMutationTimeout+time.Second, time.Second, nil)

			var stdout, stderr bytes.Buffer
			code := doHookClaim("query", "/rig", hookClaimOptions{
				Assignee:     "worker-1",
				RouteTargets: []string{"worker"},
				JSON:         true,
				DrainAck:     true,
			}, ops, &stdout, &stderr)

			if code != 1 {
				t.Fatalf("code = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want no work receipt for an unwound claim", stdout.String())
			}
			if want := []string{"work-1"}; !reflect.DeepEqual(rec.releases, want) {
				t.Fatalf("releases = %v, want %v: a late CAS must self-release", rec.releases, want)
			}
			if len(rec.claimReleased) != 1 || rec.claimReleased[0].Reason != hookClaimReleaseReasonStraddled {
				t.Fatalf("bead.claim_released events = %+v, want one %q", rec.claimReleased, hookClaimReleaseReasonStraddled)
			}
			if !strings.Contains(stderr.String(), "landed after the 40s claim window") ||
				!strings.Contains(stderr.String(), "(invocation age 51s at landing)") {
				t.Fatalf("stderr = %q, want the claim-window diagnostic at the CAS landing age", stderr.String())
			}
			if want := []string{"work-1"}; !reflect.DeepEqual(store.updates, want) {
				t.Fatalf("updates = %v, want one CAS", store.updates)
			}
		})
	}
}

// TestHookClaimTiersStopOnCommittedClaimReadbackFailure pins that a committed
// CAS whose canonical readback fails stops either claiming tier: exit 1 with the
// committed-claim diagnostic, and no second candidate, CAS replay, work
// receipt, idle drain acknowledgement or release.
func TestHookClaimTiersStopOnCommittedClaimReadbackFailure(t *testing.T) {
	for _, tier := range hookClaimLandingTiers {
		t.Run(tier.name, func(t *testing.T) {
			rec := &turnBoundClaimRecorder{}
			ops := rec.ops(t, tier.output)
			invokedAt := time.Now()
			clock := invokedAt
			ops.Now = func() time.Time { return clock }
			ops.InvokedAt = invokedAt
			ops.ClaimWindow = 40 * time.Second
			ops.Claim = hookClaimWithBdStore
			store := &hookClaimLandingStore{}
			store.install(t, &clock, invokedAt, time.Second, time.Second, errors.New("store unavailable"))

			var stdout, stderr bytes.Buffer
			code := doHookClaim("query", "/rig", hookClaimOptions{
				Assignee:     "worker-1",
				RouteTargets: []string{"worker"},
				JSON:         true,
				DrainAck:     true,
			}, ops, &stdout, &stderr)

			if code != 1 {
				t.Fatalf("code = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "claimed work-1 but loading canonical bead failed") ||
				!strings.Contains(stderr.String(), "store unavailable") {
				t.Fatalf("stderr = %q, want the committed-claim readback diagnostic", stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want no work receipt and no drain result", stdout.String())
			}
			if want := []string{"work-1"}; !reflect.DeepEqual(store.updates, want) {
				t.Fatalf("updates = %v, want exactly one CAS: no replay and no second candidate", store.updates)
			}
			if rec.drainAcked {
				t.Fatal("a committed claim's readback failure acknowledged the drain")
			}
			if len(rec.releases) != 0 || len(rec.claimReleased) != 0 {
				t.Fatalf("releases = %v events = %+v, want none: the canonical state is unknown", rec.releases, rec.claimReleased)
			}
		})
	}
}

// claimRouteLandingStore is a class binding whose CAS moves a controlled clock
// to claimAt and whose reads after that CAS advance it by getTakes, so a test
// can tell the CAS return from the canonical readback. It is a pointer with
// pointer receivers: the binding builder keys maps by store, so it must be
// comparable.
type claimRouteLandingStore struct {
	beads.Store
	clock    *time.Time
	claimAt  time.Time
	getTakes time.Duration

	conflict       bool   // the CAS loses the race to other-seat
	projectedOwner string // overrides the CAS projection's assignee
	canonicalOwner string // overrides the canonical readback's assignee
	getErr         error  // fails reads after the CAS

	claims   int
	claimed  bool
	postGets int
}

func (s *claimRouteLandingStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	s.claims++
	*s.clock = s.claimAt
	if s.conflict {
		return beads.Bead{ID: id, Status: "in_progress", Assignee: "other-seat"}, false, nil
	}
	claimer, ok := s.Store.(interface {
		Claim(id, assignee string) (beads.Bead, bool, error)
	})
	if !ok {
		return beads.Bead{}, false, fmt.Errorf("the landing binding's leaf %T has no compare-and-swap claim", s.Store)
	}
	claimed, won, err := claimer.Claim(id, assignee)
	if err != nil || !won {
		return claimed, won, err
	}
	s.claimed = true
	// The CAS projection carries no metadata, so the caller can only see the
	// canonical metadata through the readback.
	projection := beads.Bead{ID: claimed.ID, Status: claimed.Status, Assignee: claimed.Assignee}
	if s.projectedOwner != "" {
		projection.Assignee = s.projectedOwner
	}
	return projection, true, nil
}

func (s *claimRouteLandingStore) Get(id string) (beads.Bead, error) {
	if !s.claimed {
		return s.Store.Get(id)
	}
	s.postGets++
	*s.clock = s.clock.Add(s.getTakes)
	if s.getErr != nil {
		return beads.Bead{}, s.getErr
	}
	bead, err := s.Store.Get(id)
	if err == nil && s.canonicalOwner != "" {
		bead.Assignee = s.canonicalOwner
	}
	return bead, err
}

// TestClassRoutedClaimRecordsLandingBeforeBindingReadback pins that a claim
// through the relocated class binding, by either routing entry (a bead already
// known resident, or the work store's not-found escalation), records its
// landing when GraphStore.Claim returns rather than after the canonical
// GraphStore.Get, and still returns the canonical bead and its metadata.
func TestClassRoutedClaimRecordsLandingBeforeBindingReadback(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	claimAt := start.Add(3 * time.Second)

	type outcome struct {
		ok       bool
		err      bool
		recorded bool
		postGets int
	}
	for _, entry := range []string{"known resident", "not-found escalation"} {
		for _, tc := range []struct {
			name  string
			setup func(*claimRouteLandingStore)
			want  outcome
		}{
			{"claimed", func(*claimRouteLandingStore) {}, outcome{ok: true, recorded: true, postGets: 1}},
			{"canonical readback fails", func(s *claimRouteLandingStore) { s.getErr = errors.New("binding unavailable") }, outcome{ok: true, err: true, recorded: true, postGets: 1}},
			{"lost race", func(s *claimRouteLandingStore) { s.conflict = true }, outcome{}},
			{"mutation projection names another seat", func(s *claimRouteLandingStore) { s.projectedOwner = "other-seat" }, outcome{recorded: true}},
			{"canonical owner is another seat", func(s *claimRouteLandingStore) { s.canonicalOwner = "other-seat" }, outcome{recorded: true, postGets: 1}},
		} {
			t.Run(entry+"/"+tc.name, func(t *testing.T) {
				leaf := newClaimRouteClassStore(t)
				mintClaimRouteBead(t, leaf, "gcg-700", map[string]string{"gc.root_bead_id": "root-7"})
				clock := start
				class := &claimRouteLandingStore{Store: leaf, clock: &clock, claimAt: claimAt, getTakes: time.Minute}
				tc.setup(class)
				route := newClaimRouteFor(t, class)
				workClaims := 0
				base := hookClaimOps{Claim: func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
					workClaims++
					return notFoundClaim(t, "gcg-700")(ctx, dir, env, beadID, assignee)
				}}
				if entry == "known resident" {
					route.resident["gcg-700"] = true
				}
				ops := classRoutedHookClaimOps(base, route)

				ctx, landing := withHookClaimLanding(context.Background(), func() time.Time { return clock })
				claimed, ok, err := ops.Claim(ctx, "/work", nil, "gcg-700", "worker-1")

				if ok != tc.want.ok || (err != nil) != tc.want.err {
					t.Fatalf("routed claim = (%+v, ok=%v, err=%v), want ok=%v err=%v", claimed, ok, err, tc.want.ok, tc.want.err)
				}
				if class.claims != 1 {
					t.Fatalf("binding CAS attempts = %d, want exactly one", class.claims)
				}
				if wantWork := map[bool]int{true: 0, false: 1}[entry == "known resident"]; workClaims != wantWork {
					t.Fatalf("work-store claims = %d, want %d: no second-store claim", workClaims, wantWork)
				}
				if class.postGets != tc.want.postGets {
					t.Fatalf("reads after the CAS = %d, want %d", class.postGets, tc.want.postGets)
				}
				if tc.want.recorded {
					if !landing.at.Equal(claimAt) {
						t.Fatalf("recorded landing = %v, want the GraphStore.Claim return %v", landing.at, claimAt)
					}
				} else if !landing.at.IsZero() {
					t.Fatalf("recorded landing = %v, want none for a CAS that did not commit", landing.at)
				}
				if tc.want.ok && !tc.want.err {
					if claimed.Assignee != "worker-1" || claimed.Metadata["gc.root_bead_id"] != "root-7" {
						t.Fatalf("claimed = %+v, want the canonical bead with its metadata", claimed)
					}
				}
			})
		}
	}
}

// TestHookClaimWithBdStoreConflictReadStaysMutationBound pins that a lost race
// keeps its diagnostic read on the claim's own mutation context: no canonical
// readback runner is constructed for a CAS this session did not commit. The
// read surfaces the winner, and a failed read degrades to an empty non-claim.
func TestHookClaimWithBdStoreConflictReadStaysMutationBound(t *testing.T) {
	for _, tc := range []struct {
		name       string
		showErr    error
		wantWinner string
	}{
		{"diagnostic read surfaces the winner", nil, "other-seat"},
		{"diagnostic read fails", errors.New("store unavailable"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalRunner := hookClaimCommandRunnerWithEnvContext
			t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })
			var ctxs []context.Context
			var ran [][]string
			hookClaimCommandRunnerWithEnvContext = func(ctx context.Context, _ map[string]string) beads.CommandRunner {
				ctxs = append(ctxs, ctx)
				return func(_ string, name string, args ...string) ([]byte, error) {
					ran = append(ran, append([]string(nil), args...))
					switch {
					case name == "bd" && reflect.DeepEqual(args, []string{"update", "work-1", "--claim", "--json"}):
						return []byte("Error: work-1 already claimed by other-seat"), errors.New("exit status 1")
					case name == "bd" && reflect.DeepEqual(args, []string{"show", "--json", "work-1"}):
						if tc.showErr != nil {
							return nil, tc.showErr
						}
						return []byte(`[{"id":"work-1","status":"in_progress","assignee":"other-seat"}]`), nil
					default:
						t.Fatalf("unexpected command: %s %q", name, args)
						return nil, nil
					}
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			current, ok, err := hookClaimWithBdStore(ctx, "/rig", nil, "work-1", "worker-1")
			if err != nil || ok {
				t.Fatalf("hookClaimWithBdStore = (%+v, ok=%v, err=%v), want a rejected non-claim", current, ok, err)
			}
			if current.Assignee != tc.wantWinner {
				t.Fatalf("rejected bead assignee = %q, want %q", current.Assignee, tc.wantWinner)
			}
			if len(ctxs) != 1 || ctxs[0] != ctx {
				t.Fatalf("runner constructions = %d, want only the mutation context's", len(ctxs))
			}
			if want := [][]string{{"update", "work-1", "--claim", "--json"}, {"show", "--json", "work-1"}}; !reflect.DeepEqual(ran, want) {
				t.Fatalf("bd calls = %q, want %q", ran, want)
			}
		})
	}
}
