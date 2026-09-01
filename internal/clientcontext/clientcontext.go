// Package clientcontext is the client-side registry of named remote cities
// (the kubeconfig analog) that the gc CLI uses to operate a city over the
// HTTP+SSE control plane. It is pure storage: load, save, look up, and
// validate ~/.gc/contexts.toml. Precedence resolution (flag > env > local
// city discovery > sticky default) lives in the cmd/gc resolver, not here,
// so this package stays a path-parameterized leaf with no dependency on GC
// home resolution — callers pass an explicit path (DefaultPath lives at the
// cmd/gc layer where supervisor.DefaultHome is already imported).
package clientcontext

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/citywriteauth"
)

// validName constrains a context name and a remote city name to characters
// that are safe in a URL path segment and in the write-auth grant digest
// preimage (no control characters, no path separators). It mirrors the
// supervisor registry's validCityName shape.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Context is a single named remote city: where it is, which city it is, and
// how to authenticate to it. The two credential techniques are independent
// and both optional (Decision 0): CredentialCommand mints a transport bearer
// consumed by an edge/proxy, GrantCommand mints an X-GC-City-Write grant for
// a direct hardened self-host. A city that needs neither is reached over the
// X-GC-Request header alone.
type Context struct {
	Name                     string   `toml:"name"`
	URL                      string   `toml:"url"`
	City                     string   `toml:"city,omitempty"`
	CredentialCommand        string   `toml:"credential_command,omitempty"`
	CredentialAudience       string   `toml:"credential_audience,omitempty"`
	CredentialRequiredScopes []string `toml:"credential_required_scopes,omitempty"`
	CredentialOrg            string   `toml:"credential_org,omitempty"`
	GrantCommand             string   `toml:"grant_command,omitempty"`
	GrantAudience            string   `toml:"grant_audience,omitempty"`
	GrantCID                 string   `toml:"grant_cid,omitempty"`
	CAFile                   string   `toml:"ca_file,omitempty"`
	TLSServerName            string   `toml:"tls_server_name,omitempty"`
	InsecureSkipVerify       bool     `toml:"insecure_skip_verify,omitempty"`
	Timeout                  string   `toml:"timeout,omitempty"` // REST overall timeout; never applied to SSE streams
}

// File is the on-disk shape of ~/.gc/contexts.toml. Default names the sticky
// context used only when no local city is discoverable from cwd (Decision 4);
// it is empty unless set by `gc context use`.
type File struct {
	Default  string    `toml:"default,omitempty"`
	Contexts []Context `toml:"context"`
}

// Load reads the contexts file at path. A missing file is not an error: it
// yields an empty File, so the CLI's first run needs no bootstrap step.
func Load(path string) (*File, error) {
	var f File
	if _, err := toml.DecodeFile(path, &f); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &File{}, nil
		}
		return nil, fmt.Errorf("loading contexts %q: %w", path, err)
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("loading contexts %q: %w", path, err)
	}
	return &f, nil
}

