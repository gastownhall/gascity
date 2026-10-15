package tmuxtest

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestConfigureProcessEnvIsolatesTmuxSocketRoot(t *testing.T) {
	socketRoot := t.TempDir()
	t.Setenv(tmuxEnv, "/tmp/tmux-parent/default,1,0")
	t.Setenv(tmuxPaneEnv, "%42")
	t.Setenv(tmuxTmpEnv, "/tmp/parent-tmux")

	if err := ConfigureProcessEnv(socketRoot); err != nil {
		t.Fatalf("ConfigureProcessEnv(): %v", err)
	}

	if value, ok := os.LookupEnv(tmuxEnv); ok {
		t.Fatalf("%s survived with value %q", tmuxEnv, value)
	}
	if value, ok := os.LookupEnv(tmuxPaneEnv); ok {
		t.Fatalf("%s survived with value %q", tmuxPaneEnv, value)
	}
	if value := os.Getenv(tmuxTmpEnv); value != socketRoot {
		t.Fatalf("%s = %q, want %q", tmuxTmpEnv, value, socketRoot)
	}
	if info, err := os.Stat(socketRoot); err != nil {
		t.Fatalf("stat socket root: %v", err)
	} else if !info.IsDir() {
		t.Fatalf("socket root is not a directory")
	}
}

func TestListTestSocketPathsSkipsLiveSiblingRoots(t *testing.T) {
	tmp := t.TempDir()
	scope := filepath.Join(tmp, "owned-scope")
	if err := os.Mkdir(scope, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(SocketParentRootEnv, scope)
	currentRun := filepath.Join(scope, "gc-integration-current")
	t.Setenv("TMPDIR", currentRun)
	currentRoot := filepath.Join(currentRun, "tmux")
	staleRoot := filepath.Join(scope, "gc-integration-stale", "tmux")
	liveRoot := filepath.Join(scope, "gc-integration-live", "tmux")
	otherRoot := filepath.Join(scope, "not-gc", "tmux")
	t.Setenv(tmuxTmpEnv, currentRoot)

	uid := strconv.Itoa(os.Getuid())
	currentSocket := filepath.Join(currentRoot, "tmux-"+uid, "gctest-current")
	staleSocket := filepath.Join(staleRoot, "tmux-"+uid, "gctest-stale")
	liveSocket := filepath.Join(liveRoot, "tmux-"+uid, "gctest-live")
	otherSocket := filepath.Join(otherRoot, "tmux-"+uid, "gctest-other")
	for _, path := range []string{currentSocket, staleSocket, liveSocket, otherSocket} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}
	staleTime := time.Now().Add(-tmuxSiblingSocketStaleAfter - time.Minute)
	for _, socketPath := range []string{staleSocket, liveSocket} {
		if err := os.Chtimes(socketPath, staleTime, staleTime); err != nil {
			t.Fatalf("Chtimes(%s): %v", socketPath, err)
		}
	}
	liveSentinel, err := HoldAliveSentinel(filepath.Dir(liveRoot))
	if err != nil {
		t.Fatalf("HoldAliveSentinel(%s): %v", filepath.Dir(liveRoot), err)
	}
	t.Cleanup(func() {
		if err := liveSentinel.Close(); err != nil {
			t.Errorf("closing live sibling sentinel: %v", err)
		}
	})

	got := listTestSocketPaths()

	if !slices.Contains(got, currentSocket) {
		t.Fatalf("listTestSocketPaths() missing current socket %s in %v", currentSocket, got)
	}
	if !slices.Contains(got, staleSocket) {
		t.Fatalf("listTestSocketPaths() missing stale socket %s in %v", staleSocket, got)
	}
	if slices.Contains(got, liveSocket) {
		t.Fatalf("listTestSocketPaths() included live sibling socket %s in %v", liveSocket, got)
	}
	if slices.Contains(got, otherSocket) {
		t.Fatalf("listTestSocketPaths() included unrelated socket %s in %v", otherSocket, got)
	}
}

