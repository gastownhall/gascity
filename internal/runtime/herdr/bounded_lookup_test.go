//go:build !windows

package herdr

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// lookupStub is a stub herdr whose per-verb behavior is chosen by marker
// files in dir: hang_<verb> stalls that verb for hangSecs, get_not_found
// answers `agent get` with a not-found envelope, get_fails exits `agent get`
// non-zero with a plain error. pipe_<verb> answers that verb as usual and
// exits 0, but leaves a descendant holding its stdout for hangSecs, so the
// call ends in exec.ErrWaitDelay. hang_get_<agent> and pipe_get_<agent> do
// the same for one agent's `agent get`. Every call is logged to calls.log.
func lookupStub(t *testing.T) (script, dir string) {
	t.Helper()
	dir = t.TempDir()
	script = `#!/bin/sh
D='` + dir + `'
shift 2
printf '%s\n' "$*" >> "$D/calls.log"
verb="$1_$2"
if [ -e "$D/hang_$verb" ]; then exec sleep ` + hangSecs + `; fi
if [ -e "$D/hang_get_$3" ] && [ "$verb" = agent_get ]; then exec sleep ` + hangSecs + `; fi
if [ -e "$D/pipe_$verb" ]; then sleep ` + hangSecs + ` & fi
if [ -e "$D/pipe_get_$3" ] && [ "$verb" = agent_get ]; then sleep ` + hangSecs + ` & fi
case "$verb" in
agent_get)
  if [ -e "$D/get_fails" ]; then echo boom >&2; exit 1; fi
  if [ -e "$D/get_running" ]; then
    printf '{"result":{"agent":{"name":"%s","pane_id":"%%5"}}}' "$3"
  else
    printf '{"error":{"code":"agent_not_found","message":"agent target not found"}}'
  fi ;;
agent_list)
  if [ -e "$D/agents.json" ]; then cat "$D/agents.json"; else printf '{"result":{"agents":[]}}'; fi ;;
agent_start) printf '{"result":{"agent":{"name":"%s","pane_id":"p1"}}}' "$3" ;;
pane_process-info) printf '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[]}}}' ;;
workspace_list) printf '{"result":{"workspaces":[]}}' ;;
workspace_create) printf '{"result":{"workspace":{"workspace_id":"w1"},"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"stray"}}}' ;;
tab_list) printf '{"result":{"tabs":[]}}' ;;
tab_create) printf '{"result":{"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"stray"}}}' ;;
*) printf '{"result":{}}' ;;
esac
`
	return writeFakeHerdr(t, script), dir
}

func touchFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// lookupProvider builds a Provider over the lookup stub with the injected bounds.
func lookupProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	bin, dir := lookupStub(t)
	p := New("gc-lookup-test", t.TempDir(), "/city/root", 0, 0)
	p.c.bin = bin
	p.c.cliTimeout, p.c.cliWaitDelay, p.c.cliMargin = testBound, testWaitDlay, testMargin
	return p, dir
}

// AT-H5: a timed-out existence check is an error, not "absent", and the error
// says the runtime could not be reached: it wraps runtime.ErrRuntimeUnavailable
// alongside the context error, so the launch path defers rather than failing.
// Each lookup tier (agent get, the agent list fallback, the bound pane's
// process-info) hung: Start fails with the deadline, and nothing past the
// check ran: no workspace/tab/pane verb, no agent start, no pre_start.
func TestStartExistenceCheckTimeoutIsNotAbsence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, p *Provider, dir string)
	}{
		{"hung agent get", func(t *testing.T, _ *Provider, dir string) { touchFile(t, dir, "hang_agent_get") }},
		{"hung agent list", func(t *testing.T, _ *Provider, dir string) { touchFile(t, dir, "hang_agent_list") }},
		{"hung pane process-info", func(t *testing.T, p *Provider, dir string) {
			touchFile(t, dir, "hang_pane_process-info")
			if err := p.SetMeta("gastown__worker", metaBoundPane, "%9"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, dir := lookupProvider(t)
			tc.setup(t, p, dir)
			marker := filepath.Join(t.TempDir(), "prestart-ran")
			cfg := runtime.Config{Command: "claude", PreStart: []string{fixtureTouch(marker)}}

			err := p.start(context.Background(), "gastown__worker", cfg)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("start = %v; want it to wrap context.DeadlineExceeded", err)
			}
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("start = %v; want it to wrap runtime.ErrRuntimeUnavailable so the launch path defers", err)
			}
			if errors.Is(err, runtime.ErrSessionExists) || runtime.IsSessionGone(err) {
				t.Fatalf("start = %v; a timeout must read as neither exists nor gone", err)
			}
			assertStartStoppedAtExistenceCheck(t, dir, marker)
		})
	}
}

