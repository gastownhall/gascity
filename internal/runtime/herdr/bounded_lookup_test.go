//go:build !windows

package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// lookupStub is a stub herdr whose per-verb behavior is chosen by marker
// files in dir: hang_<verb> stalls that verb for hangSecs, get_not_found
// answers `agent get` with a not-found envelope, get_fails exits `agent get`
// non-zero with a plain error. Every call is logged to calls.log.
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

// AT-H5: a timed-out existence check is an error, not "absent". Each lookup tier
// (agent get, the agent list fallback, the bound pane's process-info) hung:
// Start fails with the deadline, and nothing past the check ran: no
// workspace/tab/pane verb, no agent start, no pre_start.
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
			if errors.Is(err, runtime.ErrSessionExists) || runtime.IsSessionGone(err) {
				t.Fatalf("start = %v; a timeout must read as neither exists nor gone", err)
			}
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
		})
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

// walkProvider has three bound sessions s1..s3, all running, and one registry-only
// agent. hang is the session whose `agent get` stalls ("" = none).
func walkProvider(t *testing.T, hang string) *Provider {
	t.Helper()
	p, dir := lookupProvider(t)
	touchFile(t, dir, "get_running")
	if hang != "" {
		touchFile(t, dir, "hang_get_"+herdrAgentName(hang))
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
	p := walkProvider(t, "s3")
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
	p := walkProvider(t, "")
	got, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning = %v; want nil", err)
	}
	if want := "s1,s2,s3,foreign"; strings.Join(got, ",") != want {
		t.Fatalf("names = %v; want %s", got, want)
	}
}
