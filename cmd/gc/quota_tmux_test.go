package main

import (
	"fmt"
	"reflect"
	"testing"
)

// FakePane represents a simulated tmux pane with preset output, environment
// variables, and an optional respawn error for testing.
type FakePane struct {
	Output     string
	Env        map[string]string
	RespawnErr error
}

// FakeTmuxOps returns a TmuxOps backed by in-memory FakePane instances.
// Unknown session names produce errors. This is the primary test double for
// the quota rotation subsystem's tmux interactions.
func FakeTmuxOps(panes map[string]*FakePane) TmuxOps {
	return TmuxOps{
		CapturePane: func(sessionName string, _ int) (string, error) {
			p, ok := panes[sessionName]
			if !ok {
				return "", fmt.Errorf("fake: unknown session %q", sessionName)
			}
			return p.Output, nil
		},
		ShowEnv: func(sessionName, key string) (string, error) {
			p, ok := panes[sessionName]
			if !ok {
				return "", fmt.Errorf("fake: unknown session %q", sessionName)
			}
			return p.Env[key], nil
		},
		SetEnv: func(sessionName, key, value string) error {
			p, ok := panes[sessionName]
			if !ok {
				return fmt.Errorf("fake: unknown session %q", sessionName)
			}
			if p.Env == nil {
				p.Env = make(map[string]string)
			}
			p.Env[key] = value
			return nil
		},
		RespawnPane: func(sessionName string) error {
			p, ok := panes[sessionName]
			if !ok {
				return fmt.Errorf("fake: unknown session %q", sessionName)
			}
			return p.RespawnErr
		},
		ListPanes: func() ([]PaneInfo, error) {
			var result []PaneInfo
			for name := range panes {
				result = append(result, PaneInfo{
					SessionName: name,
					PaneID:      "%" + name, // synthetic pane ID
				})
			}
			return result, nil
		},
		IsRunning: func() bool {
			return len(panes) > 0
		},
	}
}

// TestFakeTmuxOps_CapturePane verifies that FakeTmuxOps returns the preset
// output string for a given session name.
func TestFakeTmuxOps_CapturePane(t *testing.T) {
	panes := map[string]*FakePane{
		"worker1": {Output: "Error: rate limit exceeded\nPlease try again later."},
		"worker2": {Output: "Build succeeded."},
	}
	ops := FakeTmuxOps(panes)

	got, err := ops.CapturePane("worker1", 100)
	if err != nil {
		t.Fatalf("CapturePane(worker1): unexpected error: %v", err)
	}
	want := "Error: rate limit exceeded\nPlease try again later."
	if got != want {
		t.Errorf("CapturePane(worker1) = %q, want %q", got, want)
	}

	got, err = ops.CapturePane("worker2", 50)
	if err != nil {
		t.Fatalf("CapturePane(worker2): unexpected error: %v", err)
	}
	if got != "Build succeeded." {
		t.Errorf("CapturePane(worker2) = %q, want %q", got, "Build succeeded.")
	}

	// Unknown session should return error.
	_, err = ops.CapturePane("unknown", 10)
	if err == nil {
		t.Error("CapturePane(unknown): expected error for unknown session, got nil")
	}
}

