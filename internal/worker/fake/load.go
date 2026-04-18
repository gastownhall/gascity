// Package fake defines the scripted fake-worker profiles used by worker tests.
package fake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadProfile reads and validates a fake worker profile from disk.
func LoadProfile(path string) (Profile, error) {
	var profile Profile
	if err := loadFile(path, &profile); err != nil {
		return Profile{}, err
	}
	if err := profile.Validate(); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

// LoadScenario reads and validates a fake worker scenario from disk.
func LoadScenario(path string) (Scenario, error) {
	var scenario Scenario
	if err := loadFile(path, &scenario); err != nil {
		return Scenario{}, err
	}
	if err := scenario.Validate(); err != nil {
		return Scenario{}, err
	}
	return scenario, nil
}

// LoadHelperConfig reads and validates standalone fake worker config from disk.
func LoadHelperConfig(path string) (HelperConfig, error) {
	var cfg HelperConfig
	if err := loadFile(path, &cfg); err != nil {
		return HelperConfig{}, err
	}
	if err := cfg.Validate(); err != nil {
		return HelperConfig{}, err
	}
	return cfg, nil
}

func loadFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".json":
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		if dec.More() {
			return fmt.Errorf("decode %s: trailing JSON content", path)
		}
		return nil
	case ".yaml", ".yml":
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported file extension %q", ext)
	}
}
