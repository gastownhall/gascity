package overlay

import (
	"bytes"
	"embed"
	"strings"
)

// KiroAgentsMDPath is the work-dir-relative path of the Kiro instruction
// fallback the core pack ships at overlay/per-provider/kiro/AGENTS.md.
const KiroAgentsMDPath = "AGENTS.md"

// releasedKiroAgentsMD holds, byte for byte, every body of the core Kiro
// fallback AGENTS.md that a gc release wrote into work dirs before
// gastownhall/gascity#8970. All of them tell the agent that plain `gc hook`
// claims work; it only lists it, so an agent that follows them works beads it
// never claimed. Each file is named for the first release that shipped it:
//
//   - v1.2.0: blob 75afa650b7 (#1369)
//   - v1.3.0: blob 4b0148068c (#3045)
//   - v1.4.0: blob 645346eeb1 (#4199), shipped through v1.5.1
//
// Never edit these files: they are matched exactly, and a test pins each one
// to its released git blob id.
//
//go:embed released/kiro-AGENTS-*.txt
var releasedKiroAgentsMD embed.FS

// IsReleasedKiroAgentsMD reports whether existing is exactly one of the
// released Kiro fallback AGENTS.md bodies that described plain `gc hook` as
// claiming work. Only such a file may be replaced by a newer core overlay;
// any other content — a repository's own AGENTS.md, an edited copy, a body
// with different line endings or a missing final newline — is user-owned and
// must be kept.
func IsReleasedKiroAgentsMD(existing []byte) bool {
	for _, body := range ReleasedKiroAgentsMDBodies() {
		if bytes.Equal(existing, body) {
			return true
		}
	}
	return false
}

// ReleasedKiroAgentsMDBodies returns copies of the released Kiro fallback
// AGENTS.md bodies IsReleasedKiroAgentsMD matches, keyed by the first gc
// release that shipped each one.
func ReleasedKiroAgentsMDBodies() map[string][]byte {
	entries, err := releasedKiroAgentsMD.ReadDir("released")
	if err != nil {
		return nil
	}
	out := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		data, err := releasedKiroAgentsMD.ReadFile("released/" + entry.Name())
		if err != nil {
			continue
		}
		version := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "kiro-AGENTS-"), ".txt")
		out[version] = data
	}
	return out
}
