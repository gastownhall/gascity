package materialize

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSkillName(t *testing.T) {
	cases := map[string]string{
		"---\nname: gc-work\ndescription: d\n---\n":             "gc-work",
		"---\nname: \"quoted\"\n---\n":                          "quoted",
		"---\nmetadata:\n  name: nested\ndescription: d\n---\n": "",
		"---\ndescription: d\n---\nname: body\n":                "",
		"no frontmatter\nname: x\n":                             "",
	}
	for body, want := range cases {
		path := filepath.Join(t.TempDir(), "SKILL.md")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := readSkillName(path); got != want {
			t.Errorf("readSkillName(%q) = %q, want %q", body, got, want)
		}
	}
}
