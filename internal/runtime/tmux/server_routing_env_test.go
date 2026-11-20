package tmux

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
)

// serverEnvExecutor answers the tmux server's global-environment read
// (`show-environment -g`) with global and globalErr, and every other command
// as its stagedRecorder does. It records every call in calls, and a
// new-session staged through a private file in staged.
type serverEnvExecutor struct {
	*stagedRecorder
	global    string
	globalErr error
}

func newServerEnvExecutor(out, global string, globalErr error) *serverEnvExecutor {
	return &serverEnvExecutor{stagedRecorder: &stagedRecorder{fakeExecutor: &fakeExecutor{out: out}}, global: global, globalErr: globalErr}
}

func (e *serverEnvExecutor) execute(args []string) (string, error) {
	if i := slices.Index(args, "show-environment"); i >= 0 && slices.Contains(args[i:], "-g") {
		e.calls = append(e.calls, slices.Clone(args))
		return e.global, e.globalErr
	}
	return e.stagedRecorder.execute(args)
}

func (e *serverEnvExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// staleServerGlobalEnv is a city tmux server's global environment after an
// earlier supervisor or shell started it with a provider's routing, as
// `show-environment -g` prints it: ANTHROPIC_ routing and a credential, a
// routing name marked removed, a provider setting, a name that is not a
// provider's, and the later lines of a multi-line value, one of which reads
// like a routing name that is no valid env name.
const staleServerGlobalEnv = "ANTHROPIC_API_KEY=sk-stale\n" +
	"ANTHROPIC_BASE_URL=https://stale.example.test\n" +
	"ANTHROPIC_DEFAULT_OPUS_MODEL=stale-opus\n" +
	"ANTHROPIC_MODEL=stale-model\n" +
	"HOME=/home/u\n" +
	"MOTD=first line\n" +
	"second line\n" +
	"ANTHROPIC_X;Y_URL=https://third.example.test\n" +
	"OLLAMA_HOST=http://stale.example.test:11434\n" +
	"-OPENAI_BASE_URL\n" +
	"OPENAI_ORG_ID=org-stale"

// markedRemoved returns the keys calls mark removed from a session env
// (`set-environment -r`), in call order.
func markedRemoved(calls [][]string) []string {
	var marked []string
	for _, call := range calls {
		if i := slices.Index(call, "set-environment"); i >= 0 && slices.Contains(call[i:], "-r") {
			marked = append(marked, call[len(call)-1])
		}
	}
	return marked
}

// callIndex returns the index of the first call that contains every arg, or
// -1.
func callIndex(calls [][]string, args ...string) int {
	return slices.IndexFunc(calls, func(call []string) bool {
		for _, arg := range args {
			if !slices.Contains(call, arg) {
				return false
			}
		}
		return true
	})
}

// createCall returns the index of the call that ran new-session, on argv or
// staged through a private file (Tmux.runNewSession), and its command line.
func createCall(e *serverEnvExecutor) (int, string) {
	if i := callIndex(e.calls, "new-session"); i >= 0 {
		return i, strings.Join(e.calls[i], " ")
	}
	if i := callIndex(e.calls, "source-file"); i >= 0 && len(e.staged) == 1 {
		return i, e.staged[0]
	}
	return -1, ""
}

// A city tmux server outlives the supervisor that started it and keeps that
// process's environment as its global environment, which every session
// inherits beneath its own env. So a server started when the shell exported
// ANTHROPIC_BASE_URL hands that endpoint to a later session whose env carries
// only a secrets.env token, whatever the latest supervisor install saw
// (#9257). Create and respawn read the server's global environment and
// withhold, like any other empty pin, every provider routing name it holds
// that the session env does not set: an `env -u` prefix on the created
// command, and a session-env removal that also covers the respawned agent.
// A routing name the env sets, explicitly or empty, is the env's to decide,
// and secret-class, setting-class and non-provider names, and names the
// server already marks removed, are left to the server.
func TestStartOpsWithholdServerHeldProviderRouting(t *testing.T) {
	paths := []struct {
		name string
		run  func(*tmuxStartOps, map[string]string) error
	}{
		{"create", func(ops *tmuxStartOps, env map[string]string) error {
			return ops.createSession("gc-test-server-routing", "", "claude", env)
		}},
		{"respawn", func(ops *tmuxStartOps, env map[string]string) error {
			return ops.respawnAgent("gc-test-server-routing", "/proj", "claude", env)
		}},
	}
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			exec := newServerEnvExecutor("agent\t@1.%0", staleServerGlobalEnv, nil)
			tm := NewTmux()
			tm.exec = exec
			env := map[string]string{
				"GC_CITY":                      "/tmp/city",
				"ANTHROPIC_AUTH_TOKEN":         "tok-file",
				"ANTHROPIC_DEFAULT_OPUS_MODEL": "configured-opus",
				"OLLAMA_HOST":                  "",
			}
			before := maps.Clone(env)

			if err := path.run(&tmuxStartOps{tm: tm}, env); err != nil {
				t.Fatalf("%s: %v", path.name, err)
			}

			launch, create := createCall(exec)
			if path.name == "respawn" {
				launch = callIndex(exec.calls, "respawn-pane")
			}
			if read := callIndex(exec.calls, "show-environment", "-g"); read < 0 || launch < 0 || read > launch {
				t.Fatalf("calls = %q, want the server's global environment read before the agent starts", exec.calls)
			}
			if got, want := markedRemoved(exec.calls), []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "OLLAMA_HOST"}; !slices.Equal(got, want) {
				t.Errorf("keys marked removed from the session env = %v, want %v", got, want)
			}
			if path.name == "create" {
				for _, want := range []string{"-u ANTHROPIC_BASE_URL", "-u ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL=configured-opus", "ANTHROPIC_AUTH_TOKEN=tok-file"} {
					if !strings.Contains(create, want) {
						t.Errorf("new-session = %q, want it to carry %q", create, want)
					}
				}
				for _, kept := range []string{"ANTHROPIC_API_KEY", "OPENAI_BASE_URL", "OPENAI_ORG_ID", "HOME", "MOTD", "ANTHROPIC_X", "-u ANTHROPIC_DEFAULT_OPUS_MODEL"} {
					if strings.Contains(create, kept) {
						t.Errorf("new-session = %q touches %q, which is not the server's routing to withhold", create, kept)
					}
				}
			}
			if !maps.Equal(env, before) {
				t.Errorf("caller's env = %v, want it unchanged %v", env, before)
			}
		})
	}
}