// AT-H5b: a lookup whose answer was lost, because herdr exited 0 while a
// descendant still held its stdout and the WaitDelay abandoned the pipes
// (exec.ErrWaitDelay), is unanswered like a timeout. At either lookup tier,
// the agent's registry entry or the bound pane's process-info, Start defers
// with runtime.ErrRuntimeUnavailable, and nothing past the check runs: no
// placement, no agent start, no pre_start.
func TestStartExistenceCheckLostAnswerIsNotAbsence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, p *Provider, dir string)
	}{
		{"agent get", func(t *testing.T, _ *Provider, dir string) {
			touchFile(t, dir, "get_running")
			touchFile(t, dir, "pipe_agent_get")
		}},
		{"bound pane process-info", func(t *testing.T, p *Provider, dir string) {
			touchFile(t, dir, "pipe_pane_process-info")
			if err := p.SetMeta("gastown__worker", metaBoundPane, "%9"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, dir := lookupProvider(t)
			p.c.cliTimeout = time.Minute // only the WaitDelay can end a lookup
			tc.setup(t, p, dir)
			marker := filepath.Join(t.TempDir(), "prestart-ran")
			cfg := runtime.Config{Command: "claude", PreStart: []string{fixtureTouch(marker)}}

			err := p.start(context.Background(), "gastown__worker", cfg)
			if !errors.Is(err, exec.ErrWaitDelay) {
				t.Fatalf("start = %v; want it to wrap exec.ErrWaitDelay", err)
			}
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("start = %v; want it to wrap runtime.ErrRuntimeUnavailable so the launch path defers", err)
			}
			assertStartStoppedAtExistenceCheck(t, dir, marker)
		})
	}
}

// assertStartStoppedAtExistenceCheck fails t if start ran anything past its
// existence check: a workspace, tab or agent start verb, a pane verb other
// than the process-info probe, or pre_start (which creates marker).
func assertStartStoppedAtExistenceCheck(t *testing.T, dir, marker string) {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(line, "workspace") || strings.HasPrefix(line, "tab") ||
			strings.HasPrefix(line, "agent start") ||
			(strings.HasPrefix(line, "pane") && !strings.HasPrefix(line, "pane process-info")) {
			t.Fatalf("start ran %q after an unanswered existence check:\n%s", line, b)
		}
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("pre_start ran after an unanswered existence check")
	}
}

