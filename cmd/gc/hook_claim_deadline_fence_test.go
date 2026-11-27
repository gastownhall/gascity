package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// hookDeadlineClaimBudget is the reporter's `gc hook --claim --timeout 15s`.
// Its answer margin is 3s, so reads end at 12s; the claim reserve is a third
// of those 12s (4s) and the claim window the 8s before it.
const hookDeadlineClaimBudget = 15 * time.Second

// deadlineClaim drives claimHookWorkWithRunner for an invocation that began at
// t0 with a deadline budget after it, against a fake clock each store read
// advances. The process start is pinned to t0 too, so the answer margin is the
// one the invocation would compute.
type deadlineClaim struct {
	rec   *turnBoundClaimRecorder
	ops   hookClaimOps
	t0    time.Time
	clock time.Time
	reads []time.Duration
	// readCtx holds the context each store read ran under.
	readCtx []context.Context
}

// newDeadlineClaim starts a claim invocation that began realAge ago on the wall
// clock, so the read and claim contexts built from the wall clock line up with
// the fake one at the instant the test starts.
func newDeadlineClaim(t *testing.T, realAge, budget time.Duration) *deadlineClaim {
	t.Helper()
	t0 := time.Now().Add(-realAge)
	prevStart := hookProcessStart
	hookProcessStart = t0
	t.Cleanup(func() { hookProcessStart = prevStart })
	c := &deadlineClaim{rec: &turnBoundClaimRecorder{}, t0: t0, clock: t0}
	ops := c.rec.ops(t, "")
	ops.Runner = nil
	ops.ReadWorkStarted = noStartedClaims
	ops.EmitClaimRejected = func(string, string, string) {}
	ops.ListContinuation = func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
		return nil, nil
	}
	ops.Now = func() time.Time { return c.clock }
	ops.Sleep = func(d time.Duration) { c.clock = c.clock.Add(d) }
	ops.InvokedAt = t0
	if budget > 0 {
		ops.Deadline = t0.Add(budget)
	}
	c.ops = ops
	return c
}

// reader is a store runner whose read ends readEnd past t0 and answers out.
func (c *deadlineClaim) reader(readEnd time.Duration, out string) hookStoreRunner {
	return func(ctx context.Context, _, _ string, _ []string) (string, error) {
		c.reads = append(c.reads, c.clock.Sub(c.t0))
		c.readCtx = append(c.readCtx, ctx)
		c.clock = c.t0.Add(readEnd)
		return out, nil
	}
}

