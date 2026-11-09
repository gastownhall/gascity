package guard

// Runtime-conformance waiver: the guard is not run through
// runtimetest.RunProviderTests. That suite proves a successful lifecycle
// (start, observe, stop), and a provider that refuses every operation by
// design cannot have one, the same reason the checked runtime provider
// ledger marks NewFailFake "not applicable". The ledger records the guard
// "not applicable" too, bound to its production construction path, cmd/gc's
// resolveWorkerSpec (runtime.worker-site.guard); providerledger's
// TestRuntimeGuardIsLedgeredOnceWired fails if a production cmd/gc file
// imports this package without that row. The test
// below is the guard's contract: every Provider method, by reflection, so a
// method added to runtime.Provider fails here until the guard covers it.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

type refusingCall func(p *Provider) error

func TestGuardRefusesEveryRuntimeOperation(t *testing.T) {
	p := New()
	refusing := map[string]refusingCall{
		"Start": func(p *Provider) error {
			return p.Start(context.Background(), "s1", runtime.Config{Command: "agent"})
		},
		"Stop":      func(p *Provider) error { return p.Stop("s1") },
		"Interrupt": func(p *Provider) error { return p.Interrupt("s1") },
		"Attach":    func(p *Provider) error { return p.Attach("s1") },
		"Nudge": func(p *Provider) error {
			return p.Nudge("s1", runtime.TextContent("hello"))
		},
		"SetMeta":    func(p *Provider) error { return p.SetMeta("s1", "k", "v") },
		"RemoveMeta": func(p *Provider) error { return p.RemoveMeta("s1", "k") },
		"GetMeta": func(p *Provider) error {
			v, err := p.GetMeta("s1", "k")
			if v != "" {
				t.Errorf("GetMeta value = %q, want empty", v)
			}
			return err
		},
		"Peek": func(p *Provider) error {
			v, err := p.Peek("s1", 10)
			if v != "" {
				t.Errorf("Peek output = %q, want empty", v)
			}
			return err
		},
		"ListRunning": func(p *Provider) error {
			names, err := p.ListRunning("gc-")
			if names != nil {
				t.Errorf("ListRunning names = %v, want nil", names)
			}
			return err
		},
		"GetLastActivity": func(p *Provider) error {
			at, err := p.GetLastActivity("s1")
			if !at.Equal(time.Time{}) {
				t.Errorf("GetLastActivity = %v, want zero", at)
			}
			return err
		},
		"ClearScrollback": func(p *Provider) error { return p.ClearScrollback("s1") },
		"CopyTo":          func(p *Provider) error { return p.CopyTo("s1", "/src", "dst") },
		"SendKeys":        func(p *Provider) error { return p.SendKeys("s1", "Enter") },
		"RunLive": func(p *Provider) error {
			return p.RunLive("s1", runtime.Config{Command: "agent"})
		},
	}
	probing := map[string]func(p *Provider) bool{
		"IsRunning":    func(p *Provider) bool { return p.IsRunning("s1") },
		"IsAttached":   func(p *Provider) bool { return p.IsAttached("s1") },
		"ProcessAlive": func(p *Provider) bool { return p.ProcessAlive("s1", []string{"agent"}) },
	}
	other := map[string]bool{"Capabilities": true}

	iface := reflect.TypeOf((*runtime.Provider)(nil)).Elem()
	for i := 0; i < iface.NumMethod(); i++ {
		name := iface.Method(i).Name
		_, r := refusing[name]
		_, q := probing[name]
		if !r && !q && !other[name] {
			t.Errorf("runtime.Provider.%s has no guard expectation; decide whether the guard refuses it", name)
		}
	}
	if total := len(refusing) + len(probing) + len(other); total != iface.NumMethod() {
		t.Errorf("guard expectations cover %d methods, runtime.Provider has %d", total, iface.NumMethod())
	}

	for name, call := range refusing {
		err := call(p)
		if err == nil {
			t.Errorf("%s returned nil, want ErrControllerOwnedRuntime", name)
			continue
		}
		if !errors.Is(err, ErrControllerOwnedRuntime) {
			t.Errorf("%s error %v does not wrap ErrControllerOwnedRuntime", name, err)
		}
		if errors.Is(err, runtime.ErrSessionNotFound) {
			t.Errorf("%s error %v reads as session-not-found; a refusal must not look like absence", name, err)
		}
	}
	for name, probe := range probing {
		if probe(p) {
			t.Errorf("%s = true, want false", name)
		}
	}
	if caps := p.Capabilities(); caps != (runtime.ProviderCapabilities{}) {
		t.Errorf("Capabilities = %+v, want zero", caps)
	}
}

func TestGuardErrorsNameTheOperationAndSession(t *testing.T) {
	err := New().Stop("mc-abc12")
	if err == nil || !strings.Contains(err.Error(), "stop") || !strings.Contains(err.Error(), `"mc-abc12"`) {
		t.Fatalf("Stop error %v must name the operation and the session", err)
	}
}
