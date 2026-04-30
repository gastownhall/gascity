package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func TestShellJoinArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"empty slice", nil, ""},
		{"single arg no metachar", []string{"--model"}, "--model"},
		{"two clean args", []string{"--model", "opus"}, "--model opus"},
		{"arg with space", []string{"hello world"}, "'hello world'"},
		{"arg with single quote", []string{"it's"}, "'it'\\''s'"},
		{"empty string arg", []string{""}, "''"},
		{"mixed clean and dirty", []string{"--flag", "value with space", "--other"}, "--flag 'value with space' --other"},
		{"arg with special chars", []string{"$(whoami)"}, "'$(whoami)'"},
		{"arg with semicolon", []string{"foo;bar"}, "'foo;bar'"},
		{"multiple special", []string{"a b", "c'd", "e|f"}, "'a b' 'c'\\''d' 'e|f'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellquote.Join(tt.args)
			if got != tt.want {
				t.Errorf("shellquote.Join(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestBuildSessionResumeUsesResolvedProviderCommand(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor", Provider: "wrapped"},
		},
		Providers: map[string]config.ProviderSpec{
			"wrapped": {
				DisplayName:       "Wrapped Gemini",
				Command:           "aimux",
				Args:              []string{"run", "gemini", "--", "--approval-mode", "yolo"},
				PathCheck:         "true", // use /usr/bin/true so LookPath succeeds in CI
				ReadyPromptPrefix: "> ",
				Env: map[string]string{
					"GC_HOME": "/tmp/gc-accept-home",
				},
			},
		},
	}

	srv := New(fs)
	info := session.Info{
		ID:       "gc-1",
		Template: "mayor",
		Command:  "gemini --approval-mode yolo",
		Provider: "wrapped",
		WorkDir:  "/tmp/workdir",
	}

	cmd, hints, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if got, want := cmd, "aimux run gemini -- --approval-mode yolo"; got != want {
		t.Fatalf("resume command = %q, want %q", got, want)
	}
	if got, want := hints.WorkDir, "/tmp/workdir"; got != want {
		t.Fatalf("hints.WorkDir = %q, want %q", got, want)
	}
	if got, want := hints.ReadyPromptPrefix, "> "; got != want {
		t.Fatalf("hints.ReadyPromptPrefix = %q, want %q", got, want)
	}
	if got, want := hints.Env["GC_HOME"], "/tmp/gc-accept-home"; got != want {
		t.Fatalf("hints.Env[GC_HOME] = %q, want %q", got, want)
	}
}

func TestBuildSessionResumePreservesStoredResolvedCommand(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor", Provider: "wrapped"},
		},
		Providers: map[string]config.ProviderSpec{
			"wrapped": {
				DisplayName: "Wrapped Claude",
				Command:     "claude",
				PathCheck:   "true",
			},
		},
	}

	srv := New(fs)
	info := session.Info{
		ID:       "gc-1",
		Template: "mayor",
		Command:  "claude --dangerously-skip-permissions --settings /tmp/settings.json",
		Provider: "wrapped",
		WorkDir:  "/tmp/workdir",
	}

	cmd, _, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if got, want := cmd, "claude --dangerously-skip-permissions --settings /tmp/settings.json"; got != want {
		t.Fatalf("resume command = %q, want %q", got, want)
	}
}

// TestBuildSessionResumeRebuildsBareStoredCommandForPoolClaudeAgent is a
// regression test for gastownhall/gascity#799: when a pool-agent session
// resumed through the control-dispatcher path has only the bare
// provider binary ("claude") as its stored command, the API must
// re-inject schema defaults (--dangerously-skip-permissions) and the
// provider-owned --settings path from the current resolved config.
// Before the fix, the bare stored command was preserved as-is and pool
// workers wedged on interactive permission prompts on resume.
func TestBuildSessionResumeRebuildsBareStoredCommandForPoolClaudeAgent(t *testing.T) {
	fs := newSessionFakeState(t)
	claude := config.BuiltinProviders()["claude"]
	claude.PathCheck = "true" // use /usr/bin/true so LookPath succeeds in CI
	maxActive := 3
	gcDir := filepath.Join(fs.cityPath, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcDir, "settings.json"), []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{
				Name:              "perspective_planner",
				Provider:          "claude",
				MaxActiveSessions: &maxActive,
			},
		},
		Providers: map[string]config.ProviderSpec{
			"claude": claude,
		},
	}

	srv := New(fs)
	info := session.Info{
		ID:         "gc-1",
		Template:   "perspective_planner",
		Command:    "claude",
		Provider:   "claude",
		WorkDir:    fs.cityPath,
		SessionKey: "abc-123",
		ResumeFlag: "--resume",
	}

	cmd, _, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if !strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Fatalf("resume command missing default args:\n  got: %s", cmd)
	}
	if !strings.Contains(cmd, "--resume abc-123") {
		t.Fatalf("resume command missing resume flag:\n  got: %s", cmd)
	}
	if !strings.Contains(cmd, "--settings") {
		t.Fatalf("resume command missing settings arg:\n  got: %s", cmd)
	}
}

