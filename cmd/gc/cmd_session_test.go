package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{3 * time.Hour, "3h"},
		{48 * time.Hour, "2d"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestParsePruneDuration(t *testing.T) {
	tests := []struct {
		input   string
		want    time.Duration
		wantErr bool
	}{
		{"7d", 7 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"-5d", 0, true},
		{"0d", 0, true},
		{"-24h", 0, true},
		{"0h", 0, true},
		{"1.5d", 0, true},
		{"7dd", 0, true},
		{"abc", 0, true},
		{"d", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parsePruneDuration(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parsePruneDuration(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parsePruneDuration(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolveWorkDir(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(t.TempDir(), "my-rig")
	tests := []struct {
		name    string
		cfg     *config.City
		agent   *config.Agent
		want    string
		wantErr bool
	}{
		{
			name:  "city-scoped",
			cfg:   &config.City{Workspace: config.Workspace{Name: "city"}},
			agent: &config.Agent{},
			want:  cityPath,
		},
		{
			name: "work-dir override",
			cfg: &config.City{
				Workspace: config.Workspace{Name: "city"},
				Rigs:      []config.Rig{{Name: "my-rig", Path: rigRoot}},
			},
			agent: &config.Agent{Dir: "my-rig", WorkDir: ".gc/worktrees/{{.Rig}}/refinery"},
			want:  filepath.Join(cityPath, ".gc", "worktrees", "my-rig", "refinery"),
		},
		{
			name: "rig-scoped defaults to configured rig root",
			cfg: &config.City{
				Workspace: config.Workspace{Name: "city"},
				Rigs:      []config.Rig{{Name: "my-rig", Path: rigRoot}},
			},
			agent: &config.Agent{Dir: "my-rig"},
			want:  rigRoot,
		},
		{
			name: "invalid work-dir template returns error",
			cfg: &config.City{
				Workspace: config.Workspace{Name: "city"},
				Rigs:      []config.Rig{{Name: "my-rig", Path: rigRoot}},
			},
			agent:   &config.Agent{Dir: "my-rig", WorkDir: ".gc/worktrees/{{.RigName}}/refinery"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveWorkDir(cityPath, tt.cfg, tt.agent)
			if tt.wantErr {
				if err == nil {
					t.Fatal("resolveWorkDir error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveWorkDir error = %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveWorkDir = %q, want %q", got, tt.want)
			}
		})
	}
}

// NOTE: session kill is tested via internal/session.Manager.Kill which
// delegates to Provider.Stop. The CLI layer (cmdSessionKill) is a thin
// wrapper that resolves the session ID and calls mgr.Kill, so it does
// not warrant a separate unit test beyond integration coverage.

// NOTE: session nudge is tested implicitly — the critical path components
// (resolveAgentIdentity, sessionName, Provider.Nudge) each have dedicated
// tests. The CLI layer (cmdSessionNudge) is a thin integration wrapper.

func TestShouldAttachNewSession(t *testing.T) {
	tests := []struct {
		name      string
		noAttach  bool
		transport string
		want      bool
	}{
		{name: "default transport attaches", noAttach: false, transport: "", want: true},
		{name: "explicit no-attach wins", noAttach: true, transport: "", want: false},
		{name: "acp skips attach", noAttach: false, transport: "acp", want: false},
		{name: "tmux attaches", noAttach: false, transport: "tmux", want: true},
		{name: "hybrid attaches", noAttach: false, transport: "hybrid", want: true},
		{name: "subprocess skips attach", noAttach: false, transport: "subprocess", want: false},
		{name: "exec skips attach", noAttach: false, transport: "exec:myscript", want: false},
		{name: "k8s skips attach", noAttach: false, transport: "k8s", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAttachNewSession(tt.noAttach, tt.transport); got != tt.want {
				t.Fatalf("shouldAttachNewSession(%v, %q) = %v, want %v", tt.noAttach, tt.transport, got, tt.want)
			}
		})
	}
}
