package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDetectBinaryDrift(t *testing.T) {
	cases := []struct {
		name         string
		localBuildID string
		supervisorID string
		wantDrift    bool
	}{
		{
			name:         "match",
			localBuildID: "acc19d24",
			supervisorID: "acc19d24",
			wantDrift:    false,
		},
		{
			name:         "mismatch",
			localBuildID: "acc19d24",
			supervisorID: "9e21abcd",
			wantDrift:    true,
		},
		{
			name:         "supervisor empty (older binary, no buildID exposed)",
			localBuildID: "acc19d24",
			supervisorID: "",
			wantDrift:    false,
		},
		{
			name:         "local empty (dev build)",
			localBuildID: "",
			supervisorID: "acc19d24",
			wantDrift:    false,
		},
		{
			name:         "both empty",
			localBuildID: "",
			supervisorID: "",
			wantDrift:    false,
		},
		{
			name:         "match with dirty suffix",
			localBuildID: "acc19d24-dirty",
			supervisorID: "acc19d24-dirty",
			wantDrift:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sv := SupervisorStatus{BuildID: tc.supervisorID}
			got := DetectBinaryDrift(tc.localBuildID, sv)
			if got != tc.wantDrift {
				t.Errorf("DetectBinaryDrift(%q, %q) = %v; want %v", tc.localBuildID, tc.supervisorID, got, tc.wantDrift)
			}
		})
	}
}

func TestDetectPackDrift(t *testing.T) {
	dir := t.TempDir()

	// Pack root with a single file. ParsedAt is set in the past — drift.
	packA := filepath.Join(dir, "packA")
	if err := os.MkdirAll(packA, 0o755); err != nil {
		t.Fatal(err)
	}
	fileA := filepath.Join(packA, "agent.toml")
	if err := os.WriteFile(fileA, []byte("name = \"a\""), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pack root B with ParsedAt in the future — no drift.
	packB := filepath.Join(dir, "packB")
	if err := os.MkdirAll(packB, 0o755); err != nil {
		t.Fatal(err)
	}
	fileB := filepath.Join(packB, "agent.toml")
	if err := os.WriteFile(fileB, []byte("name = \"b\""), 0o644); err != nil {
		t.Fatal(err)
	}

	past := time.Now().Add(-1 * time.Hour)
	future := time.Now().Add(1 * time.Hour)

	t.Run("no roots", func(t *testing.T) {
		drifted, err := DetectPackDrift(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(drifted) != 0 {
			t.Errorf("expected no drift; got %v", drifted)
		}
	})

	t.Run("one drifted, one not", func(t *testing.T) {
		drifted, err := DetectPackDrift([]PackRootStatus{
			{Dir: packA, ParsedAt: past},
			{Dir: packB, ParsedAt: future},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(drifted) != 1 || drifted[0] != packA {
			t.Errorf("expected only packA drifted; got %v", drifted)
		}
	})

	t.Run("zero ParsedAt skips check", func(t *testing.T) {
		drifted, err := DetectPackDrift([]PackRootStatus{
			{Dir: packA, ParsedAt: time.Time{}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(drifted) != 0 {
			t.Errorf("expected zero ParsedAt to skip check; got %v", drifted)
		}
	})

	t.Run("missing dir is reported as error", func(t *testing.T) {
		_, err := DetectPackDrift([]PackRootStatus{
			{Dir: filepath.Join(dir, "no-such-dir"), ParsedAt: past},
		})
		if err == nil {
			t.Errorf("expected error for missing dir")
		}
	})

	t.Run("both drifted", func(t *testing.T) {
		drifted, err := DetectPackDrift([]PackRootStatus{
			{Dir: packA, ParsedAt: past},
			{Dir: packB, ParsedAt: past},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(drifted) != 2 {
			t.Errorf("expected both drifted; got %v", drifted)
		}
	})
}

// fakeSupervisorClient implements SupervisorClient for tests.
type fakeSupervisorClient struct {
	pingErr   error
	pingDelay time.Duration
	pingCount int
}

func (f *fakeSupervisorClient) Status(_ context.Context) (SupervisorStatus, error) {
	return SupervisorStatus{}, errors.New("not implemented")
}

func (f *fakeSupervisorClient) Ping(_ context.Context) error {
	f.pingCount++
	if f.pingDelay > 0 {
		time.Sleep(f.pingDelay)
	}
	return f.pingErr
}

func TestPollReady_succeedsImmediately(t *testing.T) {
	c := &fakeSupervisorClient{pingErr: nil}
	if err := PollReady(c, 1*time.Second); err != nil {
		t.Errorf("expected nil; got %v", err)
	}
	if c.pingCount == 0 {
		t.Errorf("expected at least one ping; got %d", c.pingCount)
	}
}

func TestPollReady_timesOut(t *testing.T) {
	c := &fakeSupervisorClient{pingErr: errors.New("connection refused")}
	err := PollReady(c, 100*time.Millisecond)
	if err == nil {
		t.Errorf("expected timeout error; got nil")
	}
	if c.pingCount == 0 {
		t.Errorf("expected at least one ping attempt; got %d", c.pingCount)
	}
}

func TestPollReady_eventuallySucceeds(t *testing.T) {
	failsBefore := 3
	calls := 0
	c := &countingClient{
		ping: func() error {
			calls++
			if calls <= failsBefore {
				return errors.New("not ready")
			}
			return nil
		},
	}
	if err := PollReady(c, 2*time.Second); err != nil {
		t.Errorf("expected success after retries; got %v", err)
	}
	if calls < failsBefore+1 {
		t.Errorf("expected at least %d calls; got %d", failsBefore+1, calls)
	}
}

type countingClient struct {
	ping func() error
}

func (c *countingClient) Status(_ context.Context) (SupervisorStatus, error) {
	return SupervisorStatus{}, errors.New("not implemented")
}

func (c *countingClient) Ping(_ context.Context) error {
	return c.ping()
}

func TestRestartLoopGuard(t *testing.T) {
	g := newRestartLoopGuard(3, 60*time.Second)
	now := time.Now()

	// First three should succeed.
	for i := 0; i < 3; i++ {
		if !g.allowAt(now.Add(time.Duration(i) * time.Second)) {
			t.Errorf("restart %d: expected allowed", i+1)
		}
	}
	// Fourth within the window should be refused.
	if g.allowAt(now.Add(10 * time.Second)) {
		t.Errorf("restart 4 within window: expected refused")
	}
	// After the window expires, restarts should be allowed again.
	if !g.allowAt(now.Add(120 * time.Second)) {
		t.Errorf("restart after window: expected allowed")
	}
}