// AT-H6 (characterization): a lookup error that is not a timeout still reads
// as "not running", so start proceeds exactly as before.
func TestStartExistenceCheckOtherErrorProceeds(t *testing.T) {
	p, dir := lookupProvider(t)
	touchFile(t, dir, "get_fails")
	marker := filepath.Join(t.TempDir(), "prestart-ran")
	cfg := runtime.Config{Command: "claude", PreStart: []string{fixtureTouch(marker)}}

	if err := p.start(context.Background(), "gastown__worker", cfg); err != nil {
		t.Fatalf("start = %v; a non-timeout lookup error must still proceed", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
	if !strings.Contains(string(b), "agent start") {
		t.Fatalf("start never launched the agent:\n%s", b)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("pre_start did not run: %v", err)
	}
}

// AT-H6b: a live session still reports ErrSessionExists.
func TestStartExistenceCheckRunningStillExists(t *testing.T) {
	p, dir := lookupProvider(t)
	touchFile(t, dir, "get_running")
	err := p.start(context.Background(), "gastown__worker", runtime.Config{Command: "claude"})
	if !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("start = %v; want ErrSessionExists", err)
	}
}

// takenStub is a stub herdr for Start's agent_name_taken recovery, bounded at
// bound per call. The first `agent start` is rejected with agent_name_taken
// and a retried one lands on p1; `agent get` names the holder on pane %7, and
// the holder's `pane process-info` answers busy (mode "ok"), stalls for
// hangSecs ("hang"), or answers busy and exits 0 while a descendant keeps its
// stdout open for hangSecs ("pipe", so the call ends in exec.ErrWaitDelay).
// Every call is logged to calls.log.
func takenStub(t *testing.T, mode string, bound time.Duration) (*Provider, string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
D='` + dir + `'
shift 2
printf '%s\n' "$*" >> "$D/calls.log"
case "$1_$2" in
agent_start)
  if [ -e "$D/rejected" ]; then printf '{"result":{"agent":{"name":"%s","pane_id":"p1"}}}' "$3"; exit 0; fi
  : > "$D/rejected"
  printf '{"error":{"code":"agent_name_taken","message":"agent name already taken"}}' ;;
agent_get) printf '{"result":{"agent":{"name":"%s","pane_id":"%%7"}}}' "$3" ;;
pane_process-info)
  if [ '` + mode + `' = hang ]; then exec sleep ` + hangSecs + `; fi
  if [ '` + mode + `' = pipe ]; then sleep ` + hangSecs + ` & fi
  printf '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[{"pid":4243,"name":"claude"}]}}}' ;;