func TestListTestSocketPathsRejectsActiveRootOutsideConfiguredScope(t *testing.T) {
	parent := t.TempDir()
	scope := filepath.Join(parent, "owned-scope")
	if err := os.Mkdir(scope, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(parent, "gct-4242-foreign", "tmux")
	foreignSocket := filepath.Join(foreign, "tmux-"+strconv.Itoa(os.Getuid()), "gctest-foreign")
	if err := os.MkdirAll(filepath.Dir(foreignSocket), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreignSocket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-tmuxSiblingSocketStaleAfter - time.Minute)
	if err := os.Chtimes(foreignSocket, old, old); err != nil {
		t.Fatal(err)
	}
	t.Setenv(SocketParentRootEnv, scope)
	t.Setenv(tmuxTmpEnv, filepath.Join(parent, "gct-1234-current", "tmux"))
	if got := listTestSocketPaths(); len(got) != 0 {
		t.Fatalf("listTestSocketPaths() widened beyond configured scope: %v", got)
	}
}

func TestSocketRootWithinParentUsesCanonicalContainmentForMissingTails(t *testing.T) {
	parent := t.TempDir()
	scope := filepath.Join(parent, "owned")
	if err := os.Mkdir(scope, 0o700); err != nil {
		t.Fatal(err)
	}
	ownedMissing := filepath.Join(scope, "gct-42-owned", "tmux")
	if !SocketRootWithinParent(scope, ownedMissing) {
		t.Fatalf("missing owned socket root %q was not contained", ownedMissing)
	}
	foreign := filepath.Join(parent, "foreign")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(scope, "escape")
	if err := os.Symlink(foreign, link); err != nil {
		t.Fatal(err)
	}
	escapedMissing := filepath.Join(link, "gct-43-foreign", "tmux")
	if SocketRootWithinParent(scope, escapedMissing) {
		t.Fatalf("symlinked missing tail escaped scope: %q", escapedMissing)
	}
	if SocketRootWithinParent(scope, filepath.Join(scope, "gct-0-default", "tmux")) {
		t.Fatal("zero-PID socket root was considered test-owned")
	}
	if SocketRootWithinParent(scope, filepath.Join(scope, "tmux-"+strconv.Itoa(os.Getuid()))) {
		t.Fatal("default tmux server root was considered test-owned")
	}
	if SocketRootWithinParent(scope, filepath.Join(scope, "gct-44-nested", "child", "tmux")) {
		t.Fatal("nested non-run root was considered test-owned")
	}
}

func TestSocketParentRootFromEnvRejectsInvalidExplicitScope(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(SocketParentRootEnv, root)
	got, err := SocketParentRootFromEnv("/tmp")
	if err == nil || got != "" {
		t.Fatalf("SocketParentRootFromEnv() = (%q, %v), want fail-closed error", got, err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv(SocketParentRootEnv, missing)
	if got, err = SocketParentRootFromEnv("/tmp"); err == nil || got != "" {
		t.Fatalf("missing explicit scope resolved to (%q, %v), want error", got, err)
	}
}

func TestTmuxSocketRootPatternsCoverKnownRuntimePrefixes(t *testing.T) {
	namespace := t.TempDir()
	tests := []struct {
		name    string
		runName string
		direct  bool // true = activeRoot is namespace/runName/tmux (no "runtime" level)
		want    string
	}{
		{
			name:    "acceptance C",
			runName: "gcac-123",
			want:    filepath.Join(namespace, "gcac-*", "runtime", "tmux"),
		},
		{
			name:    "worker inference",
			runName: "gcwi-123",
			want:    filepath.Join(namespace, "gcwi-*", "runtime", "tmux"),
		},
		{
			name:    "worker inference live",
			runName: "gcwi-live-123",
			want:    filepath.Join(namespace, "gcwi-*", "runtime", "tmux"),
		},
		{
			name:    "acceptance B",
			runName: "gc-acceptance-b-123",
			want:    filepath.Join(namespace, "gc-acceptance-b-*", "runtime", "tmux"),
		},
		{
			name:    "acceptance",
			runName: "gc-acceptance-123",
			want:    filepath.Join(namespace, "gc-acceptance-*", "runtime", "tmux"),
		},
		{
			name:    "integration direct",
			runName: "gc-integration-123",
			direct:  true,
			want:    filepath.Join(namespace, "gc-integration-*", "tmux"),
		},
		{
			// gct- is the short-path tmux socket root created by the integration
			// test suite when $TMPDIR is too long (e.g., macOS).
			name:    "gct short root",
			runName: "gct-1234567890",
			direct:  true,
			want:    filepath.Join(namespace, "gct*", "tmux"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var activeRoot string
			if tt.direct {
				activeRoot = filepath.Join(namespace, tt.runName, "tmux")
			} else {
				activeRoot = filepath.Join(namespace, tt.runName, "runtime", "tmux")
			}
			got := tmuxSocketRootPatterns(activeRoot)
			if !slices.Contains(got, tt.want) {
				t.Fatalf("tmuxSocketRootPatterns(%q) = %v, want %q", activeRoot, got, tt.want)
			}
		})
	}
}

func TestNewGuardWithSocketCityNameFormat(t *testing.T) {
	// City name must be "gctest-<8hex>" (no per-character hyphens).
	// macOS's UNIX socket path limit is 104 bytes; per-char hyphenation
	// creates names like "gctest-4-f-d-9-6-0-8-c" (22 chars) instead of
	// "gctest-4fd9608c" (15 chars), which pushes socket paths over the limit.
	for range 100 {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("gctest-%x", b)
		if len(name) != 15 {
			t.Fatalf("city name %q has length %d, want 15", name, len(name))
		}
	}
}
