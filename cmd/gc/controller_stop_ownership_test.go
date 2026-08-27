package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitForAcknowledgedControllerOwnershipReturnsHeldLock(t *testing.T) {
	cityPath := t.TempDir()
	witness := controllerStopStatInfo(t, "acknowledged-socket")
	result := controllerStopResult{
		outcome:    controllerStopAcknowledged,
		socketPath: controllerSocketPath(cityPath),
		socketInfo: witness,
	}
	lease, err := os.OpenFile(filepath.Join(t.TempDir(), "lease"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })

	now := time.Unix(100, 0)
	statCalls := 0
	acquireCalls := 0
	retries := 0
	ownership, err := waitForAcknowledgedControllerOwnershipWithOps(
		cityPath,
		result,
		time.Second,
		controllerStopOwnershipOps{
			stat: func(string) (os.FileInfo, error) {
				statCalls++
				if statCalls < 3 {
					return witness, nil
				}
				return nil, os.ErrNotExist
			},
			acquire: func(string) (*os.File, error) {
				acquireCalls++
				if acquireCalls == 1 {
					return nil, errControllerAlreadyRunning
				}
				return lease, nil
			},
			now: func() time.Time { return now },
			retry: func() {
				retries++
				now = now.Add(time.Millisecond)
			},
		},
	)
	if err != nil {
		t.Fatalf("waitForAcknowledgedControllerOwnershipWithOps() error = %v", err)
	}
	if ownership != lease {
		t.Fatalf("ownership = %p, want lease %p", ownership, lease)
	}
	if _, err := ownership.Stat(); err != nil {
		t.Fatalf("returned ownership is not held: %v", err)
	}
	if acquireCalls != 2 || retries != 1 {
		t.Fatalf("acquire calls = %d, retries = %d; want 2, 1", acquireCalls, retries)
	}
}

func TestWaitForAcknowledgedControllerOwnershipRejectsUnprovenWitness(t *testing.T) {
	cityPath := t.TempDir()
	witness := controllerStopStatInfo(t, "acknowledged-socket")
	replacement := controllerStopStatInfo(t, "replacement-socket")
	base := controllerStopResult{
		outcome:    controllerStopAcknowledged,
		socketPath: controllerSocketPath(cityPath),
		socketInfo: witness,
	}

	tests := []struct {
		name       string
		mutate     func(*controllerStopResult)
		current    os.FileInfo
		wantTarget error
	}{
		{
			name: "invalid outcome",
			mutate: func(result *controllerStopResult) {
				result.outcome = controllerStopOutcomeInvalid
			},
			current:    witness,
			wantTarget: errControllerStopOwnershipUnproven,
		},
		{
			name: "acknowledgement carries error",
			mutate: func(result *controllerStopResult) {
				result.err = errors.New("ambiguous")
			},
			current:    witness,
			wantTarget: errControllerStopOwnershipUnproven,
		},
		{
			name: "missing socket identity",
			mutate: func(result *controllerStopResult) {
				result.socketInfo = nil
			},
			current:    witness,
			wantTarget: errControllerStopOwnershipUnproven,
		},
		{
			name: "wrong socket path",
			mutate: func(result *controllerStopResult) {
				result.socketPath = filepath.Join(cityPath, ".gc", "other.sock")
			},
			current:    witness,
			wantTarget: errControllerStopOwnershipUnproven,
		},
		{
			name:       "socket replaced",
			mutate:     func(*controllerStopResult) {},
			current:    replacement,
			wantTarget: errControllerStopSocketReplaced,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := base
			tt.mutate(&result)
			ownership, err := waitForAcknowledgedControllerOwnershipWithOps(
				cityPath,
				result,
				time.Second,
				controllerStopOwnershipOps{
					stat: func(string) (os.FileInfo, error) { return tt.current, nil },
					acquire: func(string) (*os.File, error) {
						t.Fatal("lock acquisition reached with an unproven witness")
						return nil, errors.New("unreachable")
					},
					now:   time.Now,
					retry: func() {},
				},
			)
			if ownership != nil {
				_ = ownership.Close()
				t.Fatal("unproven witness returned controller ownership")
			}
			if !errors.Is(err, tt.wantTarget) {
				t.Fatalf("error = %v, want errors.Is(..., %v)", err, tt.wantTarget)
			}
		})
	}
}

