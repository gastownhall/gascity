package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

type attachmentAwareProvider struct {
	*runtime.Fake
	sleepCapability runtime.SessionSleepCapability
	pending         *runtime.PendingInteraction
	pendingErr      error
	responded       runtime.InteractionResponse
	respondErr      error
}

func (p *attachmentAwareProvider) SleepCapability(string) runtime.SessionSleepCapability {
	return p.sleepCapability
}

func (p *attachmentAwareProvider) Pending(string) (*runtime.PendingInteraction, error) {
	if p.pendingErr != nil {
		return nil, p.pendingErr
	}
	if p.pending == nil {
		return nil, nil
	}
	pendingCopy := *p.pending
	return &pendingCopy, nil
}

func (p *attachmentAwareProvider) Respond(_ string, response runtime.InteractionResponse) error {
	if p.respondErr != nil {
		return p.respondErr
	}
	p.responded = response
	return nil
}

type transportCapableSessionProvider struct {
	*runtime.Fake
}

func (p *transportCapableSessionProvider) SupportsTransport(transport string) bool {
	return transport == "acp"
}

type routedRejectingSessionProvider struct {
	*runtime.Fake
}

func (p *routedRejectingSessionProvider) SupportsTransport(string) bool {
	return false
}

func (p *routedRejectingSessionProvider) RouteACP(string) {}

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

func TestCmdSessionList_ManagedExecLifecycleProviderReadsSessions(t *testing.T) {
	cityDir, _ := setupManagedBdWaitTestCity(t)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "managed exec session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "mayor",
			"template":     "worker",
			"state":        "asleep",
		},
	}); err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(cityDir))
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionList("", "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionList() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "mayor") {
		t.Fatalf("stdout missing session name %q:\n%s", "mayor", stdout.String())
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

func TestCmdSessionNew_PoolTemplateUsesAliasBackedWorkDirIdentity(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writePoolSessionCityTOML(t, cityDir)

	var stdout, stderr bytes.Buffer
	for _, alias := range []string{"demo/ant-fenrir", "demo/ant-grendel"} {
		stdout.Reset()
		stderr.Reset()
		if code := cmdSessionNew([]string{"demo/ant"}, alias, "", "", true, &stdout, &stderr); code != 0 {
			t.Fatalf("cmdSessionNew(%q) = %d, want 0; stderr=%s", alias, code, stderr.String())
		}
	}

	all := sessionBeads(t, cityDir)
	if len(all) != 2 {
		t.Fatalf("session beads = %d, want 2", len(all))
	}

	wantWorkDir := map[string]string{
		"demo/ant-fenrir":  filepath.Join(cityDir, ".gc", "worktrees", "demo", "ants", "ant-fenrir"),
		"demo/ant-grendel": filepath.Join(cityDir, ".gc", "worktrees", "demo", "ants", "ant-grendel"),
	}
	seenWorkDir := make(map[string]string, len(all))
	for _, bead := range all {
		alias := bead.Metadata["alias"]
		want, ok := wantWorkDir[alias]
		if !ok {
			t.Fatalf("unexpected alias %q in bead metadata", alias)
		}
		if got := bead.Metadata["work_dir"]; got != want {
			t.Fatalf("work_dir(%q) = %q, want %q", alias, got, want)
		}
		if otherAlias, collision := seenWorkDir[bead.Metadata["work_dir"]]; collision {
			t.Fatalf("work_dir collision: %q and %q both use %q", otherAlias, alias, bead.Metadata["work_dir"])
		}
		seenWorkDir[bead.Metadata["work_dir"]] = alias
	}
}

func TestCmdSessionNew_PoolTemplateCanonicalizesQualifiedAliasCollisions(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writePoolSessionCityTOML(t, cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionNew([]string{"demo/ant"}, "ant-fenrir", "", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionNew(first) = %d, want 0; stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdSessionNew([]string{"demo/ant"}, "demo/ant-fenrir", "", "", true, &stdout, &stderr); code == 0 {
		t.Fatal("cmdSessionNew(second) = 0, want alias conflict")
	}
	if !strings.Contains(stderr.String(), session.ErrSessionAliasExists.Error()) {
		t.Fatalf("stderr = %q, want alias conflict", stderr.String())
	}

	all := sessionBeads(t, cityDir)
	if len(all) != 1 {
		t.Fatalf("session beads = %d, want 1", len(all))
	}
	if got := all[0].Metadata["alias"]; got != "demo/ant-fenrir" {
		t.Fatalf("alias = %q, want canonical qualified alias", got)
	}
}

func TestCmdSessionNew_PoolTemplateBareAliasStillResolves(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writePoolSessionCityTOML(t, cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionNew([]string{"demo/ant"}, "ant-fenrir", "", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionNew = %d, want 0; stderr=%s", code, stderr.String())
	}

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig(%q): %v", cityDir, err)
	}
	store, code := openCityStore(io.Discard, "test")
	if store == nil {
		t.Fatalf("openCityStore = nil, code=%d", code)
	}

	id, err := resolveSessionIDMaterializingNamed(cityDir, cfg, store, "ant-fenrir")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(ant-fenrir): %v", err)
	}
	all := sessionBeads(t, cityDir)
	if len(all) != 1 {
		t.Fatalf("session beads = %d, want 1", len(all))
	}
	if id != all[0].ID {
		t.Fatalf("resolved ID = %q, want %q", id, all[0].ID)
	}
}

func TestCmdSessionNew_PoolTemplateWithoutAliasUsesGeneratedWorkDirIdentity(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writePoolSessionCityTOML(t, cityDir)

	var stdout, stderr bytes.Buffer
	for i := 0; i < 2; i++ {
		stdout.Reset()
		stderr.Reset()
		if code := cmdSessionNew([]string{"demo/ant"}, "", "", "", true, &stdout, &stderr); code != 0 {
			t.Fatalf("cmdSessionNew(aliasless #%d) = %d, want 0; stderr=%s", i+1, code, stderr.String())
		}
	}

	all := sessionBeads(t, cityDir)
	if len(all) != 2 {
		t.Fatalf("session beads = %d, want 2", len(all))
	}

	seenSessionName := make(map[string]bool, len(all))
	seenWorkDir := make(map[string]bool, len(all))
	for _, bead := range all {
		if got := bead.Metadata["alias"]; got != "" {
			t.Fatalf("alias = %q, want empty for aliasless pooled session", got)
		}
		sessionName := bead.Metadata["session_name"]
		if sessionName == "" {
			t.Fatal("session_name should be populated for aliasless pooled session")
		}
		if got := bead.Metadata["session_name_explicit"]; got != boolMetadata(true) {
			t.Fatalf("session_name_explicit = %q, want %q", got, boolMetadata(true))
		}
		if !strings.HasPrefix(sessionName, "ant-adhoc-") {
			t.Fatalf("session_name = %q, want ant-adhoc-*", sessionName)
		}
		if seenSessionName[sessionName] {
			t.Fatalf("duplicate session_name %q for aliasless pooled sessions", sessionName)
		}
		seenSessionName[sessionName] = true
		workDir := bead.Metadata["work_dir"]
		if filepath.Dir(workDir) != filepath.Join(cityDir, ".gc", "worktrees", "demo", "ants") {
			t.Fatalf("work_dir(%q) parent = %q, want %q", sessionName, filepath.Dir(workDir), filepath.Join(cityDir, ".gc", "worktrees", "demo", "ants"))
		}
		base := filepath.Base(workDir)
		if base == "ant" {
			t.Fatalf("work_dir(%q) base = %q, want unique generated identity", sessionName, base)
		}
		if !strings.HasPrefix(base, "ant-adhoc-") {
			t.Fatalf("work_dir(%q) base = %q, want ant-adhoc-*", sessionName, base)
		}
		if seenWorkDir[workDir] {
			t.Fatalf("duplicate work_dir %q for aliasless pooled sessions", workDir)
		}
		seenWorkDir[workDir] = true
		if got := bead.Metadata["agent_name"]; got != "demo/"+sessionName {
			t.Fatalf("agent_name(%q) = %q, want %q", sessionName, got, "demo/"+sessionName)
		}
	}
}

func TestCmdSessionNew_ACPTemplatePersistsStoredMCPMetadata(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-mcp-")
	t.Setenv("GC_CITY", cityDir)
	writePoolACPSessionCityTOML(t, cityDir)
	writeCatalogFile(t, cityDir, "mcp/identity.template.toml", `
name = "identity"
command = "/bin/mcp"
args = ["{{.AgentName}}", "{{.WorkDir}}", "{{.TemplateName}}"]
`)

	sockPath := filepath.Join(cityDir, ".gc", "controller.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(%q): %v", sockPath, err)
	}
	defer lis.Close() //nolint:errcheck

	commands := make(chan string, 3)
	errCh := make(chan error, 1)
	go func() {
		defer close(commands)
		for i := 0; i < 3; i++ {
			conn, err := lis.Accept()
			if err != nil {
				errCh <- err
				return
			}
			buf := make([]byte, 64)
			n, err := conn.Read(buf)
			if err != nil {
				conn.Close() //nolint:errcheck
				errCh <- err
				return
			}
			cmd := string(buf[:n])
			commands <- cmd
			reply := "ok\n"
			if cmd == "ping\n" {
				reply = "123\n"
			}
			if _, err := conn.Write([]byte(reply)); err != nil {
				conn.Close() //nolint:errcheck
				errCh <- err
				return
			}
			conn.Close() //nolint:errcheck
		}
	}()

	var stdout, stderr bytes.Buffer
	if code := cmdSessionNew([]string{"demo/ant"}, "", "", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionNew(acp) = %d, want 0; stderr=%s", code, stderr.String())
	}

	gotCommands := make([]string, 0, 3)
	deadline := time.After(2 * time.Second)
	for len(gotCommands) < 3 {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("controller socket: %v", err)
			}
		case cmd, ok := <-commands:
			if !ok {
				if len(gotCommands) != 3 {
					t.Fatalf("controller commands = %v, want ping plus 2 pokes", gotCommands)
				}
				break
			}
			gotCommands = append(gotCommands, cmd)
		case <-deadline:
			t.Fatalf("timed out waiting for controller pokes, got %v", gotCommands)
		}
	}

	bead := onlySessionBead(t, cityDir)
	if got := bead.Metadata[session.MCPIdentityMetadataKey]; got == "" {
		t.Fatal("mcp_identity metadata = empty, want persisted identity")
	}
	if got, want := bead.Metadata[session.MCPIdentityMetadataKey], bead.Metadata["agent_name"]; got != want {
		t.Fatalf("mcp_identity = %q, want agent_name %q", got, want)
	}
	if got := bead.Metadata[session.MCPServersSnapshotMetadataKey]; got == "" {
		t.Fatal("mcp_servers_snapshot metadata = empty, want persisted snapshot")
	}

	servers, err := session.DecodeMCPServersSnapshot(bead.Metadata[session.MCPServersSnapshotMetadataKey])
	if err != nil {
		t.Fatalf("DecodeMCPServersSnapshot: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("len(snapshot) = %d, want 1", len(servers))
	}
	if got, want := servers[0].Args[0], bead.Metadata[session.MCPIdentityMetadataKey]; got != want {
		t.Fatalf("snapshot Args[0] = %q, want %q", got, want)
	}
	if got, want := servers[0].Args[1], bead.Metadata["work_dir"]; got != want {
		t.Fatalf("snapshot Args[1] = %q, want %q", got, want)
	}
	if got, want := servers[0].Args[2], "demo/ant"; got != want {
		t.Fatalf("snapshot Args[2] = %q, want %q", got, want)
	}
}

