package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const completeBoundStorageMetadata = `{
  "backend": "postgres",
  "storage_endpoint": "opaque-remote",
  "storage_database": "work",
  "dolt_mode": "server",
  "dolt_database": "legacy",
  "unknown": {"preserve": true}
}
`

func writeBoundStorageLifecycleFixture(t *testing.T, metadata string) (string, string) {
	t.Helper()
	cityPath := t.TempDir()
	metadataPath := scopeMetadataJSONPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	script := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	return cityPath, metadataPath
}

func TestManagedDoltLifecycleOwnedSkipsCompleteStorageBinding(t *testing.T) {
	cityPath, metadataPath := writeBoundStorageLifecycleFixture(t, completeBoundStorageMetadata)
	wantMetadata, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}

	owned, err := managedDoltLifecycleOwned(cityPath)
	if err != nil {
		t.Fatalf("managedDoltLifecycleOwned: %v", err)
	}
	if owned {
		t.Fatal("managedDoltLifecycleOwned = true, want false for complete storage binding")
	}
	gotMetadata, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotMetadata, wantMetadata) {
		t.Fatal("managed Dolt ownership probe changed storage metadata")
	}
}

func TestManagedDoltRuntimePreflightSkipsCompleteStorageBinding(t *testing.T) {
	cityPath, _ := writeBoundStorageLifecycleFixture(t, completeBoundStorageMetadata)
	var stderr bytes.Buffer
	healthCalls := 0
	portCalls := 0

	ensureManagedDoltPublishedForRuntime(
		cityPath,
		&stderr,
		"gc test",
		func(string) error { healthCalls++; return nil },
		managedDoltLifecycleOwned,
		func(string) string { portCalls++; return "" },
	)

	if stderr.Len() != 0 {
		t.Fatalf("runtime preflight stderr = %q, want empty", stderr.String())
	}
	if healthCalls != 0 || portCalls != 0 {
		t.Fatalf("runtime preflight calls: health=%d port=%d, want neither", healthCalls, portCalls)
	}
}

func TestShutdownBeadsProviderSkipsCompleteStorageBinding(t *testing.T) {
	cityPath, metadataPath := writeBoundStorageLifecycleFixture(t, completeBoundStorageMetadata)
	runtimePath := managedDoltStatePath(cityPath)
	if err := os.MkdirAll(filepath.Dir(runtimePath), 0o755); err != nil {
		t.Fatal(err)
	}
	const runtimeState = "stale managed runtime state\n"
	if err := os.WriteFile(runtimePath, []byte(runtimeState), 0o600); err != nil {
		t.Fatal(err)
	}
	wantMetadata, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := shutdownBeadsProvider(cityPath); err != nil {
		t.Fatalf("shutdownBeadsProvider: %v", err)
	}
	gotRuntime, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatalf("read preserved managed runtime state: %v", err)
	}
	if string(gotRuntime) != runtimeState {
		t.Fatalf("managed runtime state = %q, want preserved %q", gotRuntime, runtimeState)
	}
	gotMetadata, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotMetadata, wantMetadata) {
		t.Fatal("shutdown changed storage metadata")
	}
}

func TestManagedDoltLifecycleOwnedStillRejectsIncompleteStorageMetadata(t *testing.T) {
	tests := []struct {
		name     string
		metadata string
		want     string
	}{
		{
			name:     "partial storage binding",
			metadata: `{"backend":"postgres","storage_endpoint":"opaque-remote","dolt_mode":"server"}`,
			want:     "partial beads storage binding",
		},
		{
			name:     "a backend this build does not register",
			metadata: `{"backend":"postgres","dolt_mode":"server","dolt_database":"legacy"}`,
			want:     `unsupported backend "postgres"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath, _ := writeBoundStorageLifecycleFixture(t, tt.metadata)
			owned, err := managedDoltLifecycleOwned(cityPath)
			if err == nil {
				t.Fatalf("managedDoltLifecycleOwned error = nil, owned=%v", owned)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("managedDoltLifecycleOwned error = %q, want %q", err, tt.want)
			}
		})
	}
}
