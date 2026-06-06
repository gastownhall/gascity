package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	testGCBinaryDirPrefix        = "gc-test-binary-pid"
	testCmdGCTempRootPrefix      = "gct"
	testCmdGCShardTempRootPrefix = "gcx"
	testShardIndexEnv            = "GC_TEST_SHARD_INDEX"
	testShardTotalEnv            = "GC_TEST_SHARD_TOTAL"
	testActiveTempRootMarker     = ".gc-test-active-root"
	testSharedFixtureDirPrefix   = "gascity-gc-test-fixtures-pid"
	testSlingFormulaDirPrefix    = "gc-sling-test-formulas-pid"
	testSlingCityDirPrefix       = "gc-sling-test-city-pid"
	testGCHomeDirPrefix          = "gascity-gc-home-pid"
	testRuntimeDirPrefix         = "gascity-runtime-pid"
	testProviderStubDirPrefix    = "gascity-provider-stubs-pid"
)

func pidPrefixedTempPattern(prefix string) string {
	return prefix + strconv.Itoa(os.Getpid()) + "-*"
}

func cmdGCTestTempRootPrefix() string {
	if strings.TrimSpace(os.Getenv(testShardIndexEnv)) != "" || strings.TrimSpace(os.Getenv(testShardTotalEnv)) != "" {
		return testCmdGCShardTempRootPrefix
	}
	return testCmdGCTempRootPrefix
}

func pidFromPrefixedDirName(name, prefix string) (int, bool) {
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	suffix := strings.TrimPrefix(name, prefix)
	end := 0
	for end < len(suffix) && suffix[end] >= '0' && suffix[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	if end < len(suffix) && suffix[end] != '-' {
		return 0, false
	}
	pid, err := strconv.Atoi(suffix[:end])
	if err != nil {
		return 0, false
	}
	return pid, true
}

// sweepOrphanPIDPrefixedDirs removes <root>/<prefix><PID> dirs whose PID
// is no longer alive, including MkdirTemp names such as <prefix><PID>-<random>.
// Best-effort; ignores errors. Used by test setup to clean leftover test
// fixtures from prior crashed/SIGKILL'd runs.
func sweepOrphanPIDPrefixedDirs(root, prefix string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	self := os.Getpid()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, ok := pidFromPrefixedDirName(e.Name(), prefix)
		if !ok || pid <= 0 || pid == self {
			continue
		}
		if pidAlive(pid) {
			continue
		}
		path := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(path, testActiveTempRootMarker)); err == nil {
			continue
		}
		_ = os.RemoveAll(path)
	}
}
