//go:build windows

package session

import (
	"os"
	"path/filepath"
	"testing"
)

// writeChildEnvSpy writes a program that records three environment variables,
// one per line, into the file named by GC_TEST_PRODUCT_METRICS_CHILD_ENV_SPY.
// It is written through a temp file and renamed so a reader never observes a
// partial snapshot.
//
// A .cmd script is used because Windows has no shebang: a file of shell text is
// not executable, so CreateProcess cannot start the Unix spy. `echo(` is the
// idiom that emits an empty line for an unset variable instead of printing
// "ECHO is on", which keeps an absent value distinguishable from a set one.
func writeChildEnvSpy(t *testing.T, dir string) string {
	t.Helper()
	spy := filepath.Join(dir, "gc-child-spy.cmd")
	script := "@echo off\r\n" +
		"setlocal\r\n" +
		"set \"snapshot=%GC_TEST_PRODUCT_METRICS_CHILD_ENV_SPY%\"\r\n" +
		"(\r\n" +
		"echo(%GC_DISABLE_USAGE_METRICS%\r\n" +
		"echo(%BD_DISABLE_METRICS%\r\n" +
		"echo(%OTEL_SERVICE_NAME%\r\n" +
		") > \"%snapshot%.tmp\"\r\n" +
		"move /y \"%snapshot%.tmp\" \"%snapshot%\" >nul\r\n"
	if err := os.WriteFile(spy, []byte(script), 0o700); err != nil {
		t.Fatalf("write child spy: %v", err)
	}
	return spy
}
