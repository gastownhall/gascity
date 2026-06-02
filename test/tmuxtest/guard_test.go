package tmuxtest

import (
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
	currentRun := filepath.Join(tmp, "gc-integration-current")
	t.Setenv("TMPDIR", currentRun)
	currentRoot := filepath.Join(currentRun, "tmux")
	staleRoot := filepath.Join(tmp, "gc-integration-stale", "tmux")
	liveRoot := filepath.Join(tmp, "gc-integration-live", "tmux")
	otherRoot := filepath.Join(tmp, "not-gc", "tmux")
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
	if err := os.Chtimes(staleSocket, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes(%s): %v", staleSocket, err)
	}

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

func TestTmuxSocketRootPatternsCoverKnownRuntimePrefixes(t *testing.T) {
	namespace := t.TempDir()
	tests := []struct {
		name    string
		runName string
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			activeRoot := filepath.Join(namespace, tt.runName, "runtime", "tmux")
			got := tmuxSocketRootPatterns(activeRoot)
			if !slices.Contains(got, tt.want) {
				t.Fatalf("tmuxSocketRootPatterns(%q) = %v, want %q", activeRoot, got, tt.want)
			}
		})
	}
}
