package sessionlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadZCodeFileNormalizesMirroredTurns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sess_zcode_phase1.json")
	body := `{
  "info": {
    "id": "sess_zcode_phase1",
    "directory": "/tmp/gascity/phase1/zcode"
  },
  "messages": [
    {
      "info": {"id":"msg_user_1","sessionID":"sess_zcode_phase1","role":"user","parentID":"","time":{"created":1770000000000}},
      "parts": [{"id":"part_msg_user_1","type":"text","text":"hello zcode"}]
    },
    {
      "info": {"id":"msg_assistant_1","sessionID":"sess_zcode_phase1","role":"assistant","parentID":"msg_user_1","time":{"created":1770000001000},"usage":{"inputTokens":11721,"outputTokens":6,"totalTokens":11727},"projection":{"turnCount":1,"totalTokenCount":11727}},
      "parts": [{"id":"part_msg_assistant_1","type":"text","text":"hello from GLM through ZCode"}]
    }
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write mirror fixture: %v", err)
	}

	sess, err := ReadZCodeFile(path, 0)
	if err != nil {
		t.Fatalf("ReadZCodeFile: %v", err)
	}
	if sess.ID != "sess_zcode_phase1" {
		t.Fatalf("ID = %q, want sess_zcode_phase1", sess.ID)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(sess.Messages))
	}
	if got := sess.Messages[0].TextContent(); got != "hello zcode" {
		t.Fatalf("user text = %q", got)
	}
	if got := sess.Messages[1].TextContent(); got != "hello from GLM through ZCode" {
		t.Fatalf("assistant text = %q", got)
	}
	// The adapter records usage for provenance; no extractor consumes it, and
	// carrying it must not perturb normalization.
	if got := sess.Messages[1].ParentUUID; got != "msg_user_1" {
		t.Fatalf("assistant parent = %q, want msg_user_1", got)
	}
}

func TestFindZCodeSessionFileMatchesMirrorDirectory(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	oldPath := filepath.Join(root, "sess_old.json")
	newPath := filepath.Join(root, "nested", "sess_new.json")
	for _, item := range []struct {
		path string
		id   string
	}{
		{oldPath, "sess_old"},
		{newPath, "sess_new"},
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

	if got := FindZCodeSessionFile([]string{root}, workDir); got != newPath {
		t.Fatalf("FindZCodeSessionFile() = %q, want %q", got, newPath)
	}
}

func TestFindZCodeSessionFileIgnoresOtherDirectories(t *testing.T) {
	root := t.TempDir()
	body := `{"info":{"id":"sess_other","directory":"/somewhere/else"},"messages":[]}`
	if err := os.WriteFile(filepath.Join(root, "sess_other.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if got := FindZCodeSessionFile([]string{root}, filepath.Join(t.TempDir(), "project")); got != "" {
		t.Fatalf("FindZCodeSessionFile() = %q, want empty", got)
	}
}

func TestProviderFamilyZCode(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{provider: "zcode", want: "zcode"},
		{provider: "my-zcode", want: "zcode"},
		{provider: "ZCode", want: "zcode"},
		{provider: "zcode/tmux-cli", want: "zcode"},
		{provider: "opencode", want: "opencode"},
		{provider: "mimocode", want: "mimocode"},
	}
	for _, tt := range tests {
		if got := ProviderFamily(tt.provider); got != tt.want {
			t.Errorf("ProviderFamily(%q) = %q, want %q", tt.provider, got, tt.want)
		}
	}
}

func TestReadProviderFileRoutesZCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sess_route.json")
	body := `{"info":{"id":"sess_route","directory":"/tmp/route"},"messages":[{"info":{"id":"msg_user_1","sessionID":"sess_route","role":"user","time":{"created":1770000000000}},"parts":[{"id":"part_msg_user_1","type":"text","text":"route me"}]}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sess, err := ReadProviderFile("zcode", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile(zcode): %v", err)
	}
	if sess.ID != "sess_route" {
		t.Fatalf("ID = %q, want sess_route", sess.ID)
	}
	if len(sess.Messages) != 1 || sess.Messages[0].TextContent() != "route me" {
		t.Fatalf("messages = %#v, want one entry with text %q", sess.Messages, "route me")
	}
}

func TestFindSessionFileForProviderRoutesZCode(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	path := filepath.Join(root, "sess_routed.json")
	body := `{"info":{"id":"sess_routed","directory":"` + filepath.ToSlash(workDir) + `"},"messages":[]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if got := FindSessionFileForProvider([]string{root}, "zcode", workDir); got != path {
		t.Fatalf("FindSessionFileForProvider(zcode) = %q, want %q", got, path)
	}
}

func TestDefaultZCodeSearchPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	paths := DefaultZCodeSearchPaths()
	if len(paths) != 1 {
		t.Fatalf("DefaultZCodeSearchPaths() = %v, want one entry", paths)
	}
	want := filepath.Join(home, ".local", "share", "gascity", "zcode-transcripts")
	if paths[0] != want {
		t.Fatalf("DefaultZCodeSearchPaths()[0] = %q, want %q", paths[0], want)
	}
}
