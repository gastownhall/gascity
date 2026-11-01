package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestRecoverManagedDoltExistingObserveTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{name: "zero defaults to 5s", timeout: 0, want: 5 * time.Second},
		{name: "negative defaults to 5s", timeout: -1, want: 5 * time.Second},
		{name: "below 5s returns input", timeout: 2 * time.Second, want: 2 * time.Second},
		{name: "exactly 5s returns 5s", timeout: 5 * time.Second, want: 5 * time.Second},
		{name: "above 5s capped at 5s", timeout: 30 * time.Second, want: 5 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := recoverManagedDoltExistingObserveTimeout(tt.timeout); got != tt.want {
				t.Errorf("recoverManagedDoltExistingObserveTimeout(%v) = %v, want %v", tt.timeout, got, tt.want)
			}
		})
	}
}

// TestRecoverManagedDoltRefusesProviderOwnedScopesBeforeLifecycleOps keeps
// the legacy managed-Dolt recovery path out of scopes whose lifecycle is
// already assigned to bd. The injected operations make any probe, health,
// stop, cleanup, start, or runtime publication observable.
func TestRecoverManagedDoltRefusesProviderOwnedScopesBeforeLifecycleOps(t *testing.T) {
	for _, tc := range []struct {
		name        string
		setup       func(t *testing.T, city string)
		wantBlocked bool
	}{
		{
			name: "initializing provider ownership",
			setup: func(t *testing.T, city string) {
				t.Helper()
				if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
					t.Fatal(err)
				}
			},
			wantBlocked: true,
		},
		{
			name: "ready provider ownership",
			setup: func(t *testing.T, city string) {
				t.Helper()
				if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
					t.Fatal(err)
				}
				if err := markProviderScopeOwnershipReady(city, city); err != nil {
					t.Fatal(err)
				}
			},
			wantBlocked: true,
		},
		{
			name: "corrupt provider ownership journal",
			setup: func(t *testing.T, city string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(providerScopeOwnershipPath(city), []byte("not json"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantBlocked: true,
		},
		{
			name:        "legacy absent journal remains admitted",
			setup:       func(*testing.T, string) {},
			wantBlocked: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			tc.setup(t, city)
			var calls []string
			ops := managedDoltRecoveryOps{
				queryProbe: func(string, string, string) error { calls = append(calls, "query"); return errors.New("unavailable") },
				healthCheck: func(string, string, string) (managedDoltSQLHealthReport, error) {
					calls = append(calls, "health")
					return managedDoltSQLHealthReport{}, nil
				},
				stop: func(string, string) (managedDoltStopReport, error) {
					calls = append(calls, "stop")
					return managedDoltStopReport{}, nil
				},
				preflightCleanup: func(string) error { calls = append(calls, "preflight"); return errors.New("stop after admission") },
				start: func(string, string, string, string, string, time.Duration) (managedDoltStartReport, error) {
					calls = append(calls, "start")
					return managedDoltStartReport{}, nil
				},
				publish:       func(string) error { calls = append(calls, "publish"); return nil },
				failedCleanup: func(_ string, _ int, _ int, cause error) error { calls = append(calls, "failed-cleanup"); return cause },
			}
			_, err := recoverManagedDoltProcessWithOps(city, "127.0.0.1", "3307", "root", "warning", time.Second, ops)
			if tc.wantBlocked {
				if err == nil || !strings.Contains(err.Error(), "provider scope ownership") {
					t.Fatalf("recover error = %v, want provider ownership refusal", err)
				}
				if len(calls) != 0 {
					t.Fatalf("provider-owned recovery invoked lifecycle operations: %v", calls)
				}
				return
			}
			if err == nil || strings.Contains(err.Error(), "provider scope ownership") {
				t.Fatalf("legacy recovery error = %v, want later recovery failure rather than ownership admission", err)
			}
			if len(calls) == 0 {
				t.Fatal("legacy recovery performed no lifecycle operation")
			}
		})
	}
}

