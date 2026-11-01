package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

func bindingPatch() sessionpkg.MetadataPatch {
	return sessionpkg.MetadataPatch{sessionpkg.MetadataRuntimeThreadID: "thread-1", sessionpkg.MetadataRuntimeProjectID: "project-1"}
}

func TestStartCommitPatchRecordsTheRuntimeBinding(t *testing.T) {
	prepared := preparedStart{runtimeBinding: bindingPatch()}
	patch := startCommitPatch(&prepared, sessionpkg.Info{}, time.Now())
	if patch[sessionpkg.MetadataRuntimeThreadID] != "thread-1" || patch[sessionpkg.MetadataRuntimeProjectID] != "project-1" {
		t.Fatalf("start commit patch = %v, want the runtime thread and project", patch)
	}
}

func TestStartedRuntimeBinding(t *testing.T) {
	fake := runtime.NewFake()
	fake.Bindings = map[string]runtime.SessionBinding{"worker": {ThreadID: "thread-1", ProjectID: "project-1"}}
	sp := runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: fake})
	city := t.TempDir()
	if got := startedRuntimeBinding(context.Background(), city, sp, "worker"); !maps.Equal(got, bindingPatch()) {
		t.Fatalf("startedRuntimeBinding = %v, want thread-1/project-1", got)
	}
	// Nothing bound: the start stands, and clears any previous thread and pane.
	cleared := sessionpkg.MetadataPatch{
		sessionpkg.MetadataRuntimeThreadID: "", sessionpkg.MetadataRuntimeProjectID: "",
		sessionpkg.MetadataRuntimePaneID: "", sessionpkg.MetadataRuntimePanePID: "",
	}
	if got := startedRuntimeBinding(context.Background(), city, sp, "other"); !maps.Equal(got, cleared) {
		t.Fatalf("startedRuntimeBinding(unbound) = %v, want every binding key cleared", got)
	}
	if got := startedRuntimeBinding(context.Background(), city, runtime.NewFake(), "worker"); len(got) != 0 {
		t.Fatalf("startedRuntimeBinding(provider without bindings) = %v, want nothing", got)
	}
}

// The legacy start path reads the binding once the start succeeded, so its
// commit records it.
func TestRunPreparedStartCandidateReadsTheRuntimeBinding(t *testing.T) {
	fake := runtime.NewFake()
	fake.Bindings = map[string]runtime.SessionBinding{"worker": {ThreadID: "thread-1", ProjectID: "project-1"}}
	sp := runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: fake})
	item := postStartItem(sessionpkg.Info{SessionNameMetadata: "worker"}, t.TempDir())
	result := runPreparedStartCandidate(context.Background(), item, t.TempDir(), sp, nil, nil, time.Minute, immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil)
	if result.err != nil {
		t.Fatalf("start: %v", result.err)
	}
	if got := result.prepared.runtimeBinding; !maps.Equal(got, bindingPatch()) {
		t.Fatalf("prepared runtime binding = %v, want thread-1/project-1", got)
	}
}

// boundLaunchLeaf is the launch kit's leaf hosting every session in one
// thread of its own.
type boundLaunchLeaf struct {
	*startHookLeaf
	binding runtime.SessionBinding
	err     error
}

func (l *boundLaunchLeaf) SessionBinding(string) (runtime.SessionBinding, error) {
	return l.binding, l.err
}

// The v2 launch reads the binding after its Start and commits it with the
// start.
func TestLaunchCommitsTheRuntimeBinding(t *testing.T) {
	k, leaf := launchKit(t, "asleep")
	k.p.Runtime = &boundLaunchLeaf{startHookLeaf: leaf, binding: runtime.SessionBinding{ThreadID: "thread-1", ProjectID: "project-1"}}
	if s := k.runKind(); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	if k.meta(sessionpkg.MetadataRuntimeThreadID) != "thread-1" || k.meta(sessionpkg.MetadataRuntimeProjectID) != "project-1" {
		t.Fatalf("row thread %q project %q, want thread-1 project-1", k.meta(sessionpkg.MetadataRuntimeThreadID), k.meta(sessionpkg.MetadataRuntimeProjectID))
	}
}

// The v2 launch stamps the pane its start spawned into on the row it commits,
// so a pane pid from the host journal names the owning session.
func TestLaunchCommitsThePaneIdentity(t *testing.T) {
	k, leaf := launchKit(t, "asleep")
	k.p.Runtime = &boundLaunchLeaf{startHookLeaf: leaf, binding: runtime.SessionBinding{PaneID: "%7", PanePID: "48213"}}
	if s := k.runKind(); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	if k.meta(sessionpkg.MetadataRuntimePaneID) != "%7" || k.meta(sessionpkg.MetadataRuntimePanePID) != "48213" {
		t.Fatalf("row pane %q pid %q, want %%7 pid 48213", k.meta(sessionpkg.MetadataRuntimePaneID), k.meta(sessionpkg.MetadataRuntimePanePID))
	}
}

