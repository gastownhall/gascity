package config

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// The reverse migration's city.toml edits (S5 §4.4 R4, §4.5): remove the
// [storage] section, and pin [daemon] session_reconciler to legacy, in the
// existing text rather than by re-serializing the file, so every other
// comment and the key order survive. Each edit is proven before it is
// returned, as RemoveRigAndWriteSiteBindingsForEdit proves its own: the
// result, decoded as plain maps, must equal the original's decode with
// exactly that change. An edit that cannot be proven is refused with
// ErrSurgicalStorageEditUnsupported, and the caller writes nothing.

// ErrSurgicalStorageEditUnsupported reports a city.toml whose [storage]
// section or session_reconciler key cannot be edited in place without
// touching anything else: an inline table, a dotted key at the root, or a
// layout the line scanner cannot tell apart.
var ErrSurgicalStorageEditUnsupported = errors.New("city.toml cannot be edited in place")

// RemoveStorageFromCityTOML returns content with its [storage] section and
// every [storage.*] sub-table removed. A comment block directly above a
// removed header goes with it. content that declares no storage key is
// returned unchanged.
func RemoveStorageFromCityTOML(content []byte) ([]byte, error) {
	var want map[string]any
	if _, err := toml.Decode(string(content), &want); err != nil {
		return nil, fmt.Errorf("parsing city.toml: %w", err)
	}
	if _, declared := want["storage"]; !declared {
		return content, nil
	}
	lines := strings.Split(string(content), "\n")
	remove := make([]bool, len(lines))
	open, start := false, 0
	closeBlock := func(end int) {
		if !open {
			return
		}
		open = false
		from := leadingCommentStart(lines, start)
		to := trailingCommentStart(lines, start+1, end)
		for i := from; i < to; i++ {
			remove[i] = true
		}
	}
	for i, line := range lines {
		name, _, ok := tomlHeader(line)
		if !ok {
			continue
		}
		inStorage := name == "storage" || strings.HasPrefix(name, "storage.")
		if open && inStorage {
			continue
		}
		closeBlock(i)
		if inStorage {
			open, start = true, i
		}
	}
	closeBlock(len(lines))
	result := strings.Join(withoutMarked(lines, remove), "\n")
	if strings.HasSuffix(string(content), "\n") && result != "" && !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	delete(want, "storage")
	if err := checkCityTOMLEdit(want, result); err != nil {
		return nil, err
	}
	return []byte(result), nil
}

// sessionReconcilerLine matches a session_reconciler assignment, capturing the
// text before its value and the text after it (a trailing comment).
var sessionReconcilerLine = regexp.MustCompile(`^([ \t]*session_reconciler[ \t]*=[ \t]*)("[^"]*"|'[^']*')([ \t]*(?:#.*)?\r?)$`)

// SetSessionReconcilerInCityTOML returns content with [daemon]
// session_reconciler set to value, rewriting the one line that assigns it.
// It is refused when the root file does not assign the key in a [daemon]
// table: a fragment that sets it is the operator's to edit.
func SetSessionReconcilerInCityTOML(content []byte, value string) ([]byte, error) {
	var want map[string]any
	if _, err := toml.Decode(string(content), &want); err != nil {
		return nil, fmt.Errorf("parsing city.toml: %w", err)
	}
	daemon, _ := want["daemon"].(map[string]any)
	if _, set := daemon["session_reconciler"]; !set {
		return nil, fmt.Errorf("%w: it assigns no [daemon] session_reconciler", ErrSurgicalStorageEditUnsupported)
	}
	daemon["session_reconciler"] = value
	lines := strings.Split(string(content), "\n")
	table, rewritten := "", 0
	for i, line := range lines {
		if name, _, ok := tomlHeader(line); ok {
			table = name
			continue
		}
		if table != "daemon" {
			continue
		}
		if m := sessionReconcilerLine.FindStringSubmatch(line); m != nil {
			lines[i] = m[1] + fmt.Sprintf("%q", value) + m[3]
			rewritten++
		}
	}
	if rewritten != 1 {
		return nil, fmt.Errorf("%w: found %d [daemon] session_reconciler line(s) it can rewrite, want 1", ErrSurgicalStorageEditUnsupported, rewritten)
	}
	result := strings.Join(lines, "\n")
	if err := checkCityTOMLEdit(want, result); err != nil {
		return nil, err
	}
	return []byte(result), nil
}

// checkCityTOMLEdit proves an edit: result must decode to want, the original
// decode with the edit applied.
func checkCityTOMLEdit(want map[string]any, result string) error {
	var got map[string]any
	if _, err := toml.Decode(result, &got); err != nil {
		return fmt.Errorf("%w: the edited file would not parse: %w", ErrSurgicalStorageEditUnsupported, err)
	}
	pruneEmptyTables(want)
	pruneEmptyTables(got)
	if !reflect.DeepEqual(want, got) {
		return fmt.Errorf("%w: the edit could not be told apart from the rest of the file", ErrSurgicalStorageEditUnsupported)
	}
	return nil
}

// CityTOMLDeclaresStorage reports whether content, one city.toml, declares a
// storage key itself (a fragment it includes may declare one instead).
func CityTOMLDeclaresStorage(content []byte) (bool, error) {
	var doc map[string]any
	if _, err := toml.Decode(string(content), &doc); err != nil {
		return false, fmt.Errorf("parsing city.toml: %w", err)
	}
	_, declared := doc["storage"]
	return declared, nil
}

// SameTOMLDocument reports whether a and b decode to the same document.
func SameTOMLDocument(a, b []byte) bool {
	var left, right map[string]any
	if _, err := toml.Decode(string(a), &left); err != nil {
		return false
	}
	if _, err := toml.Decode(string(b), &right); err != nil {
		return false
	}
	pruneEmptyTables(left)
	pruneEmptyTables(right)
	return reflect.DeepEqual(left, right)
}
