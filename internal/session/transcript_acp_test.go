package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

type acpDiscoveryFixture struct {
	city       string
	workDir    string
	searchBase string
	store      *beads.MemStore
	mgr        *Manager
}

func newACPDiscoveryFixture(t *testing.T, withCity bool) acpDiscoveryFixture {
	t.Helper()
	f := acpDiscoveryFixture{
		city:       t.TempDir(),
		workDir:    t.TempDir(),
		searchBase: t.TempDir(),
		store:      beads.NewMemStore(),
	}
	var opts []ManagerOption
	if withCity {
		opts = append(opts, WithCityPath(f.city))
	}
	f.mgr = NewManagerWithOptions(f.store, runtime.NewFake(), opts...)
	return f
}

func (f acpDiscoveryFixture) create(t *testing.T, provider, transport string, resume ProviderResume) Info {
	t.Helper()
	info, err := f.mgr.CreateSession(context.Background(), CreateOptions{
		Template:  "helper",
		Command:   provider,
		WorkDir:   f.workDir,
		Provider:  provider,
		Transport: transport,
		Resume:    resume,
		ExtraMeta: map[string]string{"session_origin": "manual"},
		BeadOnly:  true,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return info
}

// writeCapture writes a capture file for id at epoch and returns its path.
func (f acpDiscoveryFixture) writeCapture(t *testing.T, id, epoch string) string {
	t.Helper()
	path, err := citylayout.ACPTranscriptPath(f.city, id, epoch)
	if err != nil {
		t.Fatalf("ACPTranscriptPath: %v", err)
	}
	writeTestFile(t, path, `{"gc_acp_capture":1,"ts":"2026-09-25T10:00:00Z","session_id":"`+id+`"}`+"\n")
	return path
}

// writeWorkDirTranscript writes a claude-layout transcript the workdir
// fallback would pick up, and returns its path.
func (f acpDiscoveryFixture) writeWorkDirTranscript(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(f.searchBase, sessionlog.ProjectSlug(f.workDir), name)
	writeTestFile(t, path, "{}\n")
	return path
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestTranscriptPathACPCaptureBeatsWorkDirFallback(t *testing.T) {
	f := newACPDiscoveryFixture(t, true)
	info := f.create(t, "claude", "acp", ProviderResume{})
	capture := f.writeCapture(t, info.ID, "1")
	f.writeWorkDirTranscript(t, "latest.jsonl")

	path, lookup, err := f.mgr.TranscriptPathClassified(info.ID, []string{f.searchBase})
	if err != nil {
		t.Fatalf("TranscriptPathClassified: %v", err)
	}
	if path != capture || lookup != TranscriptFound {
		t.Fatalf("TranscriptPathClassified = %q, %v; want the capture %q", path, lookup, capture)
	}
}

func TestTranscriptPathKeyedNativeBeatsACPCapture(t *testing.T) {
	f := newACPDiscoveryFixture(t, true)
	info := f.create(t, "claude", "acp", ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", SessionIDFlag: "--session-id"})
	if info.SessionKey == "" {
		t.Fatal("session has no session key")
	}
	f.writeCapture(t, info.ID, "1")
	keyed := f.writeWorkDirTranscript(t, info.SessionKey+".jsonl")

	path, err := f.mgr.TranscriptPath(info.ID, []string{f.searchBase})
	if err != nil {
		t.Fatalf("TranscriptPath: %v", err)
	}
	if path != keyed {
		t.Fatalf("TranscriptPath = %q, want the keyed native transcript %q", path, keyed)
	}
}

func TestTranscriptPathACPWithoutCaptureFallsThrough(t *testing.T) {
	f := newACPDiscoveryFixture(t, true)
	info := f.create(t, "claude", "acp", ProviderResume{})
	fallback := f.writeWorkDirTranscript(t, "latest.jsonl")

	path, err := f.mgr.TranscriptPath(info.ID, []string{f.searchBase})
	if err != nil {
		t.Fatalf("TranscriptPath: %v", err)
	}
	if path != fallback {
		t.Fatalf("TranscriptPath = %q, want the workdir fallback %q", path, fallback)
	}
}

func TestTranscriptPathACPCaptureFollowsContinuationEpoch(t *testing.T) {
	f := newACPDiscoveryFixture(t, true)
	info := f.create(t, "unreal", "acp", ProviderResume{})
	first := f.writeCapture(t, info.ID, "1")
	third := f.writeCapture(t, info.ID, "3")

	for _, tt := range []struct {
		epoch string
		want  string
	}{
		{"", first}, // no epoch recorded yet: the default epoch
		{"bogus", first},
		{"3", third},
		{"2", ""}, // reset committed, agent not restarted: no capture yet
	} {
		if err := f.store.SetMetadata(info.ID, "continuation_epoch", tt.epoch); err != nil {
			t.Fatalf("SetMetadata: %v", err)
		}
		path, err := f.mgr.TranscriptPath(info.ID, []string{f.searchBase})
		if err != nil {
			t.Fatalf("TranscriptPath: %v", err)
		}
		if path != tt.want {
			t.Errorf("epoch %q: TranscriptPath = %q, want %q", tt.epoch, path, tt.want)
		}
	}
}

func TestTranscriptPathACPCaptureWithoutWorkDir(t *testing.T) {
	f := newACPDiscoveryFixture(t, true)
	info := f.create(t, "unreal", "acp", ProviderResume{})
	capture := f.writeCapture(t, info.ID, "1")
	if err := f.store.SetMetadata(info.ID, "work_dir", ""); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	path, lookup, err := f.mgr.TranscriptPathClassified(info.ID, nil)
	if err != nil {
		t.Fatalf("TranscriptPathClassified: %v", err)
	}
	if path != capture || lookup != TranscriptFound {
		t.Fatalf("TranscriptPathClassified = %q, %v; want the capture", path, lookup)
	}
}

func TestTranscriptPathIgnoresCaptureForNonACPOrCitylessManager(t *testing.T) {
	t.Run("tmux transport", func(t *testing.T) {
		f := newACPDiscoveryFixture(t, true)
		info := f.create(t, "claude", "", ProviderResume{})
		f.writeCapture(t, info.ID, "1")
		fallback := f.writeWorkDirTranscript(t, "latest.jsonl")
		path, err := f.mgr.TranscriptPath(info.ID, []string{f.searchBase})
		if err != nil {
			t.Fatalf("TranscriptPath: %v", err)
		}
		if path != fallback {
			t.Fatalf("TranscriptPath = %q, want the workdir fallback %q", path, fallback)
		}
	})
	t.Run("no city path", func(t *testing.T) {
		f := newACPDiscoveryFixture(t, false)
		info := f.create(t, "unreal", "acp", ProviderResume{})
		f.writeCapture(t, info.ID, "1")
		path, err := f.mgr.TranscriptPath(info.ID, []string{f.searchBase})
		if err != nil {
			t.Fatalf("TranscriptPath: %v", err)
		}
		if path != "" {
			t.Fatalf("TranscriptPath = %q, want none without a city", path)
		}
	})
}
