// Package codexhooks contains the shared safety contract for Codex hook files.
package codexhooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ParseDocument parses data without rounding JSON numbers and validates the
// structural portion of the Codex hooks schema that Gas City merges. Unknown
// top-level and handler fields are preserved for forward compatibility.
func ParseDocument(data []byte, label string) (map[string]any, error) {
	var raw any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return nil, fmt.Errorf("parsing %s: %w", label, err)
	}
	doc, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parsing %s: expected a JSON object", label)
	}
	hooksValue, exists := doc["hooks"]
	if !exists {
		return doc, nil
	}
	hooksMap, ok := hooksValue.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parsing %s: hooks must be an object", label)
	}
	for event, entriesValue := range hooksMap {
		entries, ok := entriesValue.([]any)
		if !ok {
			return nil, fmt.Errorf("parsing %s: hooks.%s must be an array", label, event)
		}
		for i, entryValue := range entries {
			entry, ok := entryValue.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("parsing %s: hooks.%s[%d] must be an object", label, event, i)
			}
			if matcher, exists := entry["matcher"]; exists {
				if _, ok := matcher.(string); !ok {
					return nil, fmt.Errorf("parsing %s: hooks.%s[%d].matcher must be a string", label, event, i)
				}
			}
			commandValue, hasCommand := entry["command"]
			if hasCommand {
				if command, ok := commandValue.(string); !ok || strings.TrimSpace(command) == "" {
					return nil, fmt.Errorf("parsing %s: hooks.%s[%d].command must be a string", label, event, i)
				}
			}
			innerValue, wrapped := entry["hooks"]
			if !wrapped {
				if !hasCommand {
					return nil, fmt.Errorf("parsing %s: hooks.%s[%d] must be a command or wrapper", label, event, i)
				}
				if typeValue, exists := entry["type"]; exists {
					if hookType, ok := typeValue.(string); !ok || strings.TrimSpace(hookType) == "" {
						return nil, fmt.Errorf("parsing %s: hooks.%s[%d].type must be a string", label, event, i)
					}
				}
				continue
			}
			if hasCommand {
				return nil, fmt.Errorf("parsing %s: hooks.%s[%d] cannot be both a command and wrapper", label, event, i)
			}
			inner, ok := innerValue.([]any)
			if !ok {
				return nil, fmt.Errorf("parsing %s: hooks.%s[%d].hooks must be an array", label, event, i)
			}
			if len(inner) == 0 {
				return nil, fmt.Errorf("parsing %s: hooks.%s[%d].hooks must not be empty", label, event, i)
			}
			for j, hookValue := range inner {
				hook, ok := hookValue.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("parsing %s: hooks.%s[%d].hooks[%d] must be an object", label, event, i, j)
				}
				if err := validateHandler(hook); err != nil {
					return nil, fmt.Errorf("parsing %s: hooks.%s[%d].hooks[%d]: %w", label, event, i, j, err)
				}
			}
		}
	}
	return doc, nil
}

// ValidateDocument validates data as a Codex hook document.
func ValidateDocument(data []byte, label string) error {
	_, err := ParseDocument(data, label)
	return err
}

func validateHandler(hook map[string]any) error {
	typeValue, hasType := hook["type"]
	hookType, typeOK := typeValue.(string)
	if !hasType || !typeOK {
		return errors.New("type must be a string")
	}
	switch hookType {
	case "command", "prompt", "agent":
	default:
		return fmt.Errorf("unsupported type %q", hookType)
	}
	commandValue, hasCommand := hook["command"]
	if hookType == "command" {
		command, ok := commandValue.(string)
		if !hasCommand || !ok || strings.TrimSpace(command) == "" {
			return errors.New("command hook requires a non-empty string command")
		}
	} else if hasCommand {
		return fmt.Errorf("%s hook must not define command", hookType)
	}
	for _, key := range []string{"commandWindows", "statusMessage"} {
		if value, exists := hook[key]; exists {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("%s must be a string", key)
			}
		}
	}
	if value, exists := hook["async"]; exists {
		if _, ok := value.(bool); !ok {
			return errors.New("async must be a boolean")
		}
	}
	if value, exists := hook["timeout"]; exists {
		number, ok := value.(json.Number)
		if !ok {
			return errors.New("timeout must be an unsigned integer")
		}
		if _, err := strconv.ParseUint(string(number), 10, 64); err != nil {
			return errors.New("timeout must be an unsigned integer")
		}
	}
	return nil
}
