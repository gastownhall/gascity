package sessionlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadMimoCodeFileNormalizesExportedMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_export.json")
	body := `{
  "info": {
    "id": "ses_mimocode_phase1",
    "directory": "/tmp/gascity/phase1/mimocode"
  },
  "messages": [
    {
      "info": {"id":"msg_user_1","sessionID":"ses_mimocode_phase1","role":"user","time":{"created":1770000000000},"agent":"build","model":{"providerID":"mimo","modelID":"mimo-auto"}},
      "parts": [{"id":"part_user_1","sessionID":"ses_mimocode_phase1","messageID":"msg_user_1","type":"text","text":"hello mimocode"}]
    },
    {
      "info": {"id":"msg_assistant_1","sessionID":"ses_mimocode_phase1","role":"assistant","time":{"created":1770000001000},"parentID":"msg_user_1","providerID":"mimo","modelID":"mimo-auto","mode":"build","path":{"cwd":"/tmp/gascity/phase1/mimocode","root":"/tmp/gascity/phase1/mimocode"},"cost":0,"tokens":{"input":1,"output":1,"reasoning":0,"cache":{"read":0,"write":0}}},
      "parts": [{"id":"part_assistant_1","sessionID":"ses_mimocode_phase1","messageID":"msg_assistant_1","type":"text","text":"hello from MiMo through MiMo Code"}]
    }
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write export fixture: %v", err)
	}

	sess, err := ReadMimoCodeFile(path, 0)
	if err != nil {
		t.Fatalf("ReadMimoCodeFile: %v", err)
	}
	if sess.ID != "ses_mimocode_phase1" {
		t.Fatalf("ID = %q, want ses_mimocode_phase1", sess.ID)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(sess.Messages))
	}
	if got := sess.Messages[0].TextContent(); got != "hello mimocode" {
		t.Fatalf("user text = %q", got)
	}
	if got := sess.Messages[1].TextContent(); got != "hello from MiMo through MiMo Code" {
		t.Fatalf("assistant text = %q", got)
	}
}

func TestFindMimoCodeSessionFileMatchesExportDirectory(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	oldPath := filepath.Join(root, "old.json")
	newPath := filepath.Join(root, "nested", "new.json")
	for _, item := range []struct {
		path string
		id   string
	}{
		{oldPath, "old"},
		{newPath, "new"},
	} {
		body := `{"info":{"id":"` + item.id + `","directory":"` + filepath.ToSlash(workDir) + `"},"messages":[]}`
		if err := os.WriteFile(item.path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", item.path, err)
		}
	}

	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(newPath, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	got := FindMimoCodeSessionFile([]string{root}, workDir)
	if got != newPath {
		t.Fatalf("FindMimoCodeSessionFile() = %q, want %q", got, newPath)
	}
}

func TestFindMimoCodeSessionFileIgnoresOtherDirectories(t *testing.T) {
	root := t.TempDir()
	body := `{"info":{"id":"other","directory":"/somewhere/else"},"messages":[]}`
	if err := os.WriteFile(filepath.Join(root, "other.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if got := FindMimoCodeSessionFile([]string{root}, filepath.Join(t.TempDir(), "project")); got != "" {
		t.Fatalf("FindMimoCodeSessionFile() = %q, want empty", got)
	}
}

func TestProviderFamilyMimoCode(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{provider: "mimocode", want: "mimocode"},
		{provider: "my-mimocode", want: "mimocode"},
		{provider: "MimoCode", want: "mimocode"},
		{provider: "opencode", want: "opencode"},
	}
	for _, tt := range tests {
		if got := ProviderFamily(tt.provider); got != tt.want {
			t.Errorf("ProviderFamily(%q) = %q, want %q", tt.provider, got, tt.want)
		}
	}
}

func TestReadProviderFileRoutesMimoCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ses_route.json")
	body := `{"info":{"id":"ses_route","directory":"/tmp/route"},"messages":[{"info":{"id":"msg_1","sessionID":"ses_route","role":"user","time":{"created":1770000000000}},"parts":[{"id":"part_1","type":"text","text":"route me"}]}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sess, err := ReadProviderFile("mimocode", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile(mimocode): %v", err)
	}
	if sess.ID != "ses_route" {
		t.Fatalf("ID = %q, want ses_route", sess.ID)
	}
	if len(sess.Messages) != 1 || sess.Messages[0].TextContent() != "route me" {
		t.Fatalf("messages = %#v, want one entry with text %q", sess.Messages, "route me")
	}
}

func TestDefaultMimoCodeSearchPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	paths := DefaultMimoCodeSearchPaths()
	if len(paths) != 1 {
		t.Fatalf("DefaultMimoCodeSearchPaths() = %v, want one entry", paths)
	}
	want := filepath.Join(home, ".local", "share", "gascity", "mimocode-transcripts")
	if paths[0] != want {
		t.Fatalf("DefaultMimoCodeSearchPaths()[0] = %q, want %q", paths[0], want)
	}
}
