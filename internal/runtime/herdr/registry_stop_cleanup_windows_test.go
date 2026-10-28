//go:build windows

package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func writeFakeAgentList(t *testing.T, state, list string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(state, "agent_list_json"), []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
}

func bindFakePlacement(t *testing.T, p *Provider, name, pane, tab string) {
	t.Helper()
	if err := p.bindPlacement(name, agentInfo{PaneID: pane, TabID: tab, WorkspaceID: "w1"}, bindModeAgent); err != nil {
		t.Fatal(err)
	}
}

func stubPaneTeardown(t *testing.T) *[]runtime.LiveRuntime {
	t.Helper()
	origIdentity := processIdentity
	origKill := killPaneRuntime
	origScan := scanPaneSessionRuntimes
	origInfrastructure := rootIsCityInfrastructure
	var killed []runtime.LiveRuntime
	t.Cleanup(func() {
		processIdentity = origIdentity
		killPaneRuntime = origKill
		scanPaneSessionRuntimes = origScan
		rootIsCityInfrastructure = origInfrastructure
	})
	processIdentity = func(pid int) (string, error) {
		return "bound-shell", nil
	}
	killPaneRuntime = func(live runtime.LiveRuntime) error {
		killed = append(killed, live)
		return nil
	}
	scanPaneSessionRuntimes = func(string) ([]runtime.LiveRuntime, error) {
		return nil, nil
	}
	return &killed
}

func TestRegistryUsesNameBeforeAgentKind(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	writeFakeAgentList(t, state, `{"result":{"agents":[{"agent":"copilot","name":"mayor","pane_id":"%7","tab_id":"t7","workspace_id":"w1","agent_status":"idle"}]}}`)

	got, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"mayor"}) {
		t.Fatalf("ListRunning = %v; want [mayor] from herdr 0.9.1 name field", got)
	}
	if err := p.Stop("copilot"); err != nil {
		t.Fatalf("Stop(copilot): %v", err)
	}
	if calls := fakeCalls(t, state); strings.Contains(calls, "pane close %7") {
		t.Fatalf("Stop(copilot) closed pane %%7 even though herdr registered name=mayor:\n%s", calls)
	}
}

func TestForeignSplitPaneRemainsVisible(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	bindFakePlacement(t, p, "mayor", "%5", "t1")
	setState(t, state, "busy")
	writeFakeAgentList(t, state, `{"result":{"agents":[{"agent":"copilot","name":"mayor","pane_id":"%5","tab_id":"t1","workspace_id":"w1","agent_status":"idle"},{"agent":"claude","name":"manual","pane_id":"%6","tab_id":"t1","workspace_id":"w1","agent_status":"idle"}]}}`)

	got, err := p.ListRunning("")
	if err != nil || !reflect.DeepEqual(got, []string{"mayor", "manual"}) {
		t.Fatalf("ListRunning = %v, %v; want [mayor manual]", got, err)
	}
}

func TestUnnamedDetectedAgentIsNotASession(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	bindFakePlacement(t, p, "shell1", "%5", "t1")
	if err := p.SetMeta("shell1", metaBoundMode, bindModeShell); err != nil {
		t.Fatal(err)
	}
	setState(t, state, "busy")
	writeFakeAgentList(t, state, `{"result":{"agents":[{"agent":"copilot","pane_id":"%5","tab_id":"t1","workspace_id":"w1","agent_status":"working"}]}}`)

	got, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(got) != 1 || got[0] != "shell1" {
		t.Fatalf("ListRunning = %v; unnamed detected kind must not become a session", got)
	}
	if got := p.ObserveLiveness("copilot", nil); got.Running || got.Alive {
		t.Fatalf("ObserveLiveness(copilot) = %+v; unnamed detected kind must not be observable by kind", got)
	}
	if err := p.Stop("copilot"); err != nil {
		t.Fatalf("Stop(copilot): %v", err)
	}
	if calls := fakeCalls(t, state); strings.Contains(calls, "pane close %5") {
		t.Fatalf("Stop(copilot) closed unnamed shell1 pane:\n%s", calls)
	}
}

func TestStopPreservesLegacyNilOnLookupError(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	setState(t, state, "processinfo_flaky")
	bindTestPane(t, p, "mayor", bindModeAgent)
	if err := p.Stop("mayor"); err != nil {
		t.Fatalf("Stop returned %v; want nil", err)
	}
}

