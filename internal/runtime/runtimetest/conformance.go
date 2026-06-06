// Package runtimetest provides a conformance test suite for [runtime.Provider]
// implementations. Each implementation's test file calls [RunProviderTests]
// with its own factory function, mirroring the beadstest pattern.
//
// The suite is split into two composable sub-suites:
//   - [RunLifecycleTests] — tests that start/stop sessions (groups 1, 3, 6)
//   - [RunSessionTests] — tests that operate on an already-running session (groups 2, 4, 5)
//
// [RunProviderTests] composes both for backward compatibility. Slow providers
// (e.g., Kubernetes) can call the sub-suites directly to share a single
// session across the metadata/observation/signaling tests.
package runtimetest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Factory creates a (provider, config, sessionName) tuple for a single test.
// The provider may be shared across tests; config and name must be unique.
type Factory func(t *testing.T) (runtime.Provider, runtime.Config, string)

// RunProviderTests runs the full conformance suite against a Provider.
// newSession returns a (provider, config, sessionName) tuple per test.
// The provider may be shared across tests; config and name must be unique.
func RunProviderTests(t *testing.T, newSession Factory) {
	t.Helper()

	RunLifecycleTests(t, newSession)

	t.Run("SharedSession", func(t *testing.T) {
		sp, cfg, name := newSession(t)
		if err := sp.Start(context.Background(), name, cfg); err != nil {
			t.Fatalf("Start shared session: %v", err)
		}
		t.Cleanup(func() { _ = sp.Stop(name) })
		RunSessionTests(t, sp, cfg, name)
	})
}

