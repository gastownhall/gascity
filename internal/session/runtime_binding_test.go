package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// boundProvider hosts every session in one fixed thread.
type boundProvider struct {
	runtime.Provider
	binding runtime.SessionBinding
	err     error
}

func (p boundProvider) SessionBinding(string) (runtime.SessionBinding, error) {
	return p.binding, p.err
}

func TestCommitStartedPatchRecordsTheRuntimeBinding(t *testing.T) {
	now := time.Now()
	patch := CommitStartedPatch(CommitStartedPatchInput{Now: now, RuntimeBinding: runtime.SessionBinding{ThreadID: "thread-1", ProjectID: "project-1"}})
	if patch[MetadataRuntimeThreadID] != "thread-1" || patch[MetadataRuntimeProjectID] != "project-1" {
		t.Fatalf("patch = %v, want the runtime thread and project", patch)
	}
	unbound := CommitStartedPatch(CommitStartedPatchInput{Now: now})
	for _, key := range []string{MetadataRuntimeThreadID, MetadataRuntimeProjectID} {
		if _, ok := unbound[key]; ok {
			t.Fatalf("patch without a binding writes %s: %v", key, unbound)
		}
	}
}

func TestReadRuntimeBinding(t *testing.T) {
	want := runtime.SessionBinding{ThreadID: "thread-1", ProjectID: "project-1"}
	got, err := ReadRuntimeBinding(boundProvider{Provider: runtime.NewFake(), binding: want}, "s")
	if err != nil || got != want {
		t.Fatalf("ReadRuntimeBinding = %+v, %v; want %+v", got, err, want)
	}
	if got, err := ReadRuntimeBinding(runtime.NewFake(), "s"); err != nil || !got.IsZero() {
		t.Fatalf("ReadRuntimeBinding(provider without bindings) = %+v, %v; want none", got, err)
	}
	if _, err := ReadRuntimeBinding(boundProvider{Provider: runtime.NewFake(), err: runtime.ErrRuntimeUnavailable}, "s"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("ReadRuntimeBinding error = %v, want the provider's", err)
	}
}

// A session the Manager starts records the thread its runtime hosts it in.
func TestCreateSessionRecordsTheRuntimeBinding(t *testing.T) {
	store := beads.NewMemStore()
	sp := boundProvider{Provider: runtime.NewFake(), binding: runtime.SessionBinding{ThreadID: "thread-1", ProjectID: "project-1"}}
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "codex", WorkDir: "/tmp", Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b.Metadata[MetadataRuntimeThreadID] != "thread-1" || b.Metadata[MetadataRuntimeProjectID] != "project-1" {
		t.Fatalf("session metadata = %v, want the runtime thread and project", b.Metadata)
	}
}

// A binding that cannot be read never fails the start it describes.
func TestCreateSessionSurvivesAnUnreadableRuntimeBinding(t *testing.T) {
	store := beads.NewMemStore()
	mgr := newTestManager(t, store, boundProvider{Provider: runtime.NewFake(), err: runtime.ErrRuntimeUnavailable})
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "codex", WorkDir: "/tmp", Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := b.Metadata[MetadataRuntimeThreadID]; ok {
		t.Fatalf("session metadata = %v, want no runtime thread", b.Metadata)
	}
}
