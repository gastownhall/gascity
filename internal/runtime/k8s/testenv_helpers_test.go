package k8s

import (
	"testing"
)

// clearDoltAndCityEnv empties the GC_DOLT_* / GC_K8S_DOLT_* / GC_CITY_PATH /
// GC_BIN / GC_STORE_ROOT env vars for the duration of the test so the child
// scripts spawned via runControllerScriptDeploy and runBeadsScript (which
// inherit the test process's env through `os.Environ()`) do not observe leaks
// from the developer's shell or an enclosing gc city. Each test's opts.Env
// continues to declare its own desired state, which overrides the emptied
// values when cmd.Env is flattened.
//
// GC_BIN is included because the test constructs its own cmd.Env with a fake
// GC_BIN entry appended after os.Environ(); on Linux, getenv() returns the
// first occurrence, so an inherited real GC_BIN would shadow the test's fake
// binary if not cleared here first.
//
// GC_STORE_ROOT in particular leaks whenever the suite runs inside a gc city
// (CI, agent worktrees), where it points at the enclosing city root. Tests
// that exercise the "store root unset" fall-back must neutralize it here, since
// they deliberately leave GC_STORE_ROOT out of their own opts.Env and so have
// nothing to override the leaked value.
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
		"GC_BIN",
		"GC_STORE_ROOT",
	} {
		t.Setenv(name, "")
	}
}
