package main

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// chunkWriter records every Write as its own chunk, which is how "one hook's
// output is emitted as one block" is asserted without depending on timing: a
// line-at-a-time emitter fails it on every run, not just an unlucky one.
type chunkWriter struct {
	mu     sync.Mutex
	chunks []string
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.chunks = append(w.chunks, string(p))
	return len(p), nil
}

func (w *chunkWriter) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, len(w.chunks))
	copy(out, w.chunks)
	return out
}

func onBootHooks(names ...string) []poolOnBootHook {
	hooks := make([]poolOnBootHook, 0, len(names))
	for _, name := range names {
		hooks = append(hooks, poolOnBootHook{agent: name, command: "bd update --unclaim", dir: "/rig/" + name})
	}
	return hooks
}

// TestPlanPoolOnBootHooksSelectsExactlyThePoolAgents pins the eligibility
// filter as a separate, inspectable step: planning decides WHICH agents get a
// hook, so parallelizing execution cannot change that set.
//
// A pool agent with no configured on_boot still gets one — the default hook is
// the per-agent bd recovery probe, and it is most valuable exactly after a
// restart. A non-pool agent gets none.
func TestPlanPoolOnBootHooksSelectsExactlyThePoolAgents(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)}, // not a pool
			{Name: "quiet", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
			{Name: "dog", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), OnBoot: "bd update --unclaim"},
		},
	}

	hooks := planPoolOnBootHooks(cfg, t.TempDir(), io.Discard)
	// Planned in config order, so the pool submits work in a stable order.
	var planned []string
	for _, hook := range hooks {
		planned = append(planned, hook.agent)
	}
	if !equalNames(planned, "quiet", "dog") {
		t.Fatalf("planned hooks for %v, want the two pool agents in config order and no hook for the non-pool agent", planned)
	}
	if !strings.Contains(hooks[1].command, "--unclaim") {
		t.Fatalf("hook %q command = %q, want the configured on_boot", hooks[1].agent, hooks[1].command)
	}
	if strings.TrimSpace(hooks[0].command) == "" {
		t.Fatalf("hook %q has no command; a pool agent without a configured on_boot must still get the default recovery hook", hooks[0].agent)
	}
}

// TestRunPoolOnBootHooksRespectsConcurrencyBound proves the hooks actually run
// in parallel and actually stop at the bound.
//
// The default on_boot hook is a per-agent bd recovery probe with its own 30s
// timeout, so a city with a dozen pool agents paid a dozen sequential probe
// budgets at boot -- 3m12s on maintainer-city (ga-1e78j). Running them
// unbounded would just move that cost onto the store; the bound is what keeps
// the probes light.
//
// The runner blocks and nothing is released until the bound is saturated, so
// concurrency is proven by causality rather than by sampling a sleep: a serial
// implementation cannot produce a second arrival at all, and every entry into
// the runner checks the bound rather than trusting one snapshot of it.
func TestRunPoolOnBootHooksRespectsConcurrencyBound(t *testing.T) {
	total := poolOnBootConcurrency + 2
	names := make([]string, 0, total)
	for i := range total {
		names = append(names, fmt.Sprintf("agent-%02d", i))
	}
	hooks := onBootHooks(names...)

	arrived := make(chan struct{}, total)
	release := make(chan struct{})
	var mu sync.Mutex
	inFlight, peak, over := 0, 0, 0

	var runner ScaleCheckRunner = func(string, string, map[string]string) (string, error) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		// Checked on every entry rather than sampled, so an over-wide pool is
		// caught by the hook that exceeds the bound rather than by whichever
		// instant the test happened to look.
		if inFlight > poolOnBootConcurrency {
			over++
		}
		mu.Unlock()

		arrived <- struct{}{}
		<-release

		mu.Lock()
		inFlight--
		mu.Unlock()
		return "", nil
	}

	done := make(chan struct{})
	go func() {
		runPoolOnBootHooks(hooks, runner, io.Discard)
		close(done)
	}()

	// Nothing is released yet, so a serial implementation can never deliver a
	// second arrival. The deadline is a failure path only: a correct pool
	// saturates immediately.
	for i := range poolOnBootConcurrency {
		select {
		case <-arrived:
		case <-time.After(30 * time.Second):
			close(release)
			t.Fatalf("only %d hook(s) started with nothing released; the hooks are not running concurrently", i)
		}
	}

	close(release)
	<-done

	mu.Lock()
	finalPeak, violations, remaining := peak, over, inFlight
	mu.Unlock()
	if violations > 0 {
		t.Fatalf("%d hook(s) started while the bound of %d was already saturated (peak %d)", violations, poolOnBootConcurrency, finalPeak)
	}
	if finalPeak != poolOnBootConcurrency {
		t.Fatalf("peak concurrency = %d, want exactly the bound of %d", finalPeak, poolOnBootConcurrency)
	}
	if remaining != 0 {
		t.Fatalf("runPoolOnBootHooks returned with %d hook(s) still running; it must not return until every hook is done", remaining)
	}
}