// Save writes f to path atomically (temp file in the same directory, then
// rename) with owner-only permissions, since contexts may reference
// credential commands. The parent directory is created if absent.
func (f *File) Save(path string) error {
	if err := f.Validate(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating contexts dir %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".contexts-*.toml")
	if err != nil {
		return fmt.Errorf("creating temp contexts file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // best-effort cleanup if rename never happened
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close() //nolint:errcheck
		return fmt.Errorf("chmod temp contexts file: %w", err)
	}
	if err := toml.NewEncoder(tmp).Encode(f); err != nil {
		tmp.Close() //nolint:errcheck
		return fmt.Errorf("encoding contexts: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp contexts file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming contexts into place: %w", err)
	}
	return nil
}

// Lookup returns a pointer to the context with the given name and whether it
// was found. The pointer aliases the slice element so callers must not retain
// it across a mutation of f.Contexts.
func (f *File) Lookup(name string) (*Context, bool) {
	for i := range f.Contexts {
		if f.Contexts[i].Name == name {
			return &f.Contexts[i], true
		}
	}
	return nil, false
}

// EffectiveCity returns the remote city name for path scoping: the explicit
// City if set, otherwise the context Name.
func (c Context) EffectiveCity() string {
	if c.City != "" {
		return c.City
	}
	return c.Name
}

// Validate checks a single context: a valid name and URL are required, and a
// non-loopback URL must be https (a bearer/grant must never ride plaintext).
// A control character or path separator in the name or city is rejected
// because both flow into URL paths and the grant digest preimage.
func (c Context) Validate() error {
	if c.Name == "" {
		return errors.New("context: name is required")
	}
	if !validName.MatchString(c.Name) {
		return fmt.Errorf("context %q: name must match %s (no control characters or path separators)", c.Name, validName)
	}
	if c.URL == "" {
		return fmt.Errorf("context %q: url is required", c.Name)
	}
	if err := validateURL(c.URL); err != nil {
		return fmt.Errorf("context %q: %w", c.Name, err)
	}
	if c.City != "" && !validName.MatchString(c.City) {
		return fmt.Errorf("context %q: city %q must match %s (no control characters or path separators)", c.Name, c.City, validName)
	}
	if (c.GrantAudience != "" || c.GrantCID != "") && c.GrantCommand == "" {
		return fmt.Errorf("context %q: grant_audience and grant_cid require grant_command", c.Name)
	}
	switch {
	case c.GrantAudience == citywriteauth.AudienceCityWriteV2:
		if c.GrantCID == "" {
			return fmt.Errorf("context %q: grant_cid is required with %s", c.Name, citywriteauth.AudienceCityWriteV2)
		}
	case c.GrantAudience != "" && c.GrantAudience != citywriteauth.AudienceCityWrite:
		return fmt.Errorf("context %q: unsupported grant_audience %q", c.Name, c.GrantAudience)
	case c.GrantCID != "":
		return fmt.Errorf("context %q: grant_cid requires grant_audience %s", c.Name, citywriteauth.AudienceCityWriteV2)
	}
	providerConfigured := c.CredentialAudience != "" || len(c.CredentialRequiredScopes) > 0 || c.CredentialOrg != ""
	if providerConfigured {
		if c.CredentialCommand != "" {
			return fmt.Errorf("context %q: credential_provider fields cannot be combined with credential_command", c.Name)
		}
		if c.CredentialAudience == "" {
			return fmt.Errorf("context %q: credential_provider tuple incomplete; credential_audience is required", c.Name)
		}
		if !validCredentialValue(c.CredentialAudience) {
			return fmt.Errorf("context %q: credential_audience is invalid", c.Name)
		}
		if len(c.CredentialRequiredScopes) == 0 {
			return fmt.Errorf("context %q: credential_provider tuple incomplete; credential_required_scopes is required", c.Name)
		}
		seenScopes := make(map[string]struct{}, len(c.CredentialRequiredScopes))
		for _, scope := range c.CredentialRequiredScopes {
			if !validCredentialValue(scope) {
				return fmt.Errorf("context %q: credential_required_scopes contains an invalid scope", c.Name)
			}
			if _, duplicate := seenScopes[scope]; duplicate {
				return fmt.Errorf("context %q: credential_required_scopes contains duplicate scopes", c.Name)
			}
			seenScopes[scope] = struct{}{}
		}
		if c.CredentialOrg != "" && !validCredentialValue(c.CredentialOrg) {
			return fmt.Errorf("context %q: credential_org is invalid", c.Name)
		}
	} else if c.CredentialOrg != "" {
		return fmt.Errorf("context %q: credential_org requires credential_audience and credential_required_scopes", c.Name)
	}
	return nil
}

func validCredentialValue(value string) bool {
	if value == "" || len(value) > 512 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Validate checks every context and the cross-context invariants: names are
// unique and a non-empty Default names a defined context.
func (f *File) Validate() error {
	seen := make(map[string]bool, len(f.Contexts))
	for i := range f.Contexts {
		if err := f.Contexts[i].Validate(); err != nil {
			return err
		}
		name := f.Contexts[i].Name
		if seen[name] {
			return fmt.Errorf("duplicate context name %q", name)
		}
		seen[name] = true
	}
	if f.Default != "" && !seen[f.Default] {
		return fmt.Errorf("default context %q is not defined", f.Default)
	}
	return nil
}

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url %q: %w", raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("url %q: missing host", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return fmt.Errorf("url %q: http is only allowed for a loopback host; use https for a remote city", raw)
		}
		return nil
	default:
		return fmt.Errorf("url %q: scheme must be https (or http for loopback)", raw)
	}
}

func isLoopbackHost(host string) bool {
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