func TestCmdSessionNew_CustomACPProviderDefaultsAgentSessionToACP(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	oldBuild := buildSessionProviderByName
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })
	buildSessionProviderByName = func(name string, sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
		if name == "acp" {
			return &transportCapableSessionProvider{Fake: runtime.NewFake()}, nil
		}
		return oldBuild(name, sc, cityName, cityPath)
	}

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writePoolProviderDefaultACPSessionCityTOML(t, cityDir)
	writeCatalogFile(t, cityDir, "mcp/identity.template.toml", `
name = "identity"
command = "/bin/mcp"
args = ["{{.AgentName}}", "{{.WorkDir}}", "{{.TemplateName}}"]
`)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionNew([]string{"demo/ant"}, "", "", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionNew(custom provider acp default) = %d, want 0; stderr=%s", code, stderr.String())
	}

	bead := onlySessionBead(t, cityDir)
	if got := bead.Metadata["transport"]; got != "acp" {
		t.Fatalf("transport = %q, want %q", got, "acp")
	}
	if got := bead.Metadata[session.MCPServersSnapshotMetadataKey]; got == "" {
		t.Fatal("mcp_servers_snapshot metadata = empty, want persisted snapshot")
	}
}

func TestCmdSessionNewRejectsExplicitTmuxAgentWhenCitySessionProviderIsACP(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	oldBuild := buildSessionProviderByName
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })
	buildSessionProviderByName = func(name string, sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
		if name == "acp" {
			return &transportCapableSessionProvider{Fake: runtime.NewFake()}, nil
		}
		return oldBuild(name, sc, cityName, cityPath)
	}

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writePoolACPCityExplicitTmuxAgentTOML(t, cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionNew([]string{"demo/ant"}, "", "", "", true, &stdout, &stderr); code == 0 {
		t.Fatalf("cmdSessionNew(explicit tmux on ACP city) = %d, want failure", code)
	}
	if !strings.Contains(stderr.String(), "requires tmux transport") {
		t.Fatalf("stderr = %q, want tmux transport error", stderr.String())
	}
	if got := sessionBeads(t, cityDir); len(got) != 0 {
		t.Fatalf("session bead count = %d, want 0", len(got))
	}
}

