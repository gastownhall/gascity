package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/transcriptmeta"
	"github.com/gastownhall/gascity/pkg/eventexport"
)

// claudeKeyedFixture stands up a claude session whose transcript is resolvable by
// a stable per-session key, mirroring production where the key lands via the
// provider hook. It returns the started handle, its session bead id, and the
// keyed transcript path.
func claudeKeyedFixture(t *testing.T) (*SessionHandle, string, string) {
	t.Helper()
	const (
		workDir    = "/tmp/gascity/phase1/claude"
		slug       = "-tmp-gascity-phase1-claude" // a claudeProjectSlugCandidates(workDir) entry
		sessionKey = "keyed-session-xyz"
	)
	root := t.TempDir()
	dir := filepath.Join(root, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	transcript := filepath.Join(dir, sessionKey+".jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"user","sessionId":"`+sessionKey+`"}`+"\n"), 0o644); err != nil {
		t.Fatalf("seed keyed transcript: %v", err)
	}

	handle, _, _, manager := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
	})
	handle.adapter.SearchPaths = []string{root}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := handle.currentSessionID()
	if id == "" {
		t.Fatal("currentSessionID is empty after Start")
	}
	// Stamp the durable session key (production stamps it from the provider hook).
	if err := manager.PersistSessionKey(id, sessionKey); err != nil {
		t.Fatalf("PersistSessionKey: %v", err)
	}
	return handle, id, transcript
}

// TestSessionHandleWritesKeyedTranscriptSidecar is the join-correctness check:
// for a keyed provider the sidecar lands on the session's own transcript and
// carries the session BEAD id — the value the event stream emits as session_id,
// not the provider resume key.
func TestSessionHandleWritesKeyedTranscriptSidecar(t *testing.T) {
	transcriptmeta.SetEnabled(true)
	t.Cleanup(func() { transcriptmeta.SetEnabled(false) })

	handle, id, transcript := claudeKeyedFixture(t)

	handle.writeTranscriptSessionMeta()

	got, err := os.ReadFile(transcript + transcriptmeta.Suffix)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if strings.TrimSpace(string(got)) != id {
		t.Fatalf("sidecar = %q, want session bead id %q", strings.TrimSpace(string(got)), id)
	}
}

// TestSessionHandleSidecarIDIsExportableRef locks the cross-rail join contract:
// the id the worker writes to the sidecar is the SAME value the redacted event
// exporter emits as session_id only if it survives eventexport's opaque-ref gate
// unchanged. If a session bead id ever gained uppercase or exceeded the length
// bound, the sidecar would still carry it but the export would drop/rewrite it,
// silently breaking the sidecar-to-event-stream correlation with no other guard.
// This ties the worker-written value to that gate so the join cannot drift apart.
func TestSessionHandleSidecarIDIsExportableRef(t *testing.T) {
	transcriptmeta.SetEnabled(true)
	t.Cleanup(func() { transcriptmeta.SetEnabled(false) })

	handle, id, transcript := claudeKeyedFixture(t)

	handle.writeTranscriptSessionMeta()

	got, err := os.ReadFile(transcript + transcriptmeta.Suffix)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	sidecarID := strings.TrimSpace(string(got))
	if sidecarID != id {
		t.Fatalf("sidecar = %q, want session bead id %q", sidecarID, id)
	}
	if !eventexport.IsOpaqueRef(sidecarID) {
		t.Fatalf("sidecar id %q is not an exportable opaque ref; the redacted export "+
			"would drop or rewrite session_id and the sidecar-to-event-stream join would break", sidecarID)
	}
}

// TestSessionHandleSidecarDisabledByDefault confirms the inert-by-default
// guarantee: with the gate off, even a fully keyed session writes nothing.
func TestSessionHandleSidecarDisabledByDefault(t *testing.T) {
	// Gate left at its default (off).
	handle, _, transcript := claudeKeyedFixture(t)

	handle.writeTranscriptSessionMeta()

	if _, err := os.Stat(transcript + transcriptmeta.Suffix); !os.IsNotExist(err) {
		t.Fatalf("expected no sidecar when disabled, stat err = %v", err)
	}
}

