package k8s

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/guard"
)

// fakeTmux is a tmuxRunner that records every command and answers from a
// scripted reply.
type fakeTmux struct {
	calls [][]string
	reply func(args []string) (stdout, stderr string, err error)
}

func (f *fakeTmux) run(_ context.Context, args ...string) (string, string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if f.reply == nil {
		return "", "", nil
	}
	return f.reply(args)
}

// fakeTmuxEnv is a fakeTmux holding one session's environment, answering
// set-environment, show-environment and has-session as tmux does.
func fakeTmuxEnv(env map[string]string) *fakeTmux {
	return &fakeTmux{reply: func(args []string) (string, string, error) {
		exit := errors.New("exit status 1")
		switch args[0] {
		case "has-session":
			return "", "", nil
		case "set-environment":
			if args[3] == "-u" {
				delete(env, args[4])
				return "", "", nil
			}
			env[args[3]] = args[4]
			return "", "", nil
		case "show-environment":
			v, ok := env[args[3]]
			if !ok {
				return "", "unknown variable: " + args[3] + "\n", exit
			}
			return args[3] + "=" + v + "\n", "", nil
		}
		return "", "unknown command\n", exit
	}}
}

func TestNewPodLocalRequiresSessionIdentity(t *testing.T) {
	for _, self := range []string{"", "   ", "\t\n"} {
		if _, err := newPodLocal(self, (&fakeTmux{}).run); err == nil {
			t.Errorf("newPodLocal(%q) succeeded; want an error naming %s", self, PodSessionEnv)
		} else if !strings.Contains(err.Error(), PodSessionEnv) {
			t.Errorf("newPodLocal(%q) error = %v; want it to name %s", self, err, PodSessionEnv)
		}
	}
}

func TestPodLocalMetadataRoundTripOnOwnSession(t *testing.T) {
	env := map[string]string{}
	tm := fakeTmuxEnv(env)
	p, err := newPodLocal(" worker-1 ", tm.run)
	if err != nil {
		t.Fatalf("newPodLocal: %v", err)
	}
	for _, name := range []string{"worker-1", tmuxSession} {
		value := "a=b c " + name
		if err := p.SetMeta(name, "GC_DRAIN_ACK", value); err != nil {
			t.Fatalf("SetMeta(%q): %v", name, err)
		}
		got, err := p.GetMeta(name, "GC_DRAIN_ACK")
		if err != nil {
			t.Fatalf("GetMeta(%q): %v", name, err)
		}
		if got != value {
			t.Fatalf("GetMeta(%q) = %q; want %q", name, got, value)
		}
		if err := p.RemoveMeta(name, "GC_DRAIN_ACK"); err != nil {
			t.Fatalf("RemoveMeta(%q): %v", name, err)
		}
		got, err = p.GetMeta(name, "GC_DRAIN_ACK")
		if err != nil || got != "" {
			t.Fatalf("GetMeta(%q) after RemoveMeta = %q, %v; want empty, nil", name, got, err)
		}
	}
	// Every command addresses the pod's one session exactly, on the default
	// server: the session the controller's pod exec reaches.
	for _, call := range tm.calls {
		if len(call) < 3 || call[1] != "-t" || call[2] != "=main" {
			t.Errorf("tmux %v does not target =main", call)
		}
		for _, a := range call {
			if a == "-L" || a == "-S" {
				t.Errorf("tmux %v names a socket; want the pod's default server", call)
			}
		}
	}
	want := []string{"set-environment", "-t", "=main", "GC_DRAIN_ACK", "a=b c worker-1"}
	if !reflect.DeepEqual(tm.calls[0], want) {
		t.Errorf("first tmux call = %v; want %v", tm.calls[0], want)
	}
}

func TestPodLocalGetMetaDistinguishesUnsetFromFailure(t *testing.T) {
	exit := errors.New("exit status 1")
	cases := []struct {
		name         string
		stdout       string
		stderr       string
		err          error
		want         string
		wantErr      bool
		wantNotFound bool
	}{
		{name: "set", stdout: "K=v=w \n", want: "v=w "},
		{name: "explicitly unset", stdout: "-K\n", want: ""},
		{name: "never set", stderr: "unknown variable: K\n", err: exit, want: ""},
		{name: "missing session", stderr: "can't find session: =main\n", err: exit, wantErr: true, wantNotFound: true},
		{name: "missing server", stderr: "no server running on /tmp/tmux-1000/default\n", err: exit, wantErr: true, wantNotFound: true},
		{name: "other failure", stderr: "permission denied\n", err: exit, wantErr: true},
		{name: "exec failure", err: errors.New("exec: \"tmux\": executable file not found in $PATH"), wantErr: true},
		{name: "malformed", stdout: "garbage\n", wantErr: true},
		{name: "other key", stdout: "OTHER=v\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tm := &fakeTmux{reply: func([]string) (string, string, error) { return tc.stdout, tc.stderr, tc.err }}
			p, err := newPodLocal("worker-1", tm.run)
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.GetMeta("worker-1", "K")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("GetMeta = %q, nil; want an error", got)
				}
				if errors.Is(err, runtime.ErrSessionNotFound) != tc.wantNotFound {
					t.Fatalf("GetMeta error %v: errors.Is(ErrSessionNotFound) = %v; want %v", err, !tc.wantNotFound, tc.wantNotFound)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("GetMeta = %q, %v; want %q, nil", got, err, tc.want)
			}
		})
	}
}

