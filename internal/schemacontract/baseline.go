package schemacontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ParseBaseline strictly decodes baseline bytes: unknown fields, an
// unsupported format, trailing data, and a schema without entries are errors.
func ParseBaseline(data []byte) (Baseline, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var b Baseline
	if err := dec.Decode(&b); err != nil {
		return Baseline{}, fmt.Errorf("decoding schema baseline: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Baseline{}, errors.New("decoding schema baseline: trailing data after the document")
	}
	if b.Format != baselineFormat {
		return Baseline{}, fmt.Errorf("schema baseline format %d is unsupported (want %d)", b.Format, baselineFormat)
	}
	for path, entries := range b.Schemas {
		if len(entries) == 0 {
			return Baseline{}, fmt.Errorf("schema baseline: %q has no entries", path)
		}
	}
	return b, nil
}

// Marshal renders a baseline as deterministic bytes: sorted schema keys,
// variant keys and field paths, one compact line per field record.
func Marshal(b Baseline) ([]byte, error) {
	var out bytes.Buffer
	fmt.Fprintf(&out, "{\n  \"format\": %d,\n  \"schemas\": ", b.Format)
	if len(b.Schemas) == 0 {
		out.WriteString("{}")
	} else {
		out.WriteString("{\n")
		paths := sortedKeys(b.Schemas)
		for i, path := range paths {
			key, err := quote(path)
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(&out, "    %s: [\n", key)
			entries := b.Schemas[path]
			for j, entry := range entries {
				if err := writeEntry(&out, entry); err != nil {
					return nil, fmt.Errorf("%s: %w", path, err)
				}
				out.WriteString(separator(j, len(entries)))
			}
			out.WriteString("    ]" + separator(i, len(paths)))
		}
		out.WriteString("  }")
	}
	out.WriteString("\n}\n")
	return out.Bytes(), nil
}

// separator returns the line end after element i of n.
func separator(i, n int) string {
	if i < n-1 {
		return ",\n"
	}
	return "\n"
}

func writeEntry(out *bytes.Buffer, e Entry) error {
	out.WriteString("      {\n")
	for _, kv := range []struct{ key, val string }{
		{"schema_version", e.SchemaVersion}, {"note", e.Note}, {"issue", e.Issue}, {"pr", e.PR},
	} {
		if kv.val == "" && kv.key != "schema_version" {
			continue
		}
		q, err := quote(kv.val)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "        %q: %s,\n", kv.key, q)
	}
	fmt.Fprintf(out, "        \"jsonl\": %t,\n        \"raw_json\": %t,\n        \"variants\": ", e.JSONL, e.RawJSON)
	if len(e.Variants) == 0 {
		out.WriteString("{}\n")
	} else {
		out.WriteString("{\n")
		keys := sortedKeys(e.Variants)
		for i, key := range keys {
			q, err := quote(key)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "          %s: [", q)
			fields := append([]Field(nil), e.Variants[key]...)
			sort.Slice(fields, func(a, b int) bool { return fields[a].Path < fields[b].Path })
			if len(fields) > 0 {
				out.WriteString("\n")
			}
			for j, f := range fields {
				line, err := json.Marshal(f)
				if err != nil {
					return err
				}
				out.WriteString("            " + string(line) + separator(j, len(fields)))
			}
			if len(fields) > 0 {
				out.WriteString("          ")
			}
			out.WriteString("]" + strings.TrimSuffix(separator(i, len(keys)), "\n"))
			out.WriteString("\n")
		}
		out.WriteString("        }\n")
	}
	out.WriteString("      }")
	return nil
}

// quote renders s as a JSON string literal.
func quote(s string) (string, error) {
	data, err := json.Marshal(s)
	return string(data), err
}
