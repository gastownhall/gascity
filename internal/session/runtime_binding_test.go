package session

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
	binding := MetadataPatch{MetadataRuntimeThreadID: "thread-1", MetadataRuntimeProjectID: "project-1"}
	patch := CommitStartedPatch(CommitStartedPatchInput{Now: now, RuntimeBinding: binding})
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

// The patch records what the runtime answered: the thread, or no thread at
// all (clearing a previous incarnation's). A runtime that hosts no threads,
// or that cannot be read, writes nothing, so the keys stay as of the last
// successful read.
func TestRuntimeBindingPatch(t *testing.T) {
	bound := runtime.SessionBinding{ThreadID: "thread-1", ProjectID: "project-1"}
	for _, tc := range []struct {
		name    string
		sp      runtime.Provider
		want    MetadataPatch
		wantErr error
	}{
		{
			name: "bound",
			sp:   boundProvider{Provider: runtime.NewFake(), binding: bound},
			want: MetadataPatch{MetadataRuntimeThreadID: "thread-1", MetadataRuntimeProjectID: "project-1"},
		},
		{
			name: "no thread bound",
			sp:   boundProvider{Provider: runtime.NewFake(), err: fmt.Errorf("%w: gone", runtime.ErrSessionNotFound)},
			want: MetadataPatch{MetadataRuntimeThreadID: "", MetadataRuntimeProjectID: ""},
		},
		{
			name:    "unreadable",
			sp:      boundProvider{Provider: runtime.NewFake(), err: runtime.ErrRuntimeUnavailable},
			want:    MetadataPatch{},
			wantErr: runtime.ErrRuntimeUnavailable,
		},
		{
			name: "not hosted in a thread",
			sp:   boundProvider{Provider: runtime.NewFake(), binding: runtime.SessionBinding{ProjectID: "project-1"}},
			want: MetadataPatch{},
		},
		{
			name: "provider without threads",
			sp:   runtime.NewFake(),
			want: MetadataPatch{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RuntimeBindingPatch(tc.sp, "s")
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("RuntimeBindingPatch error = %v, want %v", err, tc.wantErr)
			}
			if !maps.Equal(got, tc.want) {
				t.Fatalf("RuntimeBindingPatch = %v, want %v", got, tc.want)
			}
		})
	}
}

// A resume (Manager.ensureRunning: wake, Submit, Attach, Start) is a start
// too: it records the thread the runtime hosts the session in now, which
// replaces the previous incarnation's.
func TestResumeRecordsTheRuntimeBinding(t *testing.T) {
	store := beads.NewMemStore()
	sp := &boundProvider{Provider: runtime.NewFake(), binding: runtime.SessionBinding{ThreadID: "thread-1", ProjectID: "project-1"}}
	mgr := newTestManager(t, store, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "codex", WorkDir: "/tmp", Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	rowBinding := func() (string, string) {
		t.Helper()
		b, err := store.Get(info.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return b.Metadata[MetadataRuntimeThreadID], b.Metadata[MetadataRuntimeProjectID]
	}
	resume := func() {
		t.Helper()
		if err := sp.Stop(info.SessionName); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, "codex", runtime.Config{WorkDir: "/tmp"}); err != nil {
			t.Fatalf("Start(resume): %v", err)
		}
	}

	sp.binding = runtime.SessionBinding{ThreadID: "thread-2", ProjectID: "project-2"}
	resume()
	if thread, project := rowBinding(); thread != "thread-2" || project != "project-2" {
		t.Fatalf("row binding after resume = %q, %q; want thread-2, project-2", thread, project)
	}

	// Unreadable: the last successful read stands.
	sp.err = runtime.ErrRuntimeUnavailable
	resume()
	if thread, _ := rowBinding(); thread != "thread-2" {
		t.Fatalf("row thread after an unreadable resume = %q, want thread-2 kept", thread)
	}

	// No thread bound: the previous incarnation's thread is cleared.
	sp.err = fmt.Errorf("%w: gone", runtime.ErrSessionNotFound)
	resume()
	if thread, project := rowBinding(); thread != "" || project != "" {
		t.Fatalf("row binding with no thread bound = %q, %q; want cleared", thread, project)
	}
}

// A resume whose start finds the runtime already up (another caller's resume,
// or its own canceled one) converges on that runtime, so it records the thread
// that runtime hosts the session in, both where it confirms the row and where
// it consumes the operator's hold on it.
func TestResumeConvergedOnALiveRuntimeRecordsItsBinding(t *testing.T) {
	for name, suspend := range map[string]func(*Manager, string) error{
		"no hold":       sweepSuspend,
		"operator hold": operatorSuspend,
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := &boundProvider{Provider: runtime.NewFake(), binding: runtime.SessionBinding{ThreadID: "thread-1", ProjectID: "project-1"}}
			mgr := newTestManager(t, store, sp)
			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "codex", WorkDir: "/tmp", Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			if err := suspend(mgr, info.ID); err != nil {
				t.Fatalf("suspend: %v", err)
			}
			// The runtime is up again, in a new thread the row does not record yet.
			sp.binding = runtime.SessionBinding{ThreadID: "thread-2", ProjectID: "project-2"}
			if err := sp.Start(context.Background(), info.SessionName, runtime.Config{WorkDir: "/tmp"}); err != nil {
				t.Fatalf("concurrent Start: %v", err)
			}

			if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, "codex", runtime.Config{WorkDir: "/tmp"}); err != nil {
				t.Fatalf("Start(resume): %v", err)
			}
			b, err := store.Get(info.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got := State(b.Metadata["state"]); got != StateActive {
				t.Fatalf("state = %q, want %q", got, StateActive)
			}
			if thread, project := b.Metadata[MetadataRuntimeThreadID], b.Metadata[MetadataRuntimeProjectID]; thread != "thread-2" || project != "project-2" {
				t.Fatalf("row binding after a converged resume = %q, %q; want thread-2, project-2", thread, project)
			}
		})
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
