package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Compile-time check: Fake implements Provider.
var _ Provider = (*Fake)(nil)

func TestFake_StartStop(t *testing.T) {
	f := NewFake()

	if err := f.Start(context.Background(), "mayor", Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !f.IsRunning("mayor") {
		t.Fatal("expected mayor to be running after Start")
	}

	// Duplicate start should fail.
	if err := f.Start(context.Background(), "mayor", Config{}); err == nil {
		t.Fatal("expected error on duplicate Start")
	}

	if err := f.Stop("mayor"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if f.IsRunning("mayor") {
		t.Fatal("expected mayor to not be running after Stop")
	}

	// Idempotent stop.
	if err := f.Stop("mayor"); err != nil {
		t.Fatalf("idempotent Stop: %v", err)
	}
}

func TestFake_Attach(t *testing.T) {
	f := NewFake()

	// Attach to nonexistent session.
	if err := f.Attach("ghost"); err == nil {
		t.Fatal("expected error attaching to nonexistent session")
	}

	_ = f.Start(context.Background(), "mayor", Config{})
	if err := f.Attach("mayor"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
}

func TestFailFake_AllOpsFail(t *testing.T) {
	f := NewFailFake()

	if err := f.Start(context.Background(), "mayor", Config{WorkDir: "/tmp"}); err == nil {
		t.Fatal("expected Start to fail on broken fake")
	}
	if f.IsRunning("mayor") {
		t.Fatal("expected IsRunning to return false on broken fake")
	}
	if err := f.Attach("mayor"); err == nil {
		t.Fatal("expected Attach to fail on broken fake")
	}
	if err := f.Stop("mayor"); err == nil {
		t.Fatal("expected Stop to fail on broken fake")
	}
}

func TestFailFake_RecordsCalls(t *testing.T) {
	f := NewFailFake()

	_ = f.Start(context.Background(), "a", Config{})
	f.IsRunning("a")
	_ = f.Attach("a")
	_ = f.Stop("a")

	want := []string{"Start", "IsRunning", "Attach", "Stop"}
	if len(f.Calls) != len(want) {
		t.Fatalf("got %d calls, want %d", len(f.Calls), len(want))
	}
	for i, c := range f.Calls {
		if c.Method != want[i] {
			t.Errorf("call %d: got %q, want %q", i, c.Method, want[i])
		}
	}
}

func TestFake_SpyRecordsCalls(t *testing.T) {
	f := NewFake()

	_ = f.Start(context.Background(), "a", Config{WorkDir: "/w"})
	f.IsRunning("a")
	_ = f.Attach("a")
	_ = f.Stop("a")

	want := []string{"Start", "IsRunning", "Attach", "Stop"}
	if len(f.Calls) != len(want) {
		t.Fatalf("got %d calls, want %d", len(f.Calls), len(want))
	}
	for i, c := range f.Calls {
		if c.Method != want[i] {
			t.Errorf("call %d: got %q, want %q", i, c.Method, want[i])
		}
		if c.Name != "a" {
			t.Errorf("call %d: got name %q, want %q", i, c.Name, "a")
		}
	}

	// Verify config was captured on Start.
	if f.Calls[0].Config.WorkDir != "/w" {
		t.Errorf("Start config WorkDir: got %q, want %q", f.Calls[0].Config.WorkDir, "/w")
	}
}

func TestFake_CapturesAllConfigFields(t *testing.T) {
	f := NewFake()

	cfg := Config{
		WorkDir:                "/proj",
		Command:                "claude --dangerously-skip-permissions",
		Lifecycle:              LifecycleOneShot,
		Env:                    map[string]string{"GC_AGENT": "mayor", "HOME": "/home/user"},
		ReadyPromptPrefix:      "❯ ",
		ReadyDelayMs:           10000,
		ProcessNames:           []string{"claude", "node"},
		EmitsPermissionWarning: true,
	}
	if err := f.Start(context.Background(), "mayor", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := f.Calls[0].Config
	if got.WorkDir != "/proj" {
		t.Errorf("WorkDir = %q, want %q", got.WorkDir, "/proj")
	}
	if got.Command != "claude --dangerously-skip-permissions" {
		t.Errorf("Command = %q, want %q", got.Command, "claude --dangerously-skip-permissions")
	}
	if got.Lifecycle != LifecycleOneShot {
		t.Errorf("Lifecycle = %q, want %q", got.Lifecycle, LifecycleOneShot)
	}
	if got.Env["GC_AGENT"] != "mayor" {
		t.Errorf("Env[GC_AGENT] = %q, want %q", got.Env["GC_AGENT"], "mayor")
	}
	if got.Env["HOME"] != "/home/user" {
		t.Errorf("Env[HOME] = %q, want %q", got.Env["HOME"], "/home/user")
	}
	if got.ReadyPromptPrefix != "❯ " {
		t.Errorf("ReadyPromptPrefix = %q, want %q", got.ReadyPromptPrefix, "❯ ")
	}
	if got.ReadyDelayMs != 10000 {
		t.Errorf("ReadyDelayMs = %d, want %d", got.ReadyDelayMs, 10000)
	}
	if len(got.ProcessNames) != 2 || got.ProcessNames[0] != "claude" || got.ProcessNames[1] != "node" {
		t.Errorf("ProcessNames = %v, want [claude node]", got.ProcessNames)
	}
	if !got.EmitsPermissionWarning {
		t.Error("EmitsPermissionWarning = false, want true")
	}
}

func TestFakeProcessAliveDefault(t *testing.T) {
	f := NewFake()
	_ = f.Start(context.Background(), "mayor", Config{})

	if !f.ProcessAlive("mayor", []string{"claude"}) {
		t.Error("ProcessAlive = false for healthy session, want true")
	}
}

func TestFakeProcessAliveZombie(t *testing.T) {
	f := NewFake()
	_ = f.Start(context.Background(), "mayor", Config{})
	f.Zombies["mayor"] = true

	if f.ProcessAlive("mayor", []string{"claude"}) {
		t.Error("ProcessAlive = true for zombie, want false")
	}
}

func TestFakeProcessAliveEmptyNames(t *testing.T) {
	f := NewFake()
	_ = f.Start(context.Background(), "mayor", Config{})
	f.Zombies["mayor"] = true // zombie, but no names to check

	if !f.ProcessAlive("mayor", nil) {
		t.Error("ProcessAlive = false with empty names, want true")
	}
}

func TestFakeProcessAliveBroken(t *testing.T) {
	f := NewFailFake()

	if f.ProcessAlive("mayor", []string{"claude"}) {
		t.Error("ProcessAlive = true on broken fake, want false")
	}
}

func TestFakeNudge(t *testing.T) {
	f := NewFake()
	_ = f.Start(context.Background(), "mayor", Config{})

	if err := f.Nudge("mayor", TextContent("wake up")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}

	// Find the Nudge call.
	var found bool
	for _, c := range f.Calls {
		if c.Method == "Nudge" {
			found = true
			if c.Name != "mayor" {
				t.Errorf("Nudge Name = %q, want %q", c.Name, "mayor")
			}
			if c.Message != "wake up" {
				t.Errorf("Nudge Message = %q, want %q", c.Message, "wake up")
			}
			if len(c.Content) != 1 || c.Content[0].Type != "text" || c.Content[0].Text != "wake up" {
				t.Errorf("Nudge Content = %v, want single text block", c.Content)
			}
		}
	}
	if !found {
		t.Error("Nudge call not recorded")
	}
}

func TestFakeNudgeBroken(t *testing.T) {
	f := NewFailFake()

	err := f.Nudge("mayor", TextContent("wake up"))
	if err == nil {
		t.Fatal("expected Nudge to fail on broken fake")
	}

	// Call should still be recorded.
	var found bool
	for _, c := range f.Calls {
		if c.Method == "Nudge" {
			found = true
		}
	}
	if !found {
		t.Error("Nudge call not recorded on broken fake")
	}
}

func TestFakeSetGetMeta(t *testing.T) {
	f := NewFake()
	_ = f.Start(context.Background(), "mayor", Config{})

	if err := f.SetMeta("mayor", "GC_DRAIN", "123"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	val, err := f.GetMeta("mayor", "GC_DRAIN")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if val != "123" {
		t.Errorf("GetMeta = %q, want %q", val, "123")
	}
}

func TestFakeGetMetaUnset(t *testing.T) {
	f := NewFake()
	val, err := f.GetMeta("mayor", "GC_DRAIN")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if val != "" {
		t.Errorf("GetMeta unset key = %q, want empty", val)
	}
}

func TestFakeRemoveMeta(t *testing.T) {
	f := NewFake()
	_ = f.SetMeta("mayor", "GC_DRAIN", "123")
	if err := f.RemoveMeta("mayor", "GC_DRAIN"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}
	val, _ := f.GetMeta("mayor", "GC_DRAIN")
	if val != "" {
		t.Errorf("GetMeta after remove = %q, want empty", val)
	}
}

func TestFakeRemoveMetaErrorForSessionKey(t *testing.T) {
	f := NewFake()
	_ = f.SetMeta("session-a", "GC_DRAIN", "123")
	f.RemoveMetaErrors["session-a"] = map[string]error{"GC_DRAIN": errors.New("remove denied")}

	if err := f.RemoveMeta("session-a", "GC_DRAIN"); err == nil {
		t.Fatal("RemoveMeta error = nil, want configured error")
	}
	val, _ := f.GetMeta("session-a", "GC_DRAIN")
	if val != "123" {
		t.Errorf("GetMeta after failed remove = %q, want original value", val)
	}

	if err := f.RemoveMeta("session-a", "OTHER"); err != nil {
		t.Fatalf("RemoveMeta unrelated key: %v", err)
	}
}

func TestFakeListRunning(t *testing.T) {
	f := NewFake()
	_ = f.Start(context.Background(), "gc-city-mayor", Config{})
	_ = f.Start(context.Background(), "gc-city-worker", Config{})
	_ = f.Start(context.Background(), "gc-other-agent", Config{})

	names, err := f.ListRunning("gc-city-")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("ListRunning = %v, want 2 sessions", names)
	}
}

func TestFakePeek(t *testing.T) {
	f := NewFake()
	_ = f.Start(context.Background(), "mayor", Config{})
	f.SetPeekOutput("mayor", "line1\nline2\n")

	output, err := f.Peek("mayor", 50)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if output != "line1\nline2\n" {
		t.Errorf("Peek output = %q, want %q", output, "line1\nline2\n")
	}

	// Verify call was recorded.
	var found bool
	for _, c := range f.Calls {
		if c.Method == "Peek" {
			found = true
			if c.Name != "mayor" {
				t.Errorf("Peek Name = %q, want %q", c.Name, "mayor")
			}
		}
	}
	if !found {
		t.Error("Peek call not recorded")
	}
}

func TestFakePeekNoOutput(t *testing.T) {
	f := NewFake()

	output, err := f.Peek("ghost", 50)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if output != "" {
		t.Errorf("Peek output = %q, want empty", output)
	}
}

func TestFakePeekBroken(t *testing.T) {
	f := NewFailFake()

	_, err := f.Peek("mayor", 50)
	if err == nil {
		t.Fatal("expected Peek to fail on broken fake")
	}

	// Call should still be recorded.
	var found bool
	for _, c := range f.Calls {
		if c.Method == "Peek" {
			found = true
		}
	}
	if !found {
		t.Error("Peek call not recorded on broken fake")
	}
}

func TestFakeMetaBroken(t *testing.T) {
	f := NewFailFake()

	if err := f.SetMeta("mayor", "k", "v"); err == nil {
		t.Error("SetMeta should fail on broken fake")
	}
	if _, err := f.GetMeta("mayor", "k"); err == nil {
		t.Error("GetMeta should fail on broken fake")
	}
	if err := f.RemoveMeta("mayor", "k"); err == nil {
		t.Error("RemoveMeta should fail on broken fake")
	}
	if _, err := f.ListRunning("gc-"); err == nil {
		t.Error("ListRunning should fail on broken fake")
	}
}

func TestTextContent(t *testing.T) {
	blocks := TextContent("hello")
	if len(blocks) != 1 {
		t.Fatalf("len = %d, want 1", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "hello" {
		t.Errorf("block = %+v, want text=hello", blocks[0])
	}
}

func TestFlattenText(t *testing.T) {
	blocks := []ContentBlock{
		{Type: "text", Text: "hello"},
		{Type: "file_path", Path: "/some/dir/readme.md"},
		{Type: "text", Text: "world"},
	}
	got := FlattenText(blocks)
	want := "hello\n[File: readme.md]\nworld"
	if got != want {
		t.Errorf("FlattenText = %q, want %q", got, want)
	}
}

func TestFlattenText_Empty(t *testing.T) {
	if got := FlattenText(nil); got != "" {
		t.Errorf("FlattenText(nil) = %q, want empty", got)
	}
	if got := FlattenText([]ContentBlock{{Type: "text"}}); got != "" {
		t.Errorf("FlattenText(empty text) = %q, want empty", got)
	}
}

func TestFakeWaitForIdleGate_BlocksUntilClosed(t *testing.T) {
	f := NewFake()
	f.WaitForIdleErrors["s1"] = nil
	gate := make(chan struct{})
	f.WaitForIdleGates["s1"] = gate

	done := make(chan error, 1)
	go func() {
		done <- f.WaitForIdle(context.Background(), "s1", time.Second)
	}()

	select {
	case <-done:
		t.Fatal("WaitForIdle returned before gate closed")
	case <-time.After(50 * time.Millisecond):
	}

	close(gate)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForIdle returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForIdle did not return after gate closed")
	}
}

func TestFakeWaitForIdleGate_RespectsContextCancel(t *testing.T) {
	f := NewFake()
	f.WaitForIdleErrors["s1"] = nil
	f.WaitForIdleGates["s1"] = make(chan struct{}) // never closed

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- f.WaitForIdle(ctx, "s1", time.Second)
	}()

	select {
	case <-done:
		t.Fatal("WaitForIdle returned before cancel")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WaitForIdle error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForIdle did not return after context cancel")
	}
}

func TestFakeWaitForIdleGate_MuReleasedWhileBlocked(t *testing.T) {
	f := NewFake()
	_ = f.Start(context.Background(), "s1", Config{WorkDir: "/tmp"})
	f.WaitForIdleErrors["s1"] = nil
	gate := make(chan struct{})
	f.WaitForIdleGates["s1"] = gate

	// Start a gated WaitForIdle in the background.
	go func() {
		_ = f.WaitForIdle(context.Background(), "s1", time.Second)
	}()

	// Give the goroutine time to acquire and release the lock.
	time.Sleep(20 * time.Millisecond)

	// Other Fake operations must not deadlock while the gate is held.
	if !f.IsRunning("s1") {
		t.Fatal("IsRunning returned false while gate is held")
	}

	close(gate)
}

func TestFakeFindRuntimesBySessionID_BrokenError(t *testing.T) {
	f := NewFailFake()

	_, err := f.FindRuntimesBySessionID("sess1")
	if err == nil {
		t.Fatal("expected error from broken fake")
	}

	var found bool
	for _, c := range f.Calls {
		if c.Method == "FindRuntimesBySessionID" {
			found = true
			if c.Name != "sess1" {
				t.Errorf("Call.Name = %q, want %q", c.Name, "sess1")
			}
		}
	}
	if !found {
		t.Error("FindRuntimesBySessionID call not recorded on broken fake")
	}
}

func TestFakeFindRuntimesBySessionID_EmptyIDReturnsAll(t *testing.T) {
	f := NewFake()
	f.OrphanedRuntimes["sid-a"] = LiveRuntime{SessionID: "sid-a", PID: 1}
	f.OrphanedRuntimes["sid-b"] = LiveRuntime{PID: 2} // empty SessionID; map key is used
	f.OrphanedRuntimes[""] = LiveRuntime{PID: 99}     // both key and SessionID empty; skipped
	_ = f.Start(context.Background(), "provider-c", Config{Env: map[string]string{"GC_SESSION_ID": "sid-c"}})
	_ = f.Start(context.Background(), "provider-d", Config{}) // no GC_SESSION_ID; skipped

	runtimes, err := f.FindRuntimesBySessionID("")
	if err != nil {
		t.Fatalf("FindRuntimesBySessionID: %v", err)
	}
	if len(runtimes) != 3 {
		t.Fatalf("got %d runtimes, want 3: %+v", len(runtimes), runtimes)
	}
}

func TestFakeFindRuntimesBySessionID_FiltersByID(t *testing.T) {
	f := NewFake()
	f.OrphanedRuntimes["sid-match"] = LiveRuntime{SessionID: "sid-match"}
	f.OrphanedRuntimes["sid-other"] = LiveRuntime{SessionID: "sid-other"}
	_ = f.Start(context.Background(), "provider-match", Config{Env: map[string]string{"GC_SESSION_ID": "sid-match"}})
	_ = f.Start(context.Background(), "provider-other", Config{Env: map[string]string{"GC_SESSION_ID": "sid-other"}})

	runtimes, err := f.FindRuntimesBySessionID("sid-match")
	if err != nil {
		t.Fatalf("FindRuntimesBySessionID: %v", err)
	}
	if len(runtimes) != 2 {
		t.Fatalf("got %d runtimes, want 2: %+v", len(runtimes), runtimes)
	}
	for _, r := range runtimes {
		if r.SessionID != "sid-match" {
			t.Errorf("returned runtime SessionID = %q, want %q", r.SessionID, "sid-match")
		}
	}
	if f.CountCalls("FindRuntimesBySessionID", "sid-match") != 1 {
		t.Error("FindRuntimesBySessionID call not recorded")
	}
}

func TestFakeFindRuntimesBySessionID_EmptySessionIDsSkipped(t *testing.T) {
	f := NewFake()
	f.OrphanedRuntimes[""] = LiveRuntime{PID: 1}                   // both key and SessionID empty; skipped
	_ = f.Start(context.Background(), "provider-no-sid", Config{}) // no GC_SESSION_ID; skipped

	runtimes, err := f.FindRuntimesBySessionID("")
	if err != nil {
		t.Fatalf("FindRuntimesBySessionID: %v", err)
	}
	if len(runtimes) != 0 {
		t.Fatalf("got %d runtimes, want 0: %+v", len(runtimes), runtimes)
	}
}

func TestFakeFindRuntimesBySessionID_OrphanForcesIsTrackedFalse(t *testing.T) {
	f := NewFake()
	f.OrphanedRuntimes["sid-orphan"] = LiveRuntime{SessionID: "sid-orphan", IsTracked: true}

	runtimes, err := f.FindRuntimesBySessionID("sid-orphan")
	if err != nil {
		t.Fatalf("FindRuntimesBySessionID: %v", err)
	}
	if len(runtimes) != 1 {
		t.Fatalf("got %d runtimes, want 1", len(runtimes))
	}
	if runtimes[0].IsTracked {
		t.Error("orphan runtime IsTracked = true, want false")
	}
}

func TestFakeFindRuntimesBySessionID_TrackedUsesProviderNameAndSessionID(t *testing.T) {
	f := NewFake()
	_ = f.Start(context.Background(), "my-provider", Config{Env: map[string]string{"GC_SESSION_ID": "sess-abc"}})

	runtimes, err := f.FindRuntimesBySessionID("sess-abc")
	if err != nil {
		t.Fatalf("FindRuntimesBySessionID: %v", err)
	}
	if len(runtimes) != 1 {
		t.Fatalf("got %d runtimes, want 1", len(runtimes))
	}
	r := runtimes[0]
	if r.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want %q", r.SessionID, "sess-abc")
	}
	if r.ProviderName != "my-provider" {
		t.Errorf("ProviderName = %q, want %q", r.ProviderName, "my-provider")
	}
	if !r.IsTracked {
		t.Error("tracked runtime IsTracked = false, want true")
	}
}

func TestFakeTerminateRuntime_RecordsCallWithSessionID(t *testing.T) {
	f := NewFake()

	if err := f.TerminateRuntime(LiveRuntime{SessionID: "my-session", ProviderName: "some-provider"}); err != nil {
		t.Fatalf("TerminateRuntime: %v", err)
	}

	var found bool
	for _, c := range f.Calls {
		if c.Method == "TerminateRuntime" {
			found = true
			if c.Name != "my-session" {
				t.Errorf("Call.Name = %q, want %q (SessionID, not ProviderName)", c.Name, "my-session")
			}
		}
	}
	if !found {
		t.Error("TerminateRuntime call not recorded")
	}
}

func TestFakeTerminateRuntime_RemovesOrphan(t *testing.T) {
	f := NewFake()
	f.OrphanedRuntimes["my-session"] = LiveRuntime{SessionID: "my-session", PID: 42}

	if err := f.TerminateRuntime(LiveRuntime{SessionID: "my-session"}); err != nil {
		t.Fatalf("TerminateRuntime: %v", err)
	}
	if _, ok := f.OrphanedRuntimes["my-session"]; ok {
		t.Error("OrphanedRuntimes still contains entry after TerminateRuntime")
	}
}

func TestFakeTerminateRuntime_MissingEntryIsNil(t *testing.T) {
	f := NewFake()

	if err := f.TerminateRuntime(LiveRuntime{SessionID: "nonexistent"}); err != nil {
		t.Fatalf("TerminateRuntime on missing entry returned error: %v", err)
	}
}

func TestFakeTerminateRuntime_BrokenError(t *testing.T) {
	f := NewFailFake()
	f.OrphanedRuntimes["sid"] = LiveRuntime{SessionID: "sid"}

	err := f.TerminateRuntime(LiveRuntime{SessionID: "sid"})
	if err == nil {
		t.Fatal("expected error from broken fake")
	}

	var found bool
	for _, c := range f.Calls {
		if c.Method == "TerminateRuntime" {
			found = true
			if c.Name != "sid" {
				t.Errorf("Call.Name = %q, want %q", c.Name, "sid")
			}
		}
	}
	if !found {
		t.Error("TerminateRuntime call not recorded on broken fake")
	}
	if _, ok := f.OrphanedRuntimes["sid"]; !ok {
		t.Error("OrphanedRuntimes entry removed despite error from broken fake")
	}
}
