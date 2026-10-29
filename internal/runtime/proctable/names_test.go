package proctable

import (
	"os"
	"path/filepath"
	"reflect"
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

// TestProcessNameSetGCExecutableAlias pins the exact alias behavior: only a
// configured "gc" gains the running executable's name (trimmed like any
// other name), and no other configured name ever does.
func TestProcessNameSetGCExecutableAlias(t *testing.T) {
	const nixArgs = "/nix/store/x-gascity-1.5.0/bin/.gc-wrapped convoy control --serve --follow r/core.control-dispatcher"
	for _, tc := range []struct {
		name  string
		names []string
		self  string
		want  map[string]struct{}
	}{
		{
			name:  "wrapped gc gains its basename",
			names: []string{"gc"},
			self:  ".gc-wrapped",
			want:  nameSet("gc", ".gc-wrapped"),
		},
		{
			name:  "padded gc is trimmed and still aliased",
			names: []string{" gc "},
			self:  ".gc-wrapped",
			want:  nameSet("gc", ".gc-wrapped"),
		},
		{
			name:  "no executable name leaves the set as configured",
			names: []string{"gc"},
			self:  "",
			want:  nameSet("gc"),
		},
		{
			name:  "self already named gc adds nothing",
			names: []string{"gc"},
			self:  "gc",
			want:  nameSet("gc"),
		},
		{
			name:  "unrelated names never gain the executable's name",
			names: []string{"claude"},
			self:  ".gc-wrapped",
			want:  nameSet("claude"),
		},
		{
			name:  "multiple unrelated names stay exact",
			names: []string{"node", "claude"},
			self:  ".gc-wrapped",
			want:  nameSet("node", "claude"),
		},
		{
			name:  "kimi keeps only its existing alias",
			names: []string{"kimi"},
			self:  ".gc-wrapped",
			want:  nameSet("kimi", "kimi-code"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := processNameSet(tc.names, tc.self); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("processNameSet(%q, %q) = %v, want %v", tc.names, tc.self, got, tc.want)
			}
		})
	}

	// The #7551 field case: the wrapped name matches by comm and by argv[0].
	set := processNameSet([]string{"gc"}, ".gc-wrapped")
	if !ProcessMatchesNameSet(".gc-wrapped", nixArgs, set) {
		t.Error(`ProcessMatchesNameSet(comm ".gc-wrapped") = false, want true`)
	}
	if !ProcessMatchesNameSet("sh", nixArgs, set) {
		t.Error(`ProcessMatchesNameSet(argv[0] ".gc-wrapped") = false, want true`)
	}

	// A basename longer than the kernel's 15-byte comm still matches through
	// argv[0], because the start command execs the full $GC_BIN path.
	const long = "gascity-1.5.0-linux-amd64" // 25 bytes
	set = processNameSet([]string{"gc"}, long)
	if ProcessMatchesNameSet(long[:15], "sh -c echo hi", set) {
		t.Error("truncated comm with non-gc args matched, want false")
	}
	if !ProcessMatchesNameSet(long[:15], "/opt/"+long+" convoy control --serve --follow r/core.control-dispatcher", set) {
		t.Error("full argv[0] path did not match, want true")
	}

	// An unrelated process never matches, aliased or not.
	set = processNameSet([]string{"gc"}, ".gc-wrapped")
	if ProcessMatchesNameSet("sleep", "sleep 60", set) {
		t.Error(`ProcessMatchesNameSet(comm "sleep") = true, want false`)
	}
}

func nameSet(names ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}
