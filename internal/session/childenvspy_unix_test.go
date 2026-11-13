//go:build unix

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
func writeChildEnvSpy(t *testing.T, dir string) string {
	t.Helper()
	spy := filepath.Join(dir, "gc-child-spy")
	script := "#!/bin/sh\n" +
		"snapshot=\"$GC_TEST_PRODUCT_METRICS_CHILD_ENV_SPY\"\n" +
		"tmp=\"${snapshot}.tmp.$$\"\n" +
		"printf '%s\\n' \"$GC_DISABLE_USAGE_METRICS\" \"$BD_DISABLE_METRICS\" \"$OTEL_SERVICE_NAME\" > \"$tmp\"\n" +
		"mv -f \"$tmp\" \"$snapshot\"\n"
	if err := os.WriteFile(spy, []byte(script), 0o700); err != nil {
		t.Fatalf("write child spy: %v", err)
	}
	return spy
}
