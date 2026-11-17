package doctor

import "github.com/gastownhall/gascity/internal/config"

// doltArchiveFooterSize is the length in bytes of a Dolt archive (.darc)
// format v3 footer, the last bytes of the file.
const doltArchiveFooterSize = 220

// DoltArchiveDictCheck warns when a managed Dolt archive holds more zstd
// dictionaries than Dolt's reader cache can keep.
type DoltArchiveDictCheck struct {
	cityPath        string
	skip            bool
	applicableKnown bool
	applicable      bool
	scopeRoots      []string
}

// NewDoltArchiveDictCheck creates a Dolt archive dictionary check.
func NewDoltArchiveDictCheck(cityPath string, skip bool) *DoltArchiveDictCheck {
	return &DoltArchiveDictCheck{cityPath: cityPath, skip: skip}
}

// NewDoltArchiveDictCheckForConfig creates a Dolt archive dictionary check
// using a preloaded city config.
func NewDoltArchiveDictCheckForConfig(cityPath string, skip bool, cfg *config.City, cfgErr error) *DoltArchiveDictCheck {
	return &DoltArchiveDictCheck{
		cityPath:        cityPath,
		skip:            skip,
		applicableKnown: true,
		applicable:      ManagedLocalDoltChecksApplicableForConfig(cityPath, cfg, cfgErr),
		scopeRoots:      managedDoltScopeRootsForConfig(cityPath, cfg, cfgErr),
	}
}

// Name returns the check identifier.
func (c *DoltArchiveDictCheck) Name() string { return "dolt-archive-dicts" }

// Run is a compile-only stub until the implementation lands.
func (c *DoltArchiveDictCheck) Run(_ *CheckContext) *CheckResult {
	return &CheckResult{Name: c.Name(), Status: StatusOK}
}

// CanFix returns false.
func (c *DoltArchiveDictCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *DoltArchiveDictCheck) Fix(_ *CheckContext) error { return nil }

// parseDoltArchiveFooter is a compile-only stub until the implementation lands.
func parseDoltArchiveFooter(_ []byte, _ int64) (int64, error) { return 0, nil }
