package core

import (
	"bytes"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"

	bdpack "github.com/gastownhall/gascity/examples/bd"
	"github.com/gastownhall/gascity/internal/orders"
)

// beadIDToken is the core shape of a bead reference that run-extraction
// tokenizers look for in agent transcripts: a short lowercase prefix, a '-', a
// lowercase base36 id, and optional numeric child segments. The boundary
// classes are applied as neighbor checks (see findBeadIDLiterals), not inside
// the regexp: RE2 returns only non-overlapping matches, so a boundary byte
// inside the match would be consumed, and a token the accept rule later
// discards ("mol-polecat") would swallow the delimiter in front of the id that
// follows it ("mol-polecat ga-abc1").
var beadIDToken = regexp.MustCompile(`[a-z][a-z0-9]{0,9}-[0-9a-z]{3,32}(?:\.[0-9]{1,4})*`)

// declaredBeadPrefixes are the project's own bead prefixes: bd is the beads
// default, ga is this project's tracker. Extractors that accept short ids for
// known prefixes take any id of three or more characters under them, digits or
// not, so placeholder-looking text such as "ga-xxxx" is a bead reference too.
// gc is deliberately absent: gc-agents, gc-work and similar are prose.
var declaredBeadPrefixes = map[string]bool{"bd": true, "ga": true}

// findBeadIDLiterals returns the bead ids in body. Under a declared prefix any
// id of three or more characters counts. Under any other prefix the id needs a
// base of at least four characters and a digit in the id or a child segment
// (the accept-any rule), which keeps hyphenated prose such as "mol-polecat",
// "wisp-compact" or "sha-256" out.
func findBeadIDLiterals(body string) []string {
	var found []string
	for _, loc := range beadIDToken.FindAllStringIndex(body, -1) {
		i, j := loc[0], loc[1]
		if i > 0 && isBeadLeadExcluded(body[i-1]) {
			continue
		}
		if j < len(body) && isBeadTrailExcluded(body[j]) {
			continue
		}
		token := body[i:j]
		dash := strings.IndexByte(token, '-')
		prefix, id := token[:dash], token[dash+1:]
		base, _, _ := strings.Cut(id, ".")
		if declaredBeadPrefixes[prefix] || (len(base) >= 4 && strings.ContainsAny(id, "0123456789")) {
			found = append(found, token)
		}
	}
	return found
}

// isBeadLeadExcluded reports whether c, the byte before a candidate, makes it
// part of a longer word, path, dotted name or hyphenated token.
func isBeadLeadExcluded(c byte) bool {
	return isASCIIAlnum(c) || c == '_' || c == '/' || c == '.' || c == '-'
}

// isBeadTrailExcluded reports whether c, the byte after a candidate, makes it
// part of a longer token. '.' and '/' may follow an id (end of a sentence, a
// path separator).
func isBeadTrailExcluded(c byte) bool {
	return isASCIIAlnum(c) || c == '_' || c == '-'
}

func isASCIIAlnum(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// isShippedPromptText reports whether a pack file is text an agent reads:
// markdown prompts, fragments and skills, and formula steps. Command help
// (commands/*/help.md) is operator-facing CLI output, not prompt text.
func isShippedPromptText(p string) bool {
	if path.Ext(p) == ".md" {
		return !strings.HasPrefix(p, "commands/") && !strings.Contains(p, "/commands/")
	}
	return path.Ext(p) == ".toml" && path.Base(path.Dir(p)) == "formulas"
}

// isOrderFile reports whether p is an order definition. Its description
// becomes the text of the wisp an agent picks up.
func isOrderFile(p string) bool {
	return path.Ext(p) == ".toml" && path.Base(path.Dir(p)) == "orders"
}

// assertPackCarriesNoBeadIDs fails for every literal bead id in the
// agent-facing text of pack.
func assertPackCarriesNoBeadIDs(t *testing.T, name string, pack fs.FS) {
	t.Helper()
	report := func(p string, line int, text string) {
		for _, id := range findBeadIDLiterals(text) {
			t.Errorf("%s %s:%d: shipped agent text contains the literal bead id %q; use a placeholder like <bead-id>", name, p, line, id)
		}
	}
	err := fs.WalkDir(pack, ".", func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		switch {
		case isShippedPromptText(p):
			data, err := fs.ReadFile(pack, p)
			if err != nil {
				return err
			}
			for i, line := range bytes.Split(data, []byte{'\n'}) {
				report(p, i+1, string(line))
			}
		case isOrderFile(p):
			data, err := fs.ReadFile(pack, p)
			if err != nil {
				return err
			}
			o, err := orders.Parse(data)
			if err != nil {
				t.Errorf("%s %s: parsing order: %v", name, p, err)
				return nil
			}
			report(p, 0, o.Description)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking embedded %s pack: %v", name, err)
	}
}

// TestShippedPromptsCarryNoBeadIDs keeps literal bead ids out of the text the
// bundled packs ship to agents. Agents echo prompt prose into their
// transcripts, so a literal id (the claim-protocol fragment once cited a
// historical incident bead) turns into a bead reference in every run of every
// city that composes the prompt, and run extraction then tries to resolve an
// id that does not exist in that city's store. Use a placeholder such as
// <bead-id>, or describe the incident without its id. The bd pack ships the
// dog prompt and the dolt formulas and orders, so it is held to the same rule.
func TestShippedPromptsCarryNoBeadIDs(t *testing.T) {
	assertPackCarriesNoBeadIDs(t, "core", PackFS)
	assertPackCarriesNoBeadIDs(t, "bd", bdpack.PackFS)
}

func TestFindBeadIDLiterals(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{name: "incident id in prose", body: "went 41 hours without running (ga-tmzjx6). Claiming", want: []string{"ga-tmzjx6"}},
		{name: "child id", body: "see bd-a1b2.3 for details", want: []string{"bd-a1b2.3"}},
		{name: "placeholder", body: "gc bd show <bead-id>"},
		{name: "hyphenated prose", body: "mol-polecat rig-scoped gc-agents wisp-compact sha-256 utf-8"},
		{name: "id after hyphenated token", body: "mol-polecat ga-abc1", want: []string{"ga-abc1"}},
		{name: "id after hyphenated word in prose", body: "see wisp-compact ga-tmzjx6 now", want: []string{"ga-tmzjx6"}},
		{name: "adjacent ids", body: "ga-abc1 ga-abc2", want: []string{"ga-abc1", "ga-abc2"}},
		{name: "overlong child segment", body: "ga-a1b2.12345"},
		{name: "child segment run into letters", body: "ga-a1b2.1x"},
		{name: "declared prefix all-letter placeholder", body: "fix: stuff (ga-xxxx)", want: []string{"ga-xxxx"}},
		{name: "declared prefix short id", body: "gc bd show ga-123", want: []string{"ga-123"}},
		{name: "declared prefix too short", body: "ga-ab bd-x"},
		{name: "undeclared prefix short id", body: "gc bd show xy-123"},
		{name: "undeclared prefix all-letter id", body: "well-known read-only"},
		{name: "gc is not declared", body: "gc-work gc-agents"},
		{name: "uppercase", body: "GA-ABC1 ga-ABC12"},
		{name: "path segment", body: "/tmp/ga-abc12/file"},
		{name: "flag", body: "--only-db1234"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findBeadIDLiterals(tt.body)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("findBeadIDLiterals(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}