func TestBuildSessionResumeUsesStoredACPCommandForProviderSession(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Providers: map[string]config.ProviderSpec{
			"opencode": {
				DisplayName: "OpenCode",
				Command:     "/bin/echo",
				PathCheck:   "true",
				SupportsACP: &supportsACP,
				ACPCommand:  "/bin/echo",
				ACPArgs:     []string{"acp"},
			},
		},
	}

	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  sessionauto.New(runtime.NewFake(), runtime.NewFake()),
	}
	srv := New(state)
	info := session.Info{
		ID:        "gc-1",
		Template:  "opencode",
		Command:   "/bin/echo",
		Provider:  "opencode",
		Transport: "acp",
		WorkDir:   "/tmp/workdir",
	}

	cmd, _, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if got, want := cmd, "/bin/echo acp"; got != want {
		t.Fatalf("resume command = %q, want %q", got, want)
	}
}

func TestBuildSessionResumeFallsBackToStoredCommandWhenTemplateOverridesInvalid(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Providers["test-agent"] = config.ProviderSpec{
		Command:   "/bin/echo",
		PathCheck: "true",
	}

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Chat")
	info.Template = "myrig/worker"
	info.Command = "/bin/echo --stored"
	if err := fs.cityBeadStore.SetMetadata(info.ID, "template_overrides", "{"); err != nil {
		t.Fatalf("SetMetadata(template_overrides): %v", err)
	}

	srv := New(fs)
	cmd, _, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if got, want := cmd, "/bin/echo --stored"; got != want {
		t.Fatalf("resume command = %q, want %q", got, want)
	}
}

func TestBuildSessionResumeUsesStoredACPCommandForLegacyProviderSessionWithoutTransportMetadataWithoutSessionAutoProvider(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Providers: map[string]config.ProviderSpec{
			"opencode": {
				DisplayName: "OpenCode",
				Command:     "/bin/echo",
				PathCheck:   "true",
				SupportsACP: &supportsACP,
				ACPCommand:  "/bin/echo",
				ACPArgs:     []string{"acp"},
			},
		},
	}

	srv := New(fs)
	info := session.Info{
		ID:       "gc-1",
		Template: "opencode",
		Command:  "/bin/echo acp",
		Provider: "opencode",
		WorkDir:  "/tmp/workdir",
	}

	cmd, _, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if got, want := cmd, "/bin/echo acp"; got != want {
		t.Fatalf("resume command = %q, want %q", got, want)
	}
}

func TestBuildSessionResumeUsesStoredACPCommandForLegacyProviderSessionWithoutTransportMetadata(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Providers: map[string]config.ProviderSpec{
			"opencode": {
				DisplayName: "OpenCode",
				Command:     "/bin/echo",
				PathCheck:   "true",
				SupportsACP: &supportsACP,
				ACPCommand:  "/bin/echo",
				ACPArgs:     []string{"acp"},
			},
		},
	}

	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  sessionauto.New(runtime.NewFake(), runtime.NewFake()),
	}
	srv := New(state)
	info := session.Info{
		ID:       "gc-1",
		Template: "opencode",
		Command:  "/bin/echo acp",
		Provider: "opencode",
		WorkDir:  "/tmp/workdir",
	}

	cmd, _, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if got, want := cmd, "/bin/echo acp"; got != want {
		t.Fatalf("resume command = %q, want %q", got, want)
	}
}

func TestBuildSessionResumeUsesStoredACPCommandForLegacyProviderSessionOnACPEnabledCustomProvider(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Providers: map[string]config.ProviderSpec{
			"custom-acp": {
				DisplayName: "Custom ACP",
				Command:     "/bin/echo",
				PathCheck:   "true",
				SupportsACP: &supportsACP,
				ACPCommand:  "/bin/echo",
				ACPArgs:     []string{"acp"},
			},
		},
	}

	srv := New(fs)
	info := session.Info{
		ID:       "gc-1",
		Template: "custom-acp",
		Command:  "/bin/echo acp",
		Provider: "custom-acp",
		WorkDir:  "/tmp/workdir",
	}

	cmd, _, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if got, want := cmd, "/bin/echo acp"; got != want {
		t.Fatalf("resume command = %q, want %q", got, want)
	}
}