// TestFakeTmuxOps_ShowEnv verifies that FakeTmuxOps returns preset environment
// variable values for a given session and key.
func TestFakeTmuxOps_ShowEnv(t *testing.T) {
	panes := map[string]*FakePane{
		"worker1": {Env: map[string]string{
			"GC_ACCOUNT":            "work1",
			"GC_ACCOUNT_CONFIG_DIR": "/home/user/.config/work1",
		}},
	}
	ops := FakeTmuxOps(panes)

	got, err := ops.ShowEnv("worker1", "GC_ACCOUNT")
	if err != nil {
		t.Fatalf("ShowEnv(worker1, GC_ACCOUNT): unexpected error: %v", err)
	}
	if got != "work1" {
		t.Errorf("ShowEnv(worker1, GC_ACCOUNT) = %q, want %q", got, "work1")
	}

	got, err = ops.ShowEnv("worker1", "GC_ACCOUNT_CONFIG_DIR")
	if err != nil {
		t.Fatalf("ShowEnv(worker1, GC_ACCOUNT_CONFIG_DIR): unexpected error: %v", err)
	}
	if got != "/home/user/.config/work1" {
		t.Errorf("ShowEnv = %q, want %q", got, "/home/user/.config/work1")
	}

	// Unknown key should return empty or error.
	got, err = ops.ShowEnv("worker1", "NONEXISTENT")
	if err != nil {
		t.Fatalf("ShowEnv(worker1, NONEXISTENT): unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("ShowEnv(worker1, NONEXISTENT) = %q, want empty string", got)
	}

	// Unknown session should return error.
	_, err = ops.ShowEnv("unknown", "GC_ACCOUNT")
	if err == nil {
		t.Error("ShowEnv(unknown): expected error for unknown session, got nil")
	}
}

// TestFakeTmuxOps_SetEnv verifies that FakeTmuxOps updates the environment
// variable in the fake pane and the change is observable via ShowEnv.
func TestFakeTmuxOps_SetEnv(t *testing.T) {
	panes := map[string]*FakePane{
		"worker1": {Env: map[string]string{}},
	}
	ops := FakeTmuxOps(panes)

	// Set a new env var.
	if err := ops.SetEnv("worker1", "GC_QUOTA_STATUS", "limited"); err != nil {
		t.Fatalf("SetEnv: unexpected error: %v", err)
	}

	// Verify it's observable via ShowEnv.
	got, err := ops.ShowEnv("worker1", "GC_QUOTA_STATUS")
	if err != nil {
		t.Fatalf("ShowEnv after SetEnv: unexpected error: %v", err)
	}
	if got != "limited" {
		t.Errorf("ShowEnv after SetEnv = %q, want %q", got, "limited")
	}

	// Overwrite existing value.
	if err := ops.SetEnv("worker1", "GC_QUOTA_STATUS", "cooldown"); err != nil {
		t.Fatalf("SetEnv overwrite: unexpected error: %v", err)
	}
	got, err = ops.ShowEnv("worker1", "GC_QUOTA_STATUS")
	if err != nil {
		t.Fatalf("ShowEnv after overwrite: unexpected error: %v", err)
	}
	if got != "cooldown" {
		t.Errorf("ShowEnv after overwrite = %q, want %q", got, "cooldown")
	}

	// Unknown session should return error.
	if err := ops.SetEnv("unknown", "KEY", "value"); err == nil {
		t.Error("SetEnv(unknown): expected error for unknown session, got nil")
	}
}

// TestFakeTmuxOps_RespawnError verifies that FakeTmuxOps returns the configured
// error from RespawnPane when RespawnErr is set on the FakePane.
func TestFakeTmuxOps_RespawnError(t *testing.T) {
	panes := map[string]*FakePane{
		"worker1": {RespawnErr: nil},
		"worker2": {RespawnErr: errFakeRespawn},
	}
	ops := FakeTmuxOps(panes)

	// worker1 should succeed.
	if err := ops.RespawnPane("worker1"); err != nil {
		t.Errorf("RespawnPane(worker1): unexpected error: %v", err)
	}

	// worker2 should return the configured error.
	if err := ops.RespawnPane("worker2"); err == nil {
		t.Error("RespawnPane(worker2): expected error, got nil")
	}

	// Unknown session should return error.
	if err := ops.RespawnPane("unknown"); err == nil {
		t.Error("RespawnPane(unknown): expected error for unknown session, got nil")
	}
}

// errFakeRespawn is a sentinel error for testing respawn failures.
var errFakeRespawn = fmt.Errorf("fake respawn error: pane not found")

// TestFakeTmuxOps_ListPanes verifies that FakeTmuxOps returns the configured
// list of panes derived from the pane map keys.
func TestFakeTmuxOps_ListPanes(t *testing.T) {
	panes := map[string]*FakePane{
		"worker1": {},
		"worker2": {},
		"coder":   {},
	}
	ops := FakeTmuxOps(panes)

	got, err := ops.ListPanes()
	if err != nil {
		t.Fatalf("ListPanes: unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListPanes: got %d panes, want 3", len(got))
	}

	// Verify all session names are present (order may vary).
	names := make(map[string]bool)
	for _, p := range got {
		names[p.SessionName] = true
	}
	for _, want := range []string{"worker1", "worker2", "coder"} {
		if !names[want] {
			t.Errorf("ListPanes: missing session %q", want)
		}
	}
}

// TestFakeTmuxOps_IsRunning verifies that FakeTmuxOps returns a configurable
// boolean for whether tmux is "running".
func TestFakeTmuxOps_IsRunning(t *testing.T) {
	// With panes → IsRunning returns true.
	panes := map[string]*FakePane{
		"worker1": {},
	}
	ops := FakeTmuxOps(panes)
	if !ops.IsRunning() {
		t.Error("IsRunning: got false, want true (panes exist)")
	}

	// Empty panes → IsRunning returns false.
	opsEmpty := FakeTmuxOps(map[string]*FakePane{})
	if opsEmpty.IsRunning() {
		t.Error("IsRunning: got true, want false (no panes)")
	}
}

// TestDefaultTmuxOps_AllFieldsSet verifies that DefaultTmuxOps("") returns a
// TmuxOps struct with all function fields set (non-nil) when using the default
// tmux server (empty socket name).
func TestDefaultTmuxOps_AllFieldsSet(t *testing.T) {
	ops := DefaultTmuxOps("")

	v := reflect.ValueOf(ops)
	ty := v.Type()
	for i := 0; i < ty.NumField(); i++ {
		field := ty.Field(i)
		fv := v.Field(i)
		if fv.Kind() == reflect.Func && fv.IsNil() {
			t.Errorf("DefaultTmuxOps(\"\").%s is nil — all function fields must be set", field.Name)
		}
	}
}

// TestDefaultTmuxOps_WithSocket_AllFieldsSet verifies that DefaultTmuxOps
// with a non-empty socket name returns a TmuxOps struct with all function
// fields set (non-nil). This validates per-city socket isolation support
// introduced for main's per-city tmux socket pattern.
func TestDefaultTmuxOps_WithSocket_AllFieldsSet(t *testing.T) {
	ops := DefaultTmuxOps("test-city-socket")

	v := reflect.ValueOf(ops)
	ty := v.Type()
	for i := 0; i < ty.NumField(); i++ {
		field := ty.Field(i)
		fv := v.Field(i)
		if fv.Kind() == reflect.Func && fv.IsNil() {
			t.Errorf("DefaultTmuxOps(\"test-city-socket\").%s is nil — all function fields must be set", field.Name)
		}
	}
}

// TestFakeTmuxOps_FullContract exercises ALL operations in sequence on a single
// FakeTmuxOps instance: list, capture, showenv, setenv, respawn. Verifies that
// state changes are observable. This validates the fake before it's used as a
// dependency in Steps 2.5 and 2.7.
func TestFakeTmuxOps_FullContract(t *testing.T) {
	panes := map[string]*FakePane{
		"coder": {
			Output: "Claude: I'll help you with that.\n$ ",
			Env: map[string]string{
				"GC_ACCOUNT": "work1",
			},
			RespawnErr: nil,
		},
		"reviewer": {
			Output: "Error: 429 Too Many Requests\nRate limit exceeded.",
			Env: map[string]string{
				"GC_ACCOUNT": "work2",
			},
			RespawnErr: nil,
		},
	}
	ops := FakeTmuxOps(panes)

	// 1. IsRunning — should be true with panes.
	if !ops.IsRunning() {
		t.Fatal("FullContract: IsRunning should be true")
	}

	// 2. ListPanes — should list both sessions.
	listed, err := ops.ListPanes()
	if err != nil {
		t.Fatalf("FullContract: ListPanes error: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("FullContract: ListPanes got %d, want 2", len(listed))
	}

	// 3. CapturePane — verify output from reviewer.
	output, err := ops.CapturePane("reviewer", 100)
	if err != nil {
		t.Fatalf("FullContract: CapturePane(reviewer) error: %v", err)
	}
	if output != "Error: 429 Too Many Requests\nRate limit exceeded." {
		t.Errorf("FullContract: CapturePane(reviewer) = %q, unexpected", output)
	}

	// 4. ShowEnv — read existing env.
	acct, err := ops.ShowEnv("coder", "GC_ACCOUNT")
	if err != nil {
		t.Fatalf("FullContract: ShowEnv(coder, GC_ACCOUNT) error: %v", err)
	}
	if acct != "work1" {
		t.Errorf("FullContract: ShowEnv = %q, want %q", acct, "work1")
	}

	// 5. SetEnv — update env and verify the change is observable.
	if err := ops.SetEnv("reviewer", "GC_QUOTA_STATUS", "limited"); err != nil {
		t.Fatalf("FullContract: SetEnv error: %v", err)
	}
	status, err := ops.ShowEnv("reviewer", "GC_QUOTA_STATUS")
	if err != nil {
		t.Fatalf("FullContract: ShowEnv after SetEnv error: %v", err)
	}
	if status != "limited" {
		t.Errorf("FullContract: ShowEnv after SetEnv = %q, want %q", status, "limited")
	}

	// 6. RespawnPane — should succeed for coder.
	if err := ops.RespawnPane("coder"); err != nil {
		t.Errorf("FullContract: RespawnPane(coder) error: %v", err)
	}

	// 7. RespawnPane — should succeed for reviewer too (no error configured).
	if err := ops.RespawnPane("reviewer"); err != nil {
		t.Errorf("FullContract: RespawnPane(reviewer) error: %v", err)
	}
}