func TestCmdSessionNew_PoolTemplateRejectsAliasMatchingConcreteIdentity(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writePoolSessionCityTOML(t, cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionNew([]string{"demo/ant"}, "", "", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionNew(aliasless) = %d, want 0; stderr=%s", code, stderr.String())
	}

	all := sessionBeads(t, cityDir)
	if len(all) != 1 {
		t.Fatalf("session beads = %d, want 1", len(all))
	}
	sessionName := all[0].Metadata["session_name"]
	if sessionName == "" {
		t.Fatal("session_name should be populated for aliasless pooled session")
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdSessionNew([]string{"demo/ant"}, "demo/"+sessionName, "", "", true, &stdout, &stderr); code == 0 {
		t.Fatal("cmdSessionNew(alias collision) = 0, want conflict")
	}
	if !strings.Contains(stderr.String(), session.ErrSessionAliasExists.Error()) {
		t.Fatalf("stderr = %q, want alias conflict", stderr.String())
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAttachNewSession(tt.noAttach, tt.transport); got != tt.want {
				t.Fatalf("shouldAttachNewSession(%v, %q) = %v, want %v", tt.noAttach, tt.transport, got, tt.want)
			}
		})
	}
}

func TestBuildAttachmentCache_CachesWorkerObservedAttachmentState(t *testing.T) {
	cache := buildAttachmentCache([]session.Info{
		{SessionName: "active-attached", State: session.StateActive, Attached: true},
		{SessionName: "active-detached", State: session.StateActive, Attached: false},
		{SessionName: "sleeping", State: session.StateAsleep, Attached: false},
		{SessionName: "suspended", State: session.StateSuspended, Attached: false},
		{State: session.StateActive, Attached: true},
	}, func(info session.Info) (bool, error) {
		if info.SessionName == "sleeping" {
			return true, nil
		}
		return info.Attached, nil
	})

	if len(cache) != 4 {
		t.Fatalf("cache entries = %d, want 4", len(cache))
	}
	if got, ok := cache["active-attached"]; !ok || !got {
		t.Fatalf("cache[active-attached] = (%v, %v), want (true, true)", got, ok)
	}
	if got, ok := cache["active-detached"]; !ok || got {
		t.Fatalf("cache[active-detached] = (%v, %v), want (false, true)", got, ok)
	}
	if got, ok := cache["sleeping"]; !ok || !got {
		t.Fatalf("cache[sleeping] = (%v, %v), want (true, true)", got, ok)
	}
	if got, ok := cache["suspended"]; !ok || got {
		t.Fatalf("cache[suspended] = (%v, %v), want (false, true)", got, ok)
	}
}

func TestBuildAttachmentCache_UsesSessionInfoForActiveSessions(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "sleeping", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.SetAttached("active", false)
	sp.SetAttached("sleeping", true)

	cache := buildAttachmentCache([]session.Info{
		{SessionName: "active", State: session.StateActive, Attached: true},
		{SessionName: "sleeping", State: session.StateAsleep, Attached: false},
	}, func(info session.Info) (bool, error) {
		if info.State == session.StateActive || sp == nil {
			return info.Attached, nil
		}
		return sessionAttachedForWakeReason(sp, info.SessionName), nil
	})

	if got, ok := cache["active"]; !ok || !got {
		t.Fatalf("cache[active] = (%v, %v), want (true, true)", got, ok)
	}
	if got, ok := cache["sleeping"]; !ok || !got {
		t.Fatalf("cache[sleeping] = (%v, %v), want (true, true)", got, ok)
	}

	activeCalls := 0
	activeRunningCalls := 0
	sleepingCalls := 0
	sleepingRunningCalls := 0
	for _, call := range sp.Calls {
		switch call.Method {
		case "IsAttached":
			switch call.Name {
			case "active":
				activeCalls++
			case "sleeping":
				sleepingCalls++
			}
		case "IsRunning":
			switch call.Name {
			case "active":
				activeRunningCalls++
			case "sleeping":
				sleepingRunningCalls++
			}
		}
	}
	if activeCalls != 0 {
		t.Fatalf("IsAttached(active) calls = %d, want 0", activeCalls)
	}
	if activeRunningCalls != 0 {
		t.Fatalf("IsRunning(active) calls = %d, want 0", activeRunningCalls)
	}
	if sleepingCalls != 1 {
		t.Fatalf("IsAttached(sleeping) calls = %d, want 1", sleepingCalls)
	}
	if sleepingRunningCalls != 1 {
		t.Fatalf("IsRunning(sleeping) calls = %d, want 1", sleepingRunningCalls)
	}
}

func TestSessionListTargetPrefersAlias(t *testing.T) {
	info := session.Info{
		Alias:       "hal",
		SessionName: "s-gc-123",
		Title:       "debug auth flow",
	}

	if got := sessionListTarget(info); got != "hal" {
		t.Fatalf("sessionListTarget(alias) = %q, want %q", got, "hal")
	}
	if got := sessionListTitle(info); got != "debug auth flow" {
		t.Fatalf("sessionListTitle(title) = %q, want %q", got, "debug auth flow")
	}
}

func TestSessionListTargetFallsBackToSessionName(t *testing.T) {
	info := session.Info{
		SessionName: "s-gc-123",
	}

	if got := sessionListTarget(info); got != "s-gc-123" {
		t.Fatalf("sessionListTarget(session_name) = %q, want %q", got, "s-gc-123")
	}
	if got := sessionListTitle(info); got != "-" {
		t.Fatalf("sessionListTitle(empty) = %q, want %q", got, "-")
	}
}

