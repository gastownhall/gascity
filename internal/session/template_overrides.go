package session

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ParseTemplateOverrides decodes persisted session template_overrides metadata.
func ParseTemplateOverrides(metadata map[string]string) (map[string]string, error) {
	if metadata == nil {
		return nil, nil
	}
	raw := strings.TrimSpace(metadata["template_overrides"])
	if raw == "" {
		return nil, nil
	}
	var overrides map[string]string
	if err := json.Unmarshal([]byte(raw), &overrides); err != nil {
		return nil, fmt.Errorf("unmarshal template_overrides: %w", err)
	}
	if len(overrides) == 0 {
		return nil, nil
	}
	return overrides, nil
}
