package main

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBdCommandEnvRunnerWithManagedRetryUsesCommandLocalEnvSeam(t *testing.T) {
	originalLegacy := beadsExecCommandRunnerWithEnv
	originalCommandEnv := beadsExecCommandEnvRunnerWithEnv
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = originalLegacy
		beadsExecCommandEnvRunnerWithEnv = originalCommandEnv
	})

	legacyCalled := false
	beadsExecCommandRunnerWithEnv = func(map[string]string) beads.CommandRunner {
		legacyCalled = true
		return func(string, string, ...string) ([]byte, error) {
			return nil, fmt.Errorf("legacy runner must not receive command-local lifecycle inheritance")
		}
	}

	var gotBase, gotExplicit map[string]string
	beadsExecCommandEnvRunnerWithEnv = func(base map[string]string) beads.CommandEnvRunner {
		gotBase = cloneLifecycleTestEnv(base)
		return func(_ string, _ string, explicit map[string]string, _ ...string) ([]byte, error) {
			gotExplicit = cloneLifecycleTestEnv(explicit)
			return []byte(`[]`), nil
		}
	}

	scope := t.TempDir()
	runner := bdCommandEnvRunnerWithManagedRetryErr(scope, func(string) (map[string]string, error) {
		return map[string]string{"BEADS_DIR": scope + "/.beads"}, nil
	})
	wantExplicit := map[string]string{
		"GC_LIFECYCLE_MUTATION_SCOPE": scope,
		"GC_LIFECYCLE_MUTATION_TOKEN": "owner-token",
	}
	if _, err := runner(scope, "bd", wantExplicit, "close", "bd-42"); err != nil {
		t.Fatalf("command-env managed runner: %v", err)
	}
	if legacyCalled {
		t.Fatal("command-local mutation routed through the legacy runner")
	}
	if gotBase["BEADS_DIR"] != scope+"/.beads" {
		t.Fatalf("base environment = %#v, want scope BEADS_DIR", gotBase)
	}
	if !reflect.DeepEqual(gotExplicit, wantExplicit) {
		t.Fatalf("explicit command environment = %#v, want %#v", gotExplicit, wantExplicit)
	}
}

func TestBdManagedRetryDoesNotReplayRevisionFencedWrites(t *testing.T) {
	tests := []struct {
		name         string
		commandLocal bool
	}{
		{name: "legacy runner"},
		{name: "command-local runner", commandLocal: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GC_BEADS", "bd")
			originalLegacy := beadsExecCommandRunnerWithEnv
			originalCommandEnv := beadsExecCommandEnvRunnerWithEnv
			t.Cleanup(func() {
				beadsExecCommandRunnerWithEnv = originalLegacy
				beadsExecCommandEnvRunnerWithEnv = originalCommandEnv
			})

			wantErr := errors.New("broken pipe after committed revision-fenced write")
			attempts := 0
			beadsExecCommandRunnerWithEnv = func(map[string]string) beads.CommandRunner {
				return func(string, string, ...string) ([]byte, error) {
					if tt.commandLocal {
						t.Fatal("command-local write routed through legacy runner")
					}
					attempts++
					return []byte("first-attempt acknowledgement lost"), wantErr
				}
			}
			beadsExecCommandEnvRunnerWithEnv = func(map[string]string) beads.CommandEnvRunner {
				return func(_ string, _ string, _ map[string]string, _ ...string) ([]byte, error) {
					if !tt.commandLocal {
						t.Fatal("legacy write routed through command-local runner")
					}
					attempts++
					return []byte("first-attempt acknowledgement lost"), wantErr
				}
			}

			scope := t.TempDir()
			envCalls := 0
			envFn := func(string) (map[string]string, error) {
				envCalls++
				return map[string]string{"GC_DOLT_PORT": "3307"}, nil
			}
			var (
				out []byte
				err error
			)
			if tt.commandLocal {
				runner := bdCommandEnvRunnerWithManagedRetryErr(scope, envFn)
				out, err = runner(scope, "bd", map[string]string{
					"GC_LIFECYCLE_MUTATION_SCOPE": scope,
					"GC_LIFECYCLE_MUTATION_TOKEN": "owner-token",
				}, "update", "--json", "bd-42", "--set-metadata", "phase=closed", "--if-revision", "7")
			} else {
				runner := bdCommandRunnerWithManagedRetryErr(scope, envFn)
				out, err = runner(scope, "bd", "close", "--json", "bd-42", "--if-revision", "7")
			}

			if !errors.Is(err, wantErr) {
				t.Fatalf("runner error = %v, want original ambiguous error %v", err, wantErr)
			}
			if got := string(out); got != "first-attempt acknowledgement lost" {
				t.Fatalf("runner output = %q, want first-attempt output", got)
			}
			if attempts != 1 {
				t.Fatalf("revision-fenced attempts = %d, want exactly 1", attempts)
			}
			if envCalls != 1 {
				t.Fatalf("managed environment loads = %d, want exactly 1", envCalls)
			}
		})
	}
}

func cloneLifecycleTestEnv(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
