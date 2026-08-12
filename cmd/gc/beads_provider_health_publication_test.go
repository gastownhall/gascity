package main

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func TestReconcileHealthyManagedRuntimePublication(t *testing.T) {
	ownershipErr := errors.New("ownership unavailable")
	publishErr := errors.New("publication unavailable")
	waitErr := errors.New("store unavailable")

	tests := []struct {
		name          string
		currentPort   string
		owned         bool
		ownershipErr  error
		publishErr    error
		waitErr       error
		waitForScopes bool
		wantCalls     []string
		wantErr       error
		wantErrText   string
	}{
		{
			name:          "already published",
			currentPort:   "3307",
			owned:         true,
			waitForScopes: true,
			wantCalls:     []string{"current-port"},
		},
		{
			name:          "ownership error",
			ownershipErr:  ownershipErr,
			waitForScopes: true,
			wantCalls:     []string{"current-port", "lifecycle-owned"},
			wantErr:       ownershipErr,
			wantErrText:   "determine managed dolt ownership: ownership unavailable",
		},
		{
			name:          "unowned",
			waitForScopes: true,
			wantCalls:     []string{"current-port", "lifecycle-owned"},
		},
		{
			name:          "publication error",
			owned:         true,
			publishErr:    publishErr,
			waitForScopes: true,
			wantCalls:     []string{"current-port", "lifecycle-owned", "publish-if-owned"},
			wantErr:       publishErr,
			wantErrText:   "healthy but failed to publish managed dolt runtime state: publication unavailable",
		},
		{
			name:      "publishes without waiting",
			owned:     true,
			wantCalls: []string{"current-port", "lifecycle-owned", "publish-if-owned"},
		},
		{
			name:          "publishes and waits",
			owned:         true,
			waitForScopes: true,
			wantCalls:     []string{"current-port", "lifecycle-owned", "publish-if-owned", "wait-scopes-ready"},
		},
		{
			name:          "readiness error",
			owned:         true,
			waitErr:       waitErr,
			waitForScopes: true,
			wantCalls:     []string{"current-port", "lifecycle-owned", "publish-if-owned", "wait-scopes-ready"},
			wantErr:       waitErr,
			wantErrText:   "healthy but store not ready after publishing managed dolt runtime state: store unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const cityPath = "/city"
			var calls []string
			record := func(call, gotCityPath string) {
				t.Helper()
				if gotCityPath != cityPath {
					t.Fatalf("%s cityPath = %q, want %q", call, gotCityPath, cityPath)
				}
				calls = append(calls, call)
			}
			deps := healthyManagedRuntimePublicationDeps{
				currentPort: func(gotCityPath string) string {
					record("current-port", gotCityPath)
					return tt.currentPort
				},
				lifecycleOwned: func(gotCityPath string) (bool, error) {
					record("lifecycle-owned", gotCityPath)
					return tt.owned, tt.ownershipErr
				},
				publishIfOwned: func(gotCityPath string) error {
					record("publish-if-owned", gotCityPath)
					return tt.publishErr
				},
				waitScopesReady: func(gotCityPath string, timeout time.Duration) error {
					record("wait-scopes-ready", gotCityPath)
					if timeout != 10*time.Second {
						t.Errorf("waitScopesReady timeout = %v, want 10s", timeout)
					}
					return tt.waitErr
				},
			}

			err := reconcileHealthyManagedRuntimePublication(cityPath, tt.waitForScopes, deps)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("reconcileHealthyManagedRuntimePublication() error = %v", err)
				}
			} else {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("reconcileHealthyManagedRuntimePublication() error = %v, want errors.Is(_, %v)", err, tt.wantErr)
				}
				if err.Error() != tt.wantErrText {
					t.Errorf("reconcileHealthyManagedRuntimePublication() error = %q, want %q", err, tt.wantErrText)
				}
			}
			if !slices.Equal(calls, tt.wantCalls) {
				t.Errorf("dependency calls = %v, want %v", calls, tt.wantCalls)
			}
		})
	}
}
