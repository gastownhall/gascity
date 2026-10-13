package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestStartCommitPatchRecordsTheRuntimeBinding(t *testing.T) {
	prepared := preparedStart{runtimeBinding: runtime.SessionBinding{ThreadID: "thread-1", ProjectID: "project-1"}}
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
	if got := startedRuntimeBinding(context.Background(), city, sp, "worker"); got != fake.Bindings["worker"] {
		t.Fatalf("startedRuntimeBinding = %+v, want %+v", got, fake.Bindings["worker"])
	}
	// No thread bound: the start stands, and records no binding.
	if got := startedRuntimeBinding(context.Background(), city, sp, "other"); !got.IsZero() {
		t.Fatalf("startedRuntimeBinding(unbound) = %+v, want none", got)
	}
	if got := startedRuntimeBinding(context.Background(), city, runtime.NewFake(), "worker"); !got.IsZero() {
		t.Fatalf("startedRuntimeBinding(provider without bindings) = %+v, want none", got)
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
	if got := result.prepared.runtimeBinding; got != fake.Bindings["worker"] {
		t.Fatalf("prepared runtime binding = %+v, want %+v", got, fake.Bindings["worker"])
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
