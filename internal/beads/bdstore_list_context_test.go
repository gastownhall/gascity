package beads_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// fakeRunnerContext returns a CommandRunnerContext that records every ctx it
// is called with and returns canned output for specific commands.
func fakeRunnerContext(responses map[string]struct {
	out []byte
	err error
},
) (beads.CommandRunnerContext, *[]context.Context) {
	var gotCtxs []context.Context
	runner := func(ctx context.Context, _, name string, args ...string) ([]byte, error) {
		gotCtxs = append(gotCtxs, ctx)
		key := name + " " + strings.Join(args, " ")
		if resp, ok := responses[key]; ok {
			return resp.out, resp.err
		}
		return nil, fmt.Errorf("unexpected command: %s %s", name, strings.Join(args, " "))
	}
	return runner, &gotCtxs
}

func TestBdStoreListContextFallsBackToPlainListWithoutRunnerContext(t *testing.T) {
	t.Parallel()
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd list --json --include-infra --include-gates --limit 0`: {
			out: []byte(`[{"id":"bd-aaa","title":"first","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z"}]`),
		},
	})
	s := beads.NewBdStore("/city", runner)

	got, err := s.ListContext(context.Background(), beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("ListContext: %v", err)
	}
	if len(got) != 1 || got[0].ID != "bd-aaa" {
		t.Fatalf("ListContext = %+v, want the plain List result (no runner context configured)", got)
	}
}

func TestBdStoreListContextUsesConfiguredRunnerContext(t *testing.T) {
	t.Parallel()
	runnerCtx, gotCtxs := fakeRunnerContext(map[string]struct {
		out []byte
		err error
	}{
		`bd list --json --include-infra --include-gates --limit 0`: {
			out: []byte(`[{"id":"bd-ctx","title":"via ctx runner","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z"}]`),
		},
	})
	fallbackRunner := func(_, _ string, _ ...string) ([]byte, error) {
		t.Fatal("plain CommandRunner should not be used when a runner context is configured for ListContext")
		return nil, nil
	}
	s := beads.NewBdStore("/city", fallbackRunner, beads.WithBdStoreRunnerContext(runnerCtx))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, err := s.ListContext(ctx, beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("ListContext: %v", err)
	}
	if len(got) != 1 || got[0].ID != "bd-ctx" {
		t.Fatalf("ListContext = %+v, want the configured runner context's result", got)
	}
	if len(*gotCtxs) != 1 || (*gotCtxs)[0] != ctx {
		t.Fatalf("runner context calls = %v, want exactly one call with the caller's ctx", *gotCtxs)
	}
}

func TestBdStoreListContextRetriesOnInvalidConnection(t *testing.T) {
	t.Parallel()
	calls := 0
	goodJSON := []byte(`[{"id":"bd-x","title":"t","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z"}]`)
	runnerCtx := func(_ context.Context, _, _ string, _ ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("begin read tx: invalid connection")
		}
		return goodJSON, nil
	}
	s := beads.NewBdStore("/city", beads.ExecCommandRunner(), beads.WithBdStoreRunnerContext(runnerCtx))

	got, err := s.ListContext(context.Background(), beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("ListContext() error = %v, want nil after retry recovered", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListContext() returned %d beads, want 1", len(got))
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (1 transient + 1 success)", calls)
	}
}

func TestBdStoreListContextRespectsWispsTierMode(t *testing.T) {
	t.Parallel()
	runnerCtx := func(_ context.Context, _, name string, args ...string) ([]byte, error) {
		gotCmd := name + " " + strings.Join(args, " ")
		if strings.HasPrefix(gotCmd, "bd query ") {
			return []byte(`[{"id":"bd-w","title":"wisp","status":"open","issue_type":"task","created_at":"2026-05-01T00:00:02Z","ephemeral":true,"labels":["order-tracking"]}]`), nil
		}
		return []byte(`[{"id":"bd-nh","title":"no-history","status":"open","issue_type":"task","created_at":"2026-05-01T00:00:00Z","no_history":true,"labels":["order-tracking"]}]`), nil
	}
	fallbackRunner := func(_, _ string, _ ...string) ([]byte, error) {
		t.Fatal("plain CommandRunner should not be used when a runner context is configured for ListContext")
		return nil, nil
	}
	s := beads.NewBdStore("/city", fallbackRunner, beads.WithBdStoreRunnerContext(runnerCtx))

	got, err := s.ListContext(context.Background(), beads.ListQuery{Label: "order-tracking", TierMode: beads.TierWisps})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListContext returned %d beads, want 2 (issue + wisp tiers merged)", len(got))
	}
}

func TestBdStoreListContextSurfacesBackingError(t *testing.T) {
	t.Parallel()
	runnerCtx := func(_ context.Context, _, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("boom")
	}
	s := beads.NewBdStore("/city", beads.ExecCommandRunner(), beads.WithBdStoreRunnerContext(runnerCtx))

	_, err := s.ListContext(context.Background(), beads.ListQuery{AllowScan: true})
	if err == nil {
		t.Fatal("ListContext() error = nil, want error from backing runner")
	}
}
