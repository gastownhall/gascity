package clientcontext

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
				Name:                     "remote",
				URL:                      "https://gc.example.com/city-api",
				City:                     "acme",
				CredentialAudience:       "gascity-control",
				CredentialRequiredScopes: []string{"gascity:read", "gascity:write"},
				CredentialOrg:            "org-acme",
				CAFile:                   "/etc/ssl/gc/remote-ca.pem",
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
		if !reflect.DeepEqual(got.Contexts[i], want.Contexts[i]) {
			t.Errorf("Contexts[%d] = %+v, want %+v", i, got.Contexts[i], want.Contexts[i])
		}
	}
}

func TestProviderContextPersistsRequestTupleWithoutProviderArgv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contexts.toml")
	file := &File{Contexts: []Context{{
		Name:                     "remote",
		URL:                      "https://city.example",
		CredentialAudience:       "gascity-control",
		CredentialRequiredScopes: []string{"gascity:read", "gascity:write"},
		CredentialOrg:            "org-acme",
	}}}
	if err := file.Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{"credential_audience", "credential_required_scopes", "credential_org"} {
		if !strings.Contains(text, required) {
			t.Errorf("contexts.toml omits %s: %s", required, text)
		}
	}
	for _, forbidden := range []string{"GC_CREDENTIAL_PROVIDER", "credential_provider_argv", "gasworks credential-provider"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("contexts.toml persisted provider argv/configuration %q: %s", forbidden, text)
		}
	}
}

func TestLoadRejectsIncompleteCredentialProviderTuple(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contexts.toml")
	if err := os.WriteFile(path, []byte(`[[context]]
name = "remote"
url = "https://city.example"
credential_audience = "gascity-control"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "credential_required_scopes") {
		t.Fatalf("Load() error = %v, want incomplete provider tuple rejection", err)
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

func TestContextValidateCredentialProviderTuple(t *testing.T) {
	valid := Context{
		Name:                     "prod",
		URL:                      "https://city.example",
		CredentialAudience:       "gascity-control",
		CredentialRequiredScopes: []string{"gascity:read"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("complete credential provider tuple: %v", err)
	}

	tests := []struct {
		name string
		edit func(*Context)
		want string
	}{
		{name: "missing audience", edit: func(c *Context) { c.CredentialAudience = "" }, want: "credential_audience"},
		{name: "missing scopes", edit: func(c *Context) { c.CredentialRequiredScopes = nil }, want: "credential_required_scopes"},
		{name: "empty scope", edit: func(c *Context) { c.CredentialRequiredScopes = []string{""} }, want: "credential_required_scopes"},
		{name: "org alone is partial", edit: func(c *Context) {
			c.CredentialAudience = ""
			c.CredentialRequiredScopes = nil
			c.CredentialOrg = "org-acme"
		}, want: "credential_provider"},
		{name: "legacy command conflicts", edit: func(c *Context) { c.CredentialCommand = "legacy-helper" }, want: "credential_command"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			candidate.CredentialRequiredScopes = append([]string(nil), valid.CredentialRequiredScopes...)
			test.edit(&candidate)
			err := candidate.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want field %q", err, test.want)
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

func TestContextGrantAudienceCIDValidation(t *testing.T) {
	base := Context{Name: "prod", URL: "https://example.com", GrantCommand: "mint"}
	for _, tc := range []struct {
		name, aud, cid string
		wantErr        bool
	}{
		{"legacy default", "", "", false},
		{"legacy explicit", "gc-city-write", "", false},
		{"scoped", "gc-city-write.v2", "deployment-42", false},
		{"scoped missing cid", "gc-city-write.v2", "", true},
		{"cid with legacy", "gc-city-write", "deployment-42", true},
		{"invented v1", "gc-city-write.v1", "", true},
		{"cid without audience", "", "deployment-42", true},
		{"unknown audience", "other", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			c.GrantAudience, c.GrantCID = tc.aud, tc.cid
			if got := c.Validate(); (got != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", got, tc.wantErr)
			}
		})
	}
}
