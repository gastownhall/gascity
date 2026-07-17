package worker

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// newStartCommandHandle builds an un-started session handle plus the persisted
// session bead its startCommand reads, so the equivalence tests can pin the
// exact resume/first-start command string across the WI-6 W3 read swap
// (Manager.GetWithBead -> session.Store.GetPersistedResponse + EnrichInfo).
func newStartCommandHandle(t *testing.T, spec SessionSpec, resume sessionpkg.ProviderResume, command string) (*SessionHandle, *beads.MemStore, sessionpkg.Info) {
	t.Helper()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(store, sp)

	info, err := manager.CreateSession(context.Background(), sessionpkg.CreateOptions{
		BeadOnly:  true,
		Template:  "worker",
		Title:     "Probe",
		Command:   command,
		WorkDir:   t.TempDir(),
		Provider:  "legacy-provider",
		Transport: "",
		Resume:    resume,
	})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}

	spec.ID = info.ID
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager: manager,
		Session: spec,
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	return handle, store, info
}

// TestStartCommandFirstProviderSessionStart pins the exact first-start command
// string: a start-pending bead with a session-id flag and a session key that
// has not yet been started (no creation_complete_at, no started_config_hash)
// resolves to `command <flag> <key>`. This is the exact-string behavior the W3
// brief flags — a wrong State source (normalized vs MetadataState) would break
// first-start detection, so it is pinned before the read swap.
func TestStartCommandFirstProviderSessionStart(t *testing.T) {
	handle, _, info := newStartCommandHandle(t,
		SessionSpec{
			Template: "worker",
			Command:  "mycmd",
			Provider: "legacy-provider",
			Resume:   sessionpkg.ProviderResume{SessionIDFlag: "--session-id"},
		},
		sessionpkg.ProviderResume{SessionIDFlag: "--session-id"},
		"mycmd",
	)

	got, err := handle.startCommand(info.ID)
	if err != nil {
		t.Fatalf("startCommand: %v", err)
	}
	want := "mycmd --session-id " + info.SessionKey
	if got != want {
		t.Fatalf("startCommand() = %q, want %q", got, want)
	}
	if info.SessionKey == "" {
		t.Fatal("fixture SessionKey is empty; first-start branch requires a generated key")
	}
}

// TestStartCommandFirstStartOffWithStartedConfigHash pins that a present
// started_config_hash flips first-start detection off, dropping the session-id
// launch form for the resume form. firstProviderSessionStart reads the hash
// from bead metadata, so this guards the pr.Metadata read after the swap.
func TestStartCommandFirstStartOffWithStartedConfigHash(t *testing.T) {
	handle, store, info := newStartCommandHandle(t,
		SessionSpec{
			Template: "worker",
			Command:  "mycmd",
			Provider: "legacy-provider",
			Resume:   sessionpkg.ProviderResume{SessionIDFlag: "--session-id"},
		},
		sessionpkg.ProviderResume{ResumeFlag: "--resume", SessionIDFlag: "--session-id"},
		"mycmd",
	)
	if err := store.SetMetadata(info.ID, "started_config_hash", "hash-abc"); err != nil {
		t.Fatalf("SetMetadata(started_config_hash): %v", err)
	}

	got, err := handle.startCommand(info.ID)
	if err != nil {
		t.Fatalf("startCommand: %v", err)
	}
	// Resume form (flag style), not the first-start `<cmd> --session-id <key>`.
	want := "mycmd --resume " + info.SessionKey
	if got != want {
		t.Fatalf("startCommand() = %q, want %q", got, want)
	}
}

// TestStartCommandFirstStartOffWithCreationComplete pins the other
// first-start-off branch: a present creation_complete_at also drops to the
// resume form.
func TestStartCommandFirstStartOffWithCreationComplete(t *testing.T) {
	handle, store, info := newStartCommandHandle(t,
		SessionSpec{
			Template: "worker",
			Command:  "mycmd",
			Provider: "legacy-provider",
			Resume:   sessionpkg.ProviderResume{SessionIDFlag: "--session-id"},
		},
		sessionpkg.ProviderResume{ResumeFlag: "--resume", SessionIDFlag: "--session-id"},
		"mycmd",
	)
	if err := store.SetMetadata(info.ID, "creation_complete_at", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("SetMetadata(creation_complete_at): %v", err)
	}

	got, err := handle.startCommand(info.ID)
	if err != nil {
		t.Fatalf("startCommand: %v", err)
	}
	want := "mycmd --resume " + info.SessionKey
	if got != want {
		t.Fatalf("startCommand() = %q, want %q", got, want)
	}
}

// TestStartCommandResumeUsesSpecOverrides pins that the resume branch layers the
// handle-spec resume overrides over the persisted Info before building the
// resume command — the exact-string resume flags the brief calls out.
func TestStartCommandResumeUsesSpecOverrides(t *testing.T) {
	handle, store, info := newStartCommandHandle(t,
		SessionSpec{
			Template: "worker",
			Command:  "spec-cmd",
			Provider: "legacy-provider",
			Resume:   sessionpkg.ProviderResume{ResumeFlag: "--spec-resume", SessionIDFlag: "--session-id"},
		},
		sessionpkg.ProviderResume{ResumeFlag: "--resume", SessionIDFlag: "--session-id"},
		"bead-cmd",
	)
	if err := store.SetMetadata(info.ID, "started_config_hash", "hash-abc"); err != nil {
		t.Fatalf("SetMetadata(started_config_hash): %v", err)
	}

	got, err := handle.startCommand(info.ID)
	if err != nil {
		t.Fatalf("startCommand: %v", err)
	}
	// Spec Command ("spec-cmd") and ResumeFlag ("--spec-resume") override the
	// persisted bead-cmd/--resume; SessionKey comes from the persisted bead.
	want := "spec-cmd --spec-resume " + info.SessionKey
	if got != want {
		t.Fatalf("startCommand() = %q, want %q", got, want)
	}
}
