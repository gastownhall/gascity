package sessionlog

import (
	"os"
	"path/filepath"
	"strings"
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

func TestFindZCodeSessionFileByIDIsExact(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	scoped := filepath.Join(root, "worker#1")
	if err := os.MkdirAll(scoped, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(dir, id, directory string) string {
		path := filepath.Join(dir, id+".json")
		body := `{"info":{"id":"` + id + `","directory":"` + filepath.ToSlash(directory) + `"},"messages":[]}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return path
	}
	wanted := write(scoped, "sess_wanted", workDir)
	other := write(scoped, "sess_other", workDir)

	// A newer sibling must not win an identity lookup.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(other, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if got := FindZCodeSessionFileByID([]string{root}, workDir, "sess_wanted"); got != wanted {
		t.Fatalf("FindZCodeSessionFileByID() = %q, want %q", got, wanted)
	}

	// An id from a different work dir never matches.
	if got := FindZCodeSessionFileByID([]string{root}, filepath.Join(t.TempDir(), "elsewhere"), "sess_wanted"); got != "" {
		t.Fatalf("cross-workdir match = %q, want empty", got)
	}
	// Path traversal is refused.
	for _, unsafe := range []string{"../escape", "a/b", ".."} {
		if got := FindZCodeSessionFileByID([]string{root}, workDir, unsafe); got != "" {
			t.Fatalf("unsafe id %q resolved to %q", unsafe, got)
		}
	}
}

// gc never learns zcode's provider session id, so the mirror has to be
// resolvable from the identity the session bead does hold.
func TestFindZCodeSessionFileByScopeSeparatesSameWorkDirSessions(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "shared-workdir")
	write := func(scope, id string) string {
		dir := filepath.Join(root, scope)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		path := filepath.Join(dir, id+".json")
		body := `{"info":{"id":"` + id + `","directory":"` + filepath.ToSlash(workDir) + `"},"messages":[]}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return path
	}
	// Two pooled workers in one work dir.
	first := write("s-gc-1#1", "sess_first")
	second := write("s-gc-2#1", "sess_second")

	if got := FindZCodeSessionFileByScope([]string{root}, workDir, "s-gc-1", "1"); got != first {
		t.Fatalf("s-gc-1 resolved %q, want %q", got, first)
	}
	if got := FindZCodeSessionFileByScope([]string{root}, workDir, "s-gc-2", "1"); got != second {
		t.Fatalf("s-gc-2 resolved %q, want %q", got, second)
	}

	// A dead session's mirror in a reused work dir must not surface for a fresh
	// session — different epoch, different scope.
	if got := FindZCodeSessionFileByScope([]string{root}, workDir, "s-gc-1", "2"); got != "" {
		t.Fatalf("fresh conversation resolved a superseded mirror: %q", got)
	}
	// A canceled-boot placeholder is the only mirror a first-turn failure
	// leaves behind (no session id was ever assigned), so when no real mirror
	// exists it must stay resolvable rather than the scope reading as empty.
	pending := write("s-gc-3#1", "pending-s-gc-3_1")
	if got := FindZCodeSessionFileByScope([]string{root}, workDir, "s-gc-3", "1"); got != pending {
		t.Fatalf("first-turn placeholder not surfaced as fallback: got %q, want %q", got, pending)
	}
	// A name needing sanitization resolves the same way the adapter wrote it.
	slashed := write("gascity_gc.worker-9#1", "sess_slashed")
	if got := FindZCodeSessionFileByScope([]string{root}, workDir, "gascity/gc.worker-9", "1"); got != slashed {
		t.Fatalf("sanitized scope resolved %q, want %q", got, slashed)
	}
}

// The reset contract reads the PRE-reset transcript after the reset is issued,
// so a rotated conversation has to stay resolvable by its own scope from the
// archive while the live tree carries only the current one. Deleting it instead
// made both scopes resolve to the same file — the conversation then looked
// preserved, and whether it did was a race against the pane restart.
func TestFindZCodeSessionFileByScopeResolvesArchivedConversations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	live := filepath.Join(home, "live")
	archive := filepath.Join(home, "state", "gascity", "zcode", "archived-transcripts")
	workDir := filepath.Join(t.TempDir(), "project")

	write := func(root, scope, id string) string {
		dir := filepath.Join(root, scope)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		path := filepath.Join(dir, id+".json")
		body := `{"info":{"id":"` + id + `","directory":"` + filepath.ToSlash(workDir) + `"},"messages":[]}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return path
	}
	// Post-reset layout: epoch 1 archived, epoch 2 live.
	archived := write(archive, "probe#1", "sess_before")
	current := write(live, "probe#2", "sess_after")

	if got := FindZCodeSessionFileByScope([]string{live}, workDir, "probe", "1"); got != archived {
		t.Fatalf("pre-reset scope resolved %q, want the archived %q", got, archived)
	}
	if got := FindZCodeSessionFileByScope([]string{live}, workDir, "probe", "2"); got != current {
		t.Fatalf("post-reset scope resolved %q, want the live %q", got, current)
	}
	// The two scopes must not collapse onto one file — that is exactly what
	// made a reset look like a preserved conversation.
	if archived == current {
		t.Fatal("pre- and post-reset scopes resolved to the same transcript")
	}
	if !strings.Contains(archived, "archived-transcripts") {
		t.Fatalf("archived path %q is not under the archive root", archived)
	}
}

// TestFindZCodeSessionFileByScopePrefersRealMirrorOverPending pins the
// placeholder-fallback contract: a first-turn failure leaves only the
// canceled-boot placeholder (no session id ever existed), so it must stay
// resolvable as a last resort, while any real mirror in the same scope wins
// outright because it adopts the placeholder on the first successful turn. The
// placeholder embeds its work dir just like a real mirror, so the work-dir
// filter still isolates it.
func TestFindZCodeSessionFileByScopePrefersRealMirrorOverPending(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "wd")
	write := func(scope, id, directory string) string {
		dir := filepath.Join(root, scope)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		path := filepath.Join(dir, id+".json")
		body := `{"info":{"id":"` + id + `","directory":"` + filepath.ToSlash(directory) + `"},"messages":[]}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return path
	}

	// First-turn failure: only the placeholder exists.
	pending := write("s-boot#1", "pending-s-boot_1", workDir)
	if got := FindZCodeSessionFileByScope([]string{root}, workDir, "s-boot", "1"); got != pending {
		t.Fatalf("placeholder-only scope resolved %q, want the placeholder %q", got, pending)
	}

	// Once a real mirror establishes in the same scope it wins outright.
	realMirror := write("s-boot#1", "sess_real", workDir)
	if got := FindZCodeSessionFileByScope([]string{root}, workDir, "s-boot", "1"); got != realMirror {
		t.Fatalf("real mirror not preferred over placeholder: got %q, want %q", got, realMirror)
	}

	// A placeholder whose embedded directory belongs to another work dir is
	// filtered out exactly like a real mirror would be.
	write("s-other#1", "pending-s-other_1", filepath.Join(t.TempDir(), "elsewhere"))
	if got := FindZCodeSessionFileByScope([]string{root}, workDir, "s-other", "1"); got != "" {
		t.Fatalf("placeholder from another work dir surfaced: %q", got)
	}
}
