package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ctxIgnoringStartProvider blocks inside Start until either startDelay
// elapses or ctx is canceled, then unconditionally marks the session as
// running and returns nil. It mirrors a real-world failure shape: a provider
// whose final stage (overlay copy, tmux handshake, ACP init) completes
// "successfully" from its own point of view even though its caller's
// deadline has already expired. The reconciler has no signal that anything
// went wrong - no err, no outcome flag - so it records outcome=success
// with a duration far larger than the configured startup timeout.
type ctxIgnoringStartProvider struct {
	*runtime.Fake
	startDelay time.Duration
}

func (p *ctxIgnoringStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	select {
	case <-time.After(p.startDelay):
	case <-ctx.Done():
	}
	// Deliberately drop ctx.Err() and register the session anyway. This is
	// the buggy provider behavior we want to expose at the executePreparedStartWave
	// layer.
	return p.Fake.Start(context.Background(), name, cfg)
}

// TestExecutePreparedStartWave_StartOutlivesDeadlineReportsDeadlineExceeded
// verifies that a provider returning nil after the startup deadline cannot
// mask the timeout as a successful wake.
func TestExecutePreparedStartWave_StartOutlivesDeadlineReportsDeadlineExceeded(t *testing.T) {
	sp := &ctxIgnoringStartProvider{
		Fake:       runtime.NewFake(),
		startDelay: 500 * time.Millisecond,
	}
	item := preparedStart{
		candidate: startCandidate{
			session: &beads.Bead{
				Metadata: map[string]string{
					"session_name": "deadline-witness",
					"template":     "worker",
				},
			},
			tp: TemplateParams{
				Command:      "claude",
				SessionName:  "deadline-witness",
				TemplateName: "worker",
			},
		},
		cfg: runtime.Config{Command: "claude"},
	}

	const startupTimeout = 50 * time.Millisecond
	before := time.Now()
	results := executePreparedStartWave(
		context.Background(),
		[]preparedStart{item},
		sp,
		nil, // store == nil uses RuntimeHandle path and skips bead-backed staleKey branch
		startupTimeout,
	)
	elapsed := time.Since(before)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]

	// Sanity: the work really outran the startup timeout - this is the
	// observable symptom. If this assertion fails the test itself is wrong.
	if elapsed <= startupTimeout {
		t.Fatalf("wave returned in %v, which is <= startupTimeout %v; provider did not hold ctx open as intended", elapsed, startupTimeout)
	}
	measured := r.finished.Sub(r.started)
	if measured <= startupTimeout {
		t.Fatalf("recorded duration = %v, want > startupTimeout %v", measured, startupTimeout)
	}

	// After the fix: outcome must reflect the deadline; err==nil must not
	// override startCtx.Err().
	if r.outcome == "success" {
		t.Fatalf("outcome = %q with err=%v and recorded duration %v; "+
			"startCtx deadline (%v) expired during Start but outcome masks it as success. "+
			"See runPreparedStartCandidate - the `err == nil` case "+
			"is evaluated before `startCtx.Err() == context.DeadlineExceeded`.",
			r.outcome, r.err, measured, startupTimeout)
	}
	if r.outcome != "deadline_exceeded" {
		t.Fatalf("outcome = %q, want %q", r.outcome, "deadline_exceeded")
	}
	if r.err == nil || !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a wrapper around context.DeadlineExceeded", r.err)
	}
	if !strings.Contains(r.err.Error(), "deadline") {
		t.Fatalf("err text = %q, want mention of deadline", r.err.Error())
	}
	if strings.Contains(r.err.Error(), "resuming session") {
		t.Fatalf("err text = %q, want start/resume-neutral text", r.err.Error())
	}
}