// A v2 launch commits a runtime alive with its PreWake token whatever the
// Start returned, so it reads the binding whatever the Start returned: a
// commit after an erroring Start records the thread too, rather than leaving
// the previous incarnation's on the row.
func TestLaunchCommitsTheRuntimeBindingWhateverTheStartReturned(t *testing.T) {
	for _, startErr := range []error{runtime.ErrSessionInitializing, runtime.ErrSessionExists, fmt.Errorf("quota: %w", runtime.ErrProviderCapacity)} {
		k, leaf := launchKit(t, "asleep", sessionpkg.MetadataRuntimeThreadID, "thread-old", sessionpkg.MetadataRuntimeProjectID, "project-old")
		k.p.Runtime = &boundLaunchLeaf{startHookLeaf: leaf, binding: runtime.SessionBinding{ThreadID: "thread-1", ProjectID: "project-1"}}
		k.sp.nextStart = startErr
		leaf.onStart = func(context.Context) {
			token := k.meta("instance_token")
			k.sp.mu.Lock()
			k.sp.put("s-gc-1", "gc-1", "4", token)
			k.sp.mu.Unlock()
		}
		if s := k.runKind(); s.Outcome != settledLanded {
			t.Fatalf("%v: settlement %+v, want landed", startErr, s)
		}
		if k.meta(sessionpkg.MetadataRuntimeThreadID) != "thread-1" || k.meta(sessionpkg.MetadataRuntimeProjectID) != "project-1" {
			t.Fatalf("%v: row thread %q project %q, want thread-1 project-1", startErr, k.meta(sessionpkg.MetadataRuntimeThreadID), k.meta(sessionpkg.MetadataRuntimeProjectID))
		}
	}
}

// A binding the v2 launch cannot read never fails the start it describes.
func TestLaunchLandsWithoutAReadableRuntimeBinding(t *testing.T) {
	k, leaf := launchKit(t, "asleep")
	k.p.Runtime = &boundLaunchLeaf{startHookLeaf: leaf, err: runtime.ErrRuntimeUnavailable}
	if s := k.runKind(); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	if got := k.meta(sessionpkg.MetadataRuntimeThreadID); got != "" {
		t.Fatalf("row thread %q, want none", got)
	}
}

// Recovering a pending create whose runtime is already live is a confirmed
// start too: its commit records the thread the runtime hosts the session in.
func TestRecoverRunningPendingCreateRecordsTheRuntimeBinding(t *testing.T) {
	testCity := t.TempDir()
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "sky",
			"pending_create_claim": "true",
			"state":                "active",
			"state_reason":         "creation_complete",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := runtime.NewFake()
	fake.Bindings = map[string]runtime.SessionBinding{"sky": {ThreadID: "thread-1", ProjectID: "project-1"}}
	sp := runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: fake})
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	tp := TemplateParams{SessionName: "sky", TemplateName: "helper"}

	if ok, _ := recoverRunningPendingCreate(testCity, sessiontest.SeedBead(t, bead), tp, cfg, store, sp, &clock.Fake{Time: time.Now()}, io.Discard, nil); !ok {
		t.Fatal("recoverRunningPendingCreate returned false, want true")
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata[sessionpkg.MetadataRuntimeThreadID] != "thread-1" || got.Metadata[sessionpkg.MetadataRuntimeProjectID] != "project-1" {
		t.Fatalf("row binding = %q, %q; want thread-1, project-1", got.Metadata[sessionpkg.MetadataRuntimeThreadID], got.Metadata[sessionpkg.MetadataRuntimeProjectID])
	}
}

// The session.woke message names the pane the start spawned into, so the pane
// of an earlier incarnation still joins to its session after a restart
// replaced the record's current pane; a start with no pane says nothing.
func TestWokePaneMessageNamesThePaneOfTheStart(t *testing.T) {
	pane := sessionpkg.MetadataPatch{sessionpkg.MetadataRuntimePaneID: "%7", sessionpkg.MetadataRuntimePanePID: "48213"}
	if got, want := wokePaneMessage(pane), "pane %7 pid 48213"; got != want {
		t.Fatalf("wokePaneMessage(pane) = %q, want %q", got, want)
	}
	for name, binding := range map[string]sessionpkg.MetadataPatch{
		"no binding":  nil,
		"thread only": bindingPatch(),
		"cleared":     {sessionpkg.MetadataRuntimePaneID: "", sessionpkg.MetadataRuntimePanePID: ""},
	} {
		if got := wokePaneMessage(binding); got != "" {
			t.Fatalf("wokePaneMessage(%s) = %q, want empty", name, got)
		}
	}
}
