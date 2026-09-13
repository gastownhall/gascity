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
	return parseTemplateOverrides(metadata["template_overrides"])
}

// ParseTemplateOverridesFromInfo decodes the template_overrides mirror carried on
// a session.Info (Info.TemplateOverrides — the raw JSON-object string). It is the
// front-door read used by the reconciler config-drift hash path and the launch
// path instead of cracking session.Metadata directly, and is byte-identical to
// ParseTemplateOverrides for the same underlying bead.
func ParseTemplateOverridesFromInfo(info Info) (map[string]string, error) {
	return parseTemplateOverrides(info.TemplateOverrides)
}

// parseTemplateOverrides is the shared decode core over the raw template_overrides
// JSON-object string: absent/blank/JSON-null/empty-object normalize to nil with no
// error; malformed payloads surface an error naming the metadata key.
func parseTemplateOverrides(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
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
