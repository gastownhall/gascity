package docgen

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/invopop/jsonschema"
)

// RenderMarkdown writes a markdown reference document from a JSON Schema.
// It walks the $defs, rendering one section per type with a table of fields.
func RenderMarkdown(w io.Writer, s *jsonschema.Schema) error {
	// Write Mintlify frontmatter (title + description, no body H1).
	title := s.Title
	if title == "" {
		title = "Configuration Reference"
	}
	if _, err := fmt.Fprintf(w, "---\ntitle: %q\n", title); err != nil {
		return err
	}
	if desc := firstSentence(s.Description); desc != "" {
		if _, err := fmt.Fprintf(w, "description: %q\n", desc); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "---\n\n"); err != nil {
		return err
	}
	if s.Description != "" {
		if _, err := fmt.Fprintf(w, "%s\n\n", s.Description); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "> **Auto-generated** — do not edit. Run `go run ./cmd/genschema` to regenerate.\n\n"); err != nil {
		return err
	}

	// Determine the root type name from $ref (e.g. "#/$defs/City" → "City").
	rootName := ""
	if s.Ref != "" {
		parts := strings.Split(s.Ref, "/")
		rootName = parts[len(parts)-1]
	}

	if s.Definitions == nil {
		return nil
	}

	// Collect definition names and sort, but put root type first.
	var names []string
	for name := range s.Definitions {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == rootName {
			return true
		}
		if names[j] == rootName {
			return false
		}
		return names[i] < names[j]
	})

	for _, name := range names {
		def := s.Definitions[name]
		if def == nil || def.Properties == nil {
			continue
		}

		if _, err := fmt.Fprintf(w, "## %s\n\n", name); err != nil {
			return err
		}
		if def.Description != "" {
			if _, err := fmt.Fprintf(w, "%s\n\n", def.Description); err != nil {
				return err
			}
		}

		// Skip the field table entirely when no schema-visible fields
		// exist (e.g. legacy types whose fields are all hidden); the
		// heading and description still render.
		if def.Properties.Oldest() == nil {
			continue
		}

		// Build required set.
		reqSet := make(map[string]bool)
		for _, r := range def.Required {
			reqSet[r] = true
		}

		// Table header.
		if _, err := fmt.Fprintf(w, "| Field | Type | Required | Default | Description |\n"); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "|-------|------|----------|---------|-------------|\n"); err != nil {
			return err
		}

		for pair := def.Properties.Oldest(); pair != nil; pair = pair.Next() {
			fieldName := pair.Key
			prop := pair.Value

			typStr := schemaTypeString(prop)
			req := ""
			if reqSet[fieldName] {
				req = "**yes**"
			}
			defVal := formatDefault(prop)
			desc := formatDescription(prop)

			if _, err := fmt.Fprintf(w, "| `%s` | %s | %s | %s | %s |\n",
				fieldName, typStr, req, defVal, desc); err != nil {
				return err
			}
		}

		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}

	return nil
}

// firstSentence returns the first sentence of s, for use as the
// frontmatter description. The full text still renders in the body.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1]
	}
	return s
}

// cleanupTempFile removes a temporary file, ignoring errors (best-effort cleanup).
func cleanupTempFile(name string) {
	_ = os.Remove(name)
}

// WriteMarkdown generates a markdown file from a schema using atomic write.
func WriteMarkdown(path string, s *jsonschema.Schema) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".genschema-md-*")
	if err != nil {
		return fmt.Errorf("creating temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()

	if err := RenderMarkdown(tmp, s); err != nil {
		_ = tmp.Close()
		cleanupTempFile(tmpName)
		return fmt.Errorf("rendering %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanupTempFile(tmpName)
		return fmt.Errorf("closing %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanupTempFile(tmpName)
		return fmt.Errorf("renaming %s: %w", path, err)
	}
	return nil
}

// schemaTypeString returns a human-readable type string for a property.
func schemaTypeString(prop *jsonschema.Schema) string {
	// Handle $ref.
	if prop.Ref != "" {
		return refName(prop.Ref)
	}

	typ := prop.Type
	switch typ {
	case "array":
		if prop.Items != nil {
			if prop.Items.Ref != "" {
				return "[]" + refName(prop.Items.Ref)
			}
			return "[]" + prop.Items.Type
		}
		return "array"
	case "object":
		if prop.AdditionalProperties != nil {
			valSchema := prop.AdditionalProperties
			if valSchema.Ref != "" {
				return "map[string]" + refName(valSchema.Ref)
			}
			return "map[string]" + valSchema.Type
		}
		return "object"
	default:
		if typ != "" {
			return typ
		}
		return "any"
	}
}

// refName extracts the type name from a $ref path like "#/$defs/Agent".
func refName(ref string) string {
	parts := strings.Split(ref, "/")
	return parts[len(parts)-1]
}

// formatDefault returns the default value as a string, or empty.
func formatDefault(prop *jsonschema.Schema) string {
	if prop.Default != nil {
		return fmt.Sprintf("`%v`", prop.Default)
	}
	return ""
}

// formatDescription returns the description, appending enum values if present.
func formatDescription(prop *jsonschema.Schema) string {
	desc := prop.Description
	// Collapse multi-line descriptions into a single line for table cells.
	desc = strings.ReplaceAll(desc, "\n", " ")
	if len(prop.Enum) > 0 {
		vals := make([]string, len(prop.Enum))
		for i, v := range prop.Enum {
			vals[i] = fmt.Sprintf("`%v`", v)
		}
		enumStr := "Enum: " + strings.Join(vals, ", ")
		if desc != "" {
			desc += " " + enumStr
		} else {
			desc = enumStr
		}
	}
	// Collapse newlines for markdown table cells.
	desc = strings.ReplaceAll(desc, "\n", " ")
	// Escape raw angle brackets so Mint/MDX does not treat placeholder text
	// like <qualified-name> as JSX.
	desc = strings.ReplaceAll(desc, "<", "&lt;")
	desc = strings.ReplaceAll(desc, ">", "&gt;")
	// Escape braces so placeholders like {{.WorkQuery}} and {} are rendered as
	// text instead of MDX expressions.
	desc = strings.ReplaceAll(desc, "{", "&#123;")
	desc = strings.ReplaceAll(desc, "}", "&#125;")
	// Escape pipe characters for markdown tables.
	desc = strings.ReplaceAll(desc, "|", "\\|")
	return desc
}
