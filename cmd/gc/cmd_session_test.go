package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAttachNewSession(tt.noAttach, tt.transport); got != tt.want {
				t.Fatalf("shouldAttachNewSession(%v, %q) = %v, want %v", tt.noAttach, tt.transport, got, tt.want)
			}
		})
	}
}

func TestBuildAttachmentCache_OnlyCachesKnownActiveSessions(t *testing.T) {
	cache := buildAttachmentCache([]session.Info{
		{SessionName: "active-attached", State: session.StateActive, Attached: true},
		{SessionName: "active-detached", State: session.StateActive, Attached: false},
		{SessionName: "sleeping", State: session.StateAsleep, Attached: false},
		{SessionName: "suspended", State: session.StateSuspended, Attached: false},
		{State: session.StateActive, Attached: true},
	})

	if len(cache) != 2 {
		t.Fatalf("cache entries = %d, want 2", len(cache))
	}
	if got, ok := cache["active-attached"]; !ok || !got {
		t.Fatalf("cache[active-attached] = (%v, %v), want (true, true)", got, ok)
	}
	if got, ok := cache["active-detached"]; !ok || got {
		t.Fatalf("cache[active-detached] = (%v, %v), want (false, true)", got, ok)
	}
	if _, ok := cache["sleeping"]; ok {
		t.Fatal("sleeping session should not be cached")
	}
	if _, ok := cache["suspended"]; ok {
		t.Fatal("suspended session should not be cached")
	}
}

func TestSessionReason_FallsThroughToProviderForSleepingAttachment(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "sleeping-worker", runtime.Config{})
	sp.SetAttached("sleeping-worker", true)

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

	reason := sessionReason(
		info,
		map[string]beads.Bead{bead.ID: bead},
		cfg,
		&attachmentCachingProvider{
			Provider: sp,
			cache:    buildAttachmentCache([]session.Info{info}),
		},
		nil,
		nil,
	)
	if reason != string(WakeAttached) {
		t.Fatalf("sessionReason = %q, want %q", reason, WakeAttached)
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

	response := runtime.InteractionResponse{RequestID: "req-1", Action: "approve"}
	if err := wrapped.Respond("worker", response); err != nil {
		t.Fatalf("Respond error = %v", err)
	}
	if provider.responded.RequestID != response.RequestID || provider.responded.Action != response.Action {
		t.Fatalf("responded = %+v, want request_id=%q action=%q", provider.responded, response.RequestID, response.Action)
	}
}

func TestAttachmentCachingProvider_RejectsUnsupportedInteraction(t *testing.T) {
	wrapped := &attachmentCachingProvider{cache: map[string]bool{}}

	if _, err := wrapped.Pending("worker"); !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("Pending error = %v, want ErrInteractionUnsupported", err)
	}
	if err := wrapped.Respond("worker", runtime.InteractionResponse{Action: "approve"}); !errors.Is(err, runtime.ErrInteractionUnsupported) {
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

func TestCmdSessionNew_AllowsReservedNamedAliasWithController(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir, "test-city", "mayor")
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
	if code := cmdSessionNew([]string{"mayor"}, "mayor", "", true, &stdout, &stderr); code != 0 {
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
	writeNamedSessionCityTOML(t, cityDir, "test-city", "mayor")

	var stdout, stderr bytes.Buffer
	if code := cmdSessionNew([]string{"mayor"}, "mayor", "", true, &stdout, &stderr); code != 0 {
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
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir, "test-city", "mayor")

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
	if code := cmdSessionNew([]string{"mayor"}, "mayor", "", true, &stdout, &stderr); code != 0 {
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

func writeNamedSessionCityTOML(t *testing.T, dir, cityName, agentName string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	data := []byte(`[workspace]
name = "` + cityName + `"

[beads]
provider = "file"

[[agent]]
name = "` + agentName + `"
provider = "codex"
start_command = "echo"

[[named_session]]
template = "` + agentName + `"
`)
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), data, 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
}

func onlySessionBead(t *testing.T, cityDir string) beads.Bead {
	t.Helper()
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("session beads = %d, want 1", len(all))
	}
	return all[0]
}
