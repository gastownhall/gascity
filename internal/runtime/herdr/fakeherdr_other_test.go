//go:build !windows

package herdr

import "testing"

// The Go fake herdr is for Windows. Elsewhere the tests run their shell
// scripts, and these are never reached.

func newGoFakeHerdr(t *testing.T, _ fakeHerdrSpec) string {
	t.Helper()
	t.Fatal("the Go fake herdr runs only on Windows")
	return ""
}

func goFakeHerdrFromScript(t *testing.T, _ string) string {
	t.Helper()
	t.Fatal("the Go fake herdr runs only on Windows")
	return ""
}

func rewriteGoFake(t *testing.T, _ *Provider, _, _ string) {
	t.Helper()
	t.Fatal("the Go fake herdr runs only on Windows")
}
