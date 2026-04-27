package core

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// goTplRe matches Go text/template action openers: "{{ " or "{{-".
// Formula-style tokens like "{{binary}}" never contain a space after
// the opening braces, so this cleanly separates the two systems.
var goTplRe = regexp.MustCompile(`\{\{\s|\{\{-`)

// TestNoGoTemplateSyntaxInSkillFiles walks all SKILL.md files in the
// embedded core pack and rejects any that contain Go template syntax.
//
// SKILL.md files use formula-style substitution ({{binary}}, {{word}})
// which is a different system from Go text/template. Mixing the two
// causes silent runtime failures. This test enforces file-level
// separation between the two substitution systems.
func TestNoGoTemplateSyntaxInSkillFiles(t *testing.T) {
	type violation struct {
		file string
		line int
		text string
	}
	var violations []violation

	err := fs.WalkDir(PackFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, "/SKILL.md") && path != "SKILL.md" {
			return nil
		}

		data, err := fs.ReadFile(PackFS, path)
		if err != nil {
			t.Errorf("reading %s: %v", path, err)
			return nil
		}

		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if goTplRe.MatchString(line) {
				violations = append(violations, violation{
					file: path,
					line: i + 1,
					text: strings.TrimSpace(line),
				})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking PackFS: %v", err)
	}

	if len(violations) == 0 {
		return
	}

	limit := 20
	if len(violations) < limit {
		limit = len(violations)
	}
	for _, v := range violations[:limit] {
		t.Errorf("  %s:%d: %s", v.file, v.line, v.text)
	}
	if len(violations) > limit {
		t.Errorf("  ... and %d more", len(violations)-limit)
	}
	t.Fatalf("found %d Go template syntax occurrences in SKILL.md files; "+
		"SKILL.md uses formula-style {{word}} substitution, not Go text/template",
		len(violations))
}
