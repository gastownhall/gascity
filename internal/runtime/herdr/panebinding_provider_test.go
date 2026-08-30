package herdr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ── provider-level pane-binding behavior against a fake herdr 0.7.5 ──────────
//
// The fake herdr is a shell script modeling the ≥0.7.5 contract: `agent start`
// launches a supported kind into an existing shell pane and registers the
// name; the name exists only while the agent runs (state file "registered");
// raw commands are typed into the pane and never register anything. State
// files drive the scenario (registered / pane_gone / busy / name_taken /
// agent_blocked, plus the startup-delivery verdicts prompt_stalled /
// prompt_times_out / prompt_blocked and wait_times_out), and calls.log records every verb so
// tests can assert what was — and crucially was NOT — issued (the spawn storm
// was one placement per reconcile tick).

var paneBindSession int64

// newFakeHerdrProvider builds a Provider whose client shells out to a fake
// herdr script. Returns the provider and the state dir.
func newFakeHerdrProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	session := fmt.Sprintf("gctest-pb-%d-%d", os.Getpid(), atomic.AddInt64(&paneBindSession, 1))
	state := t.TempDir()
	metaDir := t.TempDir()
	script := filepath.Join(t.TempDir(), "herdr")
	fake := `#!/bin/sh
STATE='` + state + `'
METADIR='` + metaDir + `'
shift 2
printf '%s\n' "$*" >> "$STATE/calls.log"
case "$1_$2" in
agent_get)
  if [ ! -e "$STATE/registered" ]; then
    printf '%s' '{"error":{"code":"agent_not_found","message":"agent target not found"}}'
  elif [ -e "$STATE/agent_blocked" ]; then
    printf '%s' '{"result":{"agent":{"name":"'"$3"'","pane_id":"%5","tab_id":"t1","workspace_id":"w1","agent_status":"blocked"}}}'
  else
    printf '%s' '{"result":{"agent":{"name":"'"$3"'","pane_id":"%5","tab_id":"t1","workspace_id":"w1","agent_status":"idle"}}}'
  fi ;;
agent_list)
  printf '%s' '{"result":{"agents":[]}}' ;;
agent_start)
  if [ -e "$STATE/name_taken" ]; then
    # A concurrent Start won the name and its agent is live: herdr rejects the
    # launch, agent get then resolves the live holder and its pane probes
    # busy, so resolveAgentNameTaken adopts instead of reaping.
    : > "$STATE/registered"
    : > "$STATE/busy"
    printf '%s' '{"error":{"code":"agent_name_taken","message":"agent name already registered"}}'
  else
    : > "$STATE/agent_started"
    : > "$STATE/registered"
    if [ -e "$METADIR/$3/GC_SESSION_ID" ]; then : > "$STATE/meta_seeded_before_launch"; fi
    if [ -e "$METADIR/$3/GC_HERDR_PANE_ID" ]; then : > "$STATE/bound_before_launch"; fi
    printf '%s' '{"result":{"agent":{"name":"'"$3"'","pane_id":"%5","tab_id":"t1","workspace_id":"w1","agent_status":"idle"}}}'
  fi ;;
agent_wait)
  if [ ! -e "$STATE/registered" ]; then
    printf '%s' '{"error":{"code":"agent_not_found","message":"agent target not found"}}'
  elif [ -e "$STATE/wait_times_out" ]; then
    printf '%s' '{"error":{"code":"timeout","message":"timed out waiting for agent status"}}'
  else
    printf '%s' '{"result":{"agent":{"name":"'"$3"'","agent_status":"idle"}}}'
  fi ;;
agent_prompt)
  if [ ! -e "$STATE/registered" ]; then
    printf '%s' '{"error":{"code":"agent_not_found","message":"agent target not found"}}'
  elif [ -e "$STATE/prompt_stalled" ]; then
    # No state change inside herdr's fixed 5000ms window: the submit CR never
    # reached the TUI and the text sits unsubmitted in an idle input box.
    printf '%s' '{"error":{"code":"agent_prompt_stalled","message":"no state change observed after submission"}}'
  elif [ -e "$STATE/prompt_times_out" ]; then
    # The state-change gate passed (the CR landed) but no --until state was
    # observed before --timeout. herdr only reports this when --timeout
    # exceeds its 5000ms window; a shorter one masks the stall as a timeout.
    printf '%s' '{"error":{"code":"timeout","message":"timed out waiting for agent status"}}'
  elif [ -e "$STATE/prompt_blocked" ]; then
    # The first turn opened a confirmation dialog. herdr matches only the
    # requested --until states, so this settles iff the caller asked for
    # blocked; otherwise the caller waits out its bound.
    case "$*" in
      *"--until blocked"*)
        : > "$STATE/prompted"
        printf '%s' '{"result":{"agent":{"name":"'"$3"'","agent_status":"blocked"}}}' ;;
      *)
        printf '%s' '{"error":{"code":"timeout","message":"timed out waiting for agent status"}}' ;;
    esac
  else
    : > "$STATE/prompted"
    printf '%s' '{"result":{"type":"agent_prompted"}}'
  fi ;;
pane_run)
  : > "$STATE/busy"
  printf '%s' "$4" | sed -e 's|^exec /bin/sh -c ||' -e "s/^'//" -e "s/'\$//" > "$STATE/rawcmd"
  exit 0 ;;
pane_process-info)
  if [ -e "$STATE/pane_gone" ]; then
    printf '%s' '{"error":{"code":"pane_not_found","message":"pane not found"}}'
  elif [ -e "$STATE/rawcmd" ]; then
    printf '%s' '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[{"pid":4242,"name":"bash","argv":["/bin/sh","-c","'"$(cat "$STATE/rawcmd")"'"]}]}}}'
  elif [ -e "$STATE/busy" ]; then
    printf '%s' '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[{"pid":4243,"name":"claude"}]}}}'
  else
    printf '%s' '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[{"pid":4242,"name":"zsh"}]}}}'
  fi ;;
workspace_list)
  : > "$STATE/placement_attempted"
  printf '%s' '{"result":{"workspaces":[]}}' ;;
workspace_create)
  printf '%s' '{"result":{"workspace":{"workspace_id":"w1"},"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"%5"}}}' ;;
tab_list)
  if [ -e "$STATE/stale_tabs" ]; then
    printf '%s' '{"result":{"tabs":[{"tab_id":"t-old1","label":"witness"},{"tab_id":"t-old2","label":"witness"},{"tab_id":"t-other","label":"deacon"}]}}'
  else
    printf '%s' '{"result":{"tabs":[]}}'
  fi ;;
tab_create)
  printf '%s' '{"result":{"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"%5"}}}' ;;
*)
  exit 0 ;;
esac
`
	if err := os.WriteFile(script, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	p := New(session, metaDir, t.TempDir(), time.Second, time.Second)
	p.c.bin = script
	// The paste fallback's settle delay guards a real paste racing a real
	// submit; against a shell script there is no race, and the production 1s
	// would be per-test wall-clock the resource census cannot see (it counts
	// only time.Sleep written directly in a _test.go).
	p.c.settleDelay = time.Millisecond
	return p, state
}

