package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// causeProvider records why each Start's ctx had already ended when the Start
// began (nil = live), and fails such a Start the way a provider call on a dead
// ctx does.
type causeProvider struct {
	*runtime.Fake
	mu     sync.Mutex
	causes map[string]error
}

func (p *causeProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.mu.Lock()
	p.causes[name] = context.Cause(ctx)
	p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.Fake.Start(ctx, name, cfg)
}

// TestSlowPrepareExpiresTheBatch starts three leased async sessions in one
// batch. With startupTimeout 0 the lease TTL is 60s and SafeUntil is 50s after
// the record. In the slow case the batch's last prepare takes 55s.
func TestSlowPrepareExpiresTheBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("about a minute of wall clock: the lease clock is wall time")
	}
	for _, tc := range []struct {
		name string
		slow time.Duration
	}{
		{"fast prepares", 0},
		{"one prepare past SafeUntil", 55 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			cfg := &config.City{}
			desired := map[string]TemplateParams{}
			var cands []startCandidate
			for _, name := range []string{"w1", "w2", "w3"} {
				bead, err := store.Create(beads.Bead{
					Title: name, Type: sessionBeadType, Labels: []string{sessionBeadLabel},
					Metadata: creatingMeta(map[string]string{
						"session_name": name, "template": name, "generation": "1",
						"continuation_epoch": "1", "instance_token": "tok-" + name, "pending_create_claim": "true",
					}),
				})
				if err != nil {
					t.Fatal(err)
				}
				tp := TemplateParams{Command: name, SessionName: name, TemplateName: name}
				desired[name] = tp
				cfg.Agents = append(cfg.Agents, config.Agent{Name: name})
				cands = append(cands, startCandidate{info: sessiontest.SeedBead(t, bead), tp: tp})
			}
			sp := &causeProvider{Fake: runtime.NewFake(), causes: map[string]error{}}
			var calls atomic.Int32
			prepare := func(startCandidate, *config.City) string {
				if calls.Add(1) == 3 {
					time.Sleep(tc.slow)
				}
				return ""
			}
			var tracker asyncStartTracker
			var log synchronizedBuffer
			clk := &clock.Fake{Time: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
			executePlannedStartsTraced(context.Background(), cands, cfg, desired, sp, store,
				"test-city", t.TempDir(), clk, events.Discard, 0, &log, &log, nil,
				withAsyncStartExecution(), withAsyncStartLimiter(newAsyncStartLimiter(3)), withAsyncStartTracker(&tracker),
				withTaskWorkDirResolver(prepare))
			if err := tracker.waitLaunchedStarts(context.Background(), 2*time.Minute); err != nil {
				t.Fatal(err)
			}
			for _, c := range cands {
				name := c.tp.SessionName
				sp.mu.Lock()
				cause, started := sp.causes[name]
				sp.mu.Unlock()
				if !started || cause != nil {
					t.Errorf("%s: Start called=%v, ctx at Start: %v; want a live lease", name, started, cause)
				}
			}
		})
	}
}