func TestWaitForAcknowledgedControllerOwnershipClosesLockWhenTerminalStateIsUnproven(t *testing.T) {
	cityPath := t.TempDir()
	witness := controllerStopStatInfo(t, "acknowledged-socket")
	replacement := controllerStopStatInfo(t, "replacement-socket")
	result := controllerStopResult{
		outcome:    controllerStopAcknowledged,
		socketPath: controllerSocketPath(cityPath),
		socketInfo: witness,
	}

	tests := []struct {
		name       string
		after      os.FileInfo
		wantTarget error
	}{
		{name: "original socket remains", after: witness, wantTarget: errControllerStopOwnershipUnproven},
		{name: "replacement appears", after: replacement, wantTarget: errControllerStopSocketReplaced},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lease, err := os.OpenFile(filepath.Join(t.TempDir(), "lease"), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			statCalls := 0
			ownership, waitErr := waitForAcknowledgedControllerOwnershipWithOps(
				cityPath,
				result,
				time.Second,
				controllerStopOwnershipOps{
					stat: func(string) (os.FileInfo, error) {
						statCalls++
						if statCalls == 1 {
							return witness, nil
						}
						return tt.after, nil
					},
					acquire: func(string) (*os.File, error) { return lease, nil },
					now:     time.Now,
					retry:   func() {},
				},
			)
			if ownership != nil {
				_ = ownership.Close()
				t.Fatal("unproven terminal state returned controller ownership")
			}
			if !errors.Is(waitErr, tt.wantTarget) {
				t.Fatalf("error = %v, want errors.Is(..., %v)", waitErr, tt.wantTarget)
			}
			if _, statErr := lease.Stat(); statErr == nil {
				_ = lease.Close()
				t.Fatal("failed terminal validation left the acquired lock open")
			}
		})
	}
}

func TestWaitForAcknowledgedControllerOwnershipBoundsLockWait(t *testing.T) {
	cityPath := t.TempDir()
	witness := controllerStopStatInfo(t, "acknowledged-socket")
	result := controllerStopResult{
		outcome:    controllerStopAcknowledged,
		socketPath: controllerSocketPath(cityPath),
		socketInfo: witness,
	}
	now := time.Unix(100, 0)
	retries := 0
	ownership, err := waitForAcknowledgedControllerOwnershipWithOps(
		cityPath,
		result,
		10*time.Millisecond,
		controllerStopOwnershipOps{
			stat:    func(string) (os.FileInfo, error) { return witness, nil },
			acquire: func(string) (*os.File, error) { return nil, errControllerAlreadyRunning },
			now:     func() time.Time { return now },
			retry: func() {
				retries++
				now = now.Add(10 * time.Millisecond)
			},
		},
	)
	if ownership != nil {
		_ = ownership.Close()
		t.Fatal("timed-out wait returned controller ownership")
	}
	if !errors.Is(err, errControllerStopOwnershipWaitTimeout) {
		t.Fatalf("error = %v, want ownership wait timeout", err)
	}
	if retries != 1 {
		t.Fatalf("retries = %d, want 1", retries)
	}
}

func TestWaitForAcknowledgedControllerOwnershipReturnsLockFailure(t *testing.T) {
	cityPath := t.TempDir()
	witness := controllerStopStatInfo(t, "acknowledged-socket")
	wantErr := errors.New("lock storage failed")
	ownership, err := waitForAcknowledgedControllerOwnershipWithOps(
		cityPath,
		controllerStopResult{
			outcome:    controllerStopAcknowledged,
			socketPath: controllerSocketPath(cityPath),
			socketInfo: witness,
		},
		time.Second,
		controllerStopOwnershipOps{
			stat:    func(string) (os.FileInfo, error) { return witness, nil },
			acquire: func(string) (*os.File, error) { return nil, wantErr },
			now:     time.Now,
			retry:   func() {},
		},
	)
	if ownership != nil {
		_ = ownership.Close()
		t.Fatal("failed lock acquisition returned controller ownership")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want lock failure", err)
	}
}
