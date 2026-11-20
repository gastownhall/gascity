package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestNativeInfrastructurePendingResumeRefusesForcedFallback(t *testing.T) {
	for _, value := range []string{"1", "true"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("GC_BEADS_FORCE_FALLBACK", "")
			city, originalConfig, plan := nativeInfrastructureFixture(t)
			originalWitness, err := os.ReadFile(initInfrastructurePath(city))
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("GC_BEADS_FORCE_FALLBACK", value)

			var effects []string
			ops := nativeInfrastructureFakeOps(t, plan)
			ops.Run = func(context.Context, string, []string, ...string) error {
				effects = append(effects, "provider command")
				return errors.New("unexpected provider command")
			}
			ops.EnsureIdentity = func(context.Context, string, initInfrastructurePlan) error {
				effects = append(effects, "native identity")
				return errors.New("unexpected native identity write")
			}
			ops.Verify = func(context.Context, string, initInfrastructurePlan) error {
				effects = append(effects, "native readiness")
				return errors.New("unexpected native readiness probe")
			}
			ops.Commit = func(string, []byte, os.FileMode) error {
				effects = append(effects, "config commit")
				return errors.New("unexpected config commit")
			}

			err = resumeInitInfrastructureWithOps(context.Background(), city, ops)
			if err == nil || !strings.Contains(err.Error(), "GC_BEADS_FORCE_FALLBACK") {
				t.Errorf("resume error = %v, want actionable force-fallback refusal", err)
			}
			if len(effects) != 0 {
				t.Errorf("force-fallback reached initialization effects: %q", effects)
			}
			currentConfig, err := os.ReadFile(filepath.Join(city, "city.toml"))
			if err != nil || !bytes.Equal(currentConfig, originalConfig) {
				t.Fatalf("force-fallback changed original config: %q; %v", currentConfig, err)
			}
			currentWitness, err := os.ReadFile(initInfrastructurePath(city))
			if err != nil || !bytes.Equal(currentWitness, originalWitness) {
				t.Fatalf("force-fallback changed pending witness: %q; %v", currentWitness, err)
			}
			for _, path := range []string{
				filepath.Join(city, ".gc", "storage", "infra"),
				providerScopeOwnershipPath(city),
			} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("force-fallback created initialization ownership at %s: %v", path, err)
				}
			}
		})
	}
}

func TestNativeInfrastructureForcedFallbackDoesNotRecordFreshIntent(t *testing.T) {
	for _, value := range []string{"1", "true"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("GC_BEADS_FORCE_FALLBACK", "")
			city, originalConfig, _ := nativeInfrastructureFixture(t)
			// Reuse the valid generated default pack and city scaffold without
			// retaining the fixture's earlier initialization intent.
			if err := os.Remove(initInfrastructurePath(city)); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GC_BEADS_FORCE_FALLBACK", value)

			if err := recordFreshInitInfrastructure(fsys.OSFS{}, city, true); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(initInfrastructurePath(city)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("force-fallback recorded a fresh native intent: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(city, "city.toml"))
			if err != nil || !bytes.Equal(got, originalConfig) {
				t.Fatalf("force-fallback changed original config: %q; %v", got, err)
			}
		})
	}
}
