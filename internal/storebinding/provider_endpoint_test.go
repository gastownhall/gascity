package storebinding

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// remoteSpec is a workspace binding whose backend is remote, with the endpoint
// fields overridden per case.
func remoteSpec(url, auth string) BindingSpec {
	return BindingSpec{
		Name:      BindingName("remote"),
		Provider:  ProviderID("beads-workspace"),
		ConfigRef: ConfigRef("shared"),
		URL:       url,
		Auth:      auth,
	}
}

// TestBindingSpecAcceptsRemoteEndpointAndCredentialReference proves the plan
// envelope carries the endpoint pair through validation unchanged. The fields
// are inert here — no provider in this build reads them — but a plan that
// dropped or refused them would silently serve a binding from the wrong place.
func TestBindingSpecAcceptsRemoteEndpointAndCredentialReference(t *testing.T) {
	for _, tc := range []struct{ url, auth string }{
		{"", ""},
		{"https://beads.example", ""},
		{"https://beads.example/workspaces", AuthCredentialProvider},
		{"http://127.0.0.1:9000/beads", "env:BEADS_TOKEN"},
	} {
		t.Run(tc.url+"|"+tc.auth, func(t *testing.T) {
			if err := remoteSpec(tc.url, tc.auth).Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

// TestBindingSpecRejectsUnsafeRemoteEndpoint enumerates the endpoint shapes a
// specification must never reach a provider with. Each one is a way a
// credential or an unintended destination rides in on a field that reads like
// a location.
func TestBindingSpecRejectsUnsafeRemoteEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    BindingSpec
		wantErr string
	}{
		{"scheme", remoteSpec("ftp://beads.example", ""), "url scheme must be http or https"},
		{"no host", remoteSpec("https:///workspaces", ""), "url has no host"},
		{"userinfo", remoteSpec("https://token@beads.example", ""), "url must not embed credentials"},
		{"query", remoteSpec("https://beads.example?token=abc", ""), "url must not carry a query"},
		{"fragment", remoteSpec("https://beads.example#frag", ""), "url must not carry a fragment"},
		{"auth without url", remoteSpec("", AuthCredentialProvider), "auth requires url"},
		// Built from the constant, not from a second spelling of it: a literal
		// here would keep passing after someone renamed the token, which is
		// precisely the one-spelling invariant the constant exists to hold.
		{"auth form", remoteSpec("https://beads.example", "bearer"), `auth must be "` + AuthCredentialProvider + `"`},
		{"auth env name", remoteSpec("https://beads.example", "env:1BAD"), "must be followed by an environment variable name"},
		{"auth material", remoteSpec("https://beads.example", "https://beads.example/t"), "credential reference, not credential material"},
		{"auth whitespace", remoteSpec("https://beads.example", "env: TOKEN"), "credential reference, not credential material"},
		{"auth length", remoteSpec("https://beads.example", "env:"+strings.Repeat("A", 64)), "auth is longer than 64 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if !errors.Is(err, ErrInvalidBindingSpec) {
				t.Fatalf("Validate() = %v, want %v", err, ErrInvalidBindingSpec)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestBindingSpecRejectsSecretBearingURL is the belt to the shape rule's
// braces: a URL that survives the shape check still goes through the shared
// secret-material scan every other spec field goes through.
func TestBindingSpecRejectsSecretBearingURL(t *testing.T) {
	spec := remoteSpec("https://beads.example/token=abcdef0123456789", "")
	err := spec.Validate()
	if !errors.Is(err, ErrInvalidBindingSpec) {
		t.Fatalf("Validate() = %v, want %v", err, ErrInvalidBindingSpec)
	}
}

// TestResolveStoragePlanCarriesRemoteEndpointIntoTheSpec proves the two config
// fields reach the provider that is constructed from them. A plan that read
// them and dropped them would leave the provider resolving the binding
// locally with no signal that the author asked for anything else.
func TestResolveStoragePlanCarriesRemoteEndpointIntoTheSpec(t *testing.T) {
	remote := &planCountingFactory{id: "remote-provider"}
	_, lookup := planRegistry(t, remote)

	storage := planStorageConfig(map[coordclass.Class]string{
		coordclass.ClassWork:      "remote",
		coordclass.ClassGraph:     "remote",
		coordclass.ClassSessions:  "remote",
		coordclass.ClassMessaging: "remote",
		coordclass.ClassOrders:    "remote",
		coordclass.ClassNudges:    "remote",
	}, map[string]config.StorageBindingConfig{
		"remote": {
			Provider:  "remote-provider",
			ConfigRef: "shared",
			URL:       "https://beads.example/workspaces",
			Auth:      AuthCredentialProvider,
		},
	})

	if _, err := resolveStoragePlan(lookup, storage, planWorkPins(), ""); err != nil {
		t.Fatalf("resolveStoragePlan: %v", err)
	}
	if len(remote.specs) != 1 {
		t.Fatalf("factory constructions = %#v, want exactly one", remote.specs)
	}
	spec := remote.specs[0]
	if spec.URL != "https://beads.example/workspaces" {
		t.Errorf("spec URL = %q, want the authored endpoint", spec.URL)
	}
	if spec.Auth != AuthCredentialProvider {
		t.Errorf("spec Auth = %q, want %q", spec.Auth, AuthCredentialProvider)
	}
}
