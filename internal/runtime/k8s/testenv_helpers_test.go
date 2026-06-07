package k8s

import (
	"testing"
)

// clearDoltAndCityEnv empties the GC_DOLT_* / GC_K8S_DOLT_* / GC_CITY_PATH /
// GC_BIN env vars for the duration of the test so the child scripts spawned
// via runControllerScriptDeploy and runBeadsScript (which inherit the test
// process's env through `os.Environ()`) do not observe leaks from the
// developer's shell. GC_BIN is included because the test constructs its own
// cmd.Env with a fake GC_BIN entry appended after os.Environ(); on Linux,
// getenv() returns the first occurrence, so an inherited real GC_BIN would
// shadow the test's fake binary if not cleared here first.
func clearDoltAndCityEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"GC_DOLT_HOST",
		"GC_DOLT_PORT",
		"GC_K8S_DOLT_HOST",
		"GC_K8S_DOLT_PORT",
		"GC_CITY_PATH",
		"GC_BIN",
	} {
		t.Setenv(name, "")
	}
}
