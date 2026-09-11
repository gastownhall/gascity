package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

const (
	doltJournalWarnBytesDefault  = int64(4) * 1024 * 1024 * 1024 // 4 GB
	doltJournalErrorBytesDefault = int64(6) * 1024 * 1024 * 1024 // 6 GB
)

// DoltJournalSizeCheck warns when Dolt write-ahead journal files approach the
// compaction threshold. Journal files grow unboundedly between compactions and
// are the primary corruption vector documented in ga-pqfk8t.
//
// Registered once per city — the managed Dolt server is shared across all rigs.
type DoltJournalSizeCheck struct {
	cityPath        string
	skip            bool
	applicableKnown bool
	applicable      bool
	scopeRoots      []string
}

// NewDoltJournalSizeCheck creates a Dolt journal size check.
func NewDoltJournalSizeCheck(cityPath string, skip bool) *DoltJournalSizeCheck {
	return &DoltJournalSizeCheck{cityPath: cityPath, skip: skip}
}

// NewDoltJournalSizeCheckForConfig creates a Dolt journal size check using a
// preloaded city config.
func NewDoltJournalSizeCheckForConfig(cityPath string, skip bool, cfg *config.City, cfgErr error) *DoltJournalSizeCheck {
	return &DoltJournalSizeCheck{
		cityPath:        cityPath,
		skip:            skip,
		applicableKnown: true,
		applicable:      ManagedLocalDoltChecksApplicableForConfig(cityPath, cfg, cfgErr),
		scopeRoots:      managedDoltScopeRootsForConfig(cityPath, cfg, cfgErr),
	}
}

func (c *DoltJournalSizeCheck) managedApplicable() bool {
	if c.applicableKnown {
		return c.applicable
	}
	return managedLocalDoltChecksApplicable(c.cityPath)
}

// Name returns the check identifier.
func (c *DoltJournalSizeCheck) Name() string { return "dolt-journal-size" }

// Run inspects *.journal files under each managed Dolt database's noms
// directory and compares the largest single-database journal to
// warning/error thresholds.
func (c *DoltJournalSizeCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skip || !c.managedApplicable() {
		r.Status = StatusOK
		r.Message = "skipped (file backend, external dolt endpoint, or GC_DOLT=skip)"
		return r
	}

	warnBytes := doltJournalThreshold("GC_DOLT_JOURNAL_WARN_BYTES", doltJournalWarnBytesDefault)
	errorBytes := doltJournalThreshold("GC_DOLT_JOURNAL_ERROR_BYTES", doltJournalErrorBytesDefault)

	targets, unresolved := managedLocalDoltScanTargets(c.cityPath)
	if c.applicableKnown {
		targets, unresolved = managedLocalDoltScanTargetsForScopeRoots(c.cityPath, c.scopeRoots)
	}
	if len(targets) == 0 {
		if unresolved {
			r.Status = StatusOK
			r.Message = "skipped (dolt target unresolved)"
			return r
		}
		r.Status = StatusOK
		r.Message = "skipped (file backend, external dolt endpoint, or GC_DOLT=skip)"
		return r
	}

	dataDir := resolveManagedDoltDataDir(c.cityPath)

	type dbResult struct {
		db   string
		size int64
	}
	var results []dbResult
	for _, target := range targets {
		journalDir := filepath.Join(target.ScanRoot, "noms")
		size, err := sumJournalFiles(journalDir)
		if err != nil {
			r.Status = StatusWarning
			r.Message = fmt.Sprintf("scan dolt journal dir: %v", err)
			return r
		}
		results = append(results, dbResult{db: target.Database, size: size})
	}

	var worstDB string
	var worstSize int64
	for _, res := range results {
		if res.size > worstSize {
			worstSize = res.size
			worstDB = res.db
		}
	}

	dbLabel := strings.TrimSpace(worstDB)
	if dbLabel == "" {
		dbLabel = "managed dolt data"
	}

	details := []string{
		fmt.Sprintf("scan path: %s", dataDir),
	}
	for _, res := range results {
		name := strings.TrimSpace(res.db)
		if name == "" {
			name = "managed dolt data"
		}
		details = append(details, fmt.Sprintf("database %s: %s journal", name, formatGB(res.size)))
	}
	details = append(details,
		fmt.Sprintf("warn threshold: GC_DOLT_JOURNAL_WARN_BYTES (default 4 GB, current %s)", formatGB(warnBytes)),
		fmt.Sprintf("error threshold: GC_DOLT_JOURNAL_ERROR_BYTES (default 6 GB, current %s)", formatGB(errorBytes)),
	)
	r.Details = details

	switch {
	case worstSize >= errorBytes:
		r.Status = StatusError
		r.Message = fmt.Sprintf("dolt journal for %s is %s — at corruption risk; compact immediately (see ga-pqfk8t for incident context)", dbLabel, formatGB(worstSize))
		r.FixHint = "run: gc dolt compact"
	case worstSize >= warnBytes:
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("dolt journal for %s is %s — approaching compaction threshold; run: gc dolt compact", dbLabel, formatGB(worstSize))
		r.FixHint = "run: gc dolt compact"
	default:
		n := len(results)
		if worstSize == 0 {
			r.Status = StatusOK
			r.Message = fmt.Sprintf("dolt-journal-size: no journal files found (%d database(s) scanned)", n)
		} else {
			r.Status = StatusOK
			r.Message = fmt.Sprintf("dolt-journal-size: %s across %d database(s) (largest: %s)", formatGB(worstSize), n, dbLabel)
		}
	}
	return r
}

// CanFix returns false — compaction must be triggered manually to avoid
// racing the live server.
func (c *DoltJournalSizeCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *DoltJournalSizeCheck) Fix(_ *CheckContext) error { return nil }

// doltJournalThreshold reads an int64 byte threshold from env, falling back
// to defaultVal on empty or invalid input.
func doltJournalThreshold(envVar string, defaultVal int64) int64 {
	v := strings.TrimSpace(os.Getenv(envVar))
	if v == "" {
		return defaultVal
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return defaultVal
	}
	return n
}

// sumJournalFiles returns the sum of sizes of all *.journal files in dir.
// Returns 0, nil for non-existent or empty directories.
func sumJournalFiles(dir string) (int64, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.journal"))
	if err != nil {
		return 0, fmt.Errorf("glob %s: %w", dir, err)
	}
	var total int64
	for _, path := range matches {
		info, statErr := os.Stat(path)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return 0, fmt.Errorf("stat %s: %w", path, statErr)
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total, nil
}
