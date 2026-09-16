package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

const (
	doltLogWarnBytesDefault  = int64(512) * 1024 * 1024      // 512 MB
	doltLogErrorBytesDefault = int64(2) * 1024 * 1024 * 1024 // 2 GB
)

// DoltLogSizeCheck warns when the managed Dolt server log has grown large.
//
// The log is opened O_APPEND at every server start and never truncated or
// rotated, so it accumulates for the life of the pack runtime directory.
// Nothing in gascity bounds it: the rotation in internal/events covers
// .gc/events.jsonl only, and no maintenance order trims this file.
//
// Growth is not hypothetical. On a two-core box the reaper logs one error
// per abandoned client connection at the wait_timeout boundary, which came
// to 21 MB in a single day of ordinary operation. The same unbounded-growth
// shape reached 645 MB on supervisor.log during a crash loop.
//
// Registered once per city — the managed Dolt server is shared across rigs.
type DoltLogSizeCheck struct {
	cityPath        string
	skip            bool
	applicableKnown bool
	applicable      bool
}

// NewDoltLogSizeCheck creates a managed Dolt server log size check.
func NewDoltLogSizeCheck(cityPath string, skip bool) *DoltLogSizeCheck {
	return &DoltLogSizeCheck{cityPath: cityPath, skip: skip}
}

// NewDoltLogSizeCheckForConfig creates a managed Dolt server log size check
// using a preloaded city config.
func NewDoltLogSizeCheckForConfig(cityPath string, skip bool, cfg *config.City, cfgErr error) *DoltLogSizeCheck {
	return &DoltLogSizeCheck{
		cityPath:        cityPath,
		skip:            skip,
		applicableKnown: true,
		applicable:      ManagedLocalDoltChecksApplicableForConfig(cityPath, cfg, cfgErr),
	}
}

func (c *DoltLogSizeCheck) managedApplicable() bool {
	if c.applicableKnown {
		return c.applicable
	}
	return managedLocalDoltChecksApplicable(c.cityPath)
}

// Name returns the check identifier.
func (c *DoltLogSizeCheck) Name() string { return "dolt-log-size" }

// Run compares the managed Dolt server log against warning/error thresholds.
func (c *DoltLogSizeCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skip || !c.managedApplicable() {
		r.Status = StatusOK
		r.Message = "skipped (file backend, external dolt endpoint, or GC_DOLT=skip)"
		return r
	}

	warnBytes := doltEnvByteThreshold("GC_DOLT_LOG_WARN_BYTES", doltLogWarnBytesDefault)
	errorBytes := doltEnvByteThreshold("GC_DOLT_LOG_ERROR_BYTES", doltLogErrorBytesDefault)

	logFile := resolveManagedDoltLogFile(c.cityPath)
	fi, err := os.Stat(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			r.Status = StatusOK
			r.Message = "dolt.log not present (nothing to check)"
			return r
		}
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("stat dolt log: %v", err)
		return r
	}
	if !fi.Mode().IsRegular() {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("%s is not a regular file", logFile)
		return r
	}

	size := fi.Size()
	r.Details = []string{
		fmt.Sprintf("log path: %s", logFile),
		fmt.Sprintf("warn threshold: GC_DOLT_LOG_WARN_BYTES (default 512 MB, current %s)", humanSize(warnBytes)),
		fmt.Sprintf("error threshold: GC_DOLT_LOG_ERROR_BYTES (default 2 GB, current %s)", humanSize(errorBytes)),
	}

	switch {
	case size >= errorBytes:
		r.Status = StatusError
		r.Message = fmt.Sprintf("dolt.log is %s — nothing rotates it; truncate or archive it", humanSize(size))
		r.FixHint = doltLogFixHint(logFile)
	case size >= warnBytes:
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("dolt.log is %s — approaching threshold; nothing rotates it", humanSize(size))
		r.FixHint = doltLogFixHint(logFile)
	default:
		r.Status = StatusOK
		r.Message = fmt.Sprintf("dolt.log size: %s", humanSize(size))
	}
	return r
}

// CanFix returns false. Truncating a log the server holds open is safe with
// an O_APPEND writer, but discarding it destroys the only record of why the
// server behaved as it did — and whether to archive first is operator policy.
func (c *DoltLogSizeCheck) CanFix() bool { return false }

// Fix is a no-op. See CanFix.
func (c *DoltLogSizeCheck) Fix(_ *CheckContext) error { return nil }

func doltLogFixHint(logFile string) string {
	return fmt.Sprintf("archive it, then truncate in place (the server appends, so : > %s is safe while it runs)", logFile)
}

// resolveManagedDoltLogFile mirrors the layout cmd/gc uses to start the
// managed server: GC_DOLT_LOG_FILE wins, otherwise dolt.log in the pack
// state directory.
func resolveManagedDoltLogFile(cityPath string) string {
	if override := strings.TrimSpace(os.Getenv("GC_DOLT_LOG_FILE")); override != "" {
		return filepath.Clean(override)
	}
	return filepath.Join(doctorDoltPackStateDir(cityPath), "dolt.log")
}
