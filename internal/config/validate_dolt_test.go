package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestValidateDoltConfigRejectsNegativeLimits(t *testing.T) {
	tests := []struct {
		name    string
		cfg     DoltConfig
		wantErr string
	}{
		{name: "omitted", cfg: DoltConfig{}},
		{name: "legacy endpoint", cfg: DoltConfig{Host: "db.example", Port: 3306}},
		{name: "negative max connections", cfg: DoltConfig{MaxConnections: -1}, wantErr: "max_connections"},
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

func TestLoadDoltConfigRejectsRemovedMode(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"removed mode", "[workspace]\nname=\"x\"\n[dolt]\nmode=\"proxied-server\"\n", "[dolt].mode is not supported"},
		{"legacy endpoint remains valid", "[workspace]\nname=\"x\"\n[dolt]\nhost=\"db\"\nport=3306\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "city.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(fsys.OSFS{}, path)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Load() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
