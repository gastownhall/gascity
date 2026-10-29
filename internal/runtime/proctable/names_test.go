package proctable

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// The configured name "gc" accepts the running gc executable's own basename:
// sessions exec "$GC_BIN", which is os.Executable(). The test binary plays the
// gc binary here.
func TestProcessNameSetAcceptsRunningGCExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	self := filepath.Base(exe)
	if self == "gc" {
		t.Fatalf("test executable is named %q; the alias case is vacuous", self)
	}
	set := ProcessNameSet([]string{"gc"})
	for _, name := range []string{"gc", self} {
		if _, ok := set[name]; !ok {
			t.Errorf("ProcessNameSet([gc]) = %v, missing %q", set, name)
		}
	}
	args := exe + " convoy control --serve --follow r/core.control-dispatcher"
	if !ProcessMatchesNameSet(self, args, set) {
		t.Errorf("ProcessMatchesNameSet(comm %q) = false, want true", self)
	}
	if !ProcessMatchesNameSet("sh", args, set) {
		t.Errorf("ProcessMatchesNameSet(argv[0] %q) = false, want true", exe)
	}
}

func TestProcessNameSetGCExecutableAlias(t *testing.T) {
	const nixExe = "/nix/store/x-gascity-1.5.0/bin/.gc-wrapped"
	const followArgs = " convoy control --serve --follow r/core.control-dispatcher"
	type match struct {
		command, args string
		want          bool
	}
	for _, tc := range []struct {
		name    string
		names   []string
		self    string
		want    []string
		matches []match
	}{
		{
			// The #7551 field case: Nix wrapProgram runs gc as .gc-wrapped.
			name:  "nix wrapped",
			names: []string{"gc"},
			self:  ".gc-wrapped",
			want:  []string{"gc", ".gc-wrapped"},
			matches: []match{
				{".gc-wrapped", ".gc-wrapped", true},
				{"sh", nixExe + followArgs, true},
				{"sleep", "sleep 60", false},
			},
		},
		{
			name:  "trimmed name",
			names: []string{" gc "},
			self:  ".gc-wrapped",
			want:  []string{"gc", ".gc-wrapped"},
		},
		{
			// A basename past the kernel's 15-byte comm matches through argv[0].
			name:  "long executable name",
			names: []string{"gc"},
			self:  "gascity-1.5.0-linux-amd64",
			want:  []string{"gc", "gascity-1.5.0-linux-amd64"},
			matches: []match{
				{"gascity-1.5.0-l", "sleep 60", false},
				{"gascity-1.5.0-l", "/opt/bin/gascity-1.5.0-linux-amd64" + followArgs, true},
			},
		},
		{
			name:  "executable name unknown",
			names: []string{"gc"},
			self:  "",
			want:  []string{"gc"},
		},
		{
			name:  "executable already named gc",
			names: []string{"gc"},
			self:  "gc",
			want:  []string{"gc"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := processNameSet(tc.names, tc.self)
			if got := setNames(set); !reflect.DeepEqual(got, sortedNames(tc.want)) {
				t.Fatalf("processNameSet(%q, %q) = %v, want %v", tc.names, tc.self, got, sortedNames(tc.want))
			}
			for _, m := range tc.matches {
				if got := ProcessMatchesNameSet(m.command, m.args, set); got != m.want {
					t.Errorf("ProcessMatchesNameSet(%q, %q) = %v, want %v", m.command, m.args, got, m.want)
				}
			}
		})
	}
}

func setNames(set map[string]struct{}) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	return sortedNames(names)
}

func sortedNames(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}