func TestSessionListTitleTruncatesLongHumanTitle(t *testing.T) {
	info := session.Info{Title: "this is a very long session title that should be truncated"}

	got := sessionListTitle(info)
	if got != "this is a very long session..." {
		t.Fatalf("sessionListTitle(truncate) = %q, want %q", got, "this is a very long session...")
	}
}

func TestBuildResumeCommandUsesResolvedProviderCommand(t *testing.T) {
	cfg := &config.City{
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

	info := session.Info{
		Template: "mayor",
		Command:  "gemini --approval-mode yolo",
		Provider: "wrapped",
		WorkDir:  "/tmp/workdir",
	}

	cmd, hints := buildResumeCommand(t.TempDir(), cfg, info, "", io.Discard)
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

func TestBuildResumeCommandIncludesSettingsAndDefaultArgs(t *testing.T) {
	cityDir := t.TempDir()
	// Write a .gc/settings.json so settingsArgs finds it.
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcDir, "settings.json"), []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor"},
		},
	}
	info := session.Info{
		Template:   "mayor",
		Command:    "claude",
		Provider:   "claude",
		WorkDir:    "/tmp/workdir",
		SessionKey: "abc-123",
		ResumeFlag: "--resume",
	}

	cmd, _ := buildResumeCommand(cityDir, cfg, info, "", io.Discard)

	// Must include --settings pointing to .gc/settings.json.
	wantSettings := fmt.Sprintf("--settings %q", filepath.Join(gcDir, "settings.json"))
	if !strings.Contains(cmd, "--settings") {
		t.Fatalf("resume command missing --settings:\n  got: %s", cmd)
	}
	if !strings.Contains(cmd, wantSettings) {
		t.Fatalf("resume command has wrong --settings path:\n  got:  %s\n  want: ...%s...", cmd, wantSettings)
	}

	// Must include --resume flag.
	if !strings.Contains(cmd, "--resume abc-123") {
		t.Fatalf("resume command missing --resume flag:\n  got: %s", cmd)
	}

	// Must include default args (--dangerously-skip-permissions for claude).
	if !strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Fatalf("resume command missing default args:\n  got: %s", cmd)
	}
}

func TestBuildResumeCommandUsesBuiltinAncestorForClaudeSettings(t *testing.T) {
	cityDir := t.TempDir()
	base := "builtin:claude"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor", Provider: "claude-max"},
		},
		Providers: map[string]config.ProviderSpec{
			"claude-max": {Base: &base},
		},
	}
	info := session.Info{
		Template: "mayor",
		Command:  "claude",
		Provider: "claude-max",
		WorkDir:  "/tmp/workdir",
	}

	cmd, _ := buildResumeCommand(cityDir, cfg, info, "", io.Discard)

	wantSettings := fmt.Sprintf("--settings %q", filepath.Join(cityDir, ".gc", "settings.json"))
	if !strings.Contains(cmd, wantSettings) {
		t.Fatalf("wrapped Claude resume command missing settings:\n  got:  %s\n  want: ...%s...", cmd, wantSettings)
	}
}

func TestBuildResumeCommandIncludesWrappedCodexResumeDefaults(t *testing.T) {
	cityDir := t.TempDir()
	base := "builtin:codex"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", Provider: "codex-mini"},
		},
		Providers: map[string]config.ProviderSpec{
			"codex-mini": {
				Base:    &base,
				Command: "aimux",
				Args: []string{
					"run", "codex", "--",
					"--dangerously-bypass-approvals-and-sandbox",
					"-m", "gpt-5.3-codex-spark",
					"-c", "model_reasoning_effort=\"medium\"",
				},
				PathCheck:     "true",
				ResumeCommand: "aimux run codex -- --dangerously-bypass-approvals-and-sandbox -m gpt-5.3-codex-spark resume {{.SessionKey}}",
			},
		},
	}
	info := session.Info{
		Template:   "worker",
		Command:    "codex",
		Provider:   "codex-mini",
		WorkDir:    "/tmp/workdir",
		SessionKey: "abc-123",
	}

	cmd, _ := buildResumeCommand(cityDir, cfg, info, "", io.Discard)
	want := "aimux run codex -- --dangerously-bypass-approvals-and-sandbox -m gpt-5.3-codex-spark resume -c model_reasoning_effort=medium abc-123"
	if cmd != want {
		t.Fatalf("resume command = %q, want %q", cmd, want)
	}
}

func TestBuildResumeCommandProviderKindSkipsTemplateCollision(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "runner", Provider: "agent-provider"},
		},
		Providers: map[string]config.ProviderSpec{
			"runner": {
				Command:    "true",
				Args:       []string{"provider"},
				ResumeFlag: "--resume",
			},
			"agent-provider": {
				Command:    "true",
				Args:       []string{"agent"},
				ResumeFlag: "--resume",
			},
		},
	}
	info := session.Info{
		Template:   "runner",
		Command:    "stale",
		WorkDir:    "/tmp/workdir",
		SessionKey: "abc-123",
	}

	cmd, _ := buildResumeCommand(t.TempDir(), cfg, info, "provider", io.Discard)
	want := "true provider --resume abc-123"
	if cmd != want {
		t.Fatalf("resume command = %q, want %q", cmd, want)
	}
}

func TestSessionReason_FallsThroughToProviderForSleepingAttachment(t *testing.T) {
	provider := runtime.NewFake()
	if err := provider.Start(context.Background(), "sleeping-worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cfg := &config.City{}
	bead := beads.Bead{
		ID:     "gc-1",
		Status: "open",
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "sleeping-worker",
			"state":        "asleep",
			"sleep_reason": "idle-timeout",
		},
	}
	info := session.Info{
		ID:          "gc-1",
		Template:    "worker",
		State:       session.StateAsleep,
		SessionName: "sleeping-worker",
		Attached:    false,
	}
	wrapped := &attachmentCachingProvider{
		Provider: provider,
		cache: buildAttachmentCache([]session.Info{info}, func(info session.Info) (bool, error) {
			return info.SessionName == "sleeping-worker", nil
		}),
	}

	reason := sessionReason(
		info,
		map[string]beads.Bead{bead.ID: bead},
		cfg,
		wrapped,
		nil,
		nil,
	)
	if reason != string(WakeAttached) {
		t.Fatalf("sessionReason = %q, want %q", reason, WakeAttached)
	}
}

func TestSessionReason_OmitsExpiredLifecycleHold(t *testing.T) {
	bead := beads.Bead{
		ID:     "gc-1",
		Status: "open",
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "sleeping-worker",
			"state":        "asleep",
			"held_until":   time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		},
	}
	info := session.Info{
		ID:          "gc-1",
		Template:    "worker",
		State:       session.StateAsleep,
		SessionName: "sleeping-worker",
	}

	reason := sessionReason(
		info,
		map[string]beads.Bead{bead.ID: bead},
		nil,
		runtime.NewFake(),
		nil,
		nil,
	)
	if reason != "-" {
		t.Fatalf("sessionReason = %q, want - after expired hold", reason)
	}
}