// With no tmux server running there is no global environment to inherit: the
// new-session starts the server from this process's environment, which the
// session env is built from. So create withholds nothing more than its env
// pins, and carries on.
func TestCreateSessionWithholdsNothingMoreWithoutServer(t *testing.T) {
	exec := newServerEnvExecutor("%1", "", ErrNoServer)
	tm := NewTmux()
	tm.exec = exec
	if err := (&tmuxStartOps{tm: tm}).createSession("gc-test-no-server", "", "claude", map[string]string{"GC_CITY": "/tmp/city", "GC_CONTROLLER_TOKEN": ""}); err != nil {
		t.Fatalf("createSession: %v", err)
	}
	if launch, _ := createCall(exec); launch < 0 {
		t.Fatalf("calls = %q, want new-session", exec.calls)
	}
	if got, want := markedRemoved(exec.calls), []string{"GC_CONTROLLER_TOKEN"}; !slices.Equal(got, want) {
		t.Errorf("keys marked removed from the session env = %v, want %v", got, want)
	}
}

// A server that answers but whose global environment cannot be read may hold
// routing the session must not inherit, so create and respawn fail closed:
// neither starts the agent nor touches the session env. "no current target"
// is the server answering, although its error wraps ErrNoServer.
func TestStartOpsFailClosedWhenServerEnvUnreadable(t *testing.T) {
	sentinel := errors.New("tmux show-environment: fork failed")
	for _, readErr := range []error{sentinel, ErrNoCurrentTarget} {
		for _, path := range []struct {
			name string
			run  func(*tmuxStartOps) error
		}{
			{"create", func(ops *tmuxStartOps) error {
				return ops.createSession("gc-test-unreadable", "", "claude", map[string]string{"GC_CITY": "/tmp/city"})
			}},
			{"respawn", func(ops *tmuxStartOps) error {
				return ops.respawnAgent("gc-test-unreadable", "/proj", "claude", map[string]string{"GC_CITY": "/tmp/city"})
			}},
		} {
			t.Run(path.name+"/"+readErr.Error(), func(t *testing.T) {
				exec := newServerEnvExecutor("agent\t@1.%0", "", readErr)
				tm := NewTmux()
				tm.exec = exec
				err := path.run(&tmuxStartOps{tm: tm})
				if !errors.Is(err, readErr) || !strings.Contains(err.Error(), "global environment") {
					t.Fatalf("%s error = %v, want the global environment read's %v", path.name, err, readErr)
				}
				if len(exec.calls) != 1 {
					t.Errorf("calls = %q, want only the global environment read", exec.calls)
				}
			})
		}
	}
}