func TestPodLocalSetAndRemoveMetaReturnFailures(t *testing.T) {
	exit := errors.New("exit status 1")
	for _, tc := range []struct {
		stderr       string
		wantNotFound bool
	}{
		{stderr: "can't find session: =main", wantNotFound: true},
		{stderr: "error connecting to /tmp/tmux-1000/default (No such file or directory)", wantNotFound: true},
		{stderr: "server busy"},
	} {
		tm := &fakeTmux{reply: func([]string) (string, string, error) { return "", tc.stderr, exit }}
		p, err := newPodLocal("worker-1", tm.run)
		if err != nil {
			t.Fatal(err)
		}
		for op, call := range map[string]func() error{
			"SetMeta":    func() error { return p.SetMeta("main", "K", "v") },
			"RemoveMeta": func() error { return p.RemoveMeta("worker-1", "K") },
		} {
			err := call()
			if err == nil {
				t.Fatalf("%s with tmux stderr %q succeeded; want the failure returned", op, tc.stderr)
			}
			if errors.Is(err, runtime.ErrSessionNotFound) != tc.wantNotFound {
				t.Errorf("%s error %v: ErrSessionNotFound match = %v; want %v", op, err, !tc.wantNotFound, tc.wantNotFound)
			}
			if errors.Is(err, guard.ErrControllerOwnedRuntime) {
				t.Errorf("%s error %v reads as a refusal; want a tmux failure", op, err)
			}
		}
	}
}

func TestPodLocalIsRunningProbesOwnSessionLocally(t *testing.T) {
	alive := true
	tm := &fakeTmux{reply: func([]string) (string, string, error) {
		if alive {
			return "", "", nil
		}
		return "", "can't find session: =main", errors.New("exit status 1")
	}}
	p, err := newPodLocal("worker-1", tm.run)
	if err != nil {
		t.Fatal(err)
	}
	if !p.IsRunning("worker-1") || !p.IsRunning("main") {
		t.Fatal("IsRunning(own session) = false with a live local session; want true")
	}
	alive = false
	if p.IsRunning("worker-1") {
		t.Fatal("IsRunning(own session) = true with no local session; want false")
	}
	want := []string{"has-session", "-t", "=main"}
	if !reflect.DeepEqual(tm.calls[0], want) {
		t.Errorf("IsRunning ran tmux %v; want %v", tm.calls[0], want)
	}
}

func TestPodLocalRefusesControllerOwnedOperations(t *testing.T) {
	tm := &fakeTmux{}
	p, err := newPodLocal("worker-1", tm.run)
	if err != nil {
		t.Fatal(err)
	}
	var sp runtime.Provider = p
	for _, name := range []string{"worker-1", "main", "other"} {
		ops := map[string]func() error{
			"Start":           func() error { return sp.Start(context.Background(), name, runtime.Config{}) },
			"Stop":            func() error { return sp.Stop(name) },
			"Interrupt":       func() error { return sp.Interrupt(name) },
			"Attach":          func() error { return sp.Attach(name) },
			"Nudge":           func() error { return sp.Nudge(name, nil) },
			"Peek":            func() error { _, err := sp.Peek(name, 10); return err },
			"ListRunning":     func() error { _, err := sp.ListRunning(name); return err },
			"GetLastActivity": func() error { _, err := sp.GetLastActivity(name); return err },
			"ClearScrollback": func() error { return sp.ClearScrollback(name) },
			"CopyTo":          func() error { return sp.CopyTo(name, "/src", "dst") },
			"SendKeys":        func() error { return sp.SendKeys(name, "Enter") },
			"RunLive":         func() error { return sp.RunLive(name, runtime.Config{}) },
		}
		if name == "other" {
			ops["SetMeta"] = func() error { return sp.SetMeta(name, "K", "v") }
			ops["GetMeta"] = func() error { _, err := sp.GetMeta(name, "K"); return err }
			ops["RemoveMeta"] = func() error { return sp.RemoveMeta(name, "K") }
		}
		for op, call := range ops {
			err := call()
			if !errors.Is(err, guard.ErrControllerOwnedRuntime) {
				t.Errorf("%s(%q) = %v; want guard.ErrControllerOwnedRuntime", op, name, err)
			}
			if errors.Is(err, runtime.ErrSessionNotFound) || runtime.IsSessionGone(err) || errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Errorf("%s(%q) refusal %v reads as absence or unavailability", op, name, err)
			}
		}
	}
	if len(tm.calls) != 0 {
		t.Fatalf("refused operations ran tmux: %v", tm.calls)
	}
	if p.IsRunning("other") || p.IsAttached("worker-1") || p.ProcessAlive("worker-1", []string{"claude"}) {
		t.Fatal("pod-local probes of other sessions, attachment or processes report true; want false")
	}
	if len(tm.calls) != 0 {
		t.Fatalf("restricted probes ran tmux: %v", tm.calls)
	}
	if caps := p.Capabilities(); caps != (runtime.ProviderCapabilities{}) {
		t.Fatalf("Capabilities() = %+v; want none", caps)
	}
}

func TestPodLocalStopStaysARefusalThroughCleanup(t *testing.T) {
	p, err := newPodLocal("worker-1", (&fakeTmux{}).run)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"worker-1", "other"} {
		err := runtime.StopForCleanup(p, name)
		if !errors.Is(err, runtime.ErrStopRefused) || !errors.Is(err, guard.ErrControllerOwnedRuntime) {
			t.Fatalf("StopForCleanup(%q) = %v; want ErrStopRefused and ErrControllerOwnedRuntime", name, err)
		}
	}
	// A false probe of another session must never become confirmed absence.
	if runtime.FreshReadable(p) {
		t.Fatal("pod-local provider is FreshReadable; its false probes would confirm another session gone")
	}
	if _, ok := any(p).(runtime.LivenessObserverWithError); ok {
		t.Fatal("pod-local provider implements LivenessObserverWithError; want only the boolean probe")
	}
}
