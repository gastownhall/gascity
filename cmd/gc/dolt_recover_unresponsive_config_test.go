package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

func TestResolveManagedDoltUnresponsiveRestartAfter(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cityToml string
		want     time.Duration
		wantErr  string
	}{
		{name: "missing city.toml selects the default", want: config.DefaultDoltUnresponsiveRestartAfter},
		{name: "absent setting selects the default", cityToml: "[workspace]\nname = \"w\"\n", want: config.DefaultDoltUnresponsiveRestartAfter},
		{name: "valid duration", cityToml: "[workspace]\nname = \"w\"\n\n[daemon]\ndolt_unresponsive_restart_after = \"45s\"\n", want: 45 * time.Second},
		{name: "zero disables the delay", cityToml: "[workspace]\nname = \"w\"\n\n[daemon]\ndolt_unresponsive_restart_after = \"0s\"\n", want: 0},
		{name: "malformed fails", cityToml: "[workspace]\nname = \"w\"\n\n[daemon]\ndolt_unresponsive_restart_after = \"soon\"\n", wantErr: "dolt_unresponsive_restart_after"},
		{name: "negative fails", cityToml: "[workspace]\nname = \"w\"\n\n[daemon]\ndolt_unresponsive_restart_after = \"-1s\"\n", wantErr: "dolt_unresponsive_restart_after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			if tc.cityToml != "" {
				if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(tc.cityToml), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := resolveManagedDoltUnresponsiveRestartAfter(city)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("resolve error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolve = %v, want %v", got, tc.want)
			}
		})
	}
}
