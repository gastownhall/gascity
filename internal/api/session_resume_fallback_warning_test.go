package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// resumeFallbackProvider is the bare provider the resume fallback fixtures
// resolve: a configured launch command with one closed option.
const resumeFallbackProvider = "fallback-probe"

func resumeFallbackProviderSpec() config.ProviderSpec {
	return config.ProviderSpec{
		Command:   "/bin/echo",
		Args:      []string{"--configured"},
		PathCheck: "true",
		OptionsSchema: []config.ProviderOption{{
			Key:  "effort",
			Type: "select",
			Choices: []config.OptionChoice{{
				Value:    "high",
				FlagArgs: []string{"--effort", "high"},
			}},
		}},
	}
}

// newResumeFallbackSession stores a session for provider with storedCommand and
// the given template_overrides metadata, and returns a server over it plus the
// stored session's info. Both resolvers read the overrides from the store, as a
// real resume does.
func newResumeFallbackSession(t *testing.T, provider config.ProviderSpec, storedCommand, overrides string) (*Server, session.Info) {
	t.Helper()
	fs := newSessionFakeState(t)
	fs.cfg.Providers[resumeFallbackProvider] = provider
	mgr := session.NewManagerWithOptions(beadstest.Sessions(fs.cityBeadStore), fs.sp, session.WithCityPath(fs.cityPath))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template:  resumeFallbackProvider,
		Title:     "chat",
		Command:   storedCommand,
		WorkDir:   t.TempDir(),
		Provider:  resumeFallbackProvider,
		Hints:     runtime.Config{},
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if overrides != "" {
		if err := fs.cityBeadStore.SetMetadata(info.ID, "template_overrides", overrides); err != nil {
			t.Fatalf("SetMetadata(template_overrides): %v", err)
		}
	}
	return New(fs), info
}

// resumeResolver drives one API resume resolver and reports the launch
// command it selected and the work dir its runtime hints carry.
type resumeResolver struct {
	name    string
	resolve func(srv *Server, info session.Info) (command, workDir string, err error)
}

var buildSessionResumeResolver = resumeResolver{
	name: "buildSessionResume",
	resolve: func(srv *Server, info session.Info) (string, string, error) {
		cmd, hints, err := srv.buildSessionResume(info)
		return cmd, hints.WorkDir, err
	},
}

var workerSessionRuntimeResolver = resumeResolver{
	name: "resolveWorkerSessionRuntimeWithMetadata",
	resolve: func(srv *Server, info session.Info) (string, string, error) {
		rt, err := srv.resolveWorkerSessionRuntimeWithMetadata(info, "", nil)
		if err != nil {
			return "", "", err
		}
		if rt == nil {
			return "", "", errNilResolvedRuntime
		}
		return rt.Command, rt.WorkDir, nil
	},
}

var errNilResolvedRuntime = errors.New("resolver returned nil runtime")

// captureResumeLog runs fn with the standard logger writing, without
// timestamps, to a buffer and returns what it logged.
func captureResumeLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	restore := captureLog(t, &buf)
	t.Cleanup(restore)
	fn()
	restore()
	return buf.String()
}

func resumeFallbackWarningLine(sessionID string) string {
	return fmt.Sprintf("gc api: warning: session %q: provider launch command resolution failed; using fallback launch command\n", sessionID)
}

func TestSessionResumeLaunchCommandFallbackWarning(t *testing.T) {
	bareProvider := config.ProviderSpec{PathCheck: "true"}
	cases := []struct {
		name          string
		provider      config.ProviderSpec
		storedCommand string
		overrides     string
		wantCommand   string
	}{
		{
			name:          "invalid closed option keeps stored command",
			provider:      resumeFallbackProviderSpec(),
			storedCommand: "/bin/echo --stored",
			overrides:     `{"effort":"bogus"}`,
			wantCommand:   "/bin/echo --stored",
		},
		{
			name:          "malformed overrides keep stored command",
			provider:      resumeFallbackProviderSpec(),
			storedCommand: "/bin/echo --stored",
			overrides:     `{`,
			wantCommand:   "/bin/echo --stored",
		},
		{
			name:          "invalid closed option without stored command uses configured command",
			provider:      resumeFallbackProviderSpec(),
			storedCommand: "",
			overrides:     `{"effort":"bogus"}`,
			wantCommand:   "/bin/echo --configured",
		},
		{
			name:          "no configured or stored command uses provider name",
			provider:      bareProvider,
			storedCommand: "",
			overrides:     "",
			wantCommand:   resumeFallbackProvider,
		},
	}
	for _, resolver := range []resumeResolver{buildSessionResumeResolver, workerSessionRuntimeResolver} {
		for _, tc := range cases {
			t.Run(resolver.name+"/"+tc.name, func(t *testing.T) {
				srv, info := newResumeFallbackSession(t, tc.provider, tc.storedCommand, tc.overrides)
				// Each failing call warns once: repeated resolutions repeat the warning.
				for call := 1; call <= 2; call++ {
					var (
						command, workDir string
						err              error
					)
					out := captureResumeLog(t, func() { command, workDir, err = resolver.resolve(srv, info) })
					if err != nil {
						t.Fatalf("call %d: %s: %v", call, resolver.name, err)
					}
					if command != tc.wantCommand {
						t.Fatalf("call %d: command = %q, want %q", call, command, tc.wantCommand)
					}
					if workDir != info.WorkDir {
						t.Fatalf("call %d: hints work dir = %q, want %q", call, workDir, info.WorkDir)
					}
					if want := resumeFallbackWarningLine(info.ID); out != want {
						t.Fatalf("call %d: log output = %q, want exactly %q", call, out, want)
					}
				}
			})
		}
	}
}