func TestExecutePreparedStartWave_ResumeSessionKeyStaleCheckAfterInTimeStartStaysSuccess(t *testing.T) {
	sp := runtime.NewFake()
	item := preparedStart{
		candidate: startCandidate{
			session: &beads.Bead{
				ID: "gc-resume",
				Metadata: map[string]string{
					"session_name": "resume-deadline-witness",
					"session_key":  "resume-key",
					"template":     "worker",
				},
			},
			tp: TemplateParams{
				Command:      "claude --resume resume-key",
				SessionName:  "resume-deadline-witness",
				TemplateName: "worker",
			},
		},
		cfg: runtime.Config{Command: "claude --resume resume-key"},
	}

	const startupTimeout = 50 * time.Millisecond
	before := time.Now()
	results := executePreparedStartWave(
		context.Background(),
		[]preparedStart{item},
		sp,
		nil,
		startupTimeout,
	)
	elapsed := time.Since(before)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if elapsed <= startupTimeout {
		t.Fatalf("wave returned in %v, which is <= startupTimeout %v; stale-key detection did not cross the deadline", elapsed, startupTimeout)
	}
	if r.outcome != "success" {
		t.Fatalf("outcome = %q, err = %v; want success because Start returned before the deadline and the session stayed alive", r.outcome, r.err)
	}
	if r.err != nil {
		t.Fatalf("err = %v, want nil", r.err)
	}
}

type ctxCancelingStartProvider struct {
	*runtime.Fake
	cancel func()
}

func (p *ctxCancelingStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.cancel()
	<-ctx.Done()
	return p.Fake.Start(context.Background(), name, cfg)
}

func TestExecutePreparedStartWave_CanceledContextReportsCanceled(t *testing.T) {
	parentCtx, cancel := context.WithCancel(context.Background())
	sp := &ctxCancelingStartProvider{
		Fake:   runtime.NewFake(),
		cancel: cancel,
	}
	item := preparedStart{
		candidate: startCandidate{
			session: &beads.Bead{
				Metadata: map[string]string{
					"session_name": "cancel-witness",
					"template":     "worker",
				},
			},
			tp: TemplateParams{
				Command:      "claude",
				SessionName:  "cancel-witness",
				TemplateName: "worker",
			},
		},
		cfg: runtime.Config{Command: "claude"},
	}

	results := executePreparedStartWave(
		parentCtx,
		[]preparedStart{item},
		sp,
		nil,
		time.Second,
	)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.outcome != "canceled" {
		t.Fatalf("outcome = %q, want %q", r.outcome, "canceled")
	}
	if r.err == nil || !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err = %v, want a wrapper around context.Canceled", r.err)
	}
	if strings.Contains(r.err.Error(), "resuming session") {
		t.Fatalf("err text = %q, want start/resume-neutral text", r.err.Error())
	}
}

type initializingAfterDeadlineProvider struct {
	*runtime.Fake
}

func (p *initializingAfterDeadlineProvider) Start(ctx context.Context, _ string, _ runtime.Config) error {
	<-ctx.Done()
	return runtime.ErrSessionInitializing
}

func TestExecutePreparedStartWave_InitializingAfterDeadlineBacksOffSilently(t *testing.T) {
	sp := &initializingAfterDeadlineProvider{Fake: runtime.NewFake()}
	item := preparedStart{
		candidate: startCandidate{
			session: &beads.Bead{
				Metadata: map[string]string{
					"session_name": "initializing-witness",
					"template":     "worker",
				},
			},
			tp: TemplateParams{
				Command:      "claude",
				SessionName:  "initializing-witness",
				TemplateName: "worker",
			},
		},
		cfg: runtime.Config{Command: "claude"},
	}

	results := executePreparedStartWave(
		context.Background(),
		[]preparedStart{item},
		sp,
		nil,
		50*time.Millisecond,
	)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.outcome != "session_initializing" {
		t.Fatalf("outcome = %q, want %q", r.outcome, "session_initializing")
	}
	if r.err != nil {
		t.Fatalf("err = %v, want nil for silent initializing backoff", r.err)
	}
}