// fakeCalls returns the verbs the fake herdr recorded.
func fakeCalls(t *testing.T, state string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, "calls.log"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

func setState(t *testing.T, state, flag string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(state, flag), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// listenHerdrSocket plants a live unix listener at the session's socket path so
// ConfigureServer's serverAlive dial succeeds without launching a real server.
// Must resolve the path through p.c.socketPath() itself, not a hand-rolled
// reconstruction — a second copy of that logic silently drifts from the real
// one (ga-nqlb8q: it did, once socketPath() started honoring XDG_CONFIG_HOME).
func listenHerdrSocket(t *testing.T, p *Provider) {
	t.Helper()
	sock := p.c.socketPath()
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = l.Close()
		_ = os.RemoveAll(filepath.Dir(sock))
	})
}

// bindTestPane seeds the sidecar with the binding Start would have persisted
// (the fake herdr always reports pane "%5").
func bindTestPane(t *testing.T, p *Provider, name, mode string) {
	t.Helper()
	if err := p.SetMeta(name, metaBoundPane, "%5"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta(name, metaBoundMode, mode); err != nil {
		t.Fatal(err)
	}
}

// The storm-killer: with no registry name but the bound pane busy running the
// agent, IsRunning must stay true so the reconciler never re-issues Start.
func TestIsRunningSurvivesNameClearViaPaneBinding(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	setState(t, state, "busy")
	bindTestPane(t, p, "gastown__witness", bindModeAgent)
	if !p.IsRunning("gastown__witness") {
		t.Fatal("IsRunning = false for a live agent whose name herdr cleared; this is the spawn-storm trigger")
	}
}

// Without a binding, an unregistered name is a genuinely absent session.
func TestIsRunningFalseWhenNameClearedAndNoBinding(t *testing.T) {
	p, _ := newFakeHerdrProvider(t)
	if p.IsRunning("gastown__witness") {
		t.Fatal("IsRunning = true with no live name and no pane binding")
	}
}

// An exited agent — pane back at its bare shell prompt — is NOT running, so
// the reconciler can restart it; a bare-shell session in the same pane state
// IS running (the shell is the session).
func TestIsRunningModeAwareAtShellPrompt(t *testing.T) {
	p, _ := newFakeHerdrProvider(t)
	bindTestPane(t, p, "gastown__witness", bindModeAgent)
	if p.IsRunning("gastown__witness") {
		t.Fatal("IsRunning = true for an exited agent (pane at shell prompt); restarts would never happen")
	}
	bindTestPane(t, p, "gastown__shellsess", bindModeShell)
	if !p.IsRunning("gastown__shellsess") {
		t.Fatal("IsRunning = false for a bare-shell session whose pane exists")
	}
}

// Start on a live-but-unregistered session must return ErrSessionExists
// WITHOUT touching placement: each wrongful placement leaked a pane, which is
// the unbounded shell storm.
func TestStartReturnsSessionExistsWithoutPlacementWhenNameCleared(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "busy")
	bindTestPane(t, p, "gastown__witness", bindModeAgent)

	err := p.Start(context.Background(), "gastown__witness", runtime.Config{})
	if !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("Start = %v; want ErrSessionExists", err)
	}
	calls := fakeCalls(t, state)
	if strings.Contains(calls, "workspace") || strings.Contains(calls, "agent start") {
		t.Fatalf("Start touched placement/spawn for a live session (the storm):\n%s", calls)
	}
}