func TestSessionResumeLaunchCommandFallbackWarningOmitsPrivateData(t *testing.T) {
	const (
		rejectedValueMarker = "private-rejected-value-7c1e"
		unknownKeyMarker    = "private-unknown-key-4b9d"
		storedCommandMarker = "private-stored-settings-2a6f"
		configuredMarker    = "private-configured-arg-9e3c"
		envMarker           = "private-env-value-5d8a"
	)
	provider := resumeFallbackProviderSpec()
	provider.Args = []string{"--token", configuredMarker}
	provider.Env = map[string]string{"FALLBACK_PROBE_SECRET": envMarker}
	storedCommand := "/bin/echo --settings /tmp/" + storedCommandMarker + "/settings.json"
	cases := []struct {
		name      string
		overrides string
	}{
		{name: "rejected closed option value", overrides: `{"effort":"` + rejectedValueMarker + `"}`},
		{name: "unknown option key", overrides: `{"` + unknownKeyMarker + `":"high"}`},
	}
	for _, resolver := range []resumeResolver{buildSessionResumeResolver, workerSessionRuntimeResolver} {
		for _, tc := range cases {
			t.Run(resolver.name+"/"+tc.name, func(t *testing.T) {
				srv, info := newResumeFallbackSession(t, provider, storedCommand, tc.overrides)
				var (
					command string
					err     error
				)
				out := captureResumeLog(t, func() { command, _, err = resolver.resolve(srv, info) })
				if err != nil {
					t.Fatalf("%s: %v", resolver.name, err)
				}
				if command != storedCommand {
					t.Fatalf("command = %q, want %q", command, storedCommand)
				}
				if want := resumeFallbackWarningLine(info.ID); out != want {
					t.Fatalf("log output = %q, want exactly %q", out, want)
				}
				for _, marker := range []string{rejectedValueMarker, unknownKeyMarker, storedCommandMarker, configuredMarker, envMarker} {
					if strings.Contains(out, marker) {
						t.Fatalf("warning leaks private marker %q: %q", marker, out)
					}
				}
			})
		}
	}
}

func TestSessionResumeResolvedLaunchCommandDoesNotWarn(t *testing.T) {
	declaredOption := resumeFallbackProviderSpec()
	declaredOption.Args = nil
	cases := []struct {
		name          string
		provider      config.ProviderSpec
		storedCommand string
		overrides     string
		wantCommand   string
	}{
		{
			name:          "valid declared option is applied",
			provider:      declaredOption,
			storedCommand: "/bin/echo",
			overrides:     `{"effort":"high"}`,
			wantCommand:   "/bin/echo --effort high",
		},
		{
			name:          "configured command without overrides",
			provider:      resumeFallbackProviderSpec(),
			storedCommand: "/bin/echo --configured",
			overrides:     "",
			wantCommand:   "/bin/echo --configured",
		},
	}
	for _, resolver := range []resumeResolver{buildSessionResumeResolver, workerSessionRuntimeResolver} {
		for _, tc := range cases {
			t.Run(resolver.name+"/"+tc.name, func(t *testing.T) {
				srv, info := newResumeFallbackSession(t, tc.provider, tc.storedCommand, tc.overrides)
				var (
					command string
					err     error
				)
				out := captureResumeLog(t, func() { command, _, err = resolver.resolve(srv, info) })
				if err != nil {
					t.Fatalf("%s: %v", resolver.name, err)
				}
				if command != tc.wantCommand {
					t.Fatalf("command = %q, want %q", command, tc.wantCommand)
				}
				if out != "" {
					t.Fatalf("log output = %q, want no warning", out)
				}
			})
		}
	}
}

// The ACP transport probe compares candidate commands without selecting one,
// so its resolution fallbacks, and the shared low-level resolver, stay quiet.
func TestSessionRuntimeTransportProbeDoesNotWarnOnFallback(t *testing.T) {
	srv := New(newSessionFakeState(t))
	resolved := &config.ResolvedProvider{
		Name:       resumeFallbackProvider,
		Command:    "/bin/echo",
		ACPCommand: "/bin/echo-acp",
	}
	metadata := map[string]string{
		"started_config_hash": "started-hash",
		"template_overrides":  `{`,
	}
	info := session.Info{
		ID:       "gc-probe",
		Template: resumeFallbackProvider,
		Command:  "/bin/echo --stored",
		Provider: resumeFallbackProvider,
		WorkDir:  t.TempDir(),
	}

	out := captureResumeLog(t, func() {
		srv.startedConfigHashProvesACPTransport(info, metadata, resolved, info.WorkDir, "acp", "")
	})
	if out != "" {
		t.Fatalf("transport probe log output = %q, want no warning", out)
	}

	var err error
	out = captureResumeLog(t, func() {
		_, err = srv.resolvedSessionRuntimeCommand(resolved, "", info.Command, metadata)
	})
	if err == nil {
		t.Fatal("resolvedSessionRuntimeCommand with malformed overrides succeeded, want error")
	}
	if out != "" {
		t.Fatalf("resolvedSessionRuntimeCommand log output = %q, want no warning", out)
	}
}