// run runs the claim door over one store with run as its reader.
func (c *deadlineClaim) run(run hookStoreRunner) (int, string, string) {
	stores := []hookStore{{dir: "/rig"}}
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("query", "/rig", nil, stores, hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		DrainAck:     true,
		JSON:         true,
	}, c.ops, run, func(string, error) {}, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// resolvedClaimBudgets is the claim window and reserve applyDefaults resolves
// for ops.
func resolvedClaimBudgets(ops hookClaimOps) (window, reserve time.Duration) {
	ops.applyDefaults()
	return ops.ClaimWindow, ops.ClaimReserve
}

// TestHookClaimRefusesCandidatePastDeadlineFence pins AC-4 of #5533: under
// `gc hook --claim --timeout 15s`, a read that reaches a claimable candidate
// only after the claim fence (8s in: the 12s read budget less the 4s claim
// reserve) mints no claim. The hook refuses it before any write, writes nothing
// on stdout, exits non-zero and records execution.claim_window_expired, so the
// caller's budget never runs out mid-claim.
func TestHookClaimRefusesCandidatePastDeadlineFence(t *testing.T) {
	t.Setenv("GC_HOOK_CLAIM_WINDOW", "")

	t.Run("the fence is fitted inside the deadline", func(t *testing.T) {
		c := newDeadlineClaim(t, 9*time.Second, hookDeadlineClaimBudget)
		if window, reserve := resolvedClaimBudgets(c.ops); window != 8*time.Second || reserve != 4*time.Second {
			t.Fatalf("claim window, reserve = %s, %s; want 8s, 4s under a 15s deadline", window, reserve)
		}
	})

	t.Run("a candidate reached at 9s is refused before any write", func(t *testing.T) {
		c := newDeadlineClaim(t, 9*time.Second, hookDeadlineClaimBudget)
		code, stdout, stderr := c.run(c.reader(9*time.Second, turnBoundRoutedWork))
		if code == 0 {
			t.Fatalf("exit = 0, want non-zero; stdout=%q stderr=%s", stdout, stderr)
		}
		if len(c.rec.claims) != 0 {
			t.Fatalf("claims = %v, want none: the candidate arrived after the deadline's claim fence", c.rec.claims)
		}
		if stdout != "" {
			t.Fatalf("stdout = %q, want nothing: a refused claim is not a work or drain answer", stdout)
		}
		if len(c.rec.windowExpired) != 1 {
			t.Fatalf("claim_window_expired events = %d, want 1", len(c.rec.windowExpired))
		}
		if !strings.Contains(stderr, "8s claim window") {
			t.Fatalf("stderr = %q, want the 8s claim window named", stderr)
		}
	})

	t.Run("a candidate reached inside the fence is claimed", func(t *testing.T) {
		c := newDeadlineClaim(t, 7*time.Second, hookDeadlineClaimBudget)
		code, stdout, stderr := c.run(c.reader(7*time.Second, turnBoundRoutedWork))
		if code != 0 || len(c.rec.claims) != 1 {
			t.Fatalf("exit = %d, claims = %v; want 0 and one claim inside the fence; stderr=%s", code, c.rec.claims, stderr)
		}
		if result := decodeTurnBoundResult(t, stdout); result.Action != "work" {
			t.Fatalf("action = %q, want work", result.Action)
		}
	})

	t.Run("a spent budget floors the window and refuses the claim", func(t *testing.T) {
		c := newDeadlineClaim(t, 0, 3*time.Nanosecond)
		if window, reserve := resolvedClaimBudgets(c.ops); window != time.Nanosecond || reserve != time.Nanosecond {
			t.Fatalf("claim window, reserve = %s, %s; want both floored at 1ns, read as spent", window, reserve)
		}
		code, stdout, stderr := c.run(c.reader(time.Millisecond, turnBoundRoutedWork))
		if code == 0 || len(c.rec.claims) != 0 || stdout != "" || len(c.rec.windowExpired) != 1 {
			t.Fatalf("exit = %d, claims = %v, stdout = %q, expired = %d; want a refusal before any write; stderr=%s",
				code, c.rec.claims, stdout, len(c.rec.windowExpired), stderr)
		}
	})

	t.Run("an empty read under a spent budget still drains no_work", func(t *testing.T) {
		c := newDeadlineClaim(t, 0, 3*time.Nanosecond)
		code, stdout, stderr := c.run(c.reader(time.Millisecond, "[]"))
		if code != 0 {
			t.Fatalf("exit = %d, want 0 for an idle store; stderr=%s", code, stderr)
		}
		if result := decodeTurnBoundResult(t, stdout); result.Action != "drain" || result.Reason != "no_work" {
			t.Fatalf("result = %+v, want a no_work drain", result)
		}
	})

	t.Run("GC_HOOK_CLAIM_WINDOW wins when shorter than the deadline's fence", func(t *testing.T) {
		t.Setenv("GC_HOOK_CLAIM_WINDOW", "5s")
		c := newDeadlineClaim(t, 6*time.Second, hookDeadlineClaimBudget)
		if window, reserve := resolvedClaimBudgets(c.ops); window != 5*time.Second || reserve != 4*time.Second {
			t.Fatalf("claim window, reserve = %s, %s; want 5s, 4s", window, reserve)
		}
		code, stdout, _ := c.run(c.reader(6*time.Second, turnBoundRoutedWork))
		if code == 0 || len(c.rec.claims) != 0 || stdout != "" {
			t.Fatalf("exit = %d, claims = %v, stdout = %q; want the 5s window to refuse a candidate reached at 6s", code, c.rec.claims, stdout)
		}
	})

	t.Run("claim-read retries stop at the fence", func(t *testing.T) {
		withProductionClaimRetryPacing(t)
		c := newDeadlineClaim(t, 3*time.Second, hookDeadlineClaimBudget)
		code, stdout, stderr := c.run(func(context.Context, string, string, []string) (string, error) {
			c.reads = append(c.reads, c.clock.Sub(c.t0))
			c.clock = c.clock.Add(3 * time.Second)
			return "", errors.New("bd ready: exit status 1")
		})
		if code == 0 || stdout != "" {
			t.Fatalf("exit = %d, stdout = %q; want a failed read reported without an answer", code, stdout)
		}
		if len(c.reads) != 1 {
			t.Fatalf("reads began at %v, want one: a retry after the 5s pacing would start past the 8s fence", c.reads)
		}
		if !strings.Contains(stderr, "8s claim window") {
			t.Fatalf("stderr = %q, want the 8s claim window named as why retries stopped", stderr)
		}
	})
}

// TestHookClaimReleasesLandingPastDeadline pins AC-5 of #5533: under
// `gc hook --claim --timeout 15s`, a claim mutation that starts inside the
// fence runs on the 4s claim reserve, never past the 12s by which the claim
// must land (the deadline less its 3s answer margin), and a claim that still
// lands after those 12s is released as a straddle (claim_window_straddled)
// with nothing on stdout, rather than handed to a caller whose budget is gone.
func TestHookClaimReleasesLandingPastDeadline(t *testing.T) {
	t.Setenv("GC_HOOK_CLAIM_WINDOW", "")
	claimBy := 12 * time.Second

	landingAt := func(c *deadlineClaim, landed time.Duration) {
		claim := c.ops.Claim
		c.ops.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
			c.clock = c.t0.Add(landed)
			return claim(ctx, dir, env, beadID, assignee)
		}
	}

	t.Run("a claim landing at 12.5s is released", func(t *testing.T) {
		c := newDeadlineClaim(t, 7*time.Second, hookDeadlineClaimBudget)
		landingAt(c, 12500*time.Millisecond)
		code, stdout, stderr := c.run(c.reader(7*time.Second, turnBoundRoutedWork))
		if len(c.rec.claimCtx) != 1 {
			t.Fatalf("claim contexts = %d, want one claim started inside the fence; stderr=%s", len(c.rec.claimCtx), stderr)
		}
		deadline, ok := c.rec.claimCtx[0].Deadline()
		if !ok {
			t.Fatal("the claim ran with no deadline")
		}
		if limit := c.t0.Add(claimBy); deadline.After(limit) {
			t.Fatalf("claim deadline = %s past the invocation, want at most %s: the claim must not run past the deadline less its answer margin",
				deadline.Sub(c.t0).Round(time.Millisecond), claimBy)
		}
		if code == 0 {
			t.Fatalf("exit = 0, want non-zero; stdout=%q stderr=%s", stdout, stderr)
		}
		if stdout != "" {
			t.Fatalf("stdout = %q, want nothing: a claim that landed past the deadline is not delivered", stdout)
		}
		if len(c.rec.releases) != 1 || c.rec.releases[0] != "work-1" {
			t.Fatalf("releases = %v, want [work-1]", c.rec.releases)
		}
		if len(c.rec.claimReleased) != 1 || c.rec.claimReleased[0].Reason != hookClaimReleaseReasonStraddled {
			t.Fatalf("claim releases = %+v, want one %s", c.rec.claimReleased, hookClaimReleaseReasonStraddled)
		}
		if !strings.Contains(stderr, "4s claim reserve") {
			t.Fatalf("stderr = %q, want the 4s claim reserve named", stderr)
		}
	})

	t.Run("a claim landing inside the reserve stands", func(t *testing.T) {
		c := newDeadlineClaim(t, 7*time.Second, hookDeadlineClaimBudget)
		landingAt(c, 11900*time.Millisecond)
		code, stdout, stderr := c.run(c.reader(7900*time.Millisecond, turnBoundRoutedWork))
		if code != 0 || len(c.rec.releases) != 0 {
			t.Fatalf("exit = %d, releases = %v; want the claim delivered; stderr=%s", code, c.rec.releases, stderr)
		}
		if result := decodeTurnBoundResult(t, stdout); result.Action != "work" || result.BeadID != "work-1" {
			t.Fatalf("result = %+v, want work-1 delivered", result)
		}
	})
}

