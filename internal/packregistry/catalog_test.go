package packregistry

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validCatalog = `schema = 1

[[pack]]
name = "lighthouse"
description = "Harbor-watch checks."
source = "https://packages.example/lighthouse.git"
source_kind = "git"

  [[pack.release]]
  version = "1.2.0"
  ref = "v1.2.0"
  commit = "0123456789abcdef0123456789abcdef01234567"
  hash = "sha256:3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"
  description = "Stable release."
`

func TestValidateCatalogNamesReleasesAndHashes(t *testing.T) {
	catalog, err := ParseCatalog([]byte(validCatalog))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	if err := ValidateCatalog(catalog, true); err != nil {
		t.Fatalf("ValidateCatalog(valid): %v", err)
	}

	cases := []struct {
		name string
		old  string
		new  string
	}{
		{"bad pack name", `name = "lighthouse"`, `name = "Acme"`},
		{"long pack name segment", `name = "lighthouse"`, `name = "` + strings.Repeat("a", 65) + `"`},
		{"bad commit", `commit = "0123456789abcdef0123456789abcdef01234567"`, `commit = "abc123"`},
		{"bad hash", `hash = "sha256:3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"`, `hash = "sha256:nope"`},
		{"bad source kind", `source_kind = "git"`, `source_kind = "archive"`},
		{"bad release version", `version = "1.2.0"`, `version = "latest"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			catalog, err := ParseCatalog([]byte(strings.Replace(validCatalog, tc.old, tc.new, 1)))
			if err != nil {
				t.Fatalf("ParseCatalog: %v", err)
			}
			if err := ValidateCatalog(catalog, true); err == nil {
				t.Fatal("ValidateCatalog succeeded, want error")
			}
		})
	}
}

func TestValidateCatalogDuplicateReleaseAndWithdrawn(t *testing.T) {
	text := validCatalog + `
  [[pack.release]]
  version = "1.2.0"
  ref = "again"
  commit = "0123456789abcdef0123456789abcdef01234567"
  hash = "sha256:3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"
  description = "Duplicate."
  withdrawn = true
  withdrawn_reason = "bad"
`
	catalog, err := ParseCatalog([]byte(text))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	if !catalog.Packs[0].Releases[1].Withdrawn || catalog.Packs[0].Releases[1].WithdrawnReason != "bad" {
		t.Fatalf("withdrawn release not parsed: %+v", catalog.Packs[0].Releases[1])
	}
	if err := ValidateCatalog(catalog, true); err == nil {
		t.Fatal("ValidateCatalog duplicate release succeeded, want error")
	}
}

func TestRemoteCatalogRejectsLocalSourcesButLocalCatalogAccepts(t *testing.T) {
	local := strings.Replace(validCatalog, `source = "https://packages.example/lighthouse.git"`, `source = "file:///tmp/lighthouse.git"`, 1)
	catalog, err := ParseCatalog([]byte(local))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	if err := ValidateCatalog(catalog, true); err == nil {
		t.Fatal("remote ValidateCatalog accepted file source")
	}
	if err := ValidateCatalog(catalog, false); err != nil {
		t.Fatalf("local ValidateCatalog rejected file source: %v", err)
	}
}

func TestRemoteCatalogRejectsNonHTTPSSources(t *testing.T) {
	for _, source := range []string{
		"http://packages.example/lighthouse.git",
		"ssh://packages.example/lighthouse.git",
		"git@packages.example:lighthouse.git",
		"../lighthouse.git",
	} {
		t.Run(source, func(t *testing.T) {
			text := strings.Replace(validCatalog, `source = "https://packages.example/lighthouse.git"`, `source = "`+source+`"`, 1)
			catalog, err := ParseCatalog([]byte(text))
			if err != nil {
				t.Fatalf("ParseCatalog: %v", err)
			}
			if err := ValidateCatalog(catalog, true); err == nil {
				t.Fatal("remote ValidateCatalog accepted non-HTTPS source")
			}
		})
	}
}

func TestLocalCatalogRejectsUnsupportedSourceSchemes(t *testing.T) {
	text := strings.Replace(validCatalog, `source = "https://packages.example/lighthouse.git"`, `source = "ssh://packages.example/lighthouse.git"`, 1)
	catalog, err := ParseCatalog([]byte(text))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	if err := ValidateCatalog(catalog, false); err == nil {
		t.Fatal("local ValidateCatalog accepted unsupported source scheme")
	}
}

func TestFetchRejectsHTTPAndHTTPRedirect(t *testing.T) {
	if _, err := NormalizeSource("http://example.com/registry.toml"); err == nil {
		t.Fatal("NormalizeSource accepted http")
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(validCatalog))
	}))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/registry.toml", http.StatusFound)
	}))
	defer server.Close()
	source, err := NormalizeSource(server.URL + "/registry.toml")
	if err != nil {
		t.Fatalf("NormalizeSource: %v", err)
	}
	_, err = FetchCatalog(context.Background(), source, FetchOptions{Client: server.Client()})
	if err == nil || !strings.Contains(err.Error(), "insecure redirect") {
		t.Fatalf("FetchCatalog redirect err = %v, want insecure redirect", err)
	}
}

func TestNormalizeHTTPSRegistrySourcePaths(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"https://example.com", "https://example.com/registry.toml"},
		{"https://example.com/", "https://example.com/registry.toml"},
		{"https://example.com/main", "https://example.com/main/registry.toml"},
		{"https://example.com/main/", "https://example.com/main/registry.toml"},
		{"https://example.com/main/registry.toml", "https://example.com/main/registry.toml"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			source, err := NormalizeSource(tc.raw)
			if err != nil {
				t.Fatalf("NormalizeSource: %v", err)
			}
			if got := source.URL.String(); got != tc.want {
				t.Fatalf("URL = %q, want %q", got, tc.want)
			}
		})
	}
	for _, raw := range []string{"https://example.com/catalog.toml", "https://example.com/main/catalog.toml"} {
		if _, err := NormalizeSource(raw); err == nil {
			t.Fatalf("NormalizeSource(%q) succeeded, want error", raw)
		}
	}
}

func TestNormalizeSourceRejectsWindowsRegistrySources(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{
			name:    "drive backslashes",
			raw:     `C:\packs\registry.toml`,
			wantErr: "registry source uses a Windows drive-letter path",
		},
		{
			name:    "drive slashes",
			raw:     `C:/packs/registry.toml`,
			wantErr: "registry source uses a Windows drive-letter path",
		},
		{
			name:    "unc backslashes",
			raw:     `\\server\share\registry.toml`,
			wantErr: "registry source uses a UNC path",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NormalizeSource(tc.raw); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("NormalizeSource(%q) err = %v, want %q", tc.raw, err, tc.wantErr)
			}
		})
	}
}

func TestLocalCatalogAcceptsWindowsPathSource(t *testing.T) {
	text := strings.Replace(validCatalog, `source = "https://packages.example/lighthouse.git"`, `source = 'C:\packs\lighthouse.git'`, 1)
	catalog, err := ParseCatalog([]byte(text))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	if err := ValidateCatalog(catalog, false); err != nil {
		t.Fatalf("local ValidateCatalog rejected Windows path source: %v", err)
	}
	if err := ValidateCatalog(catalog, true); err == nil {
		t.Fatal("remote ValidateCatalog accepted Windows path source")
	}
}

func TestFileURLPathPreservesEscapedPercent(t *testing.T) {
	source, err := NormalizeSource("file:///tmp/repo%2520name/registry.toml")
	if err != nil {
		t.Fatalf("NormalizeSource: %v", err)
	}
	if !strings.Contains(source.Path, "%20") {
		t.Fatalf("file path double-unescaped: %q", source.Path)
	}
}

func TestFetchVerifiesTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(validCatalog))
	}))
	defer server.Close()
	source, err := NormalizeSource(server.URL + "/registry.toml")
	if err != nil {
		t.Fatalf("NormalizeSource: %v", err)
	}
	if _, err := FetchCatalog(context.Background(), source, FetchOptions{}); err == nil {
		t.Fatal("FetchCatalog with default client accepted self-signed TLS")
	}
}

func TestFetchTimeoutAndSizeLimits(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(validCatalog))
	}))
	defer server.Close()
	source, err := NormalizeSource(server.URL + "/registry.toml")
	if err != nil {
		t.Fatalf("NormalizeSource: %v", err)
	}
	if _, err := FetchCatalog(context.Background(), source, FetchOptions{Client: server.Client(), Timeout: time.Nanosecond}); err == nil {
		t.Fatal("FetchCatalog succeeded, want timeout")
	}

	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(validCatalog))
	})
	if _, err := FetchCatalog(context.Background(), source, FetchOptions{Client: server.Client(), MaxBytes: 8}); err == nil {
		t.Fatal("FetchCatalog succeeded, want size error")
	}
}

func TestFetchRejectsDecompressionBomb(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, _ = gz.Write([]byte(strings.Repeat("a", 1024)))
	_ = gz.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed.Bytes())
	}))
	defer server.Close()
	source, err := NormalizeSource(server.URL + "/registry.toml")
	if err != nil {
		t.Fatalf("NormalizeSource: %v", err)
	}
	if _, err := FetchCatalog(context.Background(), source, FetchOptions{Client: server.Client(), MaxBytes: 32}); err == nil {
		t.Fatal("FetchCatalog succeeded, want decompressed size error")
	}
}

func TestLocalSourceReadsFileAndDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "registry.toml"), []byte(validCatalog), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	source, err := NormalizeSource(dir)
	if err != nil {
		t.Fatalf("NormalizeSource: %v", err)
	}
	data, err := FetchCatalog(context.Background(), source, FetchOptions{})
	if err != nil {
		t.Fatalf("FetchCatalog(dir): %v", err)
	}
	if !strings.Contains(string(data), "lighthouse") {
		t.Fatalf("unexpected local data: %s", data)
	}
	source, err = NormalizeSource((&urlForTest{path: filepath.Join(dir, "registry.toml")}).String())
	if err != nil {
		t.Fatalf("NormalizeSource(file): %v", err)
	}
	if _, err := FetchCatalog(context.Background(), source, FetchOptions{}); err != nil {
		t.Fatalf("FetchCatalog(file): %v", err)
	}
}

type urlForTest struct{ path string }

func (u *urlForTest) String() string {
	return "file://" + filepath.ToSlash(u.path)
}

func TestImmutableReleaseMetadata(t *testing.T) {
	prev, _ := ParseCatalog([]byte(validCatalog))
	next, _ := ParseCatalog([]byte(strings.Replace(validCatalog, `ref = "v1.2.0"`, `ref = "moved"`, 1)))
	if err := CheckImmutable(prev, next); err == nil {
		t.Fatal("CheckImmutable accepted changed ref")
	}
}

func TestRefreshRegistryRejectsUnreadablePriorCache(t *testing.T) {
	home := t.TempDir()
	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "registry.toml"), []byte(validCatalog), 0o644); err != nil {
		t.Fatalf("WriteFile(source registry): %v", err)
	}
	if err := WriteCatalogCache(home, "main", []byte("not = valid = toml")); err != nil {
		t.Fatalf("WriteCatalogCache(invalid): %v", err)
	}

	_, err := RefreshRegistry(context.Background(), home, Registry{Name: "main", Source: sourceDir}, FetchOptions{})
	if err == nil {
		t.Fatal("RefreshRegistry succeeded with unreadable prior cache")
	}
	if !strings.Contains(err.Error(), "previous registry cache") {
		t.Fatalf("RefreshRegistry err = %v, want previous cache context", err)
	}
	got, readErr := os.ReadFile(CachePath(home, "main"))
	if readErr != nil {
		t.Fatalf("ReadFile(cache): %v", readErr)
	}
	if string(got) != "not = valid = toml" {
		t.Fatalf("RefreshRegistry overwrote unreadable cache:\n%s", got)
	}
}

func TestFreshnessEnv(t *testing.T) {
	t.Setenv("GC_REGISTRY_FRESHNESS", "2s")
	d, err := FreshnessFromEnv(time.Hour)
	if err != nil {
		t.Fatalf("FreshnessFromEnv: %v", err)
	}
	if d != 2*time.Second {
		t.Fatalf("freshness = %v, want 2s", d)
	}
	for _, raw := range []string{"abc", "-1s", "0s"} {
		t.Setenv("GC_REGISTRY_FRESHNESS", raw)
		if _, err := FreshnessFromEnv(time.Hour); err == nil {
			t.Fatalf("FreshnessFromEnv(%q) succeeded, want error", raw)
		}
	}
}

func TestPruneRemovedRegistryCaches(t *testing.T) {
	home := t.TempDir()
	if err := WriteCatalogCache(home, "main", []byte(validCatalog)); err != nil {
		t.Fatalf("WriteCatalogCache(main): %v", err)
	}
	if err := WriteCatalogCache(home, "old", []byte(validCatalog)); err != nil {
		t.Fatalf("WriteCatalogCache(old): %v", err)
	}
	if err := PruneRemovedRegistryCaches(home, map[string]bool{"main": true}); err != nil {
		t.Fatalf("PruneRemovedRegistryCaches: %v", err)
	}
	if _, err := os.Stat(CachePath(home, "old")); !os.IsNotExist(err) {
		t.Fatalf("old cache still exists or stat err = %v", err)
	}
	if _, err := os.Stat(CachePath(home, "main")); err != nil {
		t.Fatalf("main cache missing: %v", err)
	}
}

func TestReadCachedRegistryCatalogUsesRegistrySourceTrustBoundary(t *testing.T) {
	home := t.TempDir()
	local := strings.Replace(validCatalog, `source = "https://packages.example/lighthouse.git"`, `source = "file:///tmp/lighthouse.git"`, 1)
	if err := WriteCatalogCache(home, "main", []byte(local)); err != nil {
		t.Fatalf("WriteCatalogCache: %v", err)
	}
	reg := Registry{Name: "main", Source: "https://registry.example/main"}
	if _, _, err := ReadCachedRegistryCatalog(home, reg); err == nil {
		t.Fatal("ReadCachedRegistryCatalog accepted local pack source for remote registry")
	}
	reg.Source = "file:///tmp/main/registry.toml"
	if _, _, err := ReadCachedRegistryCatalog(home, reg); err != nil {
		t.Fatalf("ReadCachedRegistryCatalog rejected local registry cache: %v", err)
	}
}