// RunLifecycleTests runs conformance tests that start and stop their own
// sessions: lifecycle (group 1), discovery (group 3), and process-alive
// (group 6). Each test creates a fresh session via the factory.
func RunLifecycleTests(t *testing.T, newSession Factory) {
	t.Helper()

	// --- Group 1: Lifecycle ---

	t.Run("Start_CreatesRunningSession", func(t *testing.T) {
		sp, cfg, name := newSession(t)
		if err := sp.Start(context.Background(), name, cfg); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = sp.Stop(name) })

		if !sp.IsRunning(name) {
			t.Error("IsRunning = false after Start, want true")
		}
	})

	t.Run("Start_DuplicateReturnsError", func(t *testing.T) {
		sp, cfg, name := newSession(t)
		if err := sp.Start(context.Background(), name, cfg); err != nil {
			t.Fatalf("first Start: %v", err)
		}
		t.Cleanup(func() { _ = sp.Stop(name) })

		err := sp.Start(context.Background(), name, cfg)
		if err == nil {
			t.Error("second Start should return error for duplicate name")
		}
	})

	t.Run("Start_ConcurrentDistinctSessions", func(t *testing.T) {
		sp, cfg1, name1 := newSession(t)
		_, cfg2, name2 := newSession(t)
		_, cfg3, name3 := newSession(t)
		names := []string{name1, name2, name3}
		cfgs := []runtime.Config{cfg1, cfg2, cfg3}
		for _, name := range names {
			t.Cleanup(func(n string) func() {
				return func() { _ = sp.Stop(n) }
			}(name))
		}
		errs := make([]error, len(names))
		var wg sync.WaitGroup
		for i := range names {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = sp.Start(context.Background(), names[i], cfgs[i])
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("concurrent Start(%s): %v", names[i], err)
			}
			if !sp.IsRunning(names[i]) {
				t.Fatalf("IsRunning(%s) = false after concurrent Start", names[i])
			}
		}
	})

	t.Run("Stop_MakesSessionNotRunning", func(t *testing.T) {
		sp, cfg, name := newSession(t)
		if err := sp.Start(context.Background(), name, cfg); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := sp.Stop(name); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		if sp.IsRunning(name) {
			t.Error("IsRunning = true after Stop, want false")
		}
	})

	t.Run("Stop_Idempotent_NotRunning", func(t *testing.T) {
		sp, _, _ := newSession(t)
		if err := sp.Stop("never-started-conformance-session"); err != nil {
			t.Errorf("Stop on never-started session: %v", err)
		}
	})

	t.Run("Stop_Idempotent_AlreadyStopped", func(t *testing.T) {
		sp, cfg, name := newSession(t)
		if err := sp.Start(context.Background(), name, cfg); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := sp.Stop(name); err != nil {
			t.Fatalf("first Stop: %v", err)
		}
		if err := sp.Stop(name); err != nil {
			t.Errorf("second Stop should be idempotent: %v", err)
		}
	})

	t.Run("Stop_ConcurrentDistinctSessions", func(t *testing.T) {
		sp, cfg1, name1 := newSession(t)
		_, cfg2, name2 := newSession(t)
		_, cfg3, name3 := newSession(t)
		names := []string{name1, name2, name3}
		cfgs := []runtime.Config{cfg1, cfg2, cfg3}
		for i := range names {
			if err := sp.Start(context.Background(), names[i], cfgs[i]); err != nil {
				t.Fatalf("Start(%s): %v", names[i], err)
			}
		}
		errs := make([]error, len(names))
		var wg sync.WaitGroup
		for i := range names {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = sp.Stop(names[i])
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("concurrent Stop(%s): %v", names[i], err)
			}
			if sp.IsRunning(names[i]) {
				t.Fatalf("IsRunning(%s) = true after concurrent Stop", names[i])
			}
		}
	})

	t.Run("Interrupt_ConcurrentDistinctSessions", func(t *testing.T) {
		sp, cfg1, name1 := newSession(t)
		_, cfg2, name2 := newSession(t)
		_, cfg3, name3 := newSession(t)
		names := []string{name1, name2, name3}
		cfgs := []runtime.Config{cfg1, cfg2, cfg3}
		for i := range names {
			if err := sp.Start(context.Background(), names[i], cfgs[i]); err != nil {
				t.Fatalf("Start(%s): %v", names[i], err)
			}
			t.Cleanup(func(n string) func() {
				return func() { _ = sp.Stop(n) }
			}(names[i]))
		}
		errs := make([]error, len(names))
		var wg sync.WaitGroup
		for i := range names {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = sp.Interrupt(names[i])
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("concurrent Interrupt(%s): %v", names[i], err)
			}
		}
	})

	t.Run("IsRunning_UnknownSession", func(t *testing.T) {
		sp, _, _ := newSession(t)
		if sp.IsRunning("unknown-conformance-session-never-existed") {
			t.Error("IsRunning = true for unknown session, want false")
		}
	})

	t.Run("IsRunning_ConcurrentDistinctSessions", func(t *testing.T) {
		sp, cfg1, name1 := newSession(t)
		_, cfg2, name2 := newSession(t)
		_, cfg3, name3 := newSession(t)
		names := []string{name1, name2, name3}
		cfgs := []runtime.Config{cfg1, cfg2, cfg3}
		for i := range names {
			if err := sp.Start(context.Background(), names[i], cfgs[i]); err != nil {
				t.Fatalf("Start(%s): %v", names[i], err)
			}
			t.Cleanup(func(n string) func() {
				return func() { _ = sp.Stop(n) }
			}(names[i]))
		}
		got := make([]bool, len(names))
		var wg sync.WaitGroup
		for i := range names {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				got[i] = sp.IsRunning(names[i])
			}(i)
		}
		wg.Wait()
		for i := range got {
			if !got[i] {
				t.Fatalf("IsRunning(%s) = false after concurrent query", names[i])
			}
		}
	})

	// --- Group 3: Discovery ---

	t.Run("ListRunning_FindsSessions", func(t *testing.T) {
		sp, cfg1, name1 := newSession(t)
		if err := sp.Start(context.Background(), name1, cfg1); err != nil {
			t.Fatalf("Start %s: %v", name1, err)
		}
		t.Cleanup(func() { _ = sp.Stop(name1) })

		_, cfg2, name2 := newSession(t)
		if err := sp.Start(context.Background(), name2, cfg2); err != nil {
			t.Fatalf("Start %s: %v", name2, err)
		}
		t.Cleanup(func() { _ = sp.Stop(name2) })

		names, err := sp.ListRunning("")
		if err != nil {
			t.Fatalf("ListRunning: %v", err)
		}
		if !contains(names, name1) {
			t.Errorf("ListRunning missing %q in %v", name1, names)
		}
		if !contains(names, name2) {
			t.Errorf("ListRunning missing %q in %v", name2, names)
		}
	})

	t.Run("ListRunning_PrefixFiltering", func(t *testing.T) {
		sp, cfg1, name1 := newSession(t)
		if err := sp.Start(context.Background(), name1, cfg1); err != nil {
			t.Fatalf("Start %s: %v", name1, err)
		}
		t.Cleanup(func() { _ = sp.Stop(name1) })

		_, cfg2, name2 := newSession(t)
		if err := sp.Start(context.Background(), name2, cfg2); err != nil {
			t.Fatalf("Start %s: %v", name2, err)
		}
		t.Cleanup(func() { _ = sp.Stop(name2) })

		// Using the full name as prefix should match only that session.
		names, err := sp.ListRunning(name1)
		if err != nil {
			t.Fatalf("ListRunning(%q): %v", name1, err)
		}
		if !contains(names, name1) {
			t.Errorf("ListRunning(%q) should contain %q, got %v", name1, name1, names)
		}
		if contains(names, name2) {
			t.Errorf("ListRunning(%q) should not contain %q, got %v", name1, name2, names)
		}
	})

	t.Run("ListRunning_ExcludesStopped", func(t *testing.T) {
		sp, cfg, name := newSession(t)
		if err := sp.Start(context.Background(), name, cfg); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := sp.Stop(name); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		names, err := sp.ListRunning(name)
		if err != nil {
			t.Fatalf("ListRunning: %v", err)
		}
		if contains(names, name) {
			t.Errorf("stopped session %q should not appear in ListRunning, got %v", name, names)
		}
	})

	t.Run("ListRunning_EmptyPrefix", func(t *testing.T) {
		sp, cfg, name := newSession(t)
		if err := sp.Start(context.Background(), name, cfg); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = sp.Stop(name) })

		names, err := sp.ListRunning("")
		if err != nil {
			t.Fatalf("ListRunning: %v", err)
		}
		if !contains(names, name) {
			t.Errorf("ListRunning(\"\") should find %q, got %v", name, names)
		}
	})

	t.Run("ListRunning_ConcurrentDistinctPrefixes", func(t *testing.T) {
		sp, cfg1, name1 := newSession(t)
		_, cfg2, name2 := newSession(t)
		_, cfg3, name3 := newSession(t)
		names := []string{name1, name2, name3}
		cfgs := []runtime.Config{cfg1, cfg2, cfg3}
		for i := range names {
			if err := sp.Start(context.Background(), names[i], cfgs[i]); err != nil {
				t.Fatalf("Start(%s): %v", names[i], err)
			}
			t.Cleanup(func(n string) func() {
				return func() { _ = sp.Stop(n) }
			}(names[i]))
		}
		results := make([][]string, len(names))
		var wg sync.WaitGroup
		for i := range names {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				list, err := sp.ListRunning(names[i])
				if err != nil {
					t.Errorf("ListRunning(%s): %v", names[i], err)
					return
				}
				results[i] = list
			}(i)
		}
		wg.Wait()
		for i := range names {
			if !contains(results[i], names[i]) {
				t.Fatalf("ListRunning(%s) missing session in %v", names[i], results[i])
			}
		}
	})

	// --- Group 6: ProcessAlive ---

	t.Run("ProcessAlive_EmptyNamesReturnsTrue", func(t *testing.T) {
		sp, cfg, name := newSession(t)
		if err := sp.Start(context.Background(), name, cfg); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = sp.Stop(name) })

		if !sp.ProcessAlive(name, nil) {
			t.Error("ProcessAlive with empty names = false, want true")
		}
	})

	t.Run("ProcessAlive_FalseAfterStop", func(t *testing.T) {
		sp, cfg, name := newSession(t)
		if err := sp.Start(context.Background(), name, cfg); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := sp.Stop(name); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		if sp.ProcessAlive(name, []string{"some-process"}) {
			t.Error("ProcessAlive after Stop = true, want false")
		}
	})

	t.Run("ProcessAlive_ConcurrentDistinctSessions", func(t *testing.T) {
		sp, cfg1, name1 := newSession(t)
		_, cfg2, name2 := newSession(t)
		_, cfg3, name3 := newSession(t)
		names := []string{name1, name2, name3}
		cfgs := []runtime.Config{cfg1, cfg2, cfg3}
		for i := range names {
			if err := sp.Start(context.Background(), names[i], cfgs[i]); err != nil {
				t.Fatalf("Start(%s): %v", names[i], err)
			}
			t.Cleanup(func(n string) func() {
				return func() { _ = sp.Stop(n) }
			}(names[i]))
		}
		got := make([]bool, len(names))
		var wg sync.WaitGroup
		for i := range names {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				got[i] = sp.ProcessAlive(names[i], nil)
			}(i)
		}
		wg.Wait()
		for i := range got {
			if !got[i] {
				t.Fatalf("ProcessAlive(%s) = false after concurrent query", names[i])
			}
		}
	})
}