func TestSessionReason_SuppressesWakeReasonsForHistoricalArchivedBead(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	bead := beads.Bead{
		ID:     "gc-1",
		Status: "open",
		Metadata: map[string]string{
			"template":                 "worker",
			"session_name":             "old-worker",
			"state":                    "archived",
			"continuity_eligible":      "false",
			"configured_named_session": "true",
			"configured_named_mode":    "always",
			"pin_awake":                "true",
		},
	}
	info := session.Info{
		ID:          "gc-1",
		Template:    "worker",
		State:       session.StateArchived,
		SessionName: "old-worker",
	}

	reason := sessionReason(
		info,
		map[string]beads.Bead{bead.ID: bead},
		cfg,
		runtime.NewFake(),
		nil,
		nil,
	)
	if reason != "-" {
		t.Fatalf("sessionReason = %q, want - for historical archived bead", reason)
	}
}

func TestAttachmentCachingProvider_DelegatesSleepCapability(t *testing.T) {
	provider := &attachmentAwareProvider{
		Fake:            runtime.NewFake(),
		sleepCapability: runtime.SessionSleepCapabilityTimedOnly,
	}
	wrapped := &attachmentCachingProvider{Provider: provider, cache: map[string]bool{}}

	if got := resolveSleepCapability(wrapped, "worker"); got != runtime.SessionSleepCapabilityTimedOnly {
		t.Fatalf("resolveSleepCapability = %q, want %q", got, runtime.SessionSleepCapabilityTimedOnly)
	}
}

func TestAttachmentCachingProvider_DelegatesPendingInteraction(t *testing.T) {
	provider := &attachmentAwareProvider{
		Fake: runtime.NewFake(),
		pending: &runtime.PendingInteraction{
			RequestID: "req-1",
			Kind:      "approval",
		},
	}
	wrapped := &attachmentCachingProvider{Provider: provider, cache: map[string]bool{}}

	if !pendingInteractionReady(wrapped, "worker") {
		t.Fatal("pendingInteractionReady should delegate to wrapped provider")
	}

	response := worker.InteractionResponse{RequestID: "req-1", Action: "approve"}
	if err := workerRespondSessionTargetWithConfig("", nil, provider, nil, "worker", response); err != nil {
		t.Fatalf("Respond error = %v", err)
	}
	if provider.responded.RequestID != response.RequestID || provider.responded.Action != response.Action {
		t.Fatalf("responded = %+v, want request_id=%q action=%q", provider.responded, response.RequestID, response.Action)
	}
}

func TestAttachmentCachingProvider_RejectsUnsupportedInteraction(t *testing.T) {
	wrapped := &attachmentCachingProvider{cache: map[string]bool{}}

	pending, err := workerSessionTargetPendingWithConfig("", nil, wrapped, nil, "worker")
	if err != nil {
		t.Fatalf("Pending error = %v, want nil for unsupported interaction", err)
	}
	if pending != nil {
		t.Fatalf("Pending = %+v, want nil for unsupported interaction", pending)
	}
	if err := workerRespondSessionTargetWithConfig("", nil, wrapped, nil, "worker", worker.InteractionResponse{Action: "approve"}); !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("Respond error = %v, want ErrInteractionUnsupported", err)
	}
}

func TestSessionNewAliasOwner_UsesConfiguredNamedIdentity(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor"},
			{Name: "worker", MaxActiveSessions: intPtr(3)},
		},
		NamedSessions: []config.NamedSession{
			{Template: "mayor"},
		},
	}

	if got := sessionNewAliasOwner(cfg, &cfg.Agents[0]); got != "mayor" {
		t.Fatalf("sessionNewAliasOwner(mayor) = %q, want mayor", got)
	}
	if got := sessionNewAliasOwner(cfg, &cfg.Agents[1]); got != "" {
		t.Fatalf("sessionNewAliasOwner(worker) = %q, want empty", got)
	}
}

func TestCmdSessionListJSONNoSessionsReturnsEmptyArray(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionList("", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionList(--json) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if strings.Contains(stdout.String(), "No sessions found") {
		t.Fatalf("stdout = %q, want JSON only", stdout.String())
	}
	var got []session.Info
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a JSON session array: %v; stdout=%q", err, stdout.String())
	}
	if got == nil {
		t.Fatalf("sessions JSON = nil, want empty array; stdout=%q", stdout.String())
	}
	if len(got) != 0 {
		t.Fatalf("sessions = %d, want 0; stdout=%q", len(got), stdout.String())
	}
}

