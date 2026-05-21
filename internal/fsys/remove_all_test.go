package fsys

import (
	"os"
	"testing"
)

func TestRemoveAllRemovesNestedTree(t *testing.T) {
	fs := NewFake()
	fs.Dirs["/city"] = true
	fs.Dirs["/city/agents"] = true
	fs.Dirs["/city/agents/coder"] = true
	fs.Dirs["/city/agents/coder/nested"] = true
	fs.Files["/city/agents/coder/agent.toml"] = []byte("provider = \"claude\"\n")
	fs.Files["/city/agents/coder/nested/prompt.template.md"] = []byte("You are the coder.\n")

	if err := RemoveAll(fs, "/city/agents/coder"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	for _, path := range []string{
		"/city/agents/coder",
		"/city/agents/coder/nested",
		"/city/agents/coder/agent.toml",
		"/city/agents/coder/nested/prompt.template.md",
	} {
		if fs.Dirs[path] {
			t.Fatalf("directory %s remains after RemoveAll", path)
		}
		if _, ok := fs.Files[path]; ok {
			t.Fatalf("file %s remains after RemoveAll", path)
		}
	}
	if !fs.Dirs["/city/agents"] {
		t.Fatal("RemoveAll removed parent directory")
	}
}

func TestRemoveAllMissingPathIsNoop(t *testing.T) {
	fs := NewFake()

	if err := RemoveAll(fs, "/missing"); err != nil {
		t.Fatalf("RemoveAll missing path: %v", err)
	}
}

func TestRemoveAllDoesNotFollowSymlinks(t *testing.T) {
	fs := NewFake()
	fs.Symlinks["/city/agents/link"] = "/outside/target"
	fs.Files["/outside/target"] = []byte("keep me\n")

	if err := RemoveAll(fs, "/city/agents/link"); err != nil {
		t.Fatalf("RemoveAll symlink: %v", err)
	}
	if _, ok := fs.Symlinks["/city/agents/link"]; ok {
		t.Fatal("symlink remains after RemoveAll")
	}
	if got := string(fs.Files["/outside/target"]); got != "keep me\n" {
		t.Fatalf("target file = %q, want preserved", got)
	}
}

func TestRemoveAllReturnsUnexpectedStatError(t *testing.T) {
	fs := NewFake()
	fs.Errors["/city/agents/coder"] = os.ErrPermission

	if err := RemoveAll(fs, "/city/agents/coder"); !os.IsPermission(err) {
		t.Fatalf("RemoveAll error = %v, want permission", err)
	}
}
