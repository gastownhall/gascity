package hybrid

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// capLeaf is a Fake that also implements the optional capabilities hybrid
// must forward, logging each call as "label:Method[:name]" into a log shared
// by both legs.
type capLeaf struct {
	*runtime.Fake
	label       string
	log         *[]string
	roster      map[string]runtime.SessionRosterEntry
	teardownErr error
}

func (l *capLeaf) record(method, name string) {
	*l.log = append(*l.log, strings.TrimSuffix(l.label+":"+method+":"+name, ":"))
}

func (l *capLeaf) DismissKnownDialogs(_ context.Context, name string, _ time.Duration) error {
	l.record("DismissKnownDialogs", name)
	return nil
}

func (l *capLeaf) GetAllEnvironment(name string) (map[string]string, error) {
	l.record("GetAllEnvironment", name)
	return map[string]string{"LEAF": l.label}, nil
}

func (l *capLeaf) SessionRoster() (map[string]runtime.SessionRosterEntry, error) {
	l.record("SessionRoster", "")
	return l.roster, nil
}

func (l *capLeaf) ConfigureServer() error {
	l.record("ConfigureServer", "")
	return nil
}

func (l *capLeaf) TeardownServer() error {
	l.record("TeardownServer", "")
	return l.teardownErr
}

func (l *capLeaf) KillCorpseObject(name, _, _ string) (runtime.SessionObjectKillResult, error) {
	l.record("KillCorpseObject", name)
	return runtime.SessionObjectKilled, nil
}

func (l *capLeaf) KillZombieObject(name, _, _, _ string) (runtime.SessionObjectKillResult, error) {
	l.record("KillZombieObject", name)
	return runtime.SessionObjectKilled, nil
}

// bareLeaf exposes only runtime.Provider, hiding every optional capability.
type bareLeaf struct{ runtime.Provider }

func twoCapLeaves() (*capLeaf, *capLeaf, *[]string) {
	log := &[]string{}
	return &capLeaf{Fake: runtime.NewFake(), label: "local", log: log},
		&capLeaf{Fake: runtime.NewFake(), label: "remote", log: log}, log
}

func wantLog(t *testing.T, log *[]string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(*log, want) && (len(*log) != 0 || len(want) != 0) {
		t.Fatalf("calls = %v, want %v", *log, want)
	}
	*log = nil
}

func TestPerSessionCapabilitiesRouteToSessionBackend(t *testing.T) {
	local, remote, log := twoCapLeaves()
	p := New(local, remote, isRemote)
	ctx := context.Background()

	for _, tc := range []struct{ name, leaf string }{{"agent", "local"}, {"remote-agent", "remote"}} {
		if err := p.DismissKnownDialogs(ctx, tc.name, time.Second); err != nil {
			t.Fatalf("DismissKnownDialogs(%s): %v", tc.name, err)
		}
		if env, err := p.GetAllEnvironment(tc.name); err != nil || env["LEAF"] != tc.leaf {
			t.Fatalf("GetAllEnvironment(%s) = %v, %v; want the %s leg's env", tc.name, env, err, tc.leaf)
		}
		if res, err := p.KillCorpseObject(tc.name, "$1", "1"); err != nil || res != runtime.SessionObjectKilled {
			t.Fatalf("KillCorpseObject(%s) = %v, %v", tc.name, res, err)
		}
		if res, err := p.KillZombieObject(tc.name, "$1", "1", "9"); err != nil || res != runtime.SessionObjectKilled {
			t.Fatalf("KillZombieObject(%s) = %v, %v", tc.name, res, err)
		}
		wantLog(t, log, tc.leaf+":DismissKnownDialogs:"+tc.name, tc.leaf+":GetAllEnvironment:"+tc.name,
			tc.leaf+":KillCorpseObject:"+tc.name, tc.leaf+":KillZombieObject:"+tc.name)
	}
}

