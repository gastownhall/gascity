// Package cliauth implements the client side of the Gas City Service Protocol
// v0 (docs/reference/specs/service-protocol-v0.md): the generic hosted-service
// auth flows (browser-callback and device-code login, identity) and the local
// credential store used by `gc login` and `gc whoami`.
//
// The protocol is deliberately vendor-neutral: the client holds an opaque
// bearer token it never parses, opens URLs the server returns, and prints
// strings the server authored. https://gascity.com is only a default endpoint;
// any conforming server works against an unmodified client.
package cliauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/gchome"
)

// StorePathEnv overrides the credential file location; when unset the store
// lives under the canonical Gas City home so isolated runs and tests stay
// sandboxed.
const StorePathEnv = "GC_CREDENTIALS_PATH"

// credentialFile is the on-disk shape of the credential store, keyed by service
// base URL so multiple services coexist (the docker model: many registries,
// one login command).
type credentialFile struct {
	DefaultServiceURL string                     `json:"default_service_url,omitempty"`
	Services          map[string]credentialEntry `json:"services"`
}

type credentialEntry struct {
	Token     string `json:"token"`
	UpdatedAt string `json:"updated_at"`
}

// Store reads and writes the local credential file.
type Store struct {
	path string
}

// NewStore returns a Store backed by the file at path.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// DefaultStorePath resolves the credential file path: the StorePathEnv override
// wins, otherwise credentials.json under the Gas City home.
func DefaultStorePath() string {
	if override := strings.TrimSpace(os.Getenv(StorePathEnv)); override != "" {
		return override
	}
	return filepath.Join(gchome.Default(), "credentials.json")
}

func (s *Store) load() (credentialFile, error) {
	cf := credentialFile{Services: map[string]credentialEntry{}}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cf, nil
		}
		return cf, fmt.Errorf("reading credential store: %w", err)
	}
	if err := json.Unmarshal(data, &cf); err != nil {
		return cf, fmt.Errorf("parsing credential store: %w", err)
	}
	if cf.Services == nil {
		cf.Services = map[string]credentialEntry{}
	}
	return cf, nil
}

func (s *Store) save(cf credentialFile) error {
	if cf.Services == nil {
		cf.Services = map[string]credentialEntry{}
	}
	data, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating credential store directory: %w", err)
	}
	// A fresh 0600 temp file renamed over the target keeps the write atomic and
	// sheds any looser permissions from a pre-existing file.
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing credential store: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing credential store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing credential store: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing credential store: %w", err)
	}
	return nil
}

// Token returns the stored bearer token for baseURL, or the empty string when
// none is stored.
func (s *Store) Token(baseURL string) (string, error) {
	cf, err := s.load()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(cf.Services[baseURL].Token), nil
}

// SetToken stores token for baseURL and records baseURL as the default service.
func (s *Store) SetToken(baseURL, token string) error {
	cf, err := s.load()
	if err != nil {
		return err
	}
	if cf.Services == nil {
		cf.Services = map[string]credentialEntry{}
	}
	cf.DefaultServiceURL = baseURL
	cf.Services[baseURL] = credentialEntry{
		Token:     strings.TrimSpace(token),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	return s.save(cf)
}

// DefaultURL returns the last service URL logged into, or the empty string.
func (s *Store) DefaultURL() (string, error) {
	cf, err := s.load()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(cf.DefaultServiceURL), nil
}

// Services returns every service URL with a stored token.
func (s *Store) Services() ([]string, error) {
	cf, err := s.load()
	if err != nil {
		return nil, err
	}
	urls := make([]string, 0, len(cf.Services))
	for url := range cf.Services {
		urls = append(urls, url)
	}
	return urls, nil
}

// Remove deletes the stored token for baseURL. When baseURL was the default, the
// default is repointed to any remaining service (or cleared).
func (s *Store) Remove(baseURL string) error {
	cf, err := s.load()
	if err != nil {
		return err
	}
	delete(cf.Services, baseURL)
	if cf.DefaultServiceURL == baseURL {
		cf.DefaultServiceURL = ""
		for url := range cf.Services {
			cf.DefaultServiceURL = url
			break
		}
	}
	return s.save(cf)
}
