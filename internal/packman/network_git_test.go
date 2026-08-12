package packman

import "testing"

// routeNetworkGitThroughRunGit points the runNetworkGit seam at the live runGit
// stub for the duration of a test, discarding the leading cityRoot/remoteURL
// arguments. Tests that fake clone/ls-remote inside their runGit stub call this
// so those network operations — which production now routes through
// runNetworkGit — reach the same fake. The delegate reads runGit at call time,
// so it tracks a stub installed after this helper runs.
func routeNetworkGitThroughRunGit(t *testing.T) {
	t.Helper()
	prev := runNetworkGit
	runNetworkGit = func(_, _, dir string, args ...string) (string, error) {
		return runGit(dir, args...)
	}
	t.Cleanup(func() { runNetworkGit = prev })
}
