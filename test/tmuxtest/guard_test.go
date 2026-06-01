package tmuxtest

import (
	"os"
	"testing"
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