// A clean claude command takes the ≥0.7.5 kind-launch path: placement creates
// the shell pane (with cwd baked in), `agent start --kind claude --pane`
// launches into it, and the binding + agent mode are persisted.
func TestStartKindPathRegistersAndPersistsBinding(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)

	cfg := runtime.Config{
		Command: "claude --dangerously-skip-permissions",
		Env:     map[string]string{"GC_SESSION_ID": "sess-1", "GC_INSTANCE_TOKEN": "tok-1"},
	}
	if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "agent start gastown__witness --kind claude --pane %5") {
		t.Fatalf("Start did not kind-launch into the placed pane:\n%s", calls)
	}
	// The kind launch blocks for seconds (readiness wait + TUI detection), so
	// the identity sidecar AND a provisional pane binding must exist BEFORE
	// the launch: reconcile ticks that fire mid-boot read them, and an
	// unseeded sidecar makes the ownership check roll the fresh runtime back
	// ("live runtime belongs to another session").
	if _, err := os.Stat(filepath.Join(state, "meta_seeded_before_launch")); err != nil {
		t.Error("GC_SESSION_ID was not in the sidecar before the agent launch")
	}
	if _, err := os.Stat(filepath.Join(state, "bound_before_launch")); err != nil {
		t.Error("pane binding was not persisted before the agent launch")
	}
	if got, _ := p.GetMeta("gastown__witness", metaBoundPane); got != "%5" {
		t.Fatalf("bound pane after Start = %q; want %%5", got)
	}
	if got, _ := p.GetMeta("gastown__witness", metaBoundMode); got != bindModeAgent {
		t.Fatalf("bound mode after Start = %q; want %q", got, bindModeAgent)
	}
}