func TestStaleShellIdentityCannotAuthorizeKill(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	bindFakePlacement(t, p, "mayor", "%5", "t1")
	setState(t, state, "registered")
	if err := p.SetMeta("mayor", metaBoundShellPID, "4242"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("mayor", metaBoundShellID, "old-shell"); err != nil {
		t.Fatal(err)
	}
	killed := stubPaneTeardown(t)
	processIdentity = func(pid int) (string, error) {
		if pid != 4242 {
			t.Fatalf("processIdentity(%d), want 4242", pid)
		}
		return "new-shell", nil
	}

	if err := p.Stop("mayor"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(*killed) != 0 {
		t.Fatalf("stale shell identity authorized kill: %+v", *killed)
	}
}

func TestStopKillsSessionOrphanWhenPaneShellAlreadyExited(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	bindFakePlacement(t, p, "mayor", "%5", "t1")
	setState(t, state, "pane_gone")
	if err := p.SetMeta("mayor", "GC_SESSION_ID", "session-1"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("mayor", "GC_CITY_PATH", p.c.cityRoot); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("mayor", "GC_RUNTIME_EPOCH", "1"); err != nil {
		t.Fatal(err)
	}
	killed := stubPaneTeardown(t)
	scans := 0
	scanPaneSessionRuntimes = func(id string) ([]runtime.LiveRuntime, error) {
		if id != "session-1" {
			return nil, errors.New("wrong session id")
		}
		scans++
		if scans > 1 {
			return nil, nil
		}
		return []runtime.LiveRuntime{{SessionID: id, City: p.c.cityRoot, Epoch: 1, PID: 4343, StartIdentity: "orphan-incarnation"}}, nil
	}

	if err := p.Stop("mayor"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(*killed) != 1 || (*killed)[0].PID != 4343 || (*killed)[0].StartIdentity != "orphan-incarnation" {
		t.Fatalf("killed = %+v; want session-tagged orphan PID 4343", *killed)
	}
}

func TestOrphanRescanFencesCityEpochAndInfrastructure(t *testing.T) {
	p, _ := newFakeHerdrProvider(t)
	if err := p.SetMeta("mayor", "GC_SESSION_ID", "shared-session"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("mayor", "GC_CITY_PATH", p.c.cityRoot); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("mayor", "GC_RUNTIME_EPOCH", "7"); err != nil {
		t.Fatal(err)
	}
	killed := stubPaneTeardown(t)
	scanPaneSessionRuntimes = func(id string) ([]runtime.LiveRuntime, error) {
		return []runtime.LiveRuntime{
			{SessionID: id, City: p.c.cityRoot, Epoch: 7, PID: 111, StartIdentity: "same-city-epoch"},
			{SessionID: id, City: `C:\other-city`, Epoch: 7, PID: 222, StartIdentity: "other-city"},
			{SessionID: id, City: p.c.cityRoot, Epoch: 8, PID: 333, StartIdentity: "wrong-epoch"},
			{SessionID: id, City: p.c.cityRoot, Epoch: 7, PID: 444, StartIdentity: "infrastructure"},
			{SessionID: id, City: p.c.cityRoot, Epoch: 7, PID: 555, StartIdentity: "provider-parent", ParentIsProviderInfrastructure: true},
		}, nil
	}
	rootIsCityInfrastructure = func(live runtime.LiveRuntime) (bool, error) {
		return live.PID == 444, nil
	}

	remaining, err := p.terminateSessionOrphans("mayor")
	if err != nil {
		t.Fatalf("terminateSessionOrphans: %v", err)
	}
	if len(remaining) != 1 || remaining[0] != 111 {
		t.Fatalf("remaining = %v; want only PID 111", remaining)
	}
	if len(*killed) != 1 || (*killed)[0].PID != 111 {
		t.Fatalf("killed = %+v; want only same-city same-epoch non-infrastructure PID 111", *killed)
	}
}

func TestHolderReapDoesNotRunOrphanRescan(t *testing.T) {
	p, _ := newFakeHerdrProvider(t)
	bindFakePlacement(t, p, "mayor", "%5", "t1")
	if err := p.SetMeta("mayor", "GC_SESSION_ID", "session-new"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("mayor", "GC_CITY_PATH", p.c.cityRoot); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("mayor", "GC_RUNTIME_EPOCH", "1"); err != nil {
		t.Fatal(err)
	}
	killed := stubPaneTeardown(t)
	var scans int
	scanPaneSessionRuntimes = func(id string) ([]runtime.LiveRuntime, error) {
		scans++
		return []runtime.LiveRuntime{{SessionID: id, City: p.c.cityRoot, Epoch: 1, PID: 5000, StartIdentity: "fresh-start"}}, nil
	}

	_ = p.closeSessionPane(context.Background(), "mayor", "%9")
	if scans != 0 || len(*killed) != 0 {
		t.Fatalf("holder reap scanned=%d killed=%+v; Start-time holder close must not orphan-scan", scans, *killed)
	}
}

func TestIncompleteStopClearsBindingAndRetriesOrphanCleanup(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	bindFakePlacement(t, p, "old", "%5", "t1")
	setState(t, state, "processinfo_flaky")
	if err := p.SetMeta("old", "GC_SESSION_ID", "session-old"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("old", "GC_CITY_PATH", p.c.cityRoot); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("old", "GC_RUNTIME_EPOCH", "1"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("old", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("old", "GC_INSTANCE_TOKEN", "old-token"); err != nil {
		t.Fatal(err)
	}
	stubPaneTeardown(t)
	var scans int
	scanPaneSessionRuntimes = func(string) ([]runtime.LiveRuntime, error) {
		scans++
		return nil, errors.New("scan unavailable")
	}

	if err := p.Stop("old"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, key := range []string{metaBoundPane, "GC_DRAIN_ACK", "GC_INSTANCE_TOKEN", "GC_SESSION_ID", "GC_HERDR_STOP_CLEANUP_INCOMPLETE", "GC_HERDR_PENDING_CLEANUP_GC_SESSION_ID"} {
		if got, _ := p.GetMeta("old", key); got != "" {
			t.Fatalf("after incomplete Stop %s = %q; want full sidecar clear", key, got)
		}
	}
	if p.IsRunning("old") {
		t.Fatalf("after incomplete Stop IsRunning(old)=true; want false")
	}
	if scans != 1 {
		t.Fatalf("scans = %d; want one bounded attempt for scan failure", scans)
	}
}

func TestStopRetriesLateAppearingFencedOrphan(t *testing.T) {
	p, _ := newFakeHerdrProvider(t)
	bindFakePlacement(t, p, "late", "%5", "t1")
	if err := p.SetMeta("late", "GC_SESSION_ID", "session-late"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("late", "GC_CITY_PATH", p.c.cityRoot); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("late", "GC_RUNTIME_EPOCH", "1"); err != nil {
		t.Fatal(err)
	}
	killed := stubPaneTeardown(t)
	scan := 0
	scanPaneSessionRuntimes = func(id string) ([]runtime.LiveRuntime, error) {
		scan++
		switch scan {
		case 1:
			return []runtime.LiveRuntime{{SessionID: id, City: p.c.cityRoot, Epoch: 1, PID: 111, StartIdentity: "first"}}, nil
		case 2:
			return []runtime.LiveRuntime{{SessionID: id, City: p.c.cityRoot, Epoch: 1, PID: 222, StartIdentity: "late"}}, nil
		default:
			return nil, nil
		}
	}

	if err := p.Stop("late"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(*killed) != 2 || (*killed)[0].PID != 111 || (*killed)[1].PID != 222 {
		t.Fatalf("killed = %+v; want first and late fenced orphans", *killed)
	}
	if scan != 3 {
		t.Fatalf("scan attempts = %d; want retry until clear", scan)
	}
}

func TestStopDoesNotWaitWhenNoFencedOrphansRemain(t *testing.T) {
	p, _ := newFakeHerdrProvider(t)
	if err := p.SetMeta("quiet", "GC_SESSION_ID", "session-quiet"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("quiet", "GC_CITY_PATH", p.c.cityRoot); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("quiet", "GC_RUNTIME_EPOCH", "1"); err != nil {
		t.Fatal(err)
	}
	stubPaneTeardown(t)
	scans := 0
	scanPaneSessionRuntimes = func(string) ([]runtime.LiveRuntime, error) {
		scans++
		return nil, nil
	}

	if err := p.Stop("quiet"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if scans != 1 {
		t.Fatalf("scans = %d; want one prompt scan when none remain", scans)
	}
}
