package integration

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// beadIDERE is the Go twin of BEAD_ID_ERE in test/agents/lib/bead-id.sh: a
// whole-token bead ID matched by shape, never by a hardcoded prefix. Since
// #5416 a bead store's minted prefix is store-configured or derived from the
// city name, and the integration bd shim can mint gc-N into the same store
// where gc mints <prefix>-N, so neither side may assume bd/gc/mc.
const beadIDERE = `[A-Za-z][A-Za-z0-9]*(-[A-Za-z0-9]+)+([.][A-Za-z0-9]+)*`

var beadIDTokenRE = regexp.MustCompile("^" + beadIDERE + "$")

// isBeadIDToken reports whether s is, in its entirety, shaped like a bead ID.
func isBeadIDToken(s string) bool {
	return beadIDTokenRE.MatchString(s)
}

// parseBeadID extracts a bead ID from bd/gc output by shape, never by
// scanning free text for the shape regex as a substring — preambles contain
// shape-identical words such as "re-initializing".
func parseBeadID(output string) (string, bool) {
	for _, anchor := range []string{"Created bead:", "Created issue:", "Created convoy"} {
		idx := strings.Index(output, anchor)
		if idx < 0 {
			continue
		}
		fields := strings.Fields(output[idx+len(anchor):])
		if len(fields) == 0 {
			continue
		}
		if candidate := strings.TrimRight(fields[0], ":,"); isBeadIDToken(candidate) {
			return candidate, true
		}
	}

	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if isBeadIDToken(fields[0]) {
			return fields[0], true
		}
	}

	return "", false
}

func TestIsBeadIDToken(t *testing.T) {
	valid := []string{
		"gc-1", "th-16", "g9-7", "bl-2", "r0-1", "BL-42",
		"ga-4w6d2r", "ga-t832q4.2", "gm-wisp-vcj7zd", "my-rig-12",
	}
	for _, s := range valid {
		if !isBeadIDToken(s) {
			t.Errorf("isBeadIDToken(%q) = false, want true", s)
		}
	}

	invalid := []string{
		"", "ID", "No", "FROM", "gc-", "-gc-1", "1gc-2", "gc_1", "gc--1",
		"ga-t832q4.", "gc-1 x",
	}
	for _, s := range invalid {
		if isBeadIDToken(s) {
			t.Errorf("isBeadIDToken(%q) = true, want false", s)
		}
	}
}

func TestParseBeadID(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
		wantOK bool
	}{
		{"shim created bead", "Created bead: gc-3", "gc-3", true},
		{"real bd created issue", "✓ Created issue: qwerty-a1b — standalone bead", "qwerty-a1b", true},
		{"multirig fake bd created issue", "Created issue: r0-1", "r0-1", true},
		{"created convoy", `Created convoy uw-19 "Sprint 1" tracking 2 issue(s)`, "uw-19", true},
		{"mail inbox table row", "ID     FROM   SUBJECT     BODY\nth-16  human  archive me  archive me", "th-16", true},
		{"no unread messages", "No unread messages for human", "", false},
		{"db reinit warning", "warning: database 'hq' missing bd schema; re-initializing", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseBeadID(tt.output)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("parseBeadID(%q) = (%q, %v), want (%q, %v)", tt.output, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestAgentScriptsShareBeadIDMatcher pins test/agents/lib/bead-id.sh's
// BEAD_ID_ERE to beadIDERE, and fails any agent script that still filters
// bead IDs by a hardcoded bd/gc/mc prefix instead of sourcing that lib.
func TestAgentScriptsShareBeadIDMatcher(t *testing.T) {
	libPath := filepath.Join("..", "agents", "lib", "bead-id.sh")
	lib, err := os.ReadFile(libPath)
	if err != nil {
		t.Fatalf("reading %s: %v", libPath, err)
	}

	assign := regexp.MustCompile(`BEAD_ID_ERE='([^']*)'`).FindSubmatch(lib)
	if assign == nil {
		t.Fatalf("%s: no BEAD_ID_ERE='...' assignment found", libPath)
	}
	if got := string(assign[1]); got != beadIDERE {
		t.Errorf("%s: BEAD_ID_ERE = %q, want %q to match beadIDERE in bead_id_test.go", libPath, got, beadIDERE)
	}

	scripts, err := filepath.Glob(filepath.Join("..", "agents", "*.sh"))
	if err != nil {
		t.Fatalf("globbing ../agents/*.sh: %v", err)
	}
	libScripts, err := filepath.Glob(filepath.Join("..", "agents", "lib", "*.sh"))
	if err != nil {
		t.Fatalf("globbing ../agents/lib/*.sh: %v", err)
	}

	banned := regexp.MustCompile(`\^(?:gc|bd|mc)-`)
	for _, path := range append(scripts, libScripts...) {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for i, line := range strings.Split(string(contents), "\n") {
			if banned.MatchString(line) {
				t.Errorf("%s:%d: hardcoded bead-ID prefix filter %q — source lib/bead-id.sh and use bead_id_rows instead", path, i+1, strings.TrimSpace(line))
			}
		}
	}
}