func TestManagedDoltLifecycleOwnedDefersToProviderScopeOwnership(t *testing.T) {
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"owned\"\n\n[beads]\nprovider = \"bd\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	owned, err := managedDoltLifecycleOwned(city)
	if err != nil || owned {
		t.Fatalf("managed lifecycle ownership with generic provider journal = (%t, %v), want (false, nil)", owned, err)
	}
	if err := os.WriteFile(providerScopeOwnershipPath(city), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := managedDoltLifecycleOwned(city); err == nil || !strings.Contains(err.Error(), "provider scope ownership") {
		t.Fatalf("corrupt provider ownership journal error = %v, want fail-closed ownership error", err)
	}
}

// TestManagedDoltLifecycleOwnedDefersToAnUnjournaledProxiedBinding covers the
// shape `gc beads city migrate-proxied` leaves behind: bd's proxied binding and
// no journal record, because the migration deliberately journals nothing. Asking
// the journal alone called that city gc-managed, so the controller forked a
// provider health op for the city and every rig on every reconcile tick while a
// freshly journaled proxied city — the default topology — got none.
func TestManagedDoltLifecycleOwnedDefersToAnUnjournaledProxiedBinding(t *testing.T) {
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"migrated\"\n\n[beads]\nprovider = \"bd\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(city, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "proxied-server",
		DoltDatabase: "hq",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(providerScopeOwnershipPath(city)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture journaled ownership: %v", err)
	}
	if owned, err := cityScopeProviderOwned(city); err != nil || !owned {
		t.Fatalf("cityScopeProviderOwned = (%t, %v), want the binding classified as provider-owned", owned, err)
	}
	if owned, err := managedDoltLifecycleOwned(city); err != nil || owned {
		t.Fatalf("managedDoltLifecycleOwned = (%t, %v), want (false, nil)", owned, err)
	}

	// The tick consequence: no provider health fan-out for a city gc does not
	// own the Dolt runtime of.
	var health int
	ensureManagedDoltPublishedForRuntime(city, io.Discard, "test", func(string) error {
		health++
		return nil
	}, managedDoltLifecycleOwned, func(string) string { return "" })
	if health != 0 {
		t.Fatalf("controller ran %d provider health ops for a bd-owned proxied city, want 0", health)
	}
}

func TestRecoverManagedDoltShouldReuseExisting(t *testing.T) {
	tests := []struct {
		name          string
		existingPort  int
		requestedPort string
		want          bool
	}{
		{name: "zero port never reuses", existingPort: 0, requestedPort: "3306", want: false},
		{name: "negative port never reuses", existingPort: -1, requestedPort: "3306", want: false},
		{name: "empty requested always reuses", existingPort: 3306, requestedPort: "", want: true},
		{name: "whitespace requested always reuses", existingPort: 3306, requestedPort: "  ", want: true},
		{name: "different port reuses", existingPort: 3307, requestedPort: "3306", want: true},
		{name: "same port does not reuse", existingPort: 3306, requestedPort: "3306", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := recoverManagedDoltShouldReuseExisting(tt.existingPort, tt.requestedPort); got != tt.want {
				t.Errorf("recoverManagedDoltShouldReuseExisting(%d, %q) = %v, want %v",
					tt.existingPort, tt.requestedPort, got, tt.want)
			}
		})
	}
}

func TestManagedDoltRecoverFields(t *testing.T) {
	report := managedDoltRecoverReport{
		DiagnosedReadOnly: true,
		HadPID:            true,
		Forced:            false,
		Ready:             true,
		PID:               9876,
		Port:              3311,
		Healthy:           true,
		Restarted:         true,
	}
	fields := managedDoltRecoverFields(report)
	want := []string{
		"diagnosed_read_only\ttrue",
		"had_pid\ttrue",
		"forced\tfalse",
		"ready\ttrue",
		"ready_inconclusive\tfalse",
		"pid\t9876",
		"port\t3311",
		"healthy\ttrue",
		"restarted\ttrue",
	}
	if len(fields) != len(want) {
		t.Fatalf("got %d fields, want %d", len(fields), len(want))
	}
	for i, w := range want {
		if fields[i] != w {
			t.Errorf("fields[%d] = %q, want %q", i, fields[i], w)
		}
	}
}