// TestSessionHandleWritesCodexSidecarByID covers the codex case the keyed gate
// must include: codex carries a session_key (its rollout uuid, captured by the
// SessionStart hook) and FindCodexSessionFileByID resolves the exact rollout by
// that id's filename suffix — so the sidecar lands on the right rollout, even
// though gc's general discovery treats codex as workdir-resolved.
func TestSessionHandleWritesCodexSidecarByID(t *testing.T) {
	transcriptmeta.SetEnabled(true)
	t.Cleanup(func() { transcriptmeta.SetEnabled(false) })

	const (
		workDir = "/work/codex-by-id"
		uuid    = "019e9966-bbbb-7000-8000-26a2dd7e15b3" // synthetic; never collides with a real rollout
	)
	root := t.TempDir()
	// Codex rollout named "rollout-<localtime>-<uuid>.jsonl" under YYYY/MM/DD,
	// dated ~now (within the [CreatedAt±1day] keyed window), with session_meta
	// cwd == workDir (FindCodexSessionFileByID confirms cwd before returning).
	now := time.Now()
	local := now.In(time.Local)
	dir := filepath.Join(root, local.Format("2006"), local.Format("01"), local.Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(dir, "rollout-"+local.Format("2006-01-02T15-04-05")+"-"+uuid+".jsonl")
	meta := fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"timestamp":%q,"cwd":%q,"originator":"codex-tui","cli_version":"0.121.0","source":"cli","model_provider":"openai"}}`+"\n",
		now.UTC().Format(time.RFC3339Nano), uuid, now.UTC().Format(time.RFC3339Nano), workDir)
	if err := os.WriteFile(rollout, []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	handle, _, _, manager := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileCodexTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "codex",
		WorkDir:  workDir,
		Provider: "codex",
	})
	handle.adapter.SearchPaths = []string{root}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := handle.currentSessionID()
	// Stamp the codex session_key (production stamps it from the SessionStart hook).
	if err := manager.PersistSessionKey(id, uuid); err != nil {
		t.Fatalf("PersistSessionKey: %v", err)
	}

	handle.writeTranscriptSessionMeta()

	got, err := os.ReadFile(rollout + transcriptmeta.Suffix)
	if err != nil {
		t.Fatalf("read codex sidecar: %v", err)
	}
	if strings.TrimSpace(string(got)) != id {
		t.Fatalf("codex sidecar = %q, want session bead id %q", strings.TrimSpace(string(got)), id)
	}
}

// TestSessionHandleSkipsSidecarForWorkdirOnlyProvider is the HIGH-finding guard:
// gemini (like opencode/mimocode) has no 1:1 by-id transcript lookup, so even
// with a session key present KeyedTranscriptPath returns "" and no sidecar is
// written. This prevents one session's id from being stamped onto another
// session's transcript via the ambiguous workdir/mtime fallback. (Codex, which
// DOES have a by-id lookup, is covered — see TestSessionHandleWritesCodexSidecarByID.)
func TestSessionHandleSkipsSidecarForWorkdirOnlyProvider(t *testing.T) {
	transcriptmeta.SetEnabled(true)
	t.Cleanup(func() { transcriptmeta.SetEnabled(false) })

	root := t.TempDir()
	handle, _, _, manager := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileGeminiTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "gemini",
		WorkDir:  "/tmp/gascity/phase1/gemini",
		Provider: "gemini",
	})
	handle.adapter.SearchPaths = []string{root}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := handle.currentSessionID()
	// Even WITH a key, gemini is workdir-only, so there is no safe keyed path.
	if err := manager.PersistSessionKey(id, "some-key"); err != nil {
		t.Fatalf("PersistSessionKey: %v", err)
	}

	if path, err := manager.KeyedTranscriptPath(id, []string{root}); err != nil || path != "" {
		t.Fatalf("KeyedTranscriptPath(gemini) = %q, %v; want \"\", nil", path, err)
	}

	handle.writeTranscriptSessionMeta()

	// No sidecar anywhere under root.
	var found []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, transcriptmeta.Suffix) {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Fatalf("workdir-only provider wrote sidecar(s): %v", found)
	}
}
