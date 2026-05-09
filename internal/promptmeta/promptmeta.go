// Package promptmeta extracts versioning metadata from agent prompt
// templates and computes per-session prompt fingerprints. Introduced by
// issue #1256 (1e) to answer two operator questions:
//
//   - "What version is running?" — answered by FrontMatter.Version, a
//     human-meaningful string declared in the template's frontmatter.
//   - "What exact bytes ran for this bead?" — answered by SHA of the
//     rendered prompt, computed after text/template substitution.
//
// Both fields are propagated through session metadata into WorkerOperation
// payloads (1a) so dashboards and `gc analyze` can group by version and
// spot drift between two runs that claim the same version.
package promptmeta

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// frontMatterDelimiter is the line marker bracketing YAML-style frontmatter
// blocks. We accept the same `---` convention used by Jekyll/Hugo/etc.
const frontMatterDelimiter = "---"

// FrontMatter describes the metadata extracted from a prompt template's
// optional leading frontmatter block. Only well-known fields are surfaced
// as named struct members; all parsed key-value pairs are preserved in Raw
// for forward compatibility.
type FrontMatter struct {
	// Version is the human-meaningful version label for this template
	// (e.g. "v3"). Surfaced as `prompt_version` in WorkerOperation
	// payloads and dashboards.
	Version string
	// Raw contains every key-value pair parsed from the frontmatter, with
	// values stored as their literal trimmed strings. Useful for tooling
	// that wants to read fields not yet promoted to typed members.
	Raw map[string]string
}

// IsZero reports whether fm has no fields set.
func (fm FrontMatter) IsZero() bool {
	return fm.Version == "" && len(fm.Raw) == 0
}

// Parse extracts a frontmatter block from raw if it begins with a `---`
// delimiter on the very first line and contains a closing `---` delimiter
// on its own line. The content between the delimiters is parsed as a flat
// `key: value` list (one pair per line, blank lines and comments allowed).
//
// Parse returns the FrontMatter and the body following the closing
// delimiter (with the trailing newline consumed). If no frontmatter is
// present, returns the zero FrontMatter and the original raw string.
//
// The grammar is intentionally minimal — full YAML is not supported.
// Templates that need richer structure should encode it in template
// helpers, not in frontmatter.
func Parse(raw string) (FrontMatter, string) {
	if !strings.HasPrefix(raw, frontMatterDelimiter) {
		return FrontMatter{}, raw
	}
	// First line must be exactly the delimiter (allow trailing whitespace
	// before the newline so editors that auto-format don't drop us into
	// a no-frontmatter path).
	firstNL := strings.IndexByte(raw, '\n')
	if firstNL < 0 {
		return FrontMatter{}, raw
	}
	first := strings.TrimRight(raw[:firstNL], " \t\r")
	if first != frontMatterDelimiter {
		return FrontMatter{}, raw
	}

	// Find the closing delimiter on its own line.
	rest := raw[firstNL+1:]
	closeIdx := findClosingDelimiter(rest)
	if closeIdx < 0 {
		return FrontMatter{}, raw
	}

	block := rest[:closeIdx]
	bodyStart := closeIdx + len(frontMatterDelimiter)
	if bodyStart < len(rest) && rest[bodyStart] == '\r' {
		bodyStart++
	}
	if bodyStart < len(rest) && rest[bodyStart] == '\n' {
		bodyStart++
	}
	body := rest[bodyStart:]

	fm := parseBlock(block)
	if fm.IsZero() {
		return FrontMatter{}, raw
	}
	return fm, body
}

// findClosingDelimiter returns the byte index in s of the closing
// frontmatter delimiter line, or -1 if none. The delimiter must appear
// at the start of a line and be followed by EOL or EOF.
func findClosingDelimiter(s string) int {
	pos := 0
	for pos < len(s) {
		nl := strings.IndexByte(s[pos:], '\n')
		var line string
		if nl < 0 {
			line = s[pos:]
		} else {
			line = s[pos : pos+nl]
		}
		if strings.TrimRight(line, " \t\r") == frontMatterDelimiter {
			return pos
		}
		if nl < 0 {
			return -1
		}
		pos += nl + 1
	}
	return -1
}

// parseBlock parses the contents between frontmatter delimiters into a
// FrontMatter. Lines are processed top-to-bottom; later occurrences of
// the same key replace earlier ones.
func parseBlock(block string) FrontMatter {
	fm := FrontMatter{Raw: make(map[string]string)}
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		colon := strings.IndexByte(trimmed, ':')
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:colon])
		val := strings.TrimSpace(trimmed[colon+1:])
		val = stripSurroundingQuotes(val)
		if key == "" {
			continue
		}
		fm.Raw[key] = val
		if key == "version" {
			fm.Version = val
		}
	}
	if len(fm.Raw) == 0 {
		fm.Raw = nil
	}
	return fm
}

// stripSurroundingQuotes removes a single matching pair of quotes around s
// without touching mismatched quotes. Both single and double quotes are
// supported. Used so `version: "v3"` and `version: v3` parse identically.
func stripSurroundingQuotes(s string) string {
	if len(s) < 2 {
		return s
	}
	first, last := s[0], s[len(s)-1]
	if first != last {
		return s
	}
	if first == '"' || first == '\'' {
		return s[1 : len(s)-1]
	}
	return s
}

// SHA returns a hex-encoded SHA-256 hash of the rendered prompt content.
// Used as `prompt_sha` to forensically identify the exact bytes that ran
// for a given session, distinguishing two runs that share a prompt_version
// but diverged because of an unbumped template edit.
//
// Returns the empty string when rendered is empty so callers can detect
// "no prompt" (e.g. a session created from inline command, not a template)
// without confusing it with "an empty prompt rendered successfully".
func SHA(rendered string) string {
	if rendered == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(rendered))
	return hex.EncodeToString(sum[:])
}
