//go:build linux

package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestManagedDoltAdopterMovesOwnedWatchdogAndServerTogether(t *testing.T) {
	const (
		city        = "/city"
		port        = "29620"
		serverPID   = 202
		watchdogPID = 201
	)
	state := doltRuntimeState{Running: true, PID: serverPID, Port: 29620}
	inspection := managedDoltProcessInspection{
		ManagedPID: serverPID, ManagedOwned: true,
		PortHolderPID: serverPID, PortHolderOwned: true,
	}
	cgroups := map[int][]string{
		watchdogPID: {
			"0::/gascity.slice/gascity-agents.slice",
			"0::/gcdolt.slice/gcdolt-adopt.scope",
			"0::/gcdolt.slice/gcdolt-adopt.scope",
		},
		serverPID: {
			"0::/gascity.slice/gascity-agents.slice",
			"0::/gcdolt.slice/gcdolt-adopt.scope",
			"0::/gcdolt.slice/gcdolt-adopt.scope",
		},
	}
	var ensured string
	var attached []int
	adopter := managedDoltAdopter{
		ensureSlice: func(_ context.Context, slice string) error {
			ensured = slice
			return nil
		},
		startScope: func(_ context.Context, _, _, _ string, pids []int) error {
			attached = append([]int(nil), pids...)
			return nil
		},
		inspect:   func(string, string) (managedDoltProcessInspection, error) { return inspection, nil },
		readState: func(string, int) (doltRuntimeState, error) { return state, nil },
		readPPID:  func(int) (int, error) { return watchdogPID, nil },
		readCmdline: func(pid int) ([]string, error) {
			if pid != watchdogPID {
				t.Fatalf("readCmdline pid = %d, want watchdog", pid)
			}
			return []string{"/opt/gc", managedDoltScopeWatchdogArg, "/city/config.yaml", "/city/dolt.log", city}, nil
		},
		readChildren: func(pid int) ([]int, error) {
			if pid == watchdogPID {
				return []int{serverPID}, nil
			}
			return nil, nil
		},
		readCgroup: func(pid int) (string, error) {
			values := cgroups[pid]
			if len(values) == 0 {
				return "", errors.New("unexpected cgroup read")
			}
			got := values[0]
			cgroups[pid] = values[1:]
			return got, nil
		},
		snapshot: func(pid int) (uint64, string) { return uint64(pid * 10), "" },
		identityMatches: func(int, uint64, string) bool {
			return true
		},
		executable: func() (string, error) { return "/opt/gc", nil },
		configFile: func(string) (string, error) { return "/city/config.yaml", nil },
		logFile:    func(string) (string, error) { return "/city/dolt.log", nil },
	}

	if err := adopter.adopt(context.Background(), city, port, "gcdolt.slice"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if ensured != "gcdolt.slice" {
		t.Fatalf("ensured slice = %q, want gcdolt.slice", ensured)
	}
	if want := []int{watchdogPID, serverPID}; !reflect.DeepEqual(attached, want) {
		t.Fatalf("attached pids = %v, want %v", attached, want)
	}
}

func TestManagedDoltAdopterRejectsForgedWatchdogBeforeAttach(t *testing.T) {
	startCalls := 0
	adopter := managedDoltAdopter{
		ensureSlice: func(context.Context, string) error { return nil },
		startScope: func(context.Context, string, string, string, []int) error {
			startCalls++
			return nil
		},
		inspect: func(string, string) (managedDoltProcessInspection, error) {
			return managedDoltProcessInspection{
				ManagedPID: 202, ManagedOwned: true,
				PortHolderPID: 202, PortHolderOwned: true,
			}, nil
		},
		readState: func(string, int) (doltRuntimeState, error) {
			return doltRuntimeState{Running: true, PID: 202, Port: 29620}, nil
		},
		readPPID:     func(int) (int, error) { return 201, nil },
		readCmdline:  func(int) ([]string, error) { return []string{"/bin/sleep", "infinity"}, nil },
		readChildren: func(int) ([]int, error) { return nil, nil },
		readCgroup:   func(int) (string, error) { return "0::/gascity.slice", nil },
		snapshot:     func(pid int) (uint64, string) { return uint64(pid), "" },
		identityMatches: func(int, uint64, string) bool {
			return true
		},
		executable: func() (string, error) { return "/opt/gc", nil },
		configFile: func(string) (string, error) { return "/city/config.yaml", nil },
		logFile:    func(string) (string, error) { return "/city/dolt.log", nil },
	}

	err := adopter.adopt(context.Background(), "/city", "29620", "gcdolt.slice")
	if err == nil || !strings.Contains(err.Error(), "watchdog argv") {
		t.Fatalf("adopt error = %v, want watchdog argv rejection", err)
	}
	if startCalls != 0 {
		t.Fatalf("startScope called %d times for forged watchdog", startCalls)
	}
}

func TestManagedDoltAdopterFailsClosedOnAttachError(t *testing.T) {
	adopter := validManagedDoltAdopterFixture()
	adopter.startScope = func(context.Context, string, string, string, []int) error {
		return errors.New("attach denied")
	}
	err := adopter.adopt(context.Background(), "/city", "29620", "gcdolt.slice")
	if err == nil || !strings.Contains(err.Error(), "attach denied") {
		t.Fatalf("adopt error = %v, want attach cause", err)
	}
}

func TestManagedDoltAdopterMovesDirectManagedServer(t *testing.T) {
	adopter := validManagedDoltAdopterFixture()
	adopter.readPPID = func(int) (int, error) { return 999, nil }
	adopter.readCmdline = func(pid int) ([]string, error) {
		if pid == 202 {
			return []string{"dolt", "sql-server", "--config", "/city/config.yaml"}, nil
		}
		return []string{"/bin/sleep", "infinity"}, nil
	}
	adopter.readEnviron = func(int) (map[string]string, error) {
		return map[string]string{managedDoltProcessSentinelEnv: managedDoltProcessSentinelValue}, nil
	}
	var attached []int
	adopter.startScope = func(_ context.Context, _, _, _ string, pids []int) error {
		attached = append([]int(nil), pids...)
		return nil
	}

	if err := adopter.adopt(context.Background(), "/city", "29620", "gcdolt.slice"); err != nil {
		t.Fatalf("adopt direct server: %v", err)
	}
	if want := []int{202}; !reflect.DeepEqual(attached, want) {
		t.Fatalf("attached pids = %v, want direct server only %v", attached, want)
	}
}

func TestManagedDoltAdopterRejectsForgedDirectServerWithoutSentinel(t *testing.T) {
	adopter := validManagedDoltAdopterFixture()
	adopter.readPPID = func(int) (int, error) { return 999, nil }
	adopter.readCmdline = func(pid int) ([]string, error) {
		if pid == 202 {
			return []string{"dolt", "sql-server", "--config", "/city/config.yaml"}, nil
		}
		return []string{"/bin/sleep", "infinity"}, nil
	}
	adopter.readEnviron = func(int) (map[string]string, error) {
		return map[string]string{}, nil
	}
	startCalls := 0
	adopter.startScope = func(context.Context, string, string, string, []int) error {
		startCalls++
		return nil
	}

	err := adopter.adopt(context.Background(), "/city", "29620", "gcdolt.slice")
	if err == nil || !strings.Contains(err.Error(), "missing managed-process sentinel") {
		t.Fatalf("adopt error = %v, want direct-server sentinel rejection", err)
	}
	if startCalls != 0 {
		t.Fatalf("startScope called %d times for forged direct server", startCalls)
	}
}

func validManagedDoltAdopterFixture() managedDoltAdopter {
	cgroupReads := map[int]int{}
	return managedDoltAdopter{
		ensureSlice: func(context.Context, string) error { return nil },
		startScope:  func(context.Context, string, string, string, []int) error { return nil },
		inspect: func(string, string) (managedDoltProcessInspection, error) {
			return managedDoltProcessInspection{
				ManagedPID: 202, ManagedOwned: true,
				PortHolderPID: 202, PortHolderOwned: true,
			}, nil
		},
		readState: func(string, int) (doltRuntimeState, error) {
			return doltRuntimeState{Running: true, PID: 202, Port: 29620}, nil
		},
		readPPID: func(int) (int, error) { return 201, nil },
		readCmdline: func(int) ([]string, error) {
			return []string{"/opt/gc", managedDoltScopeWatchdogArg, "/city/config.yaml", "/city/dolt.log", "/city"}, nil
		},
		readChildren: func(pid int) ([]int, error) {
			if pid == 201 {
				return []int{202}, nil
			}
			return nil, nil
		},
		readCgroup: func(pid int) (string, error) {
			cgroupReads[pid]++
			if cgroupReads[pid] == 1 {
				return "0::/gascity.slice/gascity-agents.slice", nil
			}
			return "0::/gcdolt.slice/gcdolt-adopt.scope", nil
		},
		snapshot: func(pid int) (uint64, string) { return uint64(pid), "" },
		identityMatches: func(int, uint64, string) bool {
			return true
		},
		executable: func() (string, error) { return "/opt/gc", nil },
		configFile: func(string) (string, error) { return "/city/config.yaml", nil },
		logFile:    func(string) (string, error) { return "/city/dolt.log", nil },
	}
}
