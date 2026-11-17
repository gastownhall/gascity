package auto

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// capLeaf is a Fake that also implements the optional capabilities auto must
// forward (dialogs, batched env, server lifecycle, roster), logging each call
// as "label:Method[:name]" into a log shared by both legs.
type capLeaf struct {
	*runtime.Fake
	label                    string
	log                      *[]string
	roster                   map[string]runtime.SessionRosterEntry
	rosterErr, teardownErr   error
	configureErr, dialogsErr error
}

// record logs a call with every argument it was forwarded.
func (l *capLeaf) record(method string, args ...string) {
	*l.log = append(*l.log, strings.Join(append([]string{l.label, method}, args...), ":"))
}

// ctxKey tags the context a test passes, so a forward that drops it shows.
type ctxKey struct{}

func (l *capLeaf) DismissKnownDialogs(ctx context.Context, name string, timeout time.Duration) error {
	tag, _ := ctx.Value(ctxKey{}).(string)
	l.record("DismissKnownDialogs", name, tag, timeout.String())
	return l.dialogsErr
}

func (l *capLeaf) GetAllEnvironment(name string) (map[string]string, error) {
	l.record("GetAllEnvironment", name)
	return map[string]string{"LEAF": l.label}, nil
}

func (l *capLeaf) SessionRoster() (map[string]runtime.SessionRosterEntry, error) {
	l.record("SessionRoster")
	return l.roster, l.rosterErr
}

func (l *capLeaf) ConfigureServer() error {
	l.record("ConfigureServer")
	return l.configureErr
}

func (l *capLeaf) TeardownServer() error {
	l.record("TeardownServer")
	return l.teardownErr
}

// bareLeaf exposes only runtime.Provider, hiding every optional capability.
type bareLeaf struct{ runtime.Provider }

func twoCapLeaves() (*capLeaf, *capLeaf, *[]string) {
	log := &[]string{}
	return &capLeaf{Fake: runtime.NewFake(), label: "tmux", log: log},
		&capLeaf{Fake: runtime.NewFake(), label: "acp", log: log}, log
}

func wantLog(t *testing.T, log *[]string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(*log, want) && (len(*log) != 0 || len(want) != 0) {
		t.Fatalf("calls = %v, want %v", *log, want)
	}
	*log = nil
}

func TestDismissKnownDialogsRoutesToSessionBackend(t *testing.T) {
	tmux, acp, log := twoCapLeaves()
	p := New(tmux, acp)
	p.RouteACP("a")
	ctx := context.WithValue(context.Background(), ctxKey{}, "caller")

	if err := p.DismissKnownDialogs(ctx, "t", 7*time.Second); err != nil {
		t.Fatalf("DismissKnownDialogs(t): %v", err)
	}
	wantLog(t, log, "tmux:DismissKnownDialogs:t:caller:7s")
	if err := p.DismissKnownDialogs(ctx, "a", 9*time.Second); err != nil {
		t.Fatalf("DismissKnownDialogs(a): %v", err)
	}
	wantLog(t, log, "acp:DismissKnownDialogs:a:caller:9s")

	tmux.dialogsErr = runtime.ErrWorkspaceTrustUnconfirmed
	if err := p.DismissKnownDialogs(ctx, "t", time.Second); !errors.Is(err, runtime.ErrWorkspaceTrustUnconfirmed) {
		t.Fatalf("DismissKnownDialogs(t) = %v, want the tmux leg's error", err)
	}
}

func TestDismissKnownDialogsUnsupportedOnBackendWithoutDialogs(t *testing.T) {
	tmux, _, log := twoCapLeaves()
	p := New(tmux, bareLeaf{runtime.NewFake()})
	p.RouteACP("a")
	if err := p.DismissKnownDialogs(context.Background(), "a", time.Second); !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("DismissKnownDialogs(a) = %v, want ErrInteractionUnsupported", err)
	}
	wantLog(t, log)
}