func TestBuildSessionResumeUsesStoredACPTransportForTemplateSession(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", Provider: "opencode", Session: "acp"},
		},
		Providers: map[string]config.ProviderSpec{
			"opencode": {
				DisplayName: "OpenCode",
				Command:     "/bin/echo",
				PathCheck:   "true",
				SupportsACP: &supportsACP,
				ACPCommand:  "/bin/echo",
				ACPArgs:     []string{"acp"},
			},
		},
	}

	srv := New(fs)
	info := session.Info{
		ID:        "gc-1",
		Template:  "worker",
		Command:   "/bin/echo",
		Provider:  "opencode",
		Transport: "acp",
		WorkDir:   "/tmp/workdir",
	}

	cmd, _, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if got, want := cmd, "/bin/echo acp"; got != want {
		t.Fatalf("resume command = %q, want %q", got, want)
	}
}

func TestBuildSessionResumeDoesNotInferConfiguredACPTransportForTemplateSessionWithoutStoredMetadata(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", Provider: "opencode", Session: "acp"},
		},
		Providers: map[string]config.ProviderSpec{
			"opencode": {
				DisplayName: "OpenCode",
				Command:     "/bin/echo",
				PathCheck:   "true",
				SupportsACP: &supportsACP,
				ACPCommand:  "/bin/echo",
				ACPArgs:     []string{"acp"},
			},
		},
	}

	srv := New(fs)
	info := session.Info{
		ID:       "gc-1",
		Template: "worker",
		Command:  "/bin/echo",
		Provider: "opencode",
		WorkDir:  "/tmp/workdir",
	}

	cmd, _, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if got, want := cmd, "/bin/echo"; got != want {
		t.Fatalf("resume command = %q, want %q", got, want)
	}
}

func TestResolvedSessionTransportUsesResumeMetadataForLegacyACPWithSameCommand(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Command:    "/bin/echo",
		ACPCommand: "/bin/echo",
	}

	got := resolvedSessionTransport(session.Info{
		Command: "/bin/echo",
	}, resolved, "acp", map[string]string{
		"resume_flag": "--resume",
	}, false)
	if got != "acp" {
		t.Fatalf("resolvedSessionTransport() = %q, want acp", got)
	}
}

func TestLegacyACPTransportAmbiguousWithSameCommand(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Command:    "/bin/echo",
		ACPCommand: "/bin/echo",
	}

	if !legacyACPTransportAmbiguous(resolved, "acp", "/bin/echo", nil) {
		t.Fatal("legacyACPTransportAmbiguous() = false, want true")
	}
}

func TestBuildSessionResumeUsesStartedConfigHashForLegacyProviderACPWithSameCommand(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Providers: map[string]config.ProviderSpec{
			"custom-acp": {
				DisplayName: "Custom ACP",
				Command:     "/bin/echo",
				PathCheck:   "true",
				SupportsACP: &supportsACP,
				ACPCommand:  "/bin/echo",
			},
		},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "identity.template.toml"), []byte(`
name = "identity"
command = "/bin/mcp"
args = ["{{.AgentName}}"]
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}

	srv := New(fs)
	resolved, err := srv.resolveBareProvider("custom-acp")
	if err != nil {
		t.Fatalf("resolveBareProvider: %v", err)
	}
	mcpServers, err := srv.sessionMCPServers("custom-acp", "custom-acp", "custom-acp", fs.cityPath, "acp", "provider")
	if err != nil {
		t.Fatalf("sessionMCPServers: %v", err)
	}
	startedHash := runtime.CoreFingerprint(runtime.Config{
		Command:    resolved.ACPCommandString(),
		Env:        resolved.Env,
		MCPServers: mcpServers,
	})
	bead, err := fs.cityBeadStore.Create(beads.Bead{
		Type: "session",
		Metadata: map[string]string{
			"mc_session_kind":     "provider",
			"started_config_hash": startedHash,
		},
	})
	if err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}

	_, hints, err := srv.buildSessionResume(session.Info{
		ID:       bead.ID,
		Template: "custom-acp",
		Command:  "/bin/echo",
		Provider: "custom-acp",
		WorkDir:  fs.cityPath,
	})
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if len(hints.MCPServers) != 1 {
		t.Fatalf("len(hints.MCPServers) = %d, want 1", len(hints.MCPServers))
	}
}

func TestBuildSessionResumeUsesStoredACPCommandForLegacyTemplateSessionWithoutTransportMetadata(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", Provider: "opencode", Session: "acp"},
		},
		Providers: map[string]config.ProviderSpec{
			"opencode": {
				DisplayName: "OpenCode",
				Command:     "/bin/echo",
				PathCheck:   "true",
				SupportsACP: &supportsACP,
				ACPCommand:  "/bin/echo",
				ACPArgs:     []string{"acp"},
			},
		},
	}

	srv := New(fs)
	info := session.Info{
		ID:       "gc-1",
		Template: "worker",
		Command:  "/bin/echo acp",
		Provider: "opencode",
		WorkDir:  "/tmp/workdir",
	}

	cmd, _, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if got, want := cmd, "/bin/echo acp"; got != want {
		t.Fatalf("resume command = %q, want %q", got, want)
	}
}

func TestBuildSessionResumeKeepsDefaultCommandForLegacyTemplateWithoutExplicitTransport(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", Provider: "opencode"},
		},
		Providers: map[string]config.ProviderSpec{
			"opencode": {
				DisplayName: "OpenCode",
				Command:     "/bin/echo",
				PathCheck:   "true",
				SupportsACP: &supportsACP,
				ACPCommand:  "/bin/echo",
				ACPArgs:     []string{"acp"},
			},
		},
	}

	srv := New(fs)
	info := session.Info{
		ID:       "gc-1",
		Template: "worker",
		Command:  "/bin/echo",
		Provider: "opencode",
		WorkDir:  "/tmp/workdir",
	}

	cmd, _, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if got, want := cmd, "/bin/echo"; got != want {
		t.Fatalf("resume command = %q, want %q", got, want)
	}
}

func TestBuildSessionResumeIgnoresMCPResolutionErrorForACPResume(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", Provider: "opencode", Session: "acp"},
		},
		Providers: map[string]config.ProviderSpec{
			"opencode": {
				DisplayName: "OpenCode",
				Command:     "/bin/echo",
				PathCheck:   "true",
				SupportsACP: &supportsACP,
				ACPCommand:  "/bin/echo",
				ACPArgs:     []string{"acp"},
			},
		},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "filesystem.toml"), []byte(`
name = "filesystem"
command = [broken
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}

	srv := New(fs)
	info := session.Info{
		ID:        "gc-1",
		Template:  "worker",
		Provider:  "opencode",
		Transport: "acp",
		WorkDir:   fs.cityPath,
	}

	cmd, hints, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if got, want := cmd, "/bin/echo acp"; got != want {
		t.Fatalf("resume command = %q, want %q", got, want)
	}
	if len(hints.MCPServers) != 0 {
		t.Fatalf("Hints.MCPServers len = %d, want 0", len(hints.MCPServers))
	}
}

