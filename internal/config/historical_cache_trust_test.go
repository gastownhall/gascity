package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestTrustedHistoricalFormulaCacheRootsAllowsSameSourceUpgrade(t *testing.T) {
	fixture := newHistoricalTrustFixture(t, "", "", "")
	fixture.stubGit(t)
	roots := TrustedHistoricalFormulaCacheRoots(fixture.cityRoot, fixture.checkPath, []string{fixture.activeFormulas})
	if !pathWithinAnyTestRoot(fixture.checkPath, roots) {
		t.Fatalf("historical same-source check path was not trusted; roots=%v", roots)
	}
}

func TestTrustedHistoricalFormulaCacheRootsRejectsUnsafeHistory(t *testing.T) {
	tests := []struct {
		name              string
		candidateSource   string
		candidateKey      string
		candidatePackPath string
		activeStatus      string
		historyStatus     string
		removeHistoryGit  bool
		malformedLock     bool
		untrackedCheck    bool
	}{
		{
			name:            "unrelated repository",
			candidateSource: "https://github.com/attacker/other.git//packs/review",
		},
		{
			name:              "different pack subpath in same repository",
			candidatePackPath: filepath.Join("packs", "other", "assets", "scripts", "checks", "unit-fast.sh"),
		},
		{
			name:         "malformed cache key",
			candidateKey: "not-a-canonical-cache-key",
		},
		{
			name:          "dirty historical checkout",
			historyStatus: " M assets/scripts/checks/unit-fast.sh",
		},
		{
			name:         "dirty active checkout",
			activeStatus: " M formulas/review.toml",
		},
		{
			name:             "missing historical git metadata",
			removeHistoryGit: true,
		},
		{
			name:          "malformed city lockfile",
			malformedLock: true,
		},
		{
			name:           "untracked historical check",
			untrackedCheck: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newHistoricalTrustFixture(t, tt.candidateSource, tt.candidateKey, tt.candidatePackPath)
			fixture.activeStatus = tt.activeStatus
			fixture.historyStatus = tt.historyStatus
			fixture.untrackedCheck = tt.untrackedCheck
			if tt.removeHistoryGit {
				if err := os.RemoveAll(filepath.Join(fixture.historyCache, ".git")); err != nil {
					t.Fatalf("remove historical .git: %v", err)
				}
			}
			if tt.malformedLock {
				writeTestFile(t, fixture.cityRoot, "packs.lock", "[packs\n")
			}
			fixture.stubGit(t)

			roots := TrustedHistoricalFormulaCacheRoots(fixture.cityRoot, fixture.checkPath, []string{fixture.activeFormulas})
			if len(roots) != 0 {
				t.Fatalf("unsafe historical path received trusted roots: %v", roots)
			}
		})
	}
}

func TestTrustedHistoricalFormulaCacheRootsRevalidatesHistoricalCheckout(t *testing.T) {
	fixture := newHistoricalTrustFixture(t, "", "", "")
	fixture.stubGit(t)

	first := TrustedHistoricalFormulaCacheRoots(fixture.cityRoot, fixture.checkPath, []string{fixture.activeFormulas})
	if !pathWithinAnyTestRoot(fixture.checkPath, first) {
		t.Fatalf("initial clean historical checkout was not trusted; roots=%v", first)
	}
	fixture.historyStatus = " M packs/review/assets/scripts/checks/unit-fast.sh"
	second := TrustedHistoricalFormulaCacheRoots(fixture.cityRoot, fixture.checkPath, []string{fixture.activeFormulas})
	if len(second) != 0 {
		t.Fatalf("dirty historical checkout bypassed trust through a cached validation: %v", second)
	}
}

type historicalTrustFixture struct {
	cityRoot       string
	currentCommit  string
	historyCommit  string
	activeCache    string
	historyCache   string
	activeFormulas string
	checkPath      string
	activeStatus   string
	historyStatus  string
	untrackedCheck bool
}

func newHistoricalTrustFixture(t *testing.T, candidateSource, candidateKey, candidatePackPath string) *historicalTrustFixture {
	t.Helper()
	const (
		source        = "https://github.com/example/workflows.git//packs/review"
		currentCommit = "1111111111111111111111111111111111111111"
		historyCommit = "2222222222222222222222222222222222222222"
	)
	if candidateSource == "" {
		candidateSource = source
	}
	if candidateKey == "" {
		candidateKey = RepoCacheKey(candidateSource, historyCommit)
	}
	if candidatePackPath == "" {
		candidatePackPath = filepath.Join("packs", "review", "assets", "scripts", "checks", "unit-fast.sh")
	}
	root := t.TempDir()
	gcHome := filepath.Join(root, "gc-home")
	t.Setenv("GC_HOME", gcHome)
	cityRoot := filepath.Join(root, "city")
	cacheRoot := filepath.Join(gcHome, "cache", "repos")
	activeCache := filepath.Join(cacheRoot, RepoCacheKey(source, currentCommit))
	historyCache := filepath.Join(cacheRoot, candidateKey)
	activeFormulas := filepath.Join(activeCache, "packs", "review", "formulas")
	checkPath := filepath.Join(historyCache, candidatePackPath)
	for _, dir := range []string{
		cityRoot,
		filepath.Join(activeCache, ".git"),
		activeFormulas,
		filepath.Join(historyCache, ".git"),
		filepath.Dir(checkPath),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeTestFile(t, cityRoot, "packs.lock", fmt.Sprintf(`
schema = 1

[packs.%q]
commit = %q
`, source, currentCommit))
	writeTestFile(t, filepath.Dir(checkPath), filepath.Base(checkPath), "#!/bin/sh\nexit 0\n")
	return &historicalTrustFixture{
		cityRoot:       cityRoot,
		currentCommit:  currentCommit,
		historyCommit:  historyCommit,
		activeCache:    activeCache,
		historyCache:   historyCache,
		activeFormulas: activeFormulas,
		checkPath:      checkPath,
	}
}

func (f *historicalTrustFixture) stubGit(t *testing.T) {
	t.Helper()
	previousRunGit := runRepoCacheGit
	runRepoCacheGit = func(dir string, args ...string) (string, error) {
		if slices.Equal(args, []string{"rev-parse", "HEAD"}) {
			switch dir {
			case f.activeCache:
				return f.currentCommit, nil
			case f.historyCache:
				return f.historyCommit, nil
			}
		}
		if slices.Equal(args, []string{"status", "--porcelain"}) {
			switch dir {
			case f.activeCache:
				return f.activeStatus, nil
			case f.historyCache:
				return f.historyStatus, nil
			}
		}
		if len(args) == 4 && slices.Equal(args[:3], []string{"ls-files", "--error-unmatch", "--"}) && dir == f.historyCache {
			if f.untrackedCheck {
				return "", fmt.Errorf("path is not tracked")
			}
			return args[3], nil
		}
		return "", fmt.Errorf("unexpected git call in %s: %v", dir, args)
	}
	t.Cleanup(func() {
		runRepoCacheGit = previousRunGit
		ResetRemoteCacheValidationCache()
	})
}

func pathWithinAnyTestRoot(path string, roots []string) bool {
	for _, root := range roots {
		if pathWithinDir(path, root) {
			return true
		}
	}
	return false
}