*) printf '{"result":{}}' ;;
esac
`
	p := New("gc-taken-test", t.TempDir(), "/city/root", 0, 0)
	p.c.bin = writeFakeHerdr(t, script)
	p.c.cliTimeout, p.c.cliWaitDelay, p.c.cliMargin = bound, testWaitDlay, testMargin
	return p, dir
}

// AT-H5c: Start's agent_name_taken recovery reads its holder probe as the
// lookups above are read. A probe that answers busy adopts the holder. One
// that the per-call bound cuts off, or whose answer a descendant holding
// herdr's stdout loses, ends while Start's ctx is still live and proves
// nothing: the holder's pane is not closed, no second agent is started, and
// the error wraps runtime.ErrRuntimeUnavailable so the launch path defers.
func TestAgentNameTakenUnansweredHolderProbeReapsNothing(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		bound time.Duration // per call; hang's must cut the probe well before hangSecs
		cause error         // what ended the probe; nil when it answered
	}{
		{mode: "ok", bound: time.Minute},
		{mode: "hang", bound: time.Second, cause: context.DeadlineExceeded},
		{mode: "pipe", bound: time.Minute, cause: exec.ErrWaitDelay},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			p, dir := takenStub(t, tc.mode, tc.bound)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			info, adopted, err := p.startAgentAdopting(ctx, "gastown__worker", "claude", "p1", nil)
			b, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
			if strings.Contains(string(b), "pane close") {
				t.Fatalf("the holder's pane was closed:\n%s", b)
			}
			if n := strings.Count(string(b), "agent start"); n != 1 {
				t.Fatalf("agent start ran %d times; want only the rejected one:\n%s", n, b)
			}
			if ctx.Err() != nil {
				t.Fatalf("Start's ctx ended (%v); the probe must end while it is live", ctx.Err())
			}
			if tc.cause == nil {
				if err != nil || !adopted || info.PaneID != "%7" {
					t.Fatalf("got info=%+v adopted=%v err=%v; want the busy holder on %%7 adopted", info, adopted, err)
				}
				return
			}
			if adopted {
				t.Fatalf("adopted the holder on an unanswered probe (info=%+v)", info)
			}
			if !errors.Is(err, tc.cause) {
				t.Fatalf("err = %v; want it to wrap %v", err, tc.cause)
			}
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("err = %v; want it to wrap runtime.ErrRuntimeUnavailable so the launch path defers", err)
			}
		})
	}
}

// walkProvider has three bound sessions s1..s3, all running, and one registry-only
// agent. markers are extra lookupStub marker files, such as hang_get_<agent>.
func walkProvider(t *testing.T, markers ...string) *Provider {
	t.Helper()
	p, dir := lookupProvider(t)
	touchFile(t, dir, "get_running")
	for _, m := range markers {
		touchFile(t, dir, m)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents.json"), []byte(`{"result":{"agents":[{"name":"foreign","pane_id":"%9"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"s1", "s2", "s3"} {
		for key, val := range map[string]string{metaBoundName: n, metaBoundPane: "%5"} {
			if err := p.SetMeta(n, key, val); err != nil {
				t.Fatal(err)
			}
		}
	}
	p.c.cliTimeout = time.Minute // only the walk's own deadline can cut a lookup
	p.listDeadline = 400 * time.Millisecond
	return p
}

// AT-H7: a stall from the third lookup stops the walk, keeps what was found
// (including the registry-only agent), and says so with an unwrapped
// PartialListError that is not "server absent".
func TestListRunningDeadlineReturnsPartial(t *testing.T) {
	p := walkProvider(t, "hang_get_"+herdrAgentName("s3"))
	start := time.Now()
	got, err := p.ListRunning("")
	if elapsed := time.Since(start); elapsed > time.Second+boundSlack {
		t.Fatalf("ListRunning took %v; want it to end at its deadline", elapsed)
	}
	if want := "s1,s2,foreign"; strings.Join(got, ",") != want {
		t.Fatalf("names = %v; want %s", got, want)
	}
	pe, ok := err.(*runtime.PartialListError) //nolint:errorlint // the assertion is the point: the error must be returned unwrapped
	if !ok {
		t.Fatalf("err = %T %v; want an unwrapped *runtime.PartialListError", err, err)
	}
	if pe.ServerAbsent || runtime.IsRuntimeServerAbsent(err) {
		t.Fatal("a stalled walk must not read as server absent")
	}
	if !errors.Is(pe.Err, context.DeadlineExceeded) {
		t.Fatalf("PartialListError.Err = %v; want it to wrap context.DeadlineExceeded", pe.Err)
	}
}

// AT-H7b: a healthy walk lists everything and returns a nil error.
func TestListRunningHealthyWalkHasNoError(t *testing.T) {
	p := walkProvider(t)
	got, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning = %v; want nil", err)
	}
	if want := "s1,s2,s3,foreign"; strings.Join(got, ",") != want {
		t.Fatalf("names = %v; want %s", got, want)
	}
}

// AT-H7c: a bound session whose `agent get` answer was lost to a descendant
// holding herdr's stdout (exec.ErrWaitDelay) is unknown, not absent, while the
// walk's own deadline is still far off. The walk goes on past it, lists every
// session that answers (and the registry-only agent), and reports the list
// partial with an unwrapped PartialListError that is not "server absent".
func TestListRunningLostAnswerReturnsPartial(t *testing.T) {
	p := walkProvider(t, "pipe_get_"+herdrAgentName("s2"))
	p.listDeadline = time.Minute // only the WaitDelay can end s2's lookup
	got, err := p.ListRunning("")
	if want := "s1,s3,foreign"; strings.Join(got, ",") != want {
		t.Fatalf("names = %v; want %s", got, want)
	}
	pe, ok := err.(*runtime.PartialListError) //nolint:errorlint // the assertion is the point: the error must be returned unwrapped
	if !ok {
		t.Fatalf("err = %T %v; want an unwrapped *runtime.PartialListError", err, err)
	}
	if pe.ServerAbsent || runtime.IsRuntimeServerAbsent(err) {
		t.Fatal("a lost answer must not read as server absent")
	}
	if !errors.Is(pe.Err, exec.ErrWaitDelay) {
		t.Fatalf("PartialListError.Err = %v; want it to wrap exec.ErrWaitDelay", pe.Err)
	}
	if errors.Is(pe.Err, context.DeadlineExceeded) {
		t.Fatalf("PartialListError.Err = %v; the walk's deadline never ended", pe.Err)
	}
}