// TestHookClaimDeadlineBoundsOnlyTheReadsAndTheClaim pins how far the --claim
// deadline reaches with every readback and write around the claim slow. The
// store read and the claim mutation run under contexts that end by the
// deadline less its answer margin. Nothing after them runs under the
// deadline: the post-claim drain re-read, the identity stamp and its readback,
// the work_started record, the current-claim stamp and an adopted claim's
// ownership readback each keep their own budget, so the answer comes after
// the deadline, as gc hook --help says it can. Their time is never charged to
// the claim: a claim that landed in time is still handed out, and an adopted
// claim is still served.
//
// Each slow step advances the clock the claim fences read (ops.Now) instead of
// sleeping. The read and claim contexts are wall-clock contexts built from t0,
// and the test ends long before any of them would.
func TestHookClaimDeadlineBoundsOnlyTheReadsAndTheClaim(t *testing.T) {
	t.Setenv("GC_HOOK_CLAIM_WINDOW", "")
	// A 1.5s budget keeps a 500ms answer margin, so the read and the claim end
	// by 1s: a 667ms claim window and a 333ms reserve. Five 400ms steps after
	// a prompt read and claim end past the 1.5s deadline.
	const (
		budget    = 1500 * time.Millisecond
		stepDelay = 400 * time.Millisecond
	)
	for _, tc := range []struct {
		name, output, reason string
		minted               bool
		steps                []string
	}{
		{
			name: "a claim minted in time", output: startedRoutedWork, reason: "claimed", minted: true,
			steps: []string{"drain_pending", "stamp", "readback", "record", "current"},
		},
		{
			name: "an adopted claim", output: startedAssignedWork, reason: "existing_assignment",
			steps: []string{"readback", "stamp", "readback", "record", "current"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t0 := time.Now()
			clock := t0
			prevStart := hookProcessStart
			hookProcessStart = t0
			t.Cleanup(func() { hookProcessStart = prevStart })
			deadline := t0.Add(budget)
			claimBy := deadline.Add(-hookArchiveMargin(deadline, t0))

			l := &startedLog{}
			ops := l.ops(tc.output, nil, func(string, hookStartedFence, hookStartedKey, string) (hookStartedResult, error) {
				return hookStartedRecorded, nil
			})
			ops.Runner = nil
			ops.InvokedAt, ops.Deadline = t0, deadline
			ops.Now = func() time.Time { return clock }
			ops.Sleep = func(d time.Duration) { clock = clock.Add(d) }
			ops.ListContinuation = func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
				return nil, nil
			}
			var (
				claimed  bool
				claimCtx context.Context
				steps    []string
				// stepEnds holds the deadline of each step that takes a context.
				stepEnds []time.Time
			)
			slow := func(step string) {
				steps = append(steps, step)
				clock = clock.Add(stepDelay)
			}
			slowWithin := func(ctx context.Context, step string) {
				end, _ := ctx.Deadline()
				stepEnds = append(stepEnds, end)
				slow(step)
			}
			claim := ops.Claim
			ops.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
				claimCtx, claimed = ctx, true
				return claim(ctx, dir, env, beadID, assignee)
			}
			// The drain probe also runs before the claim; only its re-read
			// after the claim landed is part of what follows the claim.
			ops.DrainPending = func(string) (bool, error) {
				if claimed {
					slow("drain_pending")
				}
				return false, nil
			}
			ops.StampWorkMeta = func(ctx context.Context, _ string, _ []string, _, _ string, _ map[string]string) error {
				slowWithin(ctx, "stamp")
				return nil
			}
			ops.ReadWorkMeta = func(ctx context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, error) {
				slowWithin(ctx, "readback")
				return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, nil
			}
			record := ops.RecordWorkStarted
			ops.RecordWorkStarted = func(sessionID string, fence hookStartedFence, key hookStartedKey, via string) (hookStartedResult, error) {
				slow("record")
				return record(sessionID, fence, key, via)
			}
			stampClaim := ops.StampSessionClaim
			ops.StampSessionClaim = func(sessionID, beadID string) error {
				slow("current")
				return stampClaim(sessionID, beadID)
			}

			// The session env rides on the store leg: the claim takes its env
			// from there, and without it every session-scoped step is skipped.
			env := startedEnv(startedToken, "7")
			var readCtx context.Context
			run := func(ctx context.Context, _, _ string, _ []string) (string, error) {
				readCtx = ctx
				return tc.output, nil
			}
			var stdout, stderr bytes.Buffer
			code := claimHookWorkWithRunner("q", "/rig", env, []hookStore{{dir: "/rig", env: env}}, startedOpts(env), ops, run, func(string, error) {}, &stdout, &stderr)
			answered := clock.Sub(t0)

			if code != 0 || len(l.releases) != 0 {
				t.Fatalf("exit = %d, releases = %v; want the claim handed out; stderr=%s", code, l.releases, stderr.String())
			}
			if got := decodeTurnBoundResult(t, stdout.String()); got.Action != "work" || got.BeadID != "work-1" || got.Reason != tc.reason {
				t.Fatalf("result = %+v, want work-1 handed out as %s", got, tc.reason)
			}
			if readCtx == nil {
				t.Fatal("the store was never read")
			}
			if end, ok := readCtx.Deadline(); !ok || !end.Equal(claimBy) {
				t.Fatalf("store read ran until %s in (deadline set %t), want %s in: the deadline less its answer margin",
					end.Sub(t0), ok, claimBy.Sub(t0))
			}
			if tc.minted {
				if claimCtx == nil {
					t.Fatal("no claim mutation ran")
				}
				if end, ok := claimCtx.Deadline(); !ok || end.After(claimBy) {
					t.Fatalf("claim mutation ran until %s in (deadline set %t), want at most %s in", end.Sub(t0), ok, claimBy.Sub(t0))
				}
			}
			if !slices.Equal(steps, tc.steps) {
				t.Fatalf("slow steps = %v, want %v", steps, tc.steps)
			}
			for i, end := range stepEnds {
				if !end.After(deadline) {
					t.Fatalf("step %d with a context ran until %s in, want its own budget, past the %s deadline", i, end.Sub(t0), budget)
				}
			}
			if answered <= budget {
				t.Fatalf("answered %s in, want past the %s deadline: the slow steps after the claim did not run", answered.Round(time.Millisecond), budget)
			}
		})
	}
}
