package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/gastownhall/gascity/internal/storehealth"
)

func TestMaintenanceStoreSizeFunc(t *testing.T) {
	city := t.TempDir()

	// A city with no Dolt store yet reads as 0 bytes (fresh install).
	if got := maintenanceStoreSizeFunc(city)(); got != 0 {
		t.Fatalf("store size with no store dir = %d, want 0", got)
	}

	storeDir := storehealth.StorePath(city)
	if err := os.MkdirAll(filepath.Join(storeDir, "noms"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("0123456789") // 10 bytes
	if err := os.WriteFile(filepath.Join(storeDir, "noms", "chunk.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := maintenanceStoreSizeFunc(city)(); got != int64(len(payload)) {
		t.Fatalf("store size = %d, want %d", got, len(payload))
	}
}

func TestBuildMaintenanceDoltDSN(t *testing.T) {
	cases := []struct {
		name     string
		user     string
		password string
		wantUser string
	}{
		{name: "explicit user", user: "agent", wantUser: "agent"},
		{name: "defaults to root", user: "", wantUser: "root"},
		{name: "escapes password", user: "agent", password: "p@ss:word", wantUser: "agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := buildMaintenanceDoltDSN(tc.user, tc.password, "db.example.com", 3307, "hq")
			cfg, err := mysql.ParseDSN(dsn)
			if err != nil {
				t.Fatalf("ParseDSN(%q) error = %v", dsn, err)
			}
			if cfg.User != tc.wantUser {
				t.Errorf("User = %q, want %q", cfg.User, tc.wantUser)
			}
			if cfg.Passwd != tc.password {
				t.Errorf("Passwd = %q, want %q", cfg.Passwd, tc.password)
			}
			if cfg.Net != "tcp" {
				t.Errorf("Net = %q, want tcp", cfg.Net)
			}
			if cfg.Addr != "db.example.com:3307" {
				t.Errorf("Addr = %q, want db.example.com:3307", cfg.Addr)
			}
			if cfg.DBName != "hq" {
				t.Errorf("DBName = %q, want hq", cfg.DBName)
			}
			if !cfg.AllowNativePasswords {
				t.Error("AllowNativePasswords disabled; managed Dolt auth needs it enabled")
			}
		})
	}
}

func TestMaintenanceDoltOpsFactoryNonPostgresReturnsFactory(t *testing.T) {
	// A plain city directory (no Postgres backend metadata) must yield a
	// non-nil factory so the loop runs in active GC mode. The Postgres-nil
	// branch is a defensive guard for non-Dolt backends and is exercised by
	// the backend-resolution tests rather than reconstructed here.
	city := t.TempDir()
	if maintenanceDoltOpsFactory(city) == nil {
		t.Fatal("maintenanceDoltOpsFactory(non-postgres city) = nil; want non-nil DoltOpsFactory")
	}
}
