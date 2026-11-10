package runtimetest

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// hostStagingFake is a host-staging provider over runtime.Fake whose every
// RUNTIME-CONTRACT-022 duty can be broken on its own, so each check is shown
// to catch the violation it names.
type hostStagingFake struct {
	*runtime.Fake
	skipStaging      bool // stages nothing
	providerBlind    bool // stages every per-provider overlay
	preStartNonFatal bool // starts despite a failing pre_start
	sendsHandled     bool // sends the fields it handled
	setupFatal       bool // fails the start on a failing session_setup
	skipSetup        bool // runs no session_setup
	runsLive         bool // runs session_live

	mu       sync.Mutex
	received map[string][]string
}

func (f *hostStagingFake) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if !f.skipStaging {
		staged := cfg
		if f.providerBlind {
			staged.ProviderName = "another-provider"
			if err := runtime.StageSessionWorkDir(staged); err != nil {
				return err
			}
			staged.ProviderName = cfg.ProviderName
		}
		if err := runtime.StageSessionWorkDir(staged); err != nil {
			return err
		}
		for _, cmd := range cfg.PreStart {
			if err := runFakeSetup(cmd); err != nil && !f.preStartNonFatal {
				return fmt.Errorf("pre_start: %w", err)
			}
		}
	}
	fields := []string{"work_dir", "command", "session_live"}
	if f.sendsHandled {
		fields = append(fields, HostHandledStartFields...)
	}
	f.mu.Lock()
	f.received[name] = fields
	f.mu.Unlock()
	if err := f.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	if !f.skipSetup {
		for _, cmd := range cfg.SessionSetup {
			if err := runFakeSetup(cmd); err != nil && f.setupFatal {
				return fmt.Errorf("session_setup: %w", err)
			}
		}
	}
	if f.runsLive {
		for _, cmd := range cfg.SessionLive {
			_ = runFakeSetup(cmd)
		}
	}
	return nil
}

// runFakeSetup carries out, in process, the two setup-command shapes the
// RUNTIME-CONTRACT-022 cases send: `touch <path>` and `exit <code>`, so the
// fake spawns no shell.
func runFakeSetup(cmd string) error {
	switch verb, arg, _ := strings.Cut(cmd, " "); verb {
	case "touch":
		return os.WriteFile(arg, nil, 0o644)
	case "exit":
		if arg == "0" {
			return nil
		}
		return fmt.Errorf("exit status %s", arg)
	default:
		return fmt.Errorf("the fake runs no %q", cmd)
	}
}

func (f *hostStagingFake) receivedFields(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.received[name])
}

func hostStagingFixtureOf(configure func(*hostStagingFake)) HostStagingFixture {
	return HostStagingFixture{New: func(*testing.T) (runtime.Provider, func(string) []string) {
		f := &hostStagingFake{Fake: runtime.NewFake(), received: map[string][]string{}}
		configure(f)
		return f, f.receivedFields
	}}
}

// TestRunHostStagingContractOnAConformingFake runs the suite against a fake
// that keeps every duty.
func TestRunHostStagingContractOnAConformingFake(t *testing.T) {
	RunHostStagingContract(t, hostStagingFixtureOf(func(*hostStagingFake) {}))
}

// Kills a vacuous RUNTIME-CONTRACT-022 check: each fake breaks one duty, and
// the check that names it must report it.
func TestHostStagingChecksCatchEachViolation(t *testing.T) {
	cases := []struct {
		name    string
		violate func(*hostStagingFake)
		check   func(hostStagingCase) error
	}{
		{"stages nothing", func(f *hostStagingFake) { f.skipStaging = true }, checkHostStagingStaged},
		{"stages another provider's overlay", func(f *hostStagingFake) { f.providerBlind = true }, checkHostStagingStaged},
		{"starts despite a failing pre_start", func(f *hostStagingFake) { f.preStartNonFatal = true }, checkHostStagingPreStartFatal},
		{"sends the fields it handled", func(f *hostStagingFake) { f.sendsHandled = true }, checkHostStagingOmits},
		{"fails the start on session_setup", func(f *hostStagingFake) { f.setupFatal = true }, checkHostStagingSessionSetup},
		{"runs no session_setup", func(f *hostStagingFake) { f.skipSetup = true }, checkHostStagingSessionSetup},
		{"runs session_live", func(f *hostStagingFake) { f.runsLive = true }, checkHostStagingNoSessionLive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newHostStagingCase(t, hostStagingFixtureOf(tc.violate), "host-staging-broken")
			if err := tc.check(c); err == nil {
				t.Errorf("the check passed a provider that %s", tc.name)
			}
		})
	}
}