func TestGetAllEnvironmentRoutesToSessionBackend(t *testing.T) {
	tmux, acp, log := twoCapLeaves()
	p := New(tmux, acp)
	p.RouteACP("a")
	if env, err := p.GetAllEnvironment("t"); err != nil || env["LEAF"] != "tmux" {
		t.Fatalf("GetAllEnvironment(t) = %v, %v; want the tmux leg's env", env, err)
	}
	wantLog(t, log, "tmux:GetAllEnvironment:t")
	if env, err := p.GetAllEnvironment("a"); err != nil || env["LEAF"] != "acp" {
		t.Fatalf("GetAllEnvironment(a) = %v, %v; want the acp leg's env", env, err)
	}
	wantLog(t, log, "acp:GetAllEnvironment:a")

	p = New(tmux, bareLeaf{runtime.NewFake()})
	p.RouteACP("a")
	if _, err := p.GetAllEnvironment("a"); !errors.Is(err, runtime.ErrEnvironmentBatchUnsupported) || errors.Is(err, runtime.ErrMetaUnsupported) {
		t.Fatalf("GetAllEnvironment(a) on a bare acp leg = %v, want ErrEnvironmentBatchUnsupported, not ErrMetaUnsupported", err)
	}
	wantLog(t, log)
}

func TestServerLifecycleReachesEveryBackend(t *testing.T) {
	tmux, acp, log := twoCapLeaves()
	p := New(tmux, acp)
	if err := p.TeardownServer(); err != nil {
		t.Fatalf("TeardownServer: %v", err)
	}
	wantLog(t, log, "tmux:TeardownServer", "acp:TeardownServer")
	if err := p.ConfigureServer(); err != nil {
		t.Fatalf("ConfigureServer: %v", err)
	}
	wantLog(t, log, "tmux:ConfigureServer", "acp:ConfigureServer")

	// A failing backend does not skip the other; both errors are reported.
	tmux.teardownErr, acp.teardownErr = errors.New("tmux-down"), errors.New("acp-down")
	err := p.TeardownServer()
	wantLog(t, log, "tmux:TeardownServer", "acp:TeardownServer")
	if err == nil || !strings.Contains(err.Error(), "default backend: tmux-down") || !strings.Contains(err.Error(), "acp backend: acp-down") {
		t.Fatalf("TeardownServer = %v, want both labeled errors", err)
	}
	tmux.configureErr = errors.New("cfg-failed")
	if err := p.ConfigureServer(); err == nil || !strings.Contains(err.Error(), "default backend: cfg-failed") {
		t.Fatalf("ConfigureServer = %v, want the default backend's error", err)
	}
	wantLog(t, log, "tmux:ConfigureServer", "acp:ConfigureServer")

	// Backends without a server are skipped, matching a caller that found no
	// ServerLifecycleProvider.
	tmux.teardownErr = nil
	if err := New(tmux, bareLeaf{runtime.NewFake()}).TeardownServer(); err != nil {
		t.Fatalf("TeardownServer with a bare acp leg: %v", err)
	}
	wantLog(t, log, "tmux:TeardownServer")
	if err := New(bareLeaf{runtime.NewFake()}, bareLeaf{runtime.NewFake()}).TeardownServer(); err != nil {
		t.Fatalf("TeardownServer with no server: %v", err)
	}
}