// RunSessionTests runs conformance tests that operate on an already-running
// session: metadata (group 2), observation (group 4), and signaling (group 5).
// The caller is responsible for starting the session before calling this
// function and stopping it afterward.
func RunSessionTests(t *testing.T, sp runtime.Provider, cfg runtime.Config, name string) {
	t.Helper()
	_ = cfg // reserved for future use

	// --- Group 2: Metadata ---

	t.Run("SetGetMeta_RoundTrip", func(t *testing.T) {
		if err := sp.SetMeta(name, "test-key", "test-value"); err != nil {
			t.Fatalf("SetMeta: %v", err)
		}
		val, err := sp.GetMeta(name, "test-key")
		if err != nil {
			t.Fatalf("GetMeta: %v", err)
		}
		if val != "test-value" {
			t.Errorf("GetMeta = %q, want %q", val, "test-value")
		}
	})

	t.Run("GetMeta_UnsetKey", func(t *testing.T) {
		val, err := sp.GetMeta(name, "nonexistent-key")
		if err != nil {
			t.Fatalf("GetMeta: %v", err)
		}
		if val != "" {
			t.Errorf("GetMeta on unset key = %q, want empty", val)
		}
	})

	t.Run("RemoveMeta_ThenGetReturnsEmpty", func(t *testing.T) {
		if err := sp.SetMeta(name, "remove-me", "value"); err != nil {
			t.Fatalf("SetMeta: %v", err)
		}
		if err := sp.RemoveMeta(name, "remove-me"); err != nil {
			t.Fatalf("RemoveMeta: %v", err)
		}
		val, err := sp.GetMeta(name, "remove-me")
		if err != nil {
			t.Fatalf("GetMeta: %v", err)
		}
		if val != "" {
			t.Errorf("GetMeta after RemoveMeta = %q, want empty", val)
		}
	})

	t.Run("SetMeta_OverwritesPrevious", func(t *testing.T) {
		if err := sp.SetMeta(name, "key", "v1"); err != nil {
			t.Fatalf("SetMeta v1: %v", err)
		}
		if err := sp.SetMeta(name, "key", "v2"); err != nil {
			t.Fatalf("SetMeta v2: %v", err)
		}
		val, err := sp.GetMeta(name, "key")
		if err != nil {
			t.Fatalf("GetMeta: %v", err)
		}
		if val != "v2" {
			t.Errorf("GetMeta = %q, want %q", val, "v2")
		}
	})

	t.Run("Meta_MultipleKeys", func(t *testing.T) {
		if err := sp.SetMeta(name, "key1", "val1"); err != nil {
			t.Fatalf("SetMeta key1: %v", err)
		}
		if err := sp.SetMeta(name, "key2", "val2"); err != nil {
			t.Fatalf("SetMeta key2: %v", err)
		}

		v1, err := sp.GetMeta(name, "key1")
		if err != nil {
			t.Fatalf("GetMeta key1: %v", err)
		}
		v2, err := sp.GetMeta(name, "key2")
		if err != nil {
			t.Fatalf("GetMeta key2: %v", err)
		}
		if v1 != "val1" {
			t.Errorf("key1 = %q, want %q", v1, "val1")
		}
		if v2 != "val2" {
			t.Errorf("key2 = %q, want %q", v2, "val2")
		}
	})

	// --- Group 4: Observation (best-effort) ---

	t.Run("Peek_NoError", func(t *testing.T) {
		_, err := sp.Peek(name, 10)
		if err != nil {
			t.Errorf("Peek: %v", err)
		}
	})

	t.Run("GetLastActivity_NoError", func(t *testing.T) {
		_, err := sp.GetLastActivity(name)
		if err != nil {
			t.Errorf("GetLastActivity: %v", err)
		}
	})

	t.Run("ClearScrollback_NoError", func(t *testing.T) {
		if err := sp.ClearScrollback(name); err != nil {
			t.Errorf("ClearScrollback: %v", err)
		}
	})

	// --- Group 4b: CopyTo (best-effort) ---

	t.Run("CopyTo_NoError", func(t *testing.T) {
		// CopyTo should not error on a running session, even if src is
		// missing (best-effort contract).
		if err := sp.CopyTo(name, "/nonexistent-path-for-conformance", ""); err != nil {
			t.Errorf("CopyTo: %v", err)
		}
	})

	// --- Group 5: Signaling (best-effort) ---

	t.Run("SendKeys_RunningSession", func(t *testing.T) {
		if err := sp.SendKeys(name, "Enter"); err != nil {
			t.Errorf("SendKeys: %v", err)
		}
	})

	t.Run("SendKeys_MissingSession", func(t *testing.T) {
		if err := sp.SendKeys("nonexistent-conformance-session", "Enter"); err != nil {
			t.Errorf("SendKeys on missing session should not error: %v", err)
		}
	})

	t.Run("Interrupt_RunningSession", func(t *testing.T) {
		if err := sp.Interrupt(name); err != nil {
			t.Errorf("Interrupt: %v", err)
		}
	})

	t.Run("Interrupt_MissingSession", func(t *testing.T) {
		if err := sp.Interrupt("nonexistent-conformance-session"); err != nil {
			t.Errorf("Interrupt on missing session should not error: %v", err)
		}
	})

	t.Run("Nudge_RunningSession", func(t *testing.T) {
		if err := sp.Nudge(name, runtime.TextContent("hello")); err != nil {
			t.Errorf("Nudge: %v", err)
		}
	})

	t.Run("Nudge_MissingSession", func(t *testing.T) {
		if err := sp.Nudge("nonexistent-conformance-session", runtime.TextContent("hello")); err != nil && !errors.Is(err, runtime.ErrSessionNotFound) {
			t.Errorf("Nudge on missing session error = %v, want nil or ErrSessionNotFound", err)
		}
	})
}

// contains reports whether ss contains target.
func contains(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}
