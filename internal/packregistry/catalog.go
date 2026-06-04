// Package packregistry manages configured pack registries and cached catalogs.
package packregistry

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/gchome"
)

const (
	// CatalogSchema is the supported registry catalog schema version.
	CatalogSchema = 1
	// DefaultMaxBytes is the default maximum registry catalog size.
	DefaultMaxBytes = 16 << 20
	// DefaultFetchTimeout is the default timeout for remote registry fetches.
	DefaultFetchTimeout = 15 * time.Second
)

var (
	packNameRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*(/[a-z0-9][a-z0-9-]*)?$`)
	releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)
	commitRE       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hashRE         = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	windowsPathRE  = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	windowsUNCPath = regexp.MustCompile(`^\\\\[^\\]+\\[^\\]+`)
)

// Catalog is the parsed contents of a registry catalog.
type Catalog struct {
	Schema int           `toml:"schema"`
	Packs  []CatalogPack `toml:"pack,omitempty"`
}

// CatalogPack describes one pack entry in a registry catalog.
type CatalogPack struct {
	Name        string           `toml:"name"`
	Description string           `toml:"description"`
	Source      string           `toml:"source"`
	SourceKind  string           `toml:"source_kind"`
	Releases    []CatalogRelease `toml:"release,omitempty"`
}

// CatalogRelease describes an immutable published version of a catalog pack.
type CatalogRelease struct {
	Version         string `toml:"version"`
	Ref             string `toml:"ref"`
	Commit          string `toml:"commit"`
	Hash            string `toml:"hash"`
	Description     string `toml:"description"`
	Withdrawn       bool   `toml:"withdrawn,omitempty"`
	WithdrawnReason string `toml:"withdrawn_reason,omitempty"`
}

// Source is a normalized registry catalog source.
type Source struct {
	Raw    string
	Remote bool
	URL    *url.URL
	Path   string
}

// FetchOptions controls registry catalog fetches.
type FetchOptions struct {
	Client   *http.Client
	Timeout  time.Duration
	MaxBytes int64
}

// NormalizeSource parses a user-provided registry source into a concrete catalog location.
func NormalizeSource(raw string) (Source, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Source{}, errors.New("registry source is required")
	}
	if windowsPathRE.MatchString(raw) {
		return Source{}, fmt.Errorf("registry source uses a Windows drive-letter path, which is not supported portably: %q; use an HTTPS URL or a file:// URL with a POSIX-style local path", raw)
	}
	if windowsUNCPath.MatchString(raw) {
		return Source{}, fmt.Errorf("registry source uses a UNC path, which is not supported portably: %q; use an HTTPS URL or a file:// URL with a POSIX-style local path", raw)
	}
	u, err := url.Parse(raw)
	if err == nil && u.Scheme != "" {
		switch u.Scheme {
		case "https":
			if u.Host == "" {
				return Source{}, fmt.Errorf("invalid HTTPS registry source %q", raw)
			}
			switch {
			case u.Path == "" || strings.HasSuffix(u.Path, "/"):
				u.Path = strings.TrimRight(u.Path, "/") + "/registry.toml"
			case path.Base(u.Path) == "registry.toml":
			case !strings.Contains(path.Base(u.Path), "."):
				u.Path = strings.TrimRight(u.Path, "/") + "/registry.toml"
			default:
				return Source{}, fmt.Errorf("HTTPS registry source path must end with registry.toml: %q", raw)
			}
			return Source{Raw: raw, Remote: true, URL: u}, nil
		case "http":
			return Source{}, fmt.Errorf("remote registry source must use HTTPS: %q", raw)
		case "file":
			path, err := fileURLPath(u)
			if err != nil {
				return Source{}, err
			}
			return Source{Raw: raw, Path: path}, nil
		default:
			return Source{}, fmt.Errorf("unsupported registry source scheme %q", u.Scheme)
		}
	}
	return Source{Raw: raw, Path: raw}, nil
}

// FetchCatalog reads a registry catalog from a normalized source.
func FetchCatalog(ctx context.Context, source Source, opts FetchOptions) ([]byte, error) {
	if source.Remote {
		return fetchRemoteCatalog(ctx, source.URL, opts)
	}
	return readLocalCatalog(source.Path, maxBytes(opts))
}

// ParseCatalog decodes and version-checks registry catalog data.
func ParseCatalog(data []byte) (Catalog, error) {
	var catalog Catalog
	if _, err := toml.Decode(string(data), &catalog); err != nil {
		return catalog, fmt.Errorf("parsing registry catalog: %w", err)
	}
	if catalog.Schema == 0 {
		catalog.Schema = CatalogSchema
	}
	if catalog.Schema != CatalogSchema {
		return catalog, fmt.Errorf("unsupported registry catalog schema %d", catalog.Schema)
	}
	return catalog, nil
}

// ValidateCatalog validates registry catalog structure and source policy.
func ValidateCatalog(catalog Catalog, remote bool) error {
	seenPacks := map[string]bool{}
	for _, pack := range catalog.Packs {
		if err := ValidatePackName(pack.Name); err != nil {
			return err
		}
		if seenPacks[pack.Name] {
			return fmt.Errorf("duplicate pack %q", pack.Name)
		}
		seenPacks[pack.Name] = true
		if pack.Description == "" {
			return fmt.Errorf("pack %q description is required", pack.Name)
		}
		if pack.Source == "" {
			return fmt.Errorf("pack %q source is required", pack.Name)
		}
		if pack.SourceKind != "git" {
			return fmt.Errorf("pack %q has unsupported source_kind %q", pack.Name, pack.SourceKind)
		}
		if err := validatePackSource(pack.Source, remote); err != nil {
			return fmt.Errorf("pack %q source: %w", pack.Name, err)
		}
		seenReleases := map[string]bool{}
		for _, release := range pack.Releases {
			if release.Version == "" {
				return fmt.Errorf("pack %q release version is required", pack.Name)
			}
			if !releaseVersion.MatchString(release.Version) {
				return fmt.Errorf("pack %q release version %q must be semver major.minor[.patch]", pack.Name, release.Version)
			}
			if seenReleases[release.Version] {
				return fmt.Errorf("duplicate release %q for pack %q", release.Version, pack.Name)
			}
			seenReleases[release.Version] = true
			if release.Ref == "" {
				return fmt.Errorf("pack %q release %q ref is required", pack.Name, release.Version)
			}
			if !commitRE.MatchString(release.Commit) {
				return fmt.Errorf("pack %q release %q commit must be a full lowercase SHA", pack.Name, release.Version)
			}
			if !hashRE.MatchString(release.Hash) {
				return fmt.Errorf("pack %q release %q hash must be sha256:<64 lowercase hex>", pack.Name, release.Version)
			}
			if release.Description == "" {
				return fmt.Errorf("pack %q release %q description is required", pack.Name, release.Version)
			}
		}
	}
	return nil
}

// ValidatePackName validates a canonical registry pack name.
func ValidatePackName(name string) error {
	if !packNameRE.MatchString(name) {
		return fmt.Errorf("invalid pack name %q", name)
	}
	for _, segment := range strings.Split(name, "/") {
		if len(segment) > 64 {
			return fmt.Errorf("invalid pack name %q: segment %q exceeds 64 characters", name, segment)
		}
	}
	return nil
}

// ReadCatalog fetches, parses, and validates a registry catalog.
func ReadCatalog(ctx context.Context, raw string, opts FetchOptions) (Catalog, []byte, Source, error) {
	source, err := NormalizeSource(raw)
	if err != nil {
		return Catalog{}, nil, Source{}, err
	}
	data, err := FetchCatalog(ctx, source, opts)
	if err != nil {
		return Catalog{}, nil, source, err
	}
	catalog, err := ParseCatalog(data)
	if err != nil {
		return Catalog{}, nil, source, err
	}
	if err := ValidateCatalog(catalog, source.Remote); err != nil {
		return Catalog{}, nil, source, err
	}
	return catalog, data, source, nil
}

// CachePath returns the cached catalog path for a registry.
func CachePath(home, registryName string) string {
	return filepath.Join(gchome.RegistryCacheRoot(home), registryName, "registry.toml")
}

// WriteCatalogCache writes catalog data to the registry cache.
func WriteCatalogCache(home, registryName string, data []byte) error {
	path := CachePath(home, registryName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating registry cache: %w", err)
	}
	return fsys.WriteFileAtomic(fsys.OSFS{}, path, data, 0o644)
}

// RemoveCatalogCache removes the cached catalog for a registry if one exists.
func RemoveCatalogCache(home, registryName string) error {
	if err := os.Remove(CachePath(home, registryName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing registry cache: %w", err)
	}
	return nil
}

// WithCatalogCacheLock serializes access to one registry cache.
func WithCatalogCacheLock(home, registryName string, fn func() error) error {
	lockPath := CachePath(home, registryName) + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("creating registry cache lock directory: %w", err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("opening registry cache lock: %w", err)
	}
	defer lockFile.Close() //nolint:errcheck
	return withExclusiveFileLock(lockFile, "registry cache lock", fn)
}

// ReadCachedCatalog reads and validates a cached catalog by registry name.
func ReadCachedCatalog(home, registryName string) (Catalog, []byte, error) {
	path := CachePath(home, registryName)
	data, err := os.ReadFile(path)
	if err != nil {
		return Catalog{}, nil, err
	}
	catalog, err := ParseCatalog(data)
	if err != nil {
		return Catalog{}, data, err
	}
	return catalog, data, ValidateCatalog(catalog, false)
}

// ReadCachedRegistryCatalog reads and validates a cached catalog using registry metadata.
func ReadCachedRegistryCatalog(home string, reg Registry) (Catalog, []byte, error) {
	source, err := NormalizeSource(reg.Source)
	if err != nil {
		return Catalog{}, nil, err
	}
	path := CachePath(home, reg.Name)
	data, err := os.ReadFile(path)
	if err != nil {
		return Catalog{}, nil, err
	}
	catalog, err := ParseCatalog(data)
	if err != nil {
		return Catalog{}, data, err
	}
	return catalog, data, ValidateCatalog(catalog, source.Remote)
}

// RefreshRegistry fetches a registry catalog and updates the local cache.
func RefreshRegistry(ctx context.Context, home string, reg Registry, opts FetchOptions) (Catalog, error) {
	next, data, _, err := ReadCatalog(ctx, reg.Source, opts)
	if err != nil {
		return Catalog{}, err
	}
	if err := WithCatalogCacheLock(home, reg.Name, func() error {
		prev, _, err := ReadCachedRegistryCatalog(home, reg)
		switch {
		case err == nil:
			if err := CheckImmutable(prev, next); err != nil {
				return err
			}
		case os.IsNotExist(err):
		default:
			return fmt.Errorf("reading previous registry cache: %w", err)
		}
		return WriteCatalogCache(home, reg.Name, data)
	}); err != nil {
		return Catalog{}, err
	}
	return next, nil
}

// CheckImmutable rejects changes to previously cached immutable release metadata.
func CheckImmutable(prev, next Catalog) error {
	prevReleases := map[string]CatalogPack{}
	for _, pack := range prev.Packs {
		for _, release := range pack.Releases {
			prevReleases[pack.Name+"\x00"+release.Version] = CatalogPack{
				Name:       pack.Name,
				Source:     pack.Source,
				SourceKind: pack.SourceKind,
				Releases:   []CatalogRelease{release},
			}
		}
	}
	for _, pack := range next.Packs {
		for _, release := range pack.Releases {
			old, ok := prevReleases[pack.Name+"\x00"+release.Version]
			if !ok {
				continue
			}
			oldRelease := old.Releases[0]
			if old.Source != pack.Source || old.SourceKind != pack.SourceKind ||
				oldRelease.Ref != release.Ref || oldRelease.Commit != release.Commit || oldRelease.Hash != release.Hash {
				return fmt.Errorf("registry release %s %s changed immutable metadata", pack.Name, release.Version)
			}
		}
	}
	return nil
}

// PruneRemovedRegistryCaches removes cache directories for registries no longer configured.
func PruneRemovedRegistryCaches(home string, active map[string]bool) error {
	root := gchome.RegistryCacheRoot(home)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading registry cache root: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || active[entry.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("removing registry cache %q: %w", entry.Name(), err)
		}
	}
	return nil
}

// CatalogFresh reports whether a cached catalog is within the maximum age.
func CatalogFresh(path string, now time.Time, maxAge time.Duration) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return now.Sub(info.ModTime()) <= maxAge, nil
}

// FreshnessFromEnv returns the registry freshness duration configured in the environment.
func FreshnessFromEnv(def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv("GC_REGISTRY_FRESHNESS"))
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid GC_REGISTRY_FRESHNESS %q: %w", raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid GC_REGISTRY_FRESHNESS %q: must be positive", raw)
	}
	return d, nil
}

func fetchRemoteCatalog(ctx context.Context, u *url.URL, opts FetchOptions) ([]byte, error) {
	if u == nil || u.Scheme != "https" {
		return nil, errors.New("remote registry source must use HTTPS")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var client *http.Client
	if opts.Client != nil {
		clientCopy := *opts.Client
		client = &clientCopy
	} else {
		clientCopy := *http.DefaultClient
		client = &clientCopy
	}
	client.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("insecure redirect to %s", req.URL.String())
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching registry catalog: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetching registry catalog: HTTP %d", resp.StatusCode)
	}
	body := resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("opening gzip registry catalog: %w", err)
		}
		defer gz.Close() //nolint:errcheck
		body = gz
	}
	return readLimited(body, maxBytes(opts))
}

func readLocalCatalog(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading local registry catalog: %w", err)
	}
	if info.IsDir() {
		path = filepath.Join(path, "registry.toml")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening local registry catalog: %w", err)
	}
	defer f.Close() //nolint:errcheck
	return readLimited(f, limit)
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, fmt.Errorf("registry catalog exceeds %d bytes", limit)
	}
	return buf.Bytes(), nil
}

func maxBytes(opts FetchOptions) int64 {
	if opts.MaxBytes > 0 {
		return opts.MaxBytes
	}
	return DefaultMaxBytes
}

func isLocalPackSource(source string) bool {
	if strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") || strings.HasPrefix(source, "/") || strings.HasPrefix(source, "~/") ||
		filepath.IsAbs(source) || windowsPathRE.MatchString(source) || windowsUNCPath.MatchString(source) {
		return true
	}
	u, err := url.Parse(source)
	return err == nil && u.Scheme == "file"
}

func validatePackSource(source string, remote bool) error {
	source = strings.TrimSpace(source)
	if source == "" {
		return errors.New("source is required")
	}
	if isLocalPackSource(source) {
		if remote {
			return fmt.Errorf("remote catalog cannot use local source %q", source)
		}
		return nil
	}
	u, err := url.Parse(source)
	if err == nil && u.Scheme != "" {
		switch u.Scheme {
		case "https":
			if u.Host == "" {
				return fmt.Errorf("invalid HTTPS source %q", source)
			}
			return nil
		case "file":
			if remote {
				return fmt.Errorf("remote catalog cannot use local source %q", source)
			}
			if _, err := fileURLPath(u); err != nil {
				return err
			}
			return nil
		default:
			return fmt.Errorf("unsupported source scheme %q", u.Scheme)
		}
	}
	if remote {
		return fmt.Errorf("remote catalog cannot use local source %q", source)
	}
	return fmt.Errorf("unsupported local catalog source %q", source)
}

func fileURLPath(u *url.URL) (string, error) {
	if u.Host != "" && u.Host != "localhost" {
		return "", fmt.Errorf("unsupported file registry host %q", u.Host)
	}
	return filepath.FromSlash(u.Path), nil
}
