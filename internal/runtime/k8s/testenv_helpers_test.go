package k8s

import (
	"testing"
)

// clearDoltAndCityEnv empties the GC_DOLT_* / GC_K8S_DOLT_* / GC_CITY_PATH env
// vars for the duration of the test so the child scripts spawned via
// runControllerScriptDeploy and runBeadsScript (which inherit the test
// process's env through `os.Environ()`) do not observe a GC_DOLT_* leak from
// the developer's shell. Each test's opts.Env continues to declare its own
// desired state, which overrides the emptied values when cmd.Env is flattened.
//
// Shell scripts read these vars via `${VAR:-…}` / `[ -n "$VAR" ]` patterns, so
// an empty string is treated the same as unset — good enough to make the tests
// deterministic without needing a raw os.Unsetenv + manual cleanup.
func clearDoltAndCityEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"GC_DOLT_HOST",
		"GC_DOLT_PORT",
		"GC_K8S_DOLT_HOST",
		"GC_K8S_DOLT_PORT",
		"GC_CITY_PATH",
	} {
		t.Setenv(name, "")
	}
}