func TestSessionRosterMergesBackends(t *testing.T) {
	tmux, acp, log := twoCapLeaves()
	t0 := time.Unix(100, 0)
	tmux.roster = map[string]runtime.SessionRosterEntry{"t": {Attached: true}, "dup": {LastActivity: t0}}
	acp.roster = map[string]runtime.SessionRosterEntry{"a": {LastActivity: t0}, "dup": {Attached: true}}
	p := New(tmux, acp)

	got, err := p.SessionRoster()
	if err != nil {
		t.Fatalf("SessionRoster: %v", err)
	}
	want := map[string]runtime.SessionRosterEntry{"t": {Attached: true}, "a": {LastActivity: t0}, "dup": {LastActivity: t0}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SessionRoster = %v, want %v (a duplicate keeps the default route's entry)", got, want)
	}
	wantLog(t, log, "tmux:SessionRoster", "acp:SessionRoster")

	p.RouteACP("dup")
	if got, _ := p.SessionRoster(); got["dup"] != acp.roster["dup"] {
		t.Fatalf("dup = %+v, want the acp entry once dup routes to ACP", got["dup"])
	}

	acp.rosterErr = errors.New("acp-roster")
	if _, err := p.SessionRoster(); err == nil || !strings.Contains(err.Error(), "acp backend: acp-roster") {
		t.Fatalf("SessionRoster = %v, want the acp backend's error", err)
	}
}

func TestSessionRosterWithoutRosterBackends(t *testing.T) {
	tmux, _, _ := twoCapLeaves()
	tmux.roster = map[string]runtime.SessionRosterEntry{"t": {Attached: true}}
	bare := func() runtime.Provider { return bareLeaf{runtime.NewFake()} }

	if _, err := New(bare(), bare()).SessionRoster(); !errors.Is(err, runtime.ErrSessionRosterUnsupported) {
		t.Fatalf("SessionRoster with no roster backend = %v, want ErrSessionRosterUnsupported", err)
	}
	// A nested composite without a roster backend contributes nothing rather
	// than failing the merge.
	got, err := New(tmux, New(bare(), bare())).SessionRoster()
	if err != nil || !reflect.DeepEqual(got, tmux.roster) {
		t.Fatalf("SessionRoster over a rosterless nested composite = %v, %v; want the tmux roster", got, err)
	}
}

// TestForwardsThroughRealHybrid nests a real hybrid under auto, as a hybrid
// city with an ACP-capable provider is built: every forward reaches the
// hybrid's local tmux-like leg, and a hybrid city now has a process-table
// scanner (orphan sweep, killExistingOrphans, drain-ack escalation).
func TestForwardsThroughRealHybrid(t *testing.T) {
	local, _, log := twoCapLeaves()
	remote := bareLeaf{runtime.NewFake()}
	p := New(hybrid.New(local, remote, func(name string) bool { return strings.HasPrefix(name, "remote-") }), runtime.NewFake())
	p.RouteACP("a")
	ctx := context.WithValue(context.Background(), ctxKey{}, "caller")

	if err := p.DismissKnownDialogs(ctx, "t", time.Second); err != nil {
		t.Fatalf("DismissKnownDialogs(t): %v", err)
	}
	if env, err := p.GetAllEnvironment("t"); err != nil || env["LEAF"] != "tmux" {
		t.Fatalf("GetAllEnvironment(t) = %v, %v; want the local leg's env", env, err)
	}
	if err := p.TeardownServer(); err != nil {
		t.Fatalf("TeardownServer: %v", err)
	}
	local.roster = map[string]runtime.SessionRosterEntry{"t": {Attached: true}}
	if got, err := p.SessionRoster(); err != nil || !reflect.DeepEqual(got, local.roster) {
		t.Fatalf("SessionRoster = %v, %v; want the local leg's roster", got, err)
	}
	wantLog(t, log, "tmux:DismissKnownDialogs:t:caller:1s", "tmux:GetAllEnvironment:t", "tmux:TeardownServer", "tmux:SessionRoster")

	if _, err := p.GetAllEnvironment("remote-x"); !errors.Is(err, runtime.ErrEnvironmentBatchUnsupported) {
		t.Fatalf("GetAllEnvironment(remote-x) = %v, want ErrEnvironmentBatchUnsupported", err)
	}
	if err := local.Start(context.Background(), "t", runtime.Config{Env: map[string]string{"GC_SESSION_ID": "sid"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := runtime.AsProcessTableScanner(p); !ok {
		t.Fatal("AsProcessTableScanner(auto over hybrid) = false, want the local leg's scanner")
	}
	if found, err := p.FindRuntimesBySessionID("sid"); err != nil || len(found) != 1 || !found[0].IsTracked {
		t.Fatalf("FindRuntimesBySessionID = %+v, %v; want the local leg's tracked root", found, err)
	}
}
