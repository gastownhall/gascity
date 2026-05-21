package config

import (
	"fmt"
	"strings"
)

const packV1MigrationDocsURL = "https://docs.gascityhall.com/guides/migrating-to-pack-vnext"

// DiagnosticLocator provides best-effort line numbers for TOML diagnostics.
// It intentionally stays lightweight and line-oriented because it is used
// after a successful decode to improve migration errors, not to validate TOML.
type DiagnosticLocator struct {
	lines []string
}

type configDiagnosticLocator = DiagnosticLocator

func optionalConfigDiagnosticLocator(data [][]byte) configDiagnosticLocator {
	if len(data) == 0 {
		return DiagnosticLocator{}
	}
	return newConfigDiagnosticLocator(data[0])
}

func newConfigDiagnosticLocator(data []byte) configDiagnosticLocator {
	return NewDiagnosticLocator(data)
}

// NewDiagnosticLocator creates a locator for a TOML document.
func NewDiagnosticLocator(data []byte) DiagnosticLocator {
	if len(data) == 0 {
		return DiagnosticLocator{}
	}
	return DiagnosticLocator{lines: strings.Split(string(data), "\n")}
}

func (l DiagnosticLocator) lineForTable(table string) int {
	return l.LineForTable(table)
}

// LineForTable returns the 1-based line for a TOML table, or 0 when absent.
func (l DiagnosticLocator) LineForTable(table string) int {
	for i, line := range l.lines {
		name, ok := parseTOMLTableHeader(line)
		if ok && name == table {
			return i + 1
		}
	}
	return 0
}

func (l DiagnosticLocator) lineForPacksTable() int {
	return l.LineForPacksTable()
}

// LineForPacksTable returns the first 1-based line for [packs] or [packs.*].
func (l DiagnosticLocator) LineForPacksTable() int {
	for i, line := range l.lines {
		name, ok := parseTOMLTableHeader(line)
		if ok && (name == "packs" || strings.HasPrefix(name, "packs.")) {
			return i + 1
		}
	}
	return 0
}

func (l DiagnosticLocator) lineForKey(table, key string) int {
	return l.LineForKey(table, key)
}

// LineForKey returns the 1-based line for a key inside a TOML table.
func (l DiagnosticLocator) LineForKey(table, key string) int {
	var currentTable string
	for i, line := range l.lines {
		trimmed := trimTOMLDiagnosticLine(line)
		if trimmed == "" {
			continue
		}
		if name, ok := parseTOMLTableHeader(trimmed); ok {
			currentTable = name
			continue
		}
		if currentTable != table {
			continue
		}
		gotKey, _, ok := parseTOMLDiagnosticKeyValue(trimmed)
		if ok && gotKey == key {
			return i + 1
		}
	}
	return 0
}

func (l DiagnosticLocator) lineForRigPath(rigName string) int {
	return l.LineForRigPath(rigName)
}

// LineForRigPath returns the 1-based line for a [[rigs]] path field.
func (l DiagnosticLocator) LineForRigPath(rigName string) int {
	var inRig bool
	var currentRigName string
	var currentPathLine int

	flushRig := func() int {
		if !inRig || currentPathLine == 0 {
			return 0
		}
		if rigName == "" || currentRigName == rigName {
			return currentPathLine
		}
		return 0
	}

	for i, line := range l.lines {
		trimmed := trimTOMLDiagnosticLine(line)
		if trimmed == "" {
			continue
		}
		if name, ok := parseTOMLTableHeader(trimmed); ok {
			if line := flushRig(); line > 0 {
				return line
			}
			inRig = name == "rigs"
			currentRigName = ""
			currentPathLine = 0
			continue
		}
		if !inRig {
			continue
		}
		key, value, ok := parseTOMLDiagnosticKeyValue(trimmed)
		if !ok {
			continue
		}
		switch key {
		case "name":
			currentRigName = unquoteTOMLDiagnosticValue(value)
		case "path":
			currentPathLine = i + 1
		}
	}
	return flushRig()
}

func sourceWithDiagnosticLine(source string, line int) string {
	if line <= 0 {
		return source
	}
	return fmt.Sprintf("%s:%d", source, line)
}

func configSurfaceError(title string, violations []string) error {
	if len(violations) == 0 {
		return nil
	}
	return fmt.Errorf("%s:\n  - %s\nsee %s for migration details",
		title,
		strings.Join(violations, "\n  - "),
		packV1MigrationDocsURL)
}

func parseTOMLTableHeader(line string) (string, bool) {
	trimmed := trimTOMLDiagnosticLine(line)
	if strings.HasPrefix(trimmed, "[[") && strings.HasSuffix(trimmed, "]]") {
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "[["), "]]")), true
	}
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")), true
	}
	return "", false
}

func trimTOMLDiagnosticLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	inBasic := false
	inLiteral := false
	escaped := false
	for i, r := range trimmed {
		switch {
		case escaped:
			escaped = false
		case inBasic && r == '\\':
			escaped = true
		case r == '"' && !inLiteral:
			inBasic = !inBasic
		case r == '\'' && !inBasic:
			inLiteral = !inLiteral
		case r == '#' && !inBasic && !inLiteral:
			trimmed = strings.TrimSpace(trimmed[:i])
			return trimmed
		}
	}
	return trimmed
}

func parseTOMLDiagnosticKeyValue(line string) (string, string, bool) {
	before, after, ok := strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(before), strings.TrimSpace(after), true
}

func unquoteTOMLDiagnosticValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 2 {
		return value
	}
	if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
		return value[1 : len(value)-1]
	}
	return value
}