// A non-kind command is exec'd through the pane shell (raw path): no herdr
// agent registration, shell mode persisted, pane still the session handle.
func TestStartRawPathExecsThroughPaneShell(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)

	cfg := runtime.Config{Command: "python3 worker.py --queue main"}
	if err := p.Start(context.Background(), "gastown__worker", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "pane run %5 exec /bin/sh -c ") {
		t.Fatalf("Start did not exec the raw command through the pane shell:\n%s", calls)
	}
	if strings.Contains(calls, "agent start") {
		t.Fatalf("raw command must not attempt a kind launch:\n%s", calls)
	}
	if got, _ := p.GetMeta("gastown__worker", metaBoundMode); got != bindModeShell {
		t.Fatalf("bound mode after raw Start = %q; want %q", got, bindModeShell)
	}
}

// gc session names carrying uppercase rig names must launch under their
// mapped herdr agent name (herdr ≥0.7.5 rejects them verbatim with
// invalid_agent_name — a hot retry loop found live), while the sidecar keeps
// the exact gc name for enumeration.
func TestStartMapsSessionNameToValidHerdrName(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)

	if err := p.Start(context.Background(), "Indigo--anthony", runtime.Config{Command: "claude"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "agent start indigo--anthony --kind claude") {
		t.Fatalf("Start did not use the mapped herdr agent name:\n%s", calls)
	}
	if strings.Contains(calls, "agent start Indigo--anthony") {
		t.Fatalf("Start used the raw gc name herdr rejects:\n%s", calls)
	}
	if got, _ := p.GetMeta("Indigo--anthony", metaBoundName); got != "Indigo--anthony" {
		t.Fatalf("sidecar name = %q; want the exact gc name", got)
	}
	// Liveness and enumeration still key on the gc name.
	if !p.IsRunning("Indigo--anthony") {
		t.Fatal("IsRunning(gc name) = false for the running mapped agent")
	}
	if names, err := p.ListRunning("Indigo"); err != nil || len(names) != 1 || names[0] != "Indigo--anthony" {
		t.Fatalf("ListRunning = %v, %v; want [Indigo--anthony]", names, err)
	}
}