func TestRecoverManagedDoltProcessWithOps(t *testing.T) {
	queryErr := errors.New("query unavailable")
	stopErr := errors.New("stop failed")
	preflightErr := errors.New("preflight failed")
	startErr := errors.New("start failed")
	healthErr := errors.New("health failed")
	publishErr := errors.New("publish failed")
	cleanupErr := errors.New("cleanup failed")

	type testCase struct {
		name         string
		stopReport   managedDoltStopReport
		stopErr      error
		preflightErr error
		startReport  managedDoltStartReport
		startErr     error
		healthReport managedDoltSQLHealthReport
		healthErr    error
		publishErr   error
		cleanupErr   error
		wantReport   managedDoltRecoverReport
		wantErrIs    error
		wantErrText  string
		wantOps      []string
	}

	const (
		cityToken = "$CITY"
		queryOp   = "query host=127.0.0.1 port=3306 user=root"
		stopOp    = "stop city=$CITY port=3306"
		preflight = "preflight city=$CITY"
		startOp   = "start city=$CITY host=127.0.0.1 port=3306 user=root log=warning timeout=2s"
		healthOp  = "health host=127.0.0.1 port=4407 user=root"
		publishOp = "publish city=$CITY"
	)
	baseStop := managedDoltStopReport{HadPID: true, PID: 111}
	baseStart := managedDoltStartReport{Ready: true, PID: 222, Port: 4407}
	baseReport := managedDoltRecoverReport{
		HadPID:    true,
		Ready:     true,
		PID:       222,
		Port:      4407,
		Healthy:   true,
		Restarted: true,
	}
	tests := []testCase{
		{
			name:         "unknown no-user final health succeeds",
			stopReport:   baseStop,
			startReport:  baseStart,
			healthReport: managedDoltSQLHealthReport{QueryReady: true, ReadOnly: "unknown"},
			wantReport:   baseReport,
			wantOps:      []string{queryOp, stopOp, preflight, startOp, healthOp, publishOp},
		},
		{
			name:         "writable final health succeeds",
			stopReport:   baseStop,
			startReport:  baseStart,
			healthReport: managedDoltSQLHealthReport{QueryReady: true, ReadOnly: "false"},
			wantReport:   baseReport,
			wantOps:      []string{queryOp, stopOp, preflight, startOp, healthOp, publishOp},
		},
		{
			name:         "stop error is ignored and report is retained",
			stopReport:   managedDoltStopReport{HadPID: true, PID: 111, Forced: true},
			stopErr:      stopErr,
			startReport:  baseStart,
			healthReport: managedDoltSQLHealthReport{QueryReady: true, ReadOnly: "false"},
			wantReport: managedDoltRecoverReport{
				HadPID: true, Forced: true, Ready: true, PID: 222, Port: 4407, Healthy: true, Restarted: true,
			},
			wantOps: []string{queryOp, stopOp, preflight, startOp, healthOp, publishOp},
		},
		{
			name:         "preflight failure cleans stopped process at requested port",
			stopReport:   baseStop,
			preflightErr: preflightErr,
			cleanupErr:   cleanupErr,
			wantReport: managedDoltRecoverReport{
				HadPID: true, PID: 111, Port: 3306,
			},
			wantErrIs:   cleanupErr,
			wantErrText: cleanupErr.Error(),
			wantOps: []string{
				queryOp, stopOp, preflight,
				"cleanup city=$CITY pid=111 port=3306 cause=preflight failed",
			},
		},
		{
			name:        "start failure returns start report without coordinator cleanup",
			stopReport:  baseStop,
			startReport: managedDoltStartReport{PID: 222, Port: 4407, AddressInUse: true, Attempts: 2},
			startErr:    startErr,
			wantReport: managedDoltRecoverReport{
				HadPID: true, PID: 222, Port: 4407, Restarted: true,
			},
			wantErrIs:   startErr,
			wantErrText: startErr.Error(),
			wantOps:     []string{queryOp, stopOp, preflight, startOp},
		},
		{
			name:        "final health error cleans replacement at resolved port",
			stopReport:  baseStop,
			startReport: baseStart,
			healthErr:   healthErr,
			cleanupErr:  cleanupErr,
			wantReport: managedDoltRecoverReport{
				HadPID: true, Ready: true, PID: 222, Port: 4407, Restarted: true,
			},
			wantErrIs:   cleanupErr,
			wantErrText: cleanupErr.Error(),
			wantOps: []string{
				queryOp, stopOp, preflight, startOp, healthOp,
				"cleanup city=$CITY pid=222 port=4407 cause=health failed",
			},
		},
		{
			name:         "still read-only cleans replacement",
			stopReport:   baseStop,
			startReport:  baseStart,
			healthReport: managedDoltSQLHealthReport{QueryReady: true, ReadOnly: "true"},
			wantReport: managedDoltRecoverReport{
				HadPID: true, Ready: true, PID: 222, Port: 4407, Restarted: true,
			},
			wantErrText: "dolt server on 127.0.0.1:4407 is still read-only after recovery",
			wantOps: []string{
				queryOp, stopOp, preflight, startOp, healthOp,
				"cleanup city=$CITY pid=222 port=4407 cause=dolt server on 127.0.0.1:4407 is still read-only after recovery",
			},
		},
		{
			name:         "query-not-ready cleans replacement",
			stopReport:   baseStop,
			startReport:  baseStart,
			healthReport: managedDoltSQLHealthReport{ReadOnly: "false"},
			wantReport: managedDoltRecoverReport{
				HadPID: true, Ready: true, PID: 222, Port: 4407, Restarted: true,
			},
			wantErrText: "dolt server on 127.0.0.1:4407 is not query-ready after recovery",
			wantOps: []string{
				queryOp, stopOp, preflight, startOp, healthOp,
				"cleanup city=$CITY pid=222 port=4407 cause=dolt server on 127.0.0.1:4407 is not query-ready after recovery",
			},
		},
		{
			name:         "publication failure cleans replacement",
			stopReport:   baseStop,
			startReport:  baseStart,
			healthReport: managedDoltSQLHealthReport{QueryReady: true, ReadOnly: "false"},
			publishErr:   publishErr,
			wantReport:   baseReport,
			wantErrIs:    publishErr,
			wantErrText:  "publish managed dolt runtime state: publish failed",
			wantOps: []string{
				queryOp, stopOp, preflight, startOp, healthOp, publishOp,
				"cleanup city=$CITY pid=222 port=4407 cause=publish managed dolt runtime state: publish failed",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			var gotOps []string
			ops := managedDoltRecoveryOps{
				queryProbe: func(host, port, user string) error {
					gotOps = append(gotOps, fmt.Sprintf("query host=%s port=%s user=%s", host, port, user))
					return queryErr
				},
				healthCheck: func(host, port, user string) (managedDoltSQLHealthReport, error) {
					gotOps = append(gotOps, fmt.Sprintf("health host=%s port=%s user=%s", host, port, user))
					return tt.healthReport, tt.healthErr
				},
				stop: func(cityPath, port string) (managedDoltStopReport, error) {
					gotOps = append(gotOps, fmt.Sprintf("stop city=%s port=%s", cityPath, port))
					return tt.stopReport, tt.stopErr
				},
				preflightCleanup: func(cityPath string) error {
					gotOps = append(gotOps, "preflight city="+cityPath)
					return tt.preflightErr
				},
				start: func(cityPath, host, port, user, logLevel string, timeout time.Duration) (managedDoltStartReport, error) {
					gotOps = append(gotOps, fmt.Sprintf("start city=%s host=%s port=%s user=%s log=%s timeout=%s", cityPath, host, port, user, logLevel, timeout))
					return tt.startReport, tt.startErr
				},
				publish: func(cityPath string) error {
					gotOps = append(gotOps, "publish city="+cityPath)
					return tt.publishErr
				},
				failedCleanup: func(cityPath string, pid, port int, cause error) error {
					gotOps = append(gotOps, fmt.Sprintf("cleanup city=%s pid=%d port=%d cause=%v", cityPath, pid, port, cause))
					if tt.cleanupErr != nil {
						return tt.cleanupErr
					}
					return cause
				},
			}

			gotReport, gotErr := recoverManagedDoltProcessWithOps(
				cityPath, "127.0.0.1", "3306", "root", "warning", 2*time.Second, ops,
			)
			if gotReport != tt.wantReport {
				t.Errorf("report = %+v, want %+v", gotReport, tt.wantReport)
			}
			switch {
			case tt.wantErrIs != nil && !errors.Is(gotErr, tt.wantErrIs):
				t.Errorf("error = %v, want errors.Is(_, %v)", gotErr, tt.wantErrIs)
			case tt.wantErrIs == nil && tt.wantErrText == "" && gotErr != nil:
				t.Errorf("error = %v, want nil", gotErr)
			}
			if tt.wantErrText != "" && (gotErr == nil || gotErr.Error() != tt.wantErrText) {
				t.Errorf("error = %v, want text %q", gotErr, tt.wantErrText)
			}
			wantOps := make([]string, len(tt.wantOps))
			for i, op := range tt.wantOps {
				wantOps[i] = strings.ReplaceAll(op, cityToken, cityPath)
			}
			if strings.Join(gotOps, "\n") != strings.Join(wantOps, "\n") {
				t.Errorf("operations:\n%s\nwant:\n%s", strings.Join(gotOps, "\n"), strings.Join(wantOps, "\n"))
			}
		})
	}
}

