//go:build integration

package bdflags

import (
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// flagNameRE matches the flag-declaration prefix of a cobra --help flag
// line, e.g. "  -a, --assignee string   Assignee" or
// "      --claim                Atomically claim...". It is anchored to
// the start of the line so mentions of "--flag" inside another flag's
// description text (which always follow on the same line, never at the
// start of one) are not mistaken for a declaration.
var flagNameRE = regexp.MustCompile(`(?m)^\s*(?:-([A-Za-z0-9]), )?--([A-Za-z0-9][A-Za-z0-9-]*)`)

// parseHelpFlagNames extracts every long (--flag) and short (-f) flag name
// declared in a bd --help transcript. The "Flags:" and "Global Flags:"
// sections are both plain flag-declaration lines and are matched the same
// way, so the result already includes global flags alongside the
// subcommand's own.
func parseHelpFlagNames(help string) map[string]bool {
	names := make(map[string]bool)
	for _, line := range strings.Split(help, "\n") {
		m := flagNameRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[1] != "" {
			names["-"+m[1]] = true
		}
		names["--"+m[2]] = true
	}
	return names
}

// TestBdFlagManifestCurrent guards against the bd CLI growing a flag that
// this package's hardcoded manifest doesn't know about. It shells the real
// installed bd binary's --help output per known subcommand and fails
// loudly — fail-closed, the same posture as bdMutationWriteIDs in
// cmd/gc/cmd_bd.go — if the live CLI declares a flag the manifest is
// MISSING.
//
// It deliberately does not fail on the reverse (the manifest listing flags
// the installed bd lacks). The manifest is intentionally the newest-known
// superset of bd's flags — see its dated-provenance comment — and its two
// consumers, the `gc lint` bd-flag check (scan.go) and the cmd_bd
// write-mutation ID guard, only misbehave when a real flag is missing from
// the manifest, never when the manifest is ahead of the installed bd. The
// bd binary is version-pinned independently of the manifest's provenance:
// CI installs the stable bd release via BD_VERSION while the manifest
// tracks the newer bd the fleet runs, so a manifest ahead of the installed
// bd is expected and benign. That skew is reported, not failed.
//
// If bd is not in PATH, the test is skipped with a clear message rather
// than failing, since manifest currency can't be checked without a bd
// binary to check it against.
func TestBdFlagManifestCurrent(t *testing.T) {
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		t.Skip("bd not found in PATH; skipping flag-manifest freshness check")
	}

	for _, sub := range Subcommands() {
		t.Run(sub, func(t *testing.T) {
			args := append(strings.Fields(sub), "--help")
			out, _ := exec.Command(bdPath, args...).CombinedOutput()
			live := parseHelpFlagNames(string(out))
			if len(live) == 0 {
				t.Fatalf("parsed zero flags from `bd %s --help`; output format may have changed:\n%s", sub, out)
			}

			manifest := mergeFlagSets(ValueFlags(sub), BoolFlags(sub))

			var missingFromManifest, aheadOfInstalled []string
			for f := range live {
				if !manifest[f] {
					missingFromManifest = append(missingFromManifest, f)
				}
			}
			for f := range manifest {
				if !live[f] {
					aheadOfInstalled = append(aheadOfInstalled, f)
				}
			}
			sort.Strings(missingFromManifest)
			sort.Strings(aheadOfInstalled)

			// Benign direction: the manifest lists flags this bd binary does
			// not have. Expected whenever the installed bd is older than the
			// manifest's provenance version (e.g. CI's pinned stable bd). A
			// superset allowlist is safe for both consumers, so report it
			// without failing.
			if len(aheadOfInstalled) > 0 {
				t.Logf("bd %s: manifest lists %d flag(s) absent from this bd's --help: %v (manifest is ahead of the installed bd; benign for a superset allowlist)",
					sub, len(aheadOfInstalled), aheadOfInstalled)
			}

			// Dangerous direction: the live bd declares a flag the manifest
			// does not know. gc lint would then false-positive on valid
			// templates and the write-mutation ID guard could misparse the
			// bead ID. Fail closed and require the manifest be regenerated.
			if len(missingFromManifest) > 0 {
				t.Errorf("bd %s flag manifest is missing flag(s) present in `bd %s --help`: %v\nThe manifest must be a superset of the installed bd's real flags. Update internal/bdflags/bdflags.go with a fresh dated-provenance comment.",
					sub, sub, missingFromManifest)
			}
		})
	}
}