// TestCmdSessionList_RendersLastNudgeColumn pins the table rendering of the
// "LAST NUDGE" column added by the warm-idle ACP nudge fix: the header is
// emitted, sessions stamped with metadata.last_nudge_delivered_at render as
// "<duration> ago", and sessions without the stamp fall back to "-".
func TestCmdSessionList_RendersLastNudgeColumn(t *testing.T) {
	clearGCEnv(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}

	nudgeStamp := time.Now().Add(-2*time.Hour - 30*time.Minute).UTC().Format(time.RFC3339)
	if _, err := store.Create(beads.Bead{
		Title:  "nudged-session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":                       "nudged-session",
			"template":                           "worker",
			"state":                              "asleep",
			session.MetadataLastNudgeDeliveredAt: nudgeStamp,
		},
	}); err != nil {
		t.Fatalf("store.Create(nudged session bead): %v", err)
	}

	if _, err := store.Create(beads.Bead{
		Title:  "quiet-session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "quiet-session",
			"template":     "worker",
			"state":        "asleep",
		},
	}); err != nil {
		t.Fatalf("store.Create(quiet session bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionList("", "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionList() = %d, want 0; stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 1 || !strings.Contains(lines[0], "LAST NUDGE") {
		t.Fatalf("missing LAST NUDGE column header; first line = %q\nfull output:\n%s", firstLine(lines), out)
	}
	if !strings.Contains(out, "2h ago") {
		t.Fatalf("output missing formatted LAST NUDGE (want %q) for nudged session:\n%s", "2h ago", out)
	}

	nudgedRow := findRowContaining(lines, "nudged-session")
	if nudgedRow == "" {
		t.Fatalf("output missing row for nudged session:\n%s", out)
	}
	if got := lastField(nudgedRow); got != "ago" {
		t.Fatalf("nudged-session row last field = %q, want %q; row=%q", got, "ago", nudgedRow)
	}

	quietRow := findRowContaining(lines, "quiet-session")
	if quietRow == "" {
		t.Fatalf("output missing row for quiet session:\n%s", out)
	}
	if got := lastField(quietRow); got != "-" {
		t.Fatalf("quiet-session LAST NUDGE = %q, want %q; row=%q", got, "-", quietRow)
	}
}

func TestCmdSessionListJSONOmitZeroLastNudgeDeliveredAt(t *testing.T) {
	clearGCEnv(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}

	nudgeStamp := time.Now().Add(-5 * time.Minute).UTC().Truncate(time.Second)
	if _, err := store.Create(beads.Bead{
		Title:  "nudged-session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":                       "nudged-session",
			"template":                           "worker",
			"state":                              "asleep",
			session.MetadataLastNudgeDeliveredAt: nudgeStamp.Format(time.RFC3339),
		},
	}); err != nil {
		t.Fatalf("store.Create(nudged session bead): %v", err)
	}

	if _, err := store.Create(beads.Bead{
		Title:  "quiet-session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "quiet-session",
			"template":     "worker",
			"state":        "asleep",
		},
	}); err != nil {
		t.Fatalf("store.Create(quiet session bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionList("", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionList(--json) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), `"last_nudge_delivered_at": "0001-01-01`) {
		t.Fatalf("stdout contains zero-time last nudge timestamp:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "LastNudgeDeliveredAt") {
		t.Fatalf("stdout uses Go field name for last nudge timestamp:\n%s", stdout.String())
	}

	var rows []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("stdout is not a JSON session array: %v; stdout=%q", err, stdout.String())
	}
	quiet := sessionListJSONRowBySessionName(rows, "quiet-session")
	if quiet == nil {
		t.Fatalf("missing quiet-session row in JSON output:\n%s", stdout.String())
	}
	if _, ok := quiet["last_nudge_delivered_at"]; ok {
		t.Fatalf("quiet-session last_nudge_delivered_at present, want omitted: %#v", quiet)
	}

	nudged := sessionListJSONRowBySessionName(rows, "nudged-session")
	if nudged == nil {
		t.Fatalf("missing nudged-session row in JSON output:\n%s", stdout.String())
	}
	if got, want := nudged["last_nudge_delivered_at"], nudgeStamp.Format(time.RFC3339); got != want {
		t.Fatalf("nudged-session last_nudge_delivered_at = %#v, want %q; row=%#v", got, want, nudged)
	}
}

func sessionListJSONRowBySessionName(rows []map[string]any, name string) map[string]any {
	for _, row := range rows {
		if row["SessionName"] == name {
			return row
		}
	}
	return nil
}

func firstLine(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

func findRowContaining(lines []string, needle string) string {
	for _, line := range lines {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

func lastField(row string) string {
	fields := strings.Fields(row)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

func TestCmdSessionNew_AllowsReservedNamedAliasWithController(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-new-")
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	sockPath := filepath.Join(cityDir, ".gc", "controller.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(%q): %v", sockPath, err)
	}
	defer lis.Close() //nolint:errcheck

	commands := make(chan string, 3)
	errCh := make(chan error, 1)
	go func() {
		defer close(commands)
		for i := 0; i < 3; i++ {
			conn, err := lis.Accept()
			if err != nil {
				errCh <- err
				return
			}
			buf := make([]byte, 64)
			n, err := conn.Read(buf)
			if err != nil {
				conn.Close() //nolint:errcheck
				errCh <- err
				return
			}
			cmd := string(buf[:n])
			commands <- cmd
			reply := "ok\n"
			if cmd == "ping\n" {
				reply = "123\n"
			}
			if _, err := conn.Write([]byte(reply)); err != nil {
				conn.Close() //nolint:errcheck
				errCh <- err
				return
			}
			conn.Close() //nolint:errcheck
		}
	}()

	var stdout, stderr bytes.Buffer
	if code := cmdSessionNew([]string{"mayor"}, "mayor", "", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionNew(controller) = %d, want 0; stderr=%s", code, stderr.String())
	}

	gotCommands := make([]string, 0, 3)
	deadline := time.After(2 * time.Second)
	for len(gotCommands) < 3 {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("controller socket: %v", err)
			}
		case cmd, ok := <-commands:
			if !ok {
				if len(gotCommands) != 3 {
					t.Fatalf("controller commands = %v, want ping plus 2 pokes", gotCommands)
				}
				break
			}
			gotCommands = append(gotCommands, cmd)
		case <-deadline:
			t.Fatalf("timed out waiting for controller pokes, got %v", gotCommands)
		}
	}
	wantCommands := []string{"ping\n", "poke\n", "poke\n"}
	for i, want := range wantCommands {
		if gotCommands[i] != want {
			t.Fatalf("controller command %d = %q, want %q", i, gotCommands[i], want)
		}
	}

	b := onlySessionBead(t, cityDir)
	if got := b.Metadata["alias"]; got != "mayor" {
		t.Fatalf("alias = %q, want mayor", got)
	}
	if got := b.Metadata["state"]; got != "creating" {
		t.Fatalf("state = %q, want creating", got)
	}
}

func TestCmdSessionNew_AllowsReservedNamedAliasWithoutController(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionNew([]string{"mayor"}, "mayor", "", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionNew(fallback) = %d, want 0; stderr=%s", code, stderr.String())
	}

	b := onlySessionBead(t, cityDir)
	if got := b.Metadata["alias"]; got != "mayor" {
		t.Fatalf("alias = %q, want mayor", got)
	}
	if got := b.Metadata["session_name"]; got == "" {
		t.Fatal("session_name should be populated on fallback create")
	}
}

func TestCmdSessionNew_IgnoresUnmanagedSupervisorSocket(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))
	t.Setenv("XDG_RUNTIME_DIR", shortSocketTempDir(t, "gc-run-"))

	cityDir := shortSocketTempDir(t, "gc-session-city-")
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	if err := os.MkdirAll(filepath.Dir(supervisorSocketPath()), 0o755); err != nil {
		t.Fatalf("MkdirAll(supervisor socket dir): %v", err)
	}
	lis, err := net.Listen("unix", supervisorSocketPath())
	if err != nil {
		t.Fatalf("Listen(%q): %v", supervisorSocketPath(), err)
	}
	defer lis.Close() //nolint:errcheck

	commandCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close() //nolint:errcheck
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			errCh <- err
			return
		}
		commandCh <- string(buf[:n])
	}()

	var stdout, stderr bytes.Buffer
	if code := cmdSessionNew([]string{"mayor"}, "mayor", "", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionNew(unmanaged supervisor) = %d, want 0; stderr=%s", code, stderr.String())
	}

	select {
	case cmd := <-commandCh:
		t.Fatalf("unexpected supervisor command %q for unmanaged city", cmd)
	case err := <-errCh:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("supervisor socket accept/read: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
	}

	b := onlySessionBead(t, cityDir)
	if got := b.Metadata["session_name"]; got == "" {
		t.Fatal("session_name should be populated on direct fallback create")
	}
	if got := b.Metadata["state"]; got == "creating" {
		t.Fatalf("state = %q, want direct-start state (not creating)", got)
	}
}

func writeNamedSessionCityTOML(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	data := []byte(`[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
provider = "codex"
start_command = "echo"

[[named_session]]
template = "mayor"
`)
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), data, 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
}

func writePoolSessionCityTOML(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	rigRoot := filepath.Join(dir, "repos", "demo")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig root): %v", err)
	}
	data := []byte(fmt.Sprintf(`[workspace]
name = "test-city"

[beads]
provider = "file"

[[rigs]]
name = "demo"
path = %q

[[agent]]
name = "ant"
dir = "demo"
provider = "codex"
start_command = "echo"
work_dir = ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}"
min_active_sessions = 0
max_active_sessions = 4
`, rigRoot))
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), data, 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
}

func writePoolACPSessionCityTOML(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	rigRoot := filepath.Join(dir, "repos", "demo")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig root): %v", err)
	}
	data := []byte(fmt.Sprintf(`[workspace]
name = "test-city"

[beads]
provider = "file"

[[rigs]]
name = "demo"
path = %q

[[agent]]
name = "ant"
dir = "demo"
provider = "stub"
session = "acp"
work_dir = ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}"
min_active_sessions = 0
max_active_sessions = 4

[providers.stub]
command = "/bin/echo"
path_check = "true"
supports_acp = true
acp_command = "/bin/echo"
acp_args = ["acp"]
`, rigRoot))
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), data, 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
}

func writePoolProviderDefaultACPSessionCityTOML(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	rigRoot := filepath.Join(dir, "repos", "demo")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig root): %v", err)
	}
	data := []byte(fmt.Sprintf(`[workspace]
name = "test-city"

[beads]
provider = "file"

[[rigs]]
name = "demo"
path = %q

[[agent]]
name = "ant"
dir = "demo"
provider = "custom-acp"
work_dir = ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}"
min_active_sessions = 0
max_active_sessions = 4

