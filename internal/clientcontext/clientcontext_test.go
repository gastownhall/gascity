package clientcontext

import (
	"os"
	"path/filepath"
	"testing"
)

func sampleFile() *File {
	return &File{
		Default: "prod",
		Contexts: []Context{
			{
				Name:         "prod",
				URL:          "https://box.internal:9443",
				City:         "example-city",
				GrantCommand: "gc-write-mint --key ~/.gc/keys/city.ed25519",
			},
			{
				Name:              "remote",
				URL:               "https://gc.example.com/city-api",
				City:              "acme",
				CredentialCommand: "token-helper --audience gc-city",
				CAFile:            "/etc/ssl/gc/remote-ca.pem",
			},
		},
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contexts.toml")
	want := sampleFile()
	if err := want.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Default != want.Default {
		t.Errorf("Default = %q, want %q", got.Default, want.Default)
	}
	if len(got.Contexts) != len(want.Contexts) {
		t.Fatalf("Contexts len = %d, want %d", len(got.Contexts), len(want.Contexts))
	}
	for i := range want.Contexts {
		if got.Contexts[i] != want.Contexts[i] {
			t.Errorf("Contexts[%d] = %+v, want %+v", i, got.Contexts[i], want.Contexts[i])
		}
	}
}

func TestLoadMissingFileReturnsEmpty(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatalf("Load of missing file should not error, got %v", err)
	}
	if got == nil {
		t.Fatal("Load returned nil *File")
	}
	if len(got.Contexts) != 0 || got.Default != "" {
		t.Errorf("expected empty File, got %+v", got)
	}
}

func TestSaveWritesOwnerOnlyPerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contexts.toml")
	if err := sampleFile().Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
}

func TestSaveIsAtomicOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contexts.toml")
	if err := sampleFile().Save(path); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	// A second save with fewer contexts must fully replace, not merge.
	smaller := &File{Contexts: []Context{{Name: "only", URL: "https://x:1"}}}
	if err := smaller.Save(path); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Contexts) != 1 || got.Contexts[0].Name != "only" || got.Default != "" {
		t.Errorf("overwrite not clean: %+v", got)
	}
}

func TestLookup(t *testing.T) {
	f := sampleFile()
	if c, ok := f.Lookup("remote"); !ok || c.URL != "https://gc.example.com/city-api" {
		t.Errorf("Lookup(remote) = %+v, %v", c, ok)
	}
	if _, ok := f.Lookup("nope"); ok {
		t.Error("Lookup(nope) returned ok=true")
	}
}

func TestEffectiveCity(t *testing.T) {
	withCity := Context{Name: "a", City: "acme"}
	if got := withCity.EffectiveCity(); got != "acme" {
		t.Errorf("EffectiveCity = %q, want acme", got)
	}
	noCity := Context{Name: "solo"}
	if got := noCity.EffectiveCity(); got != "solo" {
		t.Errorf("EffectiveCity fallback = %q, want solo (the name)", got)
	}
}

func TestContextValidate(t *testing.T) {
	tests := []struct {
		name    string
		ctx     Context
		wantErr bool
	}{
		{"ok", Context{Name: "prod", URL: "https://x:1", City: "acme"}, false},
		{"ok grant+cred coexist", Context{Name: "p", URL: "https://x:1", CredentialCommand: "a", GrantCommand: "b"}, false},
		{"empty name", Context{URL: "https://x:1"}, true},
		{"empty url", Context{Name: "p"}, true},
		{"control char in name", Context{Name: "pr\nod", URL: "https://x:1"}, true},
		{"path sep in name", Context{Name: "a/b", URL: "https://x:1"}, true},
		{"control char in city", Context{Name: "p", URL: "https://x:1", City: "ac\x00me"}, true},
		{"path sep in city", Context{Name: "p", URL: "https://x:1", City: "ac/me"}, true},
		{"non-https non-loopback", Context{Name: "p", URL: "http://box.internal:9443"}, true},
		{"http loopback allowed", Context{Name: "p", URL: "http://127.0.0.1:9443"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.ctx.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestFileValidate(t *testing.T) {
	t.Run("duplicate names", func(t *testing.T) {
		f := &File{Contexts: []Context{
			{Name: "dup", URL: "https://x:1"},
			{Name: "dup", URL: "https://y:1"},
		}}
		if err := f.Validate(); err == nil {
			t.Error("expected duplicate-name error")
		}
	})
	t.Run("default must exist", func(t *testing.T) {
		f := &File{Default: "ghost", Contexts: []Context{{Name: "real", URL: "https://x:1"}}}
		if err := f.Validate(); err == nil {
			t.Error("expected error: default names a missing context")
		}
	})
	t.Run("empty default ok", func(t *testing.T) {
		f := &File{Contexts: []Context{{Name: "real", URL: "https://x:1"}}}
		if err := f.Validate(); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}
