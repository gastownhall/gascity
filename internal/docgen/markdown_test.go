package docgen

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderMarkdownCitySchema(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	var buf bytes.Buffer
	if err := RenderMarkdown(&buf, s); err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}

	md := buf.String()
	if md == "" {
		t.Fatal("empty markdown output")
	}

	// Check for expected section headers.
	for _, section := range []string{"## City", "## Agent", "## Workspace"} {
		if !strings.Contains(md, section) {
			t.Errorf("missing section %q", section)
		}
	}

	// City should come first (before other sections).
	cityIdx := strings.Index(md, "## City")
	agentIdx := strings.Index(md, "## Agent")
	if cityIdx > agentIdx {
		t.Error("City section should come before Agent section")
	}
}

func TestRenderMarkdownFrontmatter(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	var buf bytes.Buffer
	if err := RenderMarkdown(&buf, s); err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}

	md := buf.String()

	// Mintlify pages carry title+description frontmatter and no body H1.
	if !strings.HasPrefix(md, "---\ntitle: \"Gas City Configuration\"\ndescription: ") {
		t.Errorf("missing title/description frontmatter; got prefix %q", md[:min(len(md), 80)])
	}
	if strings.Contains(md, "# Gas City Configuration") {
		t.Error("body H1 should be replaced by frontmatter title")
	}
}

func TestRenderMarkdownEmptyFieldTableSuppressed(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	var buf bytes.Buffer
	if err := RenderMarkdown(&buf, s); err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}

	md := buf.String()

	// FormulasConfig has no schema-visible fields; its section keeps the
	// heading and description but must not render an empty field table.
	start := strings.Index(md, "## FormulasConfig")
	if start < 0 {
		t.Fatal("missing FormulasConfig section")
	}
	rest := md[start+len("## FormulasConfig"):]
	end := strings.Index(rest, "\n## ")
	if end < 0 {
		end = len(rest)
	}
	section := rest[:end]
	if strings.Contains(section, "| Field |") {
		t.Errorf("FormulasConfig section should not render an empty field table:\n%s", section)
	}
}

func TestRenderMarkdownTableFormat(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	var buf bytes.Buffer
	if err := RenderMarkdown(&buf, s); err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}

	md := buf.String()
	lines := strings.Split(md, "\n")

	// Find table rows (lines starting with |).
	for _, line := range lines {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		// Each table row should have 6 pipe characters (5 columns).
		pipes := strings.Count(line, "|")
		// Account for escaped pipes in descriptions.
		escaped := strings.Count(line, "\\|")
		actual := pipes - escaped
		if actual != 6 {
			t.Errorf("table row has %d columns (expected 5): %s", actual-1, line)
		}
	}
}

func TestRenderMarkdownRequiredFields(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	var buf bytes.Buffer
	if err := RenderMarkdown(&buf, s); err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}

	md := buf.String()

	// Agent.name should be marked required.
	if !strings.Contains(md, "| `name` | string | **yes**") {
		t.Error("Agent.name not marked as required in markdown")
	}
}

func TestRenderMarkdownEnumValues(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	var buf bytes.Buffer
	if err := RenderMarkdown(&buf, s); err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}

	md := buf.String()

	// pre_start field should appear in output.
	if !strings.Contains(md, "pre_start") {
		t.Error("pre_start not shown in markdown")
	}
}
