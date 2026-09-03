package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestValidateDoltConfigMode(t *testing.T) {
	tests := []struct {
		name    string
		cfg     DoltConfig
		wantErr string
	}{
		{name: "omitted defaults direct", cfg: DoltConfig{}},
		{name: "server", cfg: DoltConfig{Mode: "server"}},
		{name: "proxied server", cfg: DoltConfig{Mode: "proxied-server"}},
		{name: "unknown mode", cfg: DoltConfig{Mode: "proxy"}, wantErr: "mode must be"},
		{name: "proxy with host", cfg: DoltConfig{Mode: "proxied-server", Host: "db.example"}, wantErr: "cannot be combined"},
		{name: "proxy with port", cfg: DoltConfig{Mode: "proxied-server", Port: 3306}, wantErr: "cannot be combined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &City{Dolt: tt.cfg}
			err := ValidateDoltConfig(cfg, "city.toml")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateDoltConfig() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateDoltConfig() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadDoltConfigModeValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"unknown", "[workspace]\nname=\"x\"\n[dolt]\nmode=\"bogus\"\n", "mode must be"},
		{"conflict", "[workspace]\nname=\"x\"\n[dolt]\nmode=\"proxied-server\"\nhost=\"db\"\n", "cannot be combined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "city.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(fsys.OSFS{}, path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
