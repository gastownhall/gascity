package proctable

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProcessNameSetAcceptsRunningGCExecutable pins #7551: sessions exec the
// gc binary as "$GC_BIN" (os.Executable), which under a Nix wrapProgram
// install is not named gc. A configured process name "gc" must also accept
// the running executable's own basename, so a live dispatcher is never read
// runtime-missing.
func TestProcessNameSetAcceptsRunningGCExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v, want the running test binary's path", err)
	}
	self := filepath.Base(exe)
	if self == "gc" {
		t.Fatalf("running executable is named %q; the alias under test is a no-op here", self)
	}
	set := ProcessNameSet([]string{"gc"})
	if _, ok := set["gc"]; !ok {
		t.Errorf(`ProcessNameSet(["gc"]) lost the configured name "gc"`)
	}
	if _, ok := set[self]; !ok {
		t.Errorf("ProcessNameSet([\"gc\"]) = %v, want it to also accept the running executable's name %q", set, self)
	}
	args := exe + " convoy control --serve --follow r/core.control-dispatcher"
	if !ProcessMatchesNameSet(self, args, set) {
		t.Errorf("ProcessMatchesNameSet(comm %q) = false, want true", self)
	}
	if !ProcessMatchesNameSet("sh", args, set) {
		t.Errorf("ProcessMatchesNameSet(comm \"sh\", argv[0] %q) = false, want true", exe)
	}
}