[providers.custom-acp]
command = "/bin/echo"
path_check = "true"
supports_acp = true
acp_command = "/bin/echo"
acp_args = ["acp"]
`, rigRoot))
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), data, 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
}

func writePoolACPCityExplicitTmuxAgentTOML(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	rigRoot := filepath.Join(dir, "repos", "demo")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig root): %v", err)
	}
	data := []byte(fmt.Sprintf(`[workspace]
name = "test-city"

[beads]
provider = "file"

[session]
provider = "acp"

[[rigs]]
name = "demo"
path = %q

[[agent]]
name = "ant"
dir = "demo"
provider = "codex"
session = "tmux"
work_dir = ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}"
min_active_sessions = 0
max_active_sessions = 4
`, rigRoot))
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), data, 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
}

func sessionBeads(t *testing.T, cityDir string) []beads.Bead {
	t.Helper()
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	return all
}

func onlySessionBead(t *testing.T, cityDir string) beads.Bead {
	t.Helper()
	all := sessionBeads(t, cityDir)
	if len(all) != 1 {
		t.Fatalf("session beads = %d, want 1", len(all))
	}
	return all[0]
}

// --- Auto-title tests for issue #500 ---

func TestCmdSessionNew_AutoTitleFromMessage(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	// Force provider resolution to fail so auto-title falls back to
	// truncation deterministically — prevents flaky auto-detection from PATH.
	t.Setenv("PATH", "")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	var stdout, stderr bytes.Buffer
	code := cmdSessionNew([]string{"mayor"}, "mayor", "", "fix the login redirect loop", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdSessionNew = %d, want 0; stderr=%s", code, stderr.String())
	}

	b := onlySessionBead(t, cityDir)
	// With no provider available, MaybeGenerateTitleAsync truncates the
	// message as the immediate title.
	if b.Title == "mayor" {
		t.Fatalf("title should be auto-generated from message, got template name %q", b.Title)
	}
	if !strings.Contains(b.Title, "fix the login redirect loop") {
		t.Fatalf("title = %q, want to contain message text", b.Title)
	}
}

func TestCmdSessionNew_ExplicitTitlePreserved(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	var stdout, stderr bytes.Buffer
	code := cmdSessionNew([]string{"mayor"}, "mayor", "my explicit title", "some message", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdSessionNew = %d, want 0; stderr=%s", code, stderr.String())
	}

	b := onlySessionBead(t, cityDir)
	// Explicit title should be preserved; auto-title should NOT overwrite it.
	if b.Title != "my explicit title" {
		t.Fatalf("title = %q, want %q", b.Title, "my explicit title")
	}
}

func TestCmdSessionNew_NoMessageKeepsTemplateName(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	var stdout, stderr bytes.Buffer
	code := cmdSessionNew([]string{"mayor"}, "mayor", "", "", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdSessionNew = %d, want 0; stderr=%s", code, stderr.String())
	}

	b := onlySessionBead(t, cityDir)
	// No message → no auto-title → keeps default (template name or similar).
	if b.Title == "" {
		t.Fatal("title should not be empty")
	}
}

func TestMaybeAutoTitle_NilProviderFallsBackToTruncation(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Title: "template-name"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var stderr bytes.Buffer
	maybeAutoTitle(store, b.ID, "", "fix the login redirect loop", nil, "", &stderr)

	// MaybeGenerateTitleAsync sets the truncated title synchronously before
	// starting the goroutine, and generateTitle(provider=nil) falls back to
	// the same truncation. Assert immediately — no polling needed.
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title == "template-name" {
		t.Fatalf("title unchanged; want auto-generated from message")
	}
	if !strings.Contains(got.Title, "fix the login redirect loop") {
		t.Fatalf("title = %q, want to contain message text", got.Title)
	}
}

func TestMaybeAutoTitle_ExplicitTitleSkipsGeneration(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Title: "original"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var stderr bytes.Buffer
	maybeAutoTitle(store, b.ID, "explicit", "some message", nil, "", &stderr)

	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "original" {
		t.Fatalf("title = %q, want unchanged %q", got.Title, "original")
	}
}

func TestMaybeAutoTitle_EmptyMessageSkipsGeneration(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Title: "original"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var stderr bytes.Buffer
	maybeAutoTitle(store, b.ID, "", "", nil, "", &stderr)

	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "original" {
		t.Fatalf("title = %q, want unchanged %q", got.Title, "original")
	}
}

func TestResolvedSessionCommandIncludesDefaultsAndSettings(t *testing.T) {
	cityPath := t.TempDir()
	settingsDir := filepath.Join(cityPath, ".gc")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatalf("mkdir settings dir: %v", err)
	}
	settingsPath := filepath.Join(settingsDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	claude := config.BuiltinProviders()["claude"]
	resolved := &config.ResolvedProvider{
		Name:              "claude",
		Command:           claude.Command,
		OptionsSchema:     claude.OptionsSchema,
		EffectiveDefaults: config.ComputeEffectiveDefaults(claude.OptionsSchema, claude.OptionDefaults, nil),
	}

	got, err := resolvedSessionCommand(cityPath, resolved, nil, "")
	if err != nil {
		t.Fatalf("resolvedSessionCommand: %v", err)
	}
	if !strings.Contains(got, "--dangerously-skip-permissions") {
		t.Fatalf("command %q should include unrestricted default permissions", got)
	}
	if !strings.Contains(got, "--effort max") {
		t.Fatalf("command %q should include effort=max default", got)
	}
	wantSettings := `--settings "` + settingsPath + `"`
	if !strings.Contains(got, wantSettings) {
		t.Fatalf("command %q should include %s", got, wantSettings)
	}
}

func TestResolvedSessionCommandAppliesOverridesOverDefaults(t *testing.T) {
	cityPath := t.TempDir()
	claude := config.BuiltinProviders()["claude"]
	resolved := &config.ResolvedProvider{
		Name:              "claude",
		Command:           claude.Command,
		OptionsSchema:     claude.OptionsSchema,
		EffectiveDefaults: config.ComputeEffectiveDefaults(claude.OptionsSchema, claude.OptionDefaults, nil),
	}

	got, err := resolvedSessionCommand(cityPath, resolved, map[string]string{
		"permission_mode": "plan",
		"effort":          "low",
	}, "")
	if err != nil {
		t.Fatalf("resolvedSessionCommand: %v", err)
	}
	if strings.Contains(got, "--dangerously-skip-permissions") {
		t.Fatalf("command %q should not keep unrestricted default when overridden", got)
	}
	if !strings.Contains(got, "--permission-mode plan") {
		t.Fatalf("command %q should include plan permission override", got)
	}
	if !strings.Contains(got, "--effort low") {
		t.Fatalf("command %q should include effort=low override", got)
	}
}

func TestResolvedSessionCommandUsesACPTransportCommand(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Name:       "opencode",
		Command:    "/bin/echo",
		ACPCommand: "/bin/echo",
		ACPArgs:    []string{"acp"},
	}

	got, err := resolvedSessionCommand("", resolved, nil, "acp")
	if err != nil {
		t.Fatalf("resolvedSessionCommand: %v", err)
	}
	if got != "/bin/echo acp" {
		t.Fatalf("command = %q, want %q", got, "/bin/echo acp")
	}
}

func TestValidateResolvedSessionTransportRejectsUnsupportedACPProvider(t *testing.T) {
	err := validateResolvedSessionTransport(&config.ResolvedProvider{
		Name: "opencode",
	}, "acp", &transportCapableSessionProvider{Fake: runtime.NewFake()})
	if err == nil || !strings.Contains(err.Error(), "does not support ACP transport") {
		t.Fatalf("validateResolvedSessionTransport() error = %v, want provider ACP support error", err)
	}
}

func TestValidateResolvedSessionTransportRejectsUnroutableACPProvider(t *testing.T) {
	err := validateResolvedSessionTransport(&config.ResolvedProvider{
		Name:        "opencode",
		SupportsACP: true,
	}, "acp", runtime.NewFake())
	if err == nil || !strings.Contains(err.Error(), "requires ACP transport") {
		t.Fatalf("validateResolvedSessionTransport() error = %v, want ACP routing error", err)
	}
}

func TestValidateResolvedSessionTransportAcceptsRoutedACPProvider(t *testing.T) {
	if err := validateResolvedSessionTransport(&config.ResolvedProvider{
		Name:        "opencode",
		SupportsACP: true,
	}, "acp", &transportCapableSessionProvider{Fake: runtime.NewFake()}); err != nil {
		t.Fatalf("validateResolvedSessionTransport() = %v, want nil", err)
	}
}

func TestValidateResolvedSessionTransportAcceptsTmuxTransport(t *testing.T) {
	if err := validateResolvedSessionTransport(&config.ResolvedProvider{
		Name: "opencode",
	}, config.SessionTransportTmux, runtime.NewFake()); err != nil {
		t.Fatalf("validateResolvedSessionTransport() = %v, want nil", err)
	}
}

func TestValidateResolvedSessionTransportRejectsTmuxWhenSessionProviderIsACPOnly(t *testing.T) {
	err := validateResolvedSessionTransport(&config.ResolvedProvider{
		Name: "opencode",
	}, config.SessionTransportTmux, &transportCapableSessionProvider{Fake: runtime.NewFake()})
	if err == nil || !strings.Contains(err.Error(), "requires tmux transport") {
		t.Fatalf("validateResolvedSessionTransport() error = %v, want tmux routing error", err)
	}
}

func TestValidateResolvedSessionTransportRejectsUnknownTransport(t *testing.T) {
	err := validateResolvedSessionTransport(&config.ResolvedProvider{
		Name: "opencode",
	}, "stdio", runtime.NewFake())
	if err == nil || !strings.Contains(err.Error(), "unknown session transport") {
		t.Fatalf("validateResolvedSessionTransport() error = %v, want unknown transport error", err)
	}
}

func TestValidateResolvedSessionTransportRejectsRoutedProviderWhenTransportCapabilityDisablesACP(t *testing.T) {
	err := validateResolvedSessionTransport(&config.ResolvedProvider{
		Name:        "opencode",
		SupportsACP: true,
	}, "acp", &routedRejectingSessionProvider{Fake: runtime.NewFake()})
	if err == nil || !strings.Contains(err.Error(), "requires ACP transport") {
		t.Fatalf("validateResolvedSessionTransport() error = %v, want ACP routing error", err)
	}
}