func TestCleanupFailedManagedDoltRecovery_NilCause(t *testing.T) {
	if err := cleanupFailedManagedDoltRecovery("/nonexistent", 0, 0, nil); err != nil {
		t.Errorf("cleanupFailedManagedDoltRecovery(nil cause) = %v, want nil", err)
	}
}

func TestCleanupFailedManagedDoltRecovery_ClearsRuntimeAndPublishedState(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "cleanup-test"

[beads]
provider = "bd"
backend = "dolt"
`), 0o644); err != nil {
		t.Fatalf("write city config: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatalf("create beads dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatalf("write beads config: %v", err)
	}
	owned, err := managedDoltLifecycleOwned(cityPath)
	if err != nil {
		t.Fatalf("managedDoltLifecycleOwned: %v", err)
	}
	if !owned {
		t.Fatal("managedDoltLifecycleOwned = false, want true for managed bd city")
	}

	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	const (
		pid  = 4242
		port = 33123
	)
	state := doltRuntimeState{
		Running:   true,
		PID:       pid,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: "2026-07-21T00:00:00Z",
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, state); err != nil {
		t.Fatalf("write provider runtime state: %v", err)
	}
	if err := os.WriteFile(layout.PIDFile, []byte("4242\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	if err := writeDoltRuntimeStateFile(managedDoltStatePath(cityPath), state); err != nil {
		t.Fatalf("write published runtime state: %v", err)
	}

	cause := errors.New("preflight cleanup failed")
	err = cleanupFailedManagedDoltRecovery(cityPath, 0, port, cause)
	if !errors.Is(err, cause) {
		t.Fatalf("cleanupFailedManagedDoltRecovery error = %v, want sentinel cause", err)
	}

	got, err := readDoltRuntimeStateFile(layout.StateFile)
	if err != nil {
		t.Fatalf("read provider runtime state: %v", err)
	}
	if got.Running || got.PID != 0 || got.Port != port {
		t.Fatalf("provider runtime state = {Running:%v PID:%d Port:%d}, want {Running:false PID:0 Port:%d}", got.Running, got.PID, got.Port, port)
	}
	if _, err := os.Stat(layout.PIDFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pid file stat error = %v, want os.ErrNotExist", err)
	}
	if _, err := os.Stat(managedDoltStatePath(cityPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published runtime state stat error = %v, want os.ErrNotExist", err)
	}
}

func TestRecoverManagedDoltObservedRebindPossible(t *testing.T) {
	t.Run("empty port always possible", func(t *testing.T) {
		if !recoverManagedDoltObservedRebindPossible(t.TempDir(), "") {
			t.Error("empty requestedPort should return true")
		}
	})

	t.Run("no state files returns false", func(t *testing.T) {
		if recoverManagedDoltObservedRebindPossible(t.TempDir(), "3306") {
			t.Error("missing state files should return false")
		}
	})

	t.Run("state with different port returns true", func(t *testing.T) {
		cityPath := t.TempDir()
		statePath := providerManagedDoltStatePath(cityPath)
		if err := writeDoltRuntimeStateFile(statePath, doltRuntimeState{
			Running: true,
			PID:     1234,
			Port:    3307,
		}); err != nil {
			t.Fatalf("writeDoltRuntimeStateFile: %v", err)
		}
		if !recoverManagedDoltObservedRebindPossible(cityPath, "3306") {
			t.Error("different port should return true")
		}
	})

	t.Run("state with same port returns false", func(t *testing.T) {
		cityPath := t.TempDir()
		statePath := providerManagedDoltStatePath(cityPath)
		if err := writeDoltRuntimeStateFile(statePath, doltRuntimeState{
			Running: true,
			PID:     1234,
			Port:    3306,
		}); err != nil {
			t.Fatalf("writeDoltRuntimeStateFile: %v", err)
		}
		if recoverManagedDoltObservedRebindPossible(cityPath, "3306") {
			t.Error("same port should return false")
		}
	})
}

func TestRecoverManagedDoltObservedPortUsesAlternatePublishedState(t *testing.T) {
	cityPath := t.TempDir()
	if err := writeDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath), doltRuntimeState{
		Running: true,
		PID:     1234,
		Port:    36267,
	}); err != nil {
		t.Fatalf("write provider state: %v", err)
	}
	if err := writeDoltRuntimeStateFile(managedDoltStatePath(cityPath), doltRuntimeState{
		Running: true,
		PID:     5678,
		Port:    36266,
	}); err != nil {
		t.Fatalf("write published state: %v", err)
	}

	if got := recoverManagedDoltObservedPort(cityPath, "36267"); got != "36266" {
		t.Fatalf("recoverManagedDoltObservedPort() = %q, want alternate published port 36266", got)
	}
}

func setupRecoveryTestCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	packStateDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(packStateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	t.Setenv("GC_DOLT_PASSWORD", "test")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	return cityPath
}

func writeRecoveryRuntimeState(t *testing.T, cityPath string, pid, port int) {
	t.Helper()
	if err := writeDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath), doltRuntimeState{
		Running:   true,
		PID:       pid,
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}
}

func TestRecoverManagedDolt_SkipsRestartWhenProbeHealthy(t *testing.T) {
	cityPath := setupRecoveryTestCity(t)
	writeRecoveryRuntimeState(t, cityPath, 4321, 3306)

	oldProbe := managedDoltQueryProbeDirectFn
	oldReadOnly := managedDoltReadOnlyStateDirectFn
	oldConnCount := managedDoltConnectionCountDirectFn
	t.Cleanup(func() {
		managedDoltQueryProbeDirectFn = oldProbe
		managedDoltReadOnlyStateDirectFn = oldReadOnly
		managedDoltConnectionCountDirectFn = oldConnCount
	})

	managedDoltQueryProbeDirectFn = func(_, _, _ string) error { return nil }
	managedDoltReadOnlyStateDirectFn = func(_, _, _ string) (string, error) { return "false", nil }
	managedDoltConnectionCountDirectFn = func(_, _, _ string) (string, error) { return "5", nil }

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", "3306", "root", "warning", 10*time.Second)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !report.Ready {
		t.Error("expected Ready=true when probe succeeds")
	}
	if !report.Healthy {
		t.Error("expected Healthy=true when probe succeeds")
	}
	if report.DiagnosedReadOnly {
		t.Error("expected DiagnosedReadOnly=false for healthy server")
	}
	if !report.HadPID {
		t.Error("expected HadPID=true from runtime state")
	}
	if report.PID != 4321 {
		t.Errorf("expected PID=4321 from runtime state, got %d", report.PID)
	}
	if report.Port != 3306 {
		t.Errorf("expected Port=3306 from runtime state, got %d", report.Port)
	}
	if report.Restarted {
		t.Error("expected Restarted=false when healthy server is reused")
	}
}

func TestRecoverManagedDolt_ProceedsWhenReadOnly(t *testing.T) {
	cityPath := setupRecoveryTestCity(t)

	oldProbe := managedDoltQueryProbeDirectFn
	oldReadOnly := managedDoltReadOnlyStateDirectFn
	oldConnCount := managedDoltConnectionCountDirectFn
	oldPreflight := managedDoltPreflightCleanupFn
	t.Cleanup(func() {
		managedDoltQueryProbeDirectFn = oldProbe
		managedDoltReadOnlyStateDirectFn = oldReadOnly
		managedDoltConnectionCountDirectFn = oldConnCount
		managedDoltPreflightCleanupFn = oldPreflight
	})

	managedDoltQueryProbeDirectFn = func(_, _, _ string) error { return nil }
	managedDoltReadOnlyStateDirectFn = func(_, _, _ string) (string, error) { return "true", nil }
	managedDoltConnectionCountDirectFn = func(_, _, _ string) (string, error) { return "5", nil }
	managedDoltPreflightCleanupFn = func(_ string) error {
		return fmt.Errorf("stop: expected — no real dolt process")
	}

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", "3306", "root", "warning", 10*time.Second)
	if err == nil {
		t.Fatal("expected error when read-only server recovery proceeds to stop/start")
	}
	if !report.DiagnosedReadOnly {
		t.Error("expected DiagnosedReadOnly=true for read-only server")
	}
	if report.Ready {
		t.Error("expected Ready=false when recovery proceeds past probe")
	}
}

func TestRecoverManagedDolt_ProceedsWhenProbeUnreachable(t *testing.T) {
	cityPath := setupRecoveryTestCity(t)
	preflightErr := errors.New("preflight cleanup failed")

	oldProbe := managedDoltQueryProbeDirectFn
	oldPreflight := managedDoltPreflightCleanupFn
	t.Cleanup(func() {
		managedDoltQueryProbeDirectFn = oldProbe
		managedDoltPreflightCleanupFn = oldPreflight
	})

	managedDoltQueryProbeDirectFn = func(_, _, _ string) error {
		return fmt.Errorf("connection refused")
	}
	managedDoltPreflightCleanupFn = func(_ string) error {
		return preflightErr
	}

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", "3306", "root", "warning", 10*time.Second)
	if !errors.Is(err, preflightErr) {
		t.Fatalf("recoverManagedDoltProcess error = %v, want preflight cleanup sentinel", err)
	}
	if report.Ready {
		t.Error("expected Ready=false when probe fails")
	}
}

func TestRecoverManagedDolt_ProceedsWhenReadOnlyUnknown(t *testing.T) {
	cityPath := setupRecoveryTestCity(t)

	oldProbe := managedDoltQueryProbeDirectFn
	oldReadOnly := managedDoltReadOnlyStateDirectFn
	oldConnCount := managedDoltConnectionCountDirectFn
	oldPreflight := managedDoltPreflightCleanupFn
	t.Cleanup(func() {
		managedDoltQueryProbeDirectFn = oldProbe
		managedDoltReadOnlyStateDirectFn = oldReadOnly
		managedDoltConnectionCountDirectFn = oldConnCount
		managedDoltPreflightCleanupFn = oldPreflight
	})

	managedDoltQueryProbeDirectFn = func(_, _, _ string) error { return nil }
	managedDoltReadOnlyStateDirectFn = func(_, _, _ string) (string, error) {
		return "unknown", errManagedDoltNoUserDatabase
	}
	managedDoltConnectionCountDirectFn = func(_, _, _ string) (string, error) { return "5", nil }
	managedDoltPreflightCleanupFn = func(_ string) error {
		return fmt.Errorf("stop: expected - no real dolt process")
	}

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", "3306", "root", "warning", 10*time.Second)
	if err == nil {
		t.Fatal("expected error when read-only state is unknown and recovery proceeds to stop/start")
	}
	if report.DiagnosedReadOnly {
		t.Error("expected DiagnosedReadOnly=false for unknown read-only state")
	}
	if report.Ready {
		t.Error("expected Ready=false when recovery proceeds past unknown read-only health")
	}
}

func TestRecoverManagedDolt_ProceedsWhenHealthCheckErrors(t *testing.T) {
	cityPath := setupRecoveryTestCity(t)

	oldProbe := managedDoltQueryProbeDirectFn
	oldReadOnly := managedDoltReadOnlyStateDirectFn
	oldPreflight := managedDoltPreflightCleanupFn
	t.Cleanup(func() {
		managedDoltQueryProbeDirectFn = oldProbe
		managedDoltReadOnlyStateDirectFn = oldReadOnly
		managedDoltPreflightCleanupFn = oldPreflight
	})

	managedDoltQueryProbeDirectFn = func(_, _, _ string) error { return nil }
	managedDoltReadOnlyStateDirectFn = func(_, _, _ string) (string, error) {
		return "", fmt.Errorf("broken pipe")
	}
	managedDoltPreflightCleanupFn = func(_ string) error {
		return fmt.Errorf("stop: expected — no real dolt process")
	}

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", "3306", "root", "warning", 10*time.Second)
	if err == nil {
		t.Fatal("expected error when health check fails and recovery proceeds to stop/start")
	}
	if report.Ready {
		t.Error("expected Ready=false when health check errors")
	}
}

// unresponsiveRecoveryHarness drives recoverManagedDoltProcessWithOps against a
// managed Dolt whose query probe fails, recording which lifecycle ops ran.
type unresponsiveRecoveryHarness struct {
	city    string
	layout  managedDoltRuntimeLayout
	ops     managedDoltRecoveryOps
	stopped int
	started int
	now     time.Time
}

const (
	unresponsiveTestPort   = "3306"
	unresponsiveTestWindow = 180 * time.Second
)

var errUnresponsiveTestProbe = errors.New("query probe timed out after 5s")

func newUnresponsiveRecoveryHarness(t *testing.T, probeErr error, listeningPID int) *unresponsiveRecoveryHarness {
	t.Helper()
	city := setupRecoveryTestCity(t)
	layout, err := resolveManagedDoltRuntimeLayout(city)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	h := &unresponsiveRecoveryHarness{city: city, layout: layout, now: time.Unix(1_700_000_000, 0).UTC()}
	h.ops = managedDoltRecoveryOps{
		queryProbe: func(string, string, string) error { return probeErr },
		healthCheck: func(string, string, string) (managedDoltSQLHealthReport, error) {
			return managedDoltSQLHealthReport{QueryReady: true, ReadOnly: "false"}, nil
		},
		stop: func(string, string) (managedDoltStopReport, error) {
			h.stopped++
			return managedDoltStopReport{HadPID: true, PID: listeningPID}, nil
		},
		preflightCleanup: func(string) error { return nil },
		start: func(string, string, string, string, string, time.Duration) (managedDoltStartReport, error) {
			h.started++
			return managedDoltStartReport{Ready: true, PID: 9999, Port: 3306}, nil
		},
		publish:       func(string) error { return nil },
		failedCleanup: func(_ string, _ int, _ int, cause error) error { return cause },
		classifyUnresponsive: func(string, string, string) (int, bool) {
			return listeningPID, listeningPID > 0
		},
		now:                      func() time.Time { return h.now },
		unresponsiveRestartAfter: unresponsiveTestWindow,
	}
	return h
}

func (h *unresponsiveRecoveryHarness) recover(t *testing.T) (managedDoltRecoverReport, error) {
	t.Helper()
	return recoverManagedDoltProcessWithOps(h.city, "127.0.0.1", unresponsiveTestPort, "root", "warning", 2*time.Second, h.ops)
}

func (h *unresponsiveRecoveryHarness) since(t *testing.T) (time.Time, bool) {
	t.Helper()
	data, err := os.ReadFile(managedDoltUnresponsiveSincePath(h.layout))
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, false
	}
	if err != nil {
		t.Fatalf("read unresponsive state: %v", err)
	}
	when, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse unresponsive state %q: %v", data, err)
	}
	return when, true
}

func (h *unresponsiveRecoveryHarness) log(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(h.layout.LogFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read dolt.log: %v", err)
	}
	return string(data)
}

// (a) A probe timeout against a live, owned, still-listening server is not
// enough to stop it: the first-unresponsive time is persisted and the recover
// reports unhealthy without touching the process.
func TestRecoverManagedDolt_DefersStopWhileLiveServerUnresponsive(t *testing.T) {
	t.Setenv("BEADS_ACTOR", "order:beads-health")
	h := newUnresponsiveRecoveryHarness(t, errUnresponsiveTestProbe, 4242)

	report, err := h.recover(t)
	if err == nil {
		t.Fatal("recover returned nil error while deferring; callers must see unhealthy-not-recovered")
	}
	if h.stopped != 0 || h.started != 0 {
		t.Fatalf("stop=%d start=%d, want the live server left alone", h.stopped, h.started)
	}
	if report.Restarted {
		t.Error("report.Restarted = true for a deferred recover")
	}
	got, ok := h.since(t)
	if !ok || !got.Equal(h.now) {
		t.Fatalf("first-unresponsive state = %v (present=%v), want %v", got, ok, h.now)
	}
	logged := h.log(t)
	for _, want := range []string{"defer", "pid=4242", "port=3306", "query probe timed out", "unresponsive_for=", "caller=order:beads-health"} {
		if !strings.Contains(logged, want) {
			t.Errorf("dolt.log missing %q; got:\n%s", want, logged)
		}
	}

	// A later recover inside the window keeps deferring and keeps the
	// original first-unresponsive time instead of restarting the clock.
	h.now = h.now.Add(60 * time.Second)
	if _, err := h.recover(t); err == nil {
		t.Fatal("second recover inside the window returned nil error")
	}
	if h.stopped != 0 {
		t.Fatalf("stop called %d times inside the window", h.stopped)
	}
	if again, _ := h.since(t); !again.Equal(got) {
		t.Fatalf("first-unresponsive time moved from %v to %v", got, again)
	}
}

// (b) A refused connection, a dead PID, or an unowned process is not the
// "alive but slow" case, so today's stop-and-start path is kept.
func TestRecoverManagedDolt_StopsWhenNotLiveListening(t *testing.T) {
	h := newUnresponsiveRecoveryHarness(t, errors.New("dial tcp 127.0.0.1:3306: connect: connection refused"), 0)

	if _, err := h.recover(t); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if h.stopped != 1 || h.started != 1 {
		t.Fatalf("stop=%d start=%d, want one stop-and-start", h.stopped, h.started)
	}
	if _, ok := h.since(t); ok {
		t.Error("unresponsive state recorded for a server that is not listening")
	}
}

// (c) Once the server has been continuously unresponsive for the window, the
// stop-and-start path runs, even when the state was written by another process.
func TestRecoverManagedDolt_StopsAfterUnresponsiveWindowElapsed(t *testing.T) {
	h := newUnresponsiveRecoveryHarness(t, errUnresponsiveTestProbe, 4242)
	first := h.now.Add(-unresponsiveTestWindow - time.Second)
	if err := os.WriteFile(managedDoltUnresponsiveSincePath(h.layout), []byte(first.Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := h.recover(t); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if h.stopped != 1 || h.started != 1 {
		t.Fatalf("stop=%d start=%d, want one stop-and-start after the window", h.stopped, h.started)
	}
}

// A window of zero restores the pre-existing behavior: stop on any probe
// failure, with no state written.
func TestRecoverManagedDolt_ZeroWindowStopsImmediately(t *testing.T) {
	h := newUnresponsiveRecoveryHarness(t, errUnresponsiveTestProbe, 4242)
	h.ops.unresponsiveRestartAfter = 0

	if _, err := h.recover(t); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if h.stopped != 1 {
		t.Fatalf("stop=%d, want 1 with the window disabled", h.stopped)
	}
	if _, ok := h.since(t); ok {
		t.Error("unresponsive state recorded with the window disabled")
	}
}

// (d) A successful probe means the server recovered by itself, so the
// continuous-unresponsive clock must start over next time.
func TestRecoverManagedDolt_SuccessfulProbeClearsUnresponsiveState(t *testing.T) {
	h := newUnresponsiveRecoveryHarness(t, nil, 4242)
	if err := os.WriteFile(managedDoltUnresponsiveSincePath(h.layout), []byte(h.now.Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := h.recover(t); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if h.stopped != 0 {
		t.Fatalf("stop=%d, want a healthy server left alone", h.stopped)
	}
	if _, ok := h.since(t); ok {
		t.Error("unresponsive state survived a successful probe")
	}
}

// The unresponsive window is a CONTINUOUS streak. A stall that cleared by
// itself (beads-health passed in between, so recover never ran) must not be
// counted into a later, unrelated stall: one failed probe hours after the
// first incident has to defer again, not stop the live server.
func TestRecoverManagedDolt_HealthyGapStartsNewUnresponsiveStreak(t *testing.T) {
	t.Setenv("BEADS_ACTOR", "order:beads-health")
	h := newUnresponsiveRecoveryHarness(t, errUnresponsiveTestProbe, 4242)
	writeHealthyProviderFakes(t, h.city)
	t.Setenv("GC_BEADS", "")

	// Incident 1: deferred, first-unresponsive time persisted.
	if _, err := h.recover(t); err == nil {
		t.Fatal("incident 1: recover returned nil error while deferring")
	}
	if _, ok := h.since(t); !ok {
		t.Fatal("incident 1: first-unresponsive time not persisted")
	}

	// The server answers again by itself: the health check passes and
	// recover is never called.
	if err := healthBeadsProvider(h.city); err != nil {
		t.Fatalf("healthBeadsProvider: %v", err)
	}

	// Incident 2, long after: a single failed probe is a new streak.
	h.now = h.now.Add(2 * time.Hour)
	if _, err := h.recover(t); err == nil {
		t.Fatalf("incident 2: recover stopped the live server on one probe error 2h after incident 1 (stop=%d start=%d)", h.stopped, h.started)
	}
	if h.stopped != 0 {
		t.Fatalf("incident 2: stop=%d, want the live server left alone", h.stopped)
	}
	if got, ok := h.since(t); !ok || !got.Equal(h.now) {
		t.Fatalf("incident 2: streak start = %v (present=%v), want a fresh %v", got, ok, h.now)
	}
}

// A recover-last stamped in the future (clock step back, bad write) must not
// suppress recovery until the clock catches up.
func TestRecentManagedDoltRecoverIgnoresFutureTimestamp(t *testing.T) {
	h := newUnresponsiveRecoveryHarness(t, nil, 0)
	if err := recordManagedDoltRecover(h.city, h.now.Add(time.Hour)); err != nil {
		t.Fatalf("recordManagedDoltRecover: %v", err)
	}
	if recentManagedDoltRecover(h.city, h.now, 30*time.Second) {
		t.Fatal("a recover-last in the future counted as a recent recover")
	}
	if err := recordManagedDoltRecover(h.city, h.now.Add(-5*time.Second)); err != nil {
		t.Fatalf("recordManagedDoltRecover: %v", err)
	}
	if !recentManagedDoltRecover(h.city, h.now, 30*time.Second) {
		t.Fatal("a recover 5s ago was not counted as recent")
	}
}

// A decision line is one line: a caller carrying a newline must not forge a
// second one.
func TestLogManagedDoltRecoverDecisionQuotesCaller(t *testing.T) {
	t.Setenv("BEADS_ACTOR", "order:evil\nforged decision=stop")
	h := newUnresponsiveRecoveryHarness(t, errUnresponsiveTestProbe, 4242)

	logManagedDoltRecoverDecision(h.layout, "defer", "unresponsive-within-window", 4242, unresponsiveTestPort, errUnresponsiveTestProbe, time.Second, h.now)
	if n := strings.Count(strings.TrimSpace(h.log(t)), "\n"); n != 0 {
		t.Fatalf("decision spans %d extra lines; got:\n%s", n, h.log(t))
	}
}

// (e) Every stop is explained in dolt.log: who asked, which process, and why.
func TestRecoverManagedDolt_StopLogsReasonToDoltLog(t *testing.T) {
	t.Setenv("BEADS_ACTOR", "order:beads-health")
	h := newUnresponsiveRecoveryHarness(t, errors.New("dial tcp 127.0.0.1:3306: connect: connection refused"), 0)

	if _, err := h.recover(t); err != nil {
		t.Fatalf("recover: %v", err)
	}
	logged := h.log(t)
	for _, want := range []string{"stop", "port=3306", "connection refused", "caller=order:beads-health"} {
		if !strings.Contains(logged, want) {
			t.Errorf("dolt.log missing %q; got:\n%s", want, logged)
		}
	}
}

// The caller falls back to the executable and parent when no order set
// BEADS_ACTOR, so an unattributed stop is still traceable.
func TestManagedDoltRecoverCallerFallsBackToProcess(t *testing.T) {
	t.Setenv("BEADS_ACTOR", "")
	got := managedDoltRecoverCaller()
	if got == "" || strings.HasPrefix(got, "order:") {
		t.Fatalf("managedDoltRecoverCaller() = %q, want an argv0/parent description", got)
	}
	if !strings.Contains(got, fmt.Sprintf("ppid=%d", os.Getppid())) {
		t.Errorf("managedDoltRecoverCaller() = %q, want it to name the parent pid", got)
	}
}