// TestRunPoolOnBootHooksEmitsEachHooksOutputAsOneBlock pins the readability
// contract that concurrency threatens. A hook can produce two lines -- a failure
// and a gc-recovery diagnostic -- and serially they were adjacent by
// construction. Written line by line from concurrent workers they would
// interleave, leaving an operator to reassemble which line belonged to which
// agent, so each hook's lines are buffered and emitted in a single write.
func TestRunPoolOnBootHooksEmitsEachHooksOutputAsOneBlock(t *testing.T) {
	hooks := onBootHooks("dog", "cat")
	runner := func(_, dir string, _ map[string]string) (string, error) {
		return config.RecoveryHookMarker + ": could not release for " + dir + "\n", fmt.Errorf("hook failed")
	}

	var stderr chunkWriter
	runPoolOnBootHooks(hooks, runner, &stderr)

	chunks := stderr.all()
	if len(chunks) != 2 {
		t.Fatalf("stderr received %d writes for 2 hooks, want one block each: %q", len(chunks), chunks)
	}
	for _, chunk := range chunks {
		lines := strings.Split(strings.TrimSuffix(chunk, "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("hook block = %q, want its failure line and its recovery line together", chunk)
		}
		agent := ""
		for _, name := range []string{"dog", "cat"} {
			if strings.Contains(lines[0], "on_boot "+name+":") {
				agent = name
			}
		}
		if agent == "" {
			t.Fatalf("hook block %q names no agent", chunk)
		}
		if !strings.Contains(lines[1], "on_boot "+agent+": "+config.RecoveryHookMarker) {
			t.Fatalf("hook block %q mixes lines from more than one agent", chunk)
		}
	}
}

// TestRunPoolOnBootHooksRunsEveryHookWhenOneFails keeps the best-effort
// semantics the serial loop had: a hook that fails is logged and the rest still
// run, because on_boot is per-agent recovery and one agent's broken probe must
// not cost every other agent theirs.
func TestRunPoolOnBootHooksRunsEveryHookWhenOneFails(t *testing.T) {
	hooks := onBootHooks("dog", "boom", "cat")
	var mu sync.Mutex
	ran := map[string]bool{}

	runner := func(_, dir string, _ map[string]string) (string, error) {
		mu.Lock()
		ran[dir] = true
		mu.Unlock()
		if strings.HasSuffix(dir, "boom") {
			return "", fmt.Errorf("bd not found")
		}
		return "", nil
	}

	var stderr chunkWriter
	runPoolOnBootHooks(hooks, runner, &stderr)

	for _, hook := range hooks {
		if !ran[hook.dir] {
			t.Fatalf("hook %q never ran after a sibling failed; ran = %v", hook.agent, ran)
		}
	}
	if got := strings.Join(stderr.all(), ""); !strings.Contains(got, "on_boot boom: bd not found") {
		t.Fatalf("stderr = %q, want the failing hook logged", got)
	}
}

// TestRunPoolOnBootHooksSurvivesAPanickingHook is the containment contract the
// move onto goroutines would otherwise have silently removed.
//
// Serially, a hook that panicked unwound into startCityWorkers' per-city
// recover: that city's boot failed and the rest of the fleet came up. From a
// hook goroutine there is nothing above it to recover, so the same panic would
// take the whole supervisor down -- every city, not one. on_boot is best-effort
// recovery work, so a panicking hook is logged and skipped exactly like a
// failing one.
func TestRunPoolOnBootHooksSurvivesAPanickingHook(t *testing.T) {
	hooks := onBootHooks("dog", "boom", "cat")
	var mu sync.Mutex
	ran := map[string]bool{}

	runner := func(_, dir string, _ map[string]string) (string, error) {
		mu.Lock()
		ran[dir] = true
		mu.Unlock()
		if strings.HasSuffix(dir, "boom") {
			panic("hook blew up")
		}
		return "", nil
	}

	var stderr chunkWriter
	runPoolOnBootHooks(hooks, runner, &stderr)

	for _, hook := range hooks {
		if !ran[hook.dir] {
			t.Fatalf("hook %q never ran after a sibling panicked; ran = %v", hook.agent, ran)
		}
	}
	log := strings.Join(stderr.all(), "")
	if !strings.Contains(log, "on_boot boom: hook panicked: hook blew up") {
		t.Fatalf("stderr = %q, want the panicking hook named: a recovered-and-silent hook is an agent whose recovery never ran", log)
	}
}

// TestRunPoolOnBootHooksReleasesItsSlotWhenAHookPanics proves the recovery does
// not leak the bound. A panic that escaped before the semaphore was released
// would shrink the pool by one slot per panicking hook and, at enough panics,
// deadlock the boot outright.
func TestRunPoolOnBootHooksReleasesItsSlotWhenAHookPanics(t *testing.T) {
	total := poolOnBootConcurrency * 2
	names := make([]string, 0, total)
	for i := range total {
		names = append(names, fmt.Sprintf("agent-%02d", i))
	}

	var mu sync.Mutex
	ran := 0
	runner := func(string, string, map[string]string) (string, error) {
		mu.Lock()
		ran++
		mu.Unlock()
		panic("every hook blows up")
	}

	done := make(chan struct{})
	go func() {
		runPoolOnBootHooks(onBootHooks(names...), runner, &chunkWriter{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runPoolOnBootHooks never returned; a panicking hook leaked its semaphore slot and starved the pool")
	}

	if ran != total {
		t.Fatalf("%d of %d hooks ran; every hook must be attempted even when all of them panic", ran, total)
	}
}
