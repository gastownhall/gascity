package cliauth

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStoreRoundTripAndDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	s := NewStore(path)

	if got, err := s.Token("https://gascity.com"); err != nil || got != "" {
		t.Fatalf("Token on empty store = %q, %v; want empty, nil", got, err)
	}
	if err := s.SetToken("https://gascity.com", "tok-1"); err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	got, err := s.Token("https://gascity.com")
	if err != nil || got != "tok-1" {
		t.Fatalf("Token = %q, %v; want tok-1, nil", got, err)
	}
	def, err := s.DefaultURL()
	if err != nil || def != "https://gascity.com" {
		t.Fatalf("DefaultURL = %q, %v; want https://gascity.com", def, err)
	}
}

func TestStoreKeepsServicesSeparateAndTracksLatestDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	s := NewStore(path)
	if err := s.SetToken("https://gascity.com", "tok-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetToken("https://gc.corp.example", "tok-b"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Token("https://gascity.com"); got != "tok-a" {
		t.Fatalf("gascity token = %q; want tok-a", got)
	}
	if got, _ := s.Token("https://gc.corp.example"); got != "tok-b" {
		t.Fatalf("corp token = %q; want tok-b", got)
	}
	// The most recent login becomes the stored default.
	if def, _ := s.DefaultURL(); def != "https://gc.corp.example" {
		t.Fatalf("DefaultURL = %q; want the most recently logged-in service", def)
	}
}

func TestStoreWritesOwnerOnlyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := NewStore(path).SetToken("https://gascity.com", "tok"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credential file perms = %o; want 600", perm)
	}
}

func TestStoreRemoveClearsEntryAndRepointsDefault(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "credentials.json"))
	if err := s.SetToken("https://a.example", "ta"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetToken("https://b.example", "tb"); err != nil { // default becomes b
		t.Fatal(err)
	}
	if err := s.Remove("https://b.example"); err != nil {
		t.Fatal(err)
	}
	if tok, _ := s.Token("https://b.example"); tok != "" {
		t.Fatalf("b still present: %q", tok)
	}
	if def, _ := s.DefaultURL(); def != "https://a.example" {
		t.Fatalf("default not repointed to the remaining service: %q", def)
	}
	if err := s.Remove("https://a.example"); err != nil {
		t.Fatal(err)
	}
	if def, _ := s.DefaultURL(); def != "" {
		t.Fatalf("default should be cleared when nothing remains: %q", def)
	}
	if svcs, _ := s.Services(); len(svcs) != 0 {
		t.Fatalf("Services should be empty: %v", svcs)
	}
}

func TestDefaultStorePathHonorsEnvOverride(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom-creds.json")
	t.Setenv(StorePathEnv, custom)
	if got := DefaultStorePath(); got != custom {
		t.Fatalf("DefaultStorePath = %q; want %q", got, custom)
	}
}
