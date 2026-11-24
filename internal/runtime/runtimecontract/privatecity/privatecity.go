// Package privatecity makes the throwaway cities the runtime-pack suites
// (gc runtime check, gc runtime conformance and its --env suite) run a
// pack's ops in: gc always names a city on an op (GC_EXEC_CITY_NAME and
// GC_EXEC_CITY_PATH), so a suite run outside one, and a probe that must not
// touch the city it runs in, makes a city of its own.
package privatecity

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/execenv"
)

// PathEnv names the city path gc sets on every op it runs.
const PathEnv = "GC_EXEC_CITY_PATH"

// Inherited reports whether the caller runs in a city: GC_EXEC_CITY_PATH is
// set and not empty.
func Inherited() bool {
	return execenv.Getenv(PathEnv) != ""
}

// New returns the env of a city no live city uses: GC_EXEC_CITY_NAME and
// GC_EXEC_CITY_PATH name a fresh temp dir whose name starts with prefix,
// which remove deletes.
func New(prefix string) (env []string, remove func(), err error) {
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return nil, nil, fmt.Errorf("creating a private city: %w", err)
	}
	env = []string{"GC_EXEC_CITY_NAME=" + filepath.Base(dir), PathEnv + "=" + dir}
	return env, func() { _ = os.RemoveAll(dir) }, nil
}

// Path returns the GC_EXEC_CITY_PATH a city env from New names, empty for
// none.
func Path(env []string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], PathEnv+"="); ok {
			return v
		}
	}
	return ""
}

// ServerStopWarning is the warning a suite reports when the server-stop it
// sends a city before removing it does not exit 0 (err), as gc warns when a
// city's server teardown fails: the server it brought up there may outlive
// the run, keyed to a city path that no longer exists.
func ServerStopWarning(city []string, err error) string {
	return fmt.Sprintf("server-stop in private city %s before removing it: %v; a server the run brought up there may still be running", Path(city), err)
}
