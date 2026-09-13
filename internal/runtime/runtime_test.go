package runtime

import "testing"

func TestSyncWorkDirEnvSetsGCDir(t *testing.T) {
	cfg := SyncWorkDirEnv(Config{WorkDir: "/tmp/work"})
	if got := cfg.Env["GC_DIR"]; got != "/tmp/work" {
		t.Fatalf("GC_DIR = %q, want %q", got, "/tmp/work")
	}
}

func TestSyncWorkDirEnvCopiesEnvBeforeMutation(t *testing.T) {
	original := map[string]string{"GC_DIR": "/stale", "GC_AGENT": "worker"}
	cfg := SyncWorkDirEnv(Config{
		WorkDir: "/tmp/work",
		Env:     original,
	})
	if got := cfg.Env["GC_DIR"]; got != "/tmp/work" {
		t.Fatalf("GC_DIR = %q, want %q", got, "/tmp/work")
	}
	if got := original["GC_DIR"]; got != "/stale" {
		t.Fatalf("original GC_DIR mutated to %q", got)
	}
	if got := cfg.Env["GC_AGENT"]; got != "worker" {
		t.Fatalf("GC_AGENT = %q, want %q", got, "worker")
	}
}

func TestHasManagedStartupHints(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{name: "none", cfg: Config{}, want: false},
		{name: "ready prompt", cfg: Config{ReadyPromptPrefix: "> "}, want: true},
		{name: "ready delay", cfg: Config{ReadyDelayMs: 100}, want: true},
		{name: "process names", cfg: Config{ProcessNames: []string{"claude"}}, want: true},
		{name: "permission warning", cfg: Config{EmitsPermissionWarning: true}, want: true},
		{name: "startup dialog override", cfg: Config{AcceptStartupDialogs: boolPtr(false)}, want: true},
		{name: "nudge", cfg: Config{Nudge: "Check your hook."}, want: true},
		{name: "pre start", cfg: Config{PreStart: []string{"echo pre"}}, want: true},
		{name: "session setup", cfg: Config{SessionSetup: []string{"echo setup"}}, want: true},
		{name: "session setup script", cfg: Config{SessionSetupScript: "setup.sh"}, want: true},
		{name: "session live", cfg: Config{SessionLive: []string{"echo live"}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasManagedStartupHints(tt.cfg); got != tt.want {
				t.Fatalf("HasManagedStartupHints() = %v, want %v", got, tt.want)
			}
		})
	}
}

func boolPtr(v bool) *bool {
	return &v
}
