package core

import (
	"bytes"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

// beadIDCandidate mirrors the accept-any shape of a bead reference that
// run-extraction tokenizers look for in agent transcripts: a short lowercase
// prefix, a '-', a base36 id of at least four characters, and optional numeric
// child segments. findBeadIDLiterals adds the digit rule (the id or a child
// segment carries a digit), which keeps hyphenated prose such as
// "mol-polecat" or "gc-agents" out.
var beadIDCandidate = regexp.MustCompile(`(?:^|[^A-Za-z0-9_./-])([a-z][a-z0-9]{0,9}-[0-9a-z]{4,32}(?:\.[0-9]{1,4})*)(?:$|[^A-Za-z0-9_-])`)

func findBeadIDLiterals(body string) []string {
	var found []string
	for _, match := range beadIDCandidate.FindAllStringSubmatch(body, -1) {
		token := match[1]
		id := token[strings.IndexByte(token, '-')+1:]
		if strings.ContainsAny(id, "0123456789") {
			found = append(found, token)
		}
	}
	return found
}

// isShippedPromptText reports whether a core pack file is text an agent
// reads: markdown prompts, fragments and skills, and formula steps.
func isShippedPromptText(p string) bool {
	return path.Ext(p) == ".md" || (strings.HasPrefix(p, "formulas/") && path.Ext(p) == ".toml")
}

// TestCoreShippedPromptsCarryNoBeadIDs keeps literal bead ids out of the text
// the core pack ships to agents. Agents echo prompt prose into their
// transcripts, so a literal id (the claim-protocol fragment once cited a
// historical incident bead) turns into a bead reference in every run of every
// city that composes the prompt, and run extraction then tries to resolve an
// id that does not exist in that city's store. Use a placeholder such as
// <bead-id>, or describe the incident without its id.
func TestCoreShippedPromptsCarryNoBeadIDs(t *testing.T) {
	err := fs.WalkDir(PackFS, ".", func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !isShippedPromptText(p) {
			return nil
		}
		data, err := fs.ReadFile(PackFS, p)
		if err != nil {
			return err
		}
		for i, line := range bytes.Split(data, []byte{'\n'}) {
			for _, id := range findBeadIDLiterals(string(line)) {
				t.Errorf("%s:%d: shipped prompt text contains the literal bead id %q; use a placeholder like <bead-id>", p, i+1, id)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking embedded core pack: %v", err)
	}
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
		{name: "hyphenated prose", body: "mol-polecat rig-scoped gc-agents wisp-compact"},
		{name: "short example id", body: "gc bd show ga-123"},
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
