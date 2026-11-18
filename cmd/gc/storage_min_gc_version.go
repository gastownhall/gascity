package main

// `.gc/min-gc-version` (S5 §7.4, upgrade-design B4 and B10).
//
// A split moves the city's infrastructure classes into a binding that an
// older gc does not read: a v1.4.x supervisor ignores [storage] and serves an
// empty infrastructure state, and a pre-1.5 gc on an agent's PATH writes
// infrastructure beads into the work store, which strands them. The split
// (`gc storage split`, S5-H) records the oldest gc that may run this city in
// .gc/min-gc-version, and every gc that knows the file refuses below it: the
// storage boot gate, which a controller boot and every one-shot command that
// routes a class pass through, refuses first. Binaries older than the file
// cannot be stopped; that is the documented floor (v1.5.1).
//
// The file holds one version, MAJOR.MINOR.PATCH, with or without a leading
// "v"; an empty file records no floor. A gc whose own version does
// not parse as one (a "dev" build) cannot be compared and is not refused: it
// is a local build, and the storage-split doctor check says it could not
// compare. A file that exists and does not parse refuses, as an unreadable
// remote-stage record does: it is still evidence of a floor.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/deps"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/searchpath"
)

// minGCVersionFileName is the floor's file under the city's .gc.
const minGCVersionFileName = "min-gc-version"

// minGCVersionPattern is the floor's one accepted shape.
var minGCVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// gcVersionCorePattern takes the release core off a gc version string:
// "1.5.2", "v1.5.2", "1.5.2-rc.1" and "1.5.2+ra.1" are all 1.5.2.
var gcVersionCorePattern = regexp.MustCompile(`^v?([0-9]+\.[0-9]+\.[0-9]+)(?:[-+].*)?$`)

func minGCVersionPath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", minGCVersionFileName)
}

// readMinGCVersion reads the city's floor. present is false when the city has
// none; a file that cannot be read or does not hold one version is an error.
func readMinGCVersion(cityPath string) (floor string, present bool, err error) {
	data, err := os.ReadFile(minGCVersionPath(cityPath))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", true, fmt.Errorf("reading %s: %w", minGCVersionPath(cityPath), err)
	}
	floor = strings.TrimPrefix(strings.TrimSpace(string(data)), "v")
	if floor == "" {
		return "", false, nil
	}
	if !minGCVersionPattern.MatchString(floor) {
		return "", true, fmt.Errorf("%s does not hold one MAJOR.MINOR.PATCH version", minGCVersionPath(cityPath))
	}
	return floor, true, nil
}

// writeMinGCVersion records floor as the city's oldest admissible gc,
// atomically. The guided split writes it with the city's [storage] (S5 §5.2
// P4).
func writeMinGCVersion(cityPath, floor string) error {
	if !minGCVersionPattern.MatchString(floor) {
		return fmt.Errorf("the gc version floor must be MAJOR.MINOR.PATCH, got %s", floor)
	}
	return fsys.WriteFileAtomic(fsys.OSFS{}, minGCVersionPath(cityPath), []byte(floor+"\n"), 0o644)
}

// gcVersionCore is the MAJOR.MINOR.PATCH of a gc version string, or false
// when it has none (a "dev" build).
func gcVersionCore(v string) (string, bool) {
	m := gcVersionCorePattern.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// gcVersionBelowFloor reports whether running is older than floor. A running
// version with no release core is not below anything (see the file comment).
// A pre-release of the floor's own version is not below it.
func gcVersionBelowFloor(running, floor string) bool {
	core, ok := gcVersionCore(running)
	return ok && deps.CompareVersions(core, floor) < 0
}

// minGCVersionRefusal refuses a gc of version running on a city whose floor
// is above it, or whose floor cannot be read.
func minGCVersionRefusal(cityPath, running string) error {
	floor, present, err := readMinGCVersion(cityPath)
	switch {
	case err != nil:
		return fmt.Errorf("this city records a gc version floor that cannot be read (%w), so it is refused as a floor this gc may not meet. Repair the file, or restore it from the split's snapshot", err)
	case !present || !gcVersionBelowFloor(running, floor):
		return nil
	}
	return fmt.Errorf("this city was split by a gc that requires gc %s or later (%s), and this gc is %s, which does not read the split. Upgrade gc on every host and PATH that runs it", floor, minGCVersionPath(cityPath), running)
}

// minGCVersionPathProbe finds the gc an agent session runs and its version.
// A variable so tests do not run a real binary.
var minGCVersionPathProbe = probeSessionPathGCVersion

// minGCVersionProbeTimeout bounds `gc version` on the session PATH.
const minGCVersionProbeTimeout = 5 * time.Second

// probeSessionPathGCVersion finds the gc on the PATH an agent session
// inherits: workspace.env's PATH when the city sets one, else the one the
// supervisor gives its sessions (searchpath.ExpandPath over this process's,
// which adds the user-managed install directories such as ~/.local/bin and
// ~/go/bin). path is "" when no gc is on it.
func probeSessionPathGCVersion(cfg *config.City) (path, version string, err error) {
	home, _ := os.UserHomeDir()
	pathList := searchpath.ExpandPath(home, goruntime.GOOS, os.Getenv("PATH"))
	if cfg != nil {
		if configured, ok := expandEnvMap(cfg.Workspace.Env)["PATH"]; ok {
			pathList = configured
		}
	}
	for _, dir := range filepath.SplitList(pathList) {
		dir = strings.TrimSpace(dir)
		if !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, "gc")
		if runnableGCBinary(candidate) {
			path = candidate
			break
		}
	}
	if path == "" {
		return "", "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), minGCVersionProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return path, "", fmt.Errorf("running %s version: %w", path, err)
	}
	version, _, _ = strings.Cut(strings.TrimSpace(string(out)), "\n")
	return path, strings.TrimSpace(version), nil
}