// A remote leg without the capabilities (k8s) answers unsupported for its
// sessions; the local leg is never asked about a remote session.
func TestPerSessionCapabilitiesUnsupportedOnBareBackend(t *testing.T) {
	local, _, log := twoCapLeaves()
	p := New(local, bareLeaf{runtime.NewFake()}, isRemote)
	const name = "remote-agent"
	if err := p.DismissKnownDialogs(context.Background(), name, time.Second); !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("DismissKnownDialogs = %v, want ErrInteractionUnsupported", err)
	}
	if _, err := p.GetAllEnvironment(name); !errors.Is(err, runtime.ErrMetaUnsupported) {
		t.Fatalf("GetAllEnvironment = %v, want ErrMetaUnsupported", err)
	}
	if res, err := p.KillCorpseObject(name, "$1", "1"); !errors.Is(err, runtime.ErrSessionObjectKillUnsupported) || res != runtime.SessionObjectNotKilled {
		t.Fatalf("KillCorpseObject = %v, %v; want NotKilled, ErrSessionObjectKillUnsupported", res, err)
	}
	if res, err := p.KillZombieObject(name, "$1", "1", "9"); !errors.Is(err, runtime.ErrSessionObjectKillUnsupported) || res != runtime.SessionObjectNotKilled {
		t.Fatalf("KillZombieObject = %v, %v; want NotKilled, ErrSessionObjectKillUnsupported", res, err)
	}
	wantLog(t, log)
}

func TestServerLifecycleAndRosterReachEveryBackend(t *testing.T) {
	local, remote, log := twoCapLeaves()
	p := New(local, remote, isRemote)
	local.teardownErr = errors.New("local-down")
	if err := p.TeardownServer(); err == nil || !strings.Contains(err.Error(), "local backend: local-down") {
		t.Fatalf("TeardownServer = %v, want the local backend's error", err)
	}
	wantLog(t, log, "local:TeardownServer", "remote:TeardownServer")
	if err := p.ConfigureServer(); err != nil {
		t.Fatalf("ConfigureServer: %v", err)
	}
	wantLog(t, log, "local:ConfigureServer", "remote:ConfigureServer")

	local.roster = map[string]runtime.SessionRosterEntry{"agent": {Attached: true}, "remote-agent": {}}
	remote.roster = map[string]runtime.SessionRosterEntry{"remote-agent": {Attached: true}}
	got, err := p.SessionRoster()
	want := map[string]runtime.SessionRosterEntry{"agent": {Attached: true}, "remote-agent": {Attached: true}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("SessionRoster = %v, %v; want %v (a duplicate keeps its routed backend's entry)", got, err, want)
	}
}

// The local tmux leg's process-table scan is forwarded; behind a scannerless
// local leg the composite has no scanner, as auto behind a scannerless default.
func TestProcessTableScannerFollowsLocalBackend(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	if err := local.Start(context.Background(), "agent", runtime.Config{Env: map[string]string{"GC_SESSION_ID": "sid"}}); err != nil {
		t.Fatal(err)
	}
	p := New(local, remote, isRemote)
	if _, ok := runtime.AsProcessTableScanner(p); !ok {
		t.Fatal("AsProcessTableScanner = false over a scanning local backend")
	}
	found, err := p.FindRuntimesBySessionID("sid")
	if err != nil || len(found) != 1 || !found[0].IsTracked {
		t.Fatalf("FindRuntimesBySessionID = %+v, %v; want the local leg's tracked root", found, err)
	}
	if remote.CountCalls("FindRuntimesBySessionID", "sid") != 1 {
		t.Error("a scanning remote backend was not scanned")
	}
	if err := p.TerminateRuntime(found[0]); err != nil {
		t.Fatalf("TerminateRuntime: %v", err)
	}
	if local.CountCalls("TerminateRuntime", "sid") != 1 || remote.CountCalls("TerminateRuntime", "sid") != 0 {
		t.Error("TerminateRuntime must run on the local backend only")
	}

	scannerless := New(bareLeaf{runtime.NewFake()}, remote, isRemote)
	if _, ok := runtime.AsProcessTableScanner(scannerless); ok {
		t.Error("AsProcessTableScanner = true over a scannerless local backend")
	}
	if found, _ := scannerless.FindRuntimesBySessionID("sid"); len(found) != 0 {
		t.Errorf("FindRuntimesBySessionID behind a scannerless local backend = %+v, want none", found)
	}
	if err := scannerless.TerminateRuntime(runtime.LiveRuntime{SessionID: "sid", PID: 1}); err == nil {
		t.Error("TerminateRuntime = nil behind a scannerless local backend, want an error")
	}
}
