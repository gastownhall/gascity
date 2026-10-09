package beads

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Clock census (mc-hbgfc): every window stamp and check reads clockNow, so a
// simulator's WithClock moves them all. A wall-clock read in caching_store*.go
// is allowed only where it is a real wait, a real measurement, or mirrors the
// backing's own clock; any other is a window a simulator cannot move.
func TestCachingStoreWallClockReadsCensus(t *testing.T) {
	allowed := map[string]map[string]bool{
		"clockNow":                {"return time.Now()": true},
		"runReconciliation":       {"start := time.Now()": true, "bdStart := time.Now()": true, "projectStart := time.Now()": true},
		"cachedReadyCompleteOnly": {"now := time.Now().UTC()": true}, // defer_until, the backing's clock
		"cachedReadyLocked":       {"now := time.Now().UTC()": true},
		"Ready":                   {"now := time.Now().UTC()": true},
		"CachedReady":             {"now := time.Now().UTC()": true},
		"ReleaseIfCurrent":        {"b.UpdatedAt = time.Now()": true}, // a patched row mirrors the backing
		"TransferIfCurrent":       {"b.UpdatedAt = time.Now()": true},
	}
	measured := map[string]bool{"runReconciliation": true} // time.Since: real latencies
	wall := regexp.MustCompile(`time\.(Now|Since|Until)\(`)
	funcRe := regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z0-9_]+)`)
	files, _ := filepath.Glob("caching_store*.go")
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		scanned++
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		fn := ""
		for i, line := range strings.Split(string(src), "\n") {
			if m := funcRe.FindStringSubmatch(line); m != nil {
				fn = m[1]
			}
			code, _, _ := strings.Cut(strings.TrimSpace(line), "//")
			code = strings.TrimSpace(code)
			if !wall.MatchString(code) {
				continue
			}
			if allowed[fn][code] || (measured[fn] && !strings.Contains(code, "time.Now(")) {
				continue
			}
			t.Errorf("%s:%d wall-clock read in %s outside the allowlist; a window stamp or check must read c.clockNow(): %s", f, i+1, fn, code)
		}
	}
	if scanned == 0 {
		t.Fatal("no caching_store*.go sources found; the census checked nothing")
	}
}
