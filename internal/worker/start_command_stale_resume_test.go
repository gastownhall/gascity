package worker

import (
	"strings"
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func stubResumeTranscriptProbe(t *testing.T, present, probeable bool) {
	t.Helper()
	orig := resumeTranscriptProbe
	resumeTranscriptProbe = func(string, string, string) (bool, bool) { return present, probeable }
	t.Cleanup(func() { resumeTranscriptProbe = orig })
}

// newKilledResumeHandle models a seat: a claude session that was
// KILLED rather than slept, so it still carries its session_key with
// continuation_reset_pending=true and reset_committed_at empty, and has been
// started before (started_config_hash set), so the next start is a resume.
func newKilledResumeHandle(t *testing.T) (*SessionHandle, sessionpkg.Info, func(key string) string) {
	t.Helper()
	resume := sessionpkg.ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", SessionIDFlag: "--session-id"}
	handle, store, info := newStartCommandHandle(t,
		SessionSpec{Template: "worker", Command: "claude", Provider: "claude", Resume: resume},
		resume,
		"claude",
	)
	if err := store.SetMetadataBatch(info.ID, map[string]string{
		"provider":                   "claude",
		"started_config_hash":        "hash-abc",
		"continuation_reset_pending": "true",
		"reset_committed_at":         "",
		"sleep_reason":               "killed",
	}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	if info.SessionKey == "" {
		t.Fatal("fixture SessionKey is empty")
	}
	meta := func(key string) string {
		b, err := store.Get(info.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return b.Metadata[key]
	}
	return handle, info, meta
}

func TestStartCommandStaleResumeKeyStartsFreshAndClearsKey(t *testing.T) {
	stubResumeTranscriptProbe(t, false, true)
	handle, info, meta := newKilledResumeHandle(t)
	oldKey := info.SessionKey

	got, err := handle.startCommand(info.ID)
	if err != nil {
		t.Fatalf("startCommand: %v", err)
	}
	if strings.Contains(got, "--resume") || strings.Contains(got, oldKey) {
		t.Fatalf("startCommand() = %q, must not resume the dead key %s", got, oldKey)
	}
	newKey := meta("session_key")
	if newKey == "" || newKey == oldKey {
		t.Fatalf("session_key after stale clear = %q, want a freshly minted key (old %q)", newKey, oldKey)
	}
	if want := "claude --session-id " + newKey; got != want {
		t.Fatalf("startCommand() = %q, want %q", got, want)
	}
	if h := meta("started_config_hash"); h != "" {
		t.Fatalf("started_config_hash = %q, want cleared so the launch counts as a first start", h)
	}
}

func TestStartCommandPresentTranscriptStillResumes(t *testing.T) {
	stubResumeTranscriptProbe(t, true, true)
	handle, info, meta := newKilledResumeHandle(t)

	got, err := handle.startCommand(info.ID)
	if err != nil {
		t.Fatalf("startCommand: %v", err)
	}
	if want := "claude --resume " + info.SessionKey; got != want {
		t.Fatalf("startCommand() = %q, want %q", got, want)
	}
	if k := meta("session_key"); k != info.SessionKey {
		t.Fatalf("session_key = %q, want unchanged %q", k, info.SessionKey)
	}
}

func TestStartCommandUnprobeableProviderKeepsResume(t *testing.T) {
	stubResumeTranscriptProbe(t, false, false)
	handle, info, _ := newKilledResumeHandle(t)

	got, err := handle.startCommand(info.ID)
	if err != nil {
		t.Fatalf("startCommand: %v", err)
	}
	if want := "claude --resume " + info.SessionKey; got != want {
		t.Fatalf("startCommand() = %q, want %q", got, want)
	}
}