// Placement must recycle EVERY stale tab carrying the session's label, not
// just the first: reconciler churn can leave several behind, and a survivor
// lingers forever (its shell pane with it).
func TestStartRecyclesAllStaleTabs(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "stale_tabs")
	// An existing workspace forces the findTab path (workspace list must hit).
	oldWorkspaceList := "workspace_list)\n  : > \"$STATE/placement_attempted\"\n  printf '%s' '{\"result\":{\"workspaces\":[]}}' ;;"
	newWorkspaceList := "workspace_list)\n  printf '%s' '{\"result\":{\"workspaces\":[{\"workspace_id\":\"w1\",\"label\":\"gastown\"}]}}' ;;"
	rewriteFake(t, p, oldWorkspaceList, newWorkspaceList)

	if err := p.Start(context.Background(), "gastown__witness", runtime.Config{Command: "claude"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	for _, tab := range []string{"tab close t-old1", "tab close t-old2"} {
		if !strings.Contains(calls, tab) {
			t.Errorf("stale duplicate not recycled (%s missing):\n%s", tab, calls)
		}
	}
	if strings.Contains(calls, "tab close t-other") {
		t.Errorf("closed another session's tab:\n%s", calls)
	}
}

// rewriteFake patches the fake herdr script in place.
func rewriteFake(t *testing.T, p *Provider, old, replacement string) {
	t.Helper()
	b, err := os.ReadFile(p.c.bin)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(b), old, replacement, 1)
	if patched == string(b) {
		t.Fatalf("fake script pattern not found:\n%s", old)
	}
	if err := os.WriteFile(p.c.bin, []byte(patched), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Stop must still close the pane via the sidecar binding when no registry
// name exists (the earlier "sleep leak": name lost ⇒ pane never found ⇒
// closePane never issued ⇒ panes piled up), even for an exited agent whose
// pane idles at a prompt — and clear the sidecar.
func TestStopClosesPaneViaBindingWhenNameCleared(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	bindTestPane(t, p, "gastown__witness", bindModeAgent)

	if err := p.Stop("gastown__witness"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if calls := fakeCalls(t, state); !strings.Contains(calls, "pane close %5") {
		t.Fatalf("Stop never closed the bound pane:\n%s", calls)
	}
	if got, _ := p.GetMeta("gastown__witness", metaBoundPane); got != "" {
		t.Fatalf("binding survived Stop: %q", got)
	}
}

// ObserveLiveness is the fast path every liveness consumer actually reads; it
// must fall back to the bound pane too, or the reconciler still sees
// Running=false each tick and drives Start.
func TestObserveLivenessFallsBackToBoundPane(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	setState(t, state, "busy")
	bindTestPane(t, p, "gastown__witness", bindModeAgent)

	if got := p.ObserveLiveness("gastown__witness", nil); !got.Running || !got.Alive {
		t.Fatalf("ObserveLiveness = %+v; want Running=true Alive=true via bound pane", got)
	}

	// Pane confirmed gone: liveness zero and the stale binding is cleared so a
	// recycled pane id can never resurrect a dead session.
	setState(t, state, "pane_gone")
	if got := p.ObserveLiveness("gastown__witness", nil); got.Running || got.Alive {
		t.Fatalf("ObserveLiveness = %+v for a gone pane; want zero", got)
	}
	if got, _ := p.GetMeta("gastown__witness", metaBoundPane); got != "" {
		t.Fatalf("confirmed-gone binding survived: %q", got)
	}
}

// An exited agent (pane at bare prompt, agent mode) reads as not running so
// the reconciler restarts it.
func TestObserveLivenessExitedAgentReadsDead(t *testing.T) {
	p, _ := newFakeHerdrProvider(t)
	bindTestPane(t, p, "gastown__witness", bindModeAgent)
	if got := p.ObserveLiveness("gastown__witness", nil); got.Running || got.Alive {
		t.Fatalf("ObserveLiveness = %+v for an exited agent; want zero", got)
	}
}

// ListRunning must see sessions that herdr's registry does not: raw shell
// sessions never register an agent, so listing by registry alone hides them
// from every session-enumeration consumer (orphan detection, gc ls).
func TestListRunningIncludesUnregisteredBoundSessions(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	setState(t, state, "busy")
	for _, name := range []string{"gastown__worker-1", "gastown__worker-2", "other__worker"} {
		bindTestPane(t, p, name, bindModeShell)
		if err := p.SetMeta(name, metaBoundName, name); err != nil {
			t.Fatal(err)
		}
	}
	got, err := p.ListRunning("gastown__")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	want := map[string]bool{"gastown__worker-1": true, "gastown__worker-2": true}
	if len(got) != len(want) {
		t.Fatalf("ListRunning = %v; want exactly %v", got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Fatalf("ListRunning = %v; unexpected %q", got, n)
		}
	}
}

// A bound session whose pane is gone must not be listed (and is pruned).
func TestListRunningSkipsGonePanes(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	setState(t, state, "pane_gone")
	bindTestPane(t, p, "gastown__worker-1", bindModeShell)
	if err := p.SetMeta("gastown__worker-1", metaBoundName, "gastown__worker-1"); err != nil {
		t.Fatal(err)
	}
	got, err := p.ListRunning("gastown__")
	if err != nil || len(got) != 0 {
		t.Fatalf("ListRunning = %v, %v; want empty", got, err)
	}
}
