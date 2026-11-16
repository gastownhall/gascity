package exec //nolint:revive // internal package, always imported with alias

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/testutil"
)

// deadlineMS parses the GC_EXEC_DEADLINE_MS an op echoed, failing the test
// when it is missing or not a non-negative integer.
func deadlineMS(t *testing.T, what, got string) time.Duration {
	t.Helper()
	ms, err := strconv.ParseInt(strings.TrimSpace(got), 10, 64)
	if err != nil || ms < 0 {
		t.Fatalf("%s: GC_EXEC_DEADLINE_MS = %q; want non-negative milliseconds", what, got)
	}
	return time.Duration(ms) * time.Millisecond
}

// within fails unless got is at most limit and more than limit-slack: the
// op saw a budget of limit, less the time it took to spawn.
func within(t *testing.T, what string, got, limit time.Duration) {
	t.Helper()
	const slack = 2 * time.Second
	if got > limit || got <= limit-slack {
		t.Errorf("%s: GC_EXEC_DEADLINE_MS = %v; want in (%v, %v]", what, got, limit-slack, limit)
	}
}

// Kills: the deadline set in cityEnv (a provider for no city never sees it),
// set on only some of the bounded ops, or a stale inherited value surviving.
func TestDeadlineEnvOnEveryOp(t *testing.T) {
	t.Setenv("GC_EXEC_DEADLINE_MS", "stale")
	const timeout = 7 * time.Second
	echo := `echo "$GC_EXEC_DEADLINE_MS"`
	for _, city := range []bool{false, true} {
		script, dir := observePack(t, `{"version":0,"capabilities":["proc.exec"]}`, map[string]string{
			"get-meta":      echo,
			"exec":          `cat >/dev/null; ` + echo,
			"watch-startup": echo + ` > "$dir/watch.env"; exit 2`,
		})
		p := NewProvider(script)
		if city {
			p = NewProviderForCity(script, "alpha", "/cities/alpha")
		}
		p.timeout = timeout
		what := map[bool]string{false: "no city", true: "city"}[city]

		v, err := p.GetMeta("s", "k")
		if err != nil {
			t.Fatalf("%s: get-meta: %v", what, err)
		}
		within(t, what+" get-meta", deadlineMS(t, what+" get-meta", v), timeout)

		out, code, err := p.Exec(t.Context(), "s", []string{"true"})
		if code != 0 || err != nil {
			t.Fatalf("%s: exec = %d, %v", what, code, err)
		}
		within(t, what+" exec", deadlineMS(t, what+" exec", string(out)), timeout)

		ctx, cancel := context.WithTimeout(t.Context(), timeout)
		if _, _, ok, err := p.startStartupWatch(ctx, "s", time.Second); ok || err != nil {
			cancel()
			t.Fatalf("%s: startStartupWatch = %v, %v; want unsupported", what, ok, err)
		}
		cancel()
		within(t, what+" watch-startup", deadlineMS(t, what+" watch-startup", readLog(t, filepath.Join(dir, "watch.env"))), timeout)
	}
}

// Kills: the value taken from the op's timeout argument alone. A parent
// deadline sooner than the op timeout is the op's real deadline.
func TestDeadlineEnvReflectsAnEarlierParentDeadline(t *testing.T) {
	script, _ := observePack(t, "", map[string]string{
		"get-meta": `echo "$GC_EXEC_DEADLINE_MS"`,
		"exec":     `cat >/dev/null; echo "$GC_EXEC_DEADLINE_MS"`,
	})
	const parent = 4 * time.Second
	for _, city := range []bool{false, true} {
		p := NewProvider(script)
		if city {
			p = NewProviderForCity(script, "alpha", "/cities/alpha")
		}
		p.timeout = time.Minute
		ctx, cancel := context.WithTimeout(t.Context(), parent)
		out, err := p.runWithContext(ctx, time.Minute, nil, "get-meta", "s", "k")
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		within(t, "runWithContext", deadlineMS(t, "runWithContext", out), parent)
		raw, _, err := p.Exec(ctx, "s", []string{"true"})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		within(t, "exec", deadlineMS(t, "exec", string(raw)), parent)
	}
}

// Kills: the parent's deadline reported when the op's own timeout is the
// sooner one.
func TestDeadlineEnvReflectsTheOpTimeoutWhenSooner(t *testing.T) {
	script, _ := observePack(t, "", map[string]string{"get-meta": `echo "$GC_EXEC_DEADLINE_MS"`})
	const op = 4 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	out, err := NewProvider(script).runWithContext(ctx, op, nil, "get-meta", "s", "k")
	if err != nil {
		t.Fatal(err)
	}
	within(t, "runWithContext", deadlineMS(t, "runWithContext", out), op)
}

// Pins: an op whose parent deadline has already passed is not spawned at
// all, so no pack ever reads a deadline that is already spent.
func TestDeadlineEnvExpiredParentSpawnsNothing(t *testing.T) {
	script, dir := observePack(t, "", map[string]string{"get-meta": `echo "$GC_EXEC_DEADLINE_MS"`})
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if out, err := NewProvider(script).runWithContext(ctx, time.Minute, nil, "get-meta", "s", "k"); err == nil {
		t.Fatalf("op under an expired parent = %q, nil; want an error", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ops")); !os.IsNotExist(err) {
		t.Fatalf("op under an expired parent ran: %v", opsRan(t, dir))
	}
}

// Kills: the deadline armed before the env is read. Waiting out an in-flight
// process-env projection is not the op's time, so the op still sees its
// whole budget once the projection restores — on a city provider and on one
// built for no city alike.
func TestDeadlineEnvExcludesTheProcessEnvProjectionWait(t *testing.T) {
	script := writeScript(t, t.TempDir(), `echo "$GC_EXEC_DEADLINE_MS"`)
	t.Run("city", func(t *testing.T) {
		assertDeadlineExcludesProjection(t, NewProviderForCity(script, "alpha", "/cities/alpha"))
	})
	t.Run("no city", func(t *testing.T) {
		assertDeadlineExcludesProjection(t, NewProvider(script))
	})
}

// assertDeadlineExcludesProjection holds a process-env projection across
// part of p's op budget and checks the op still sees most of that budget.
func assertDeadlineExcludesProjection(t *testing.T, p *Provider) {
	t.Helper()
	const budget, held = 6 * time.Second, 3 * time.Second
	execenv.ProcessEnvLock.Lock()
	projected := true
	restore := func() {
		if projected {
			projected = false
			execenv.ProcessEnvLock.Unlock()
		}
	}
	t.Cleanup(restore)

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := p.runWithTimeout(budget, nil, "get-meta", "s", "k")
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		restore()
		t.Fatalf("op returned (%q, %v) while a process-env projection was in flight", r.out, r.err)
	case <-time.After(held):
	}
	restore()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		// Armed before the read, the op would see budget-held at most.
		if got := deadlineMS(t, "get-meta", r.out); got <= budget-held/2 || got > budget {
			t.Errorf("GC_EXEC_DEADLINE_MS = %v after a %v projection; want in (%v, %v]", got, held, budget-held/2, budget)
		}
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatal("op did not return after the projection restored")
	}
}