func TestBuildSessionResumeIgnoresMCPResolutionErrorWithoutACPTransport(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", Provider: "stub"},
		},
		Providers: map[string]config.ProviderSpec{
			"stub": {
				DisplayName: "Stub",
				Command:     "/bin/echo",
			},
		},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "filesystem.toml"), []byte(`
name = "filesystem"
command = [broken
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}

	srv := New(fs)
	info := session.Info{
		ID:       "gc-1",
		Template: "worker",
		Provider: "stub",
		WorkDir:  fs.cityPath,
	}

	cmd, _, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if got, want := cmd, "/bin/echo"; got != want {
		t.Fatalf("resume command = %q, want %q", got, want)
	}
}

func TestBuildSessionResumeUsesStoredAgentNameForResumeMCPMaterialization(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "ant",
			Dir:               "myrig",
			Provider:          "opencode",
			Session:           "acp",
			WorkDir:           ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(4),
		}},
		Providers: map[string]config.ProviderSpec{
			"opencode": {
				DisplayName: "OpenCode",
				Command:     "/bin/echo",
				PathCheck:   "true",
				SupportsACP: &supportsACP,
				ACPCommand:  "/bin/echo",
				ACPArgs:     []string{"acp"},
			},
		},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "identity.template.toml"), []byte(`
name = "identity"
command = "/bin/mcp"
args = ["{{.AgentName}}", "{{.WorkDir}}", "{{.TemplateName}}"]
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}

	workDir := filepath.Join(fs.cityPath, ".gc", "worktrees", "myrig", "ants", "ant")
	srv := New(fs)
	info := session.Info{
		ID:        "gc-1",
		Template:  "myrig/ant",
		Alias:     "ant",
		AgentName: "myrig/ant-adhoc-123",
		Provider:  "opencode",
		Transport: "acp",
		WorkDir:   workDir,
	}

	cmd, hints, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if got, want := cmd, "/bin/echo acp"; got != want {
		t.Fatalf("resume command = %q, want %q", got, want)
	}
	if len(hints.MCPServers) != 1 {
		t.Fatalf("Hints.MCPServers len = %d, want 1", len(hints.MCPServers))
	}
	if got, want := hints.MCPServers[0].Args[0], info.AgentName; got != want {
		t.Fatalf("Args[0] = %q, want %q", got, want)
	}
	if got, want := hints.MCPServers[0].Args[1], workDir; got != want {
		t.Fatalf("Args[1] = %q, want %q", got, want)
	}
	if got, want := hints.MCPServers[0].Args[2], "myrig/ant"; got != want {
		t.Fatalf("Args[2] = %q, want %q", got, want)
	}
}
