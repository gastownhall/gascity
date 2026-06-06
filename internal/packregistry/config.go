package packregistry

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/gchome"
)

// ConfigSchema is the supported registries.toml schema version.
const ConfigSchema = 1

const (
	// DefaultRegistryName is the built-in public pack registry name.
	DefaultRegistryName = "main"
	// DefaultRegistrySource is the public gascity-packs registry catalog.
	DefaultRegistrySource = "https://raw.githubusercontent.com/gastownhall/gascity-packs/main/registry.toml"
)

var registryNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Config is the parsed registry configuration stored under the Gas City home.
type Config struct {
	Schema     int        `toml:"schema"`
	Registry   []Registry `toml:"registry,omitempty"`
	Registries []Registry `toml:"-"`
}

// Registry names one configured pack registry and its source.
type Registry struct {
	Name   string `toml:"name"`
	Source string `toml:"source"`
}

// DefaultRegistry returns the first-party public pack registry.
func DefaultRegistry() Registry {
	return Registry{Name: DefaultRegistryName, Source: DefaultRegistrySource}
}

// ConfigPath returns the registries.toml path for a Gas City home.
func ConfigPath(home string) string {
	return gchome.RegistriesPath(home)
}

// LoadConfig reads and validates registry configuration.
func LoadConfig(home string) (Config, error) {
	path := ConfigPath(home)
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{
				Schema: ConfigSchema,
				Registries: []Registry{{
					Name:   DefaultRegistryName,
					Source: DefaultRegistrySource,
				}},
			}, nil
		}
		return cfg, fmt.Errorf("reading registries.toml: %w", err)
	}
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return cfg, fmt.Errorf("parsing registries.toml: %w", err)
	}
	if cfg.Schema == 0 {
		cfg.Schema = ConfigSchema
	}
	if cfg.Schema != ConfigSchema {
		return cfg, fmt.Errorf("unsupported registries.toml schema %d", cfg.Schema)
	}
	cfg.Registries = append([]Registry(nil), cfg.Registry...)
	return cfg, validateConfig(cfg)
}

// EnsureDefaultRegistryConfig writes the first-party registry config for a fresh Gas City home.
func EnsureDefaultRegistryConfig(home string) error {
	return WithConfigLock(home, func() error {
		path := ConfigPath(home)
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking registries.toml: %w", err)
		}
		return SaveConfig(home, Config{Registries: []Registry{DefaultRegistry()}})
	})
}

// SaveConfig validates and writes registry configuration.
func SaveConfig(home string, cfg Config) error {
	cfg.Schema = ConfigSchema
	cfg.Registry = append([]Registry(nil), cfg.Registries...)
	if len(cfg.Registry) == 0 {
		cfg.Registry = nil
	}
	if err := validateConfig(cfg); err != nil {
		return err
	}
	slices.SortFunc(cfg.Registry, func(a, b Registry) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return fmt.Errorf("encoding registries.toml: %w", err)
	}
	path := ConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating registry config directory: %w", err)
	}
	return fsys.WriteFileAtomic(fsys.OSFS{}, path, buf.Bytes(), 0o644)
}

// AddRegistry adds a registry to the registry configuration.
func AddRegistry(home string, reg Registry) error {
	return AddRegistryWithCache(home, reg, nil)
}

// AddRegistryWithCache adds a registry and optionally seeds its catalog cache.
func AddRegistryWithCache(home string, reg Registry, catalogData []byte) error {
	if err := ValidateRegistryName(reg.Name); err != nil {
		return err
	}
	if reg.Source == "" {
		return errors.New("registry source is required")
	}
	if _, err := NormalizeSource(reg.Source); err != nil {
		return err
	}
	return WithConfigLock(home, func() error {
		cfg, err := LoadConfig(home)
		if err != nil {
			return err
		}
		for _, existing := range cfg.Registries {
			if existing.Name == reg.Name {
				return fmt.Errorf("registry %q already exists", reg.Name)
			}
		}
		cfg.Registries = append(cfg.Registries, reg)
		if catalogData != nil {
			if err := WriteCatalogCache(home, reg.Name, catalogData); err != nil {
				return err
			}
		} else if err := RemoveCatalogCache(home, reg.Name); err != nil {
			return err
		}
		return SaveConfig(home, cfg)
	})
}

// RemoveRegistry removes a registry from the registry configuration.
func RemoveRegistry(home, name string) (bool, error) {
	if err := ValidateRegistryName(name); err != nil {
		return false, err
	}
	removed := false
	err := WithConfigLock(home, func() error {
		cfg, err := LoadConfig(home)
		if err != nil {
			return err
		}
		next := cfg.Registries[:0]
		for _, reg := range cfg.Registries {
			if reg.Name == name {
				removed = true
				continue
			}
			next = append(next, reg)
		}
		if !removed {
			return nil
		}
		cfg.Registries = next
		return SaveConfig(home, cfg)
	})
	return removed, err
}

// ValidateRegistryName validates a configured registry name.
func ValidateRegistryName(name string) error {
	if len(name) == 0 {
		return errors.New("registry name is required")
	}
	if len(name) > 64 {
		return fmt.Errorf("registry name %q is too long; maximum length is 64", name)
	}
	if !registryNameRE.MatchString(name) {
		return fmt.Errorf("invalid registry name %q; use lowercase letters, digits, and dashes", name)
	}
	return nil
}

func validateConfig(cfg Config) error {
	seen := map[string]bool{}
	for _, reg := range cfg.Registries {
		if err := ValidateRegistryName(reg.Name); err != nil {
			return err
		}
		if reg.Source == "" {
			return fmt.Errorf("registry %q source is required", reg.Name)
		}
		if _, err := NormalizeSource(reg.Source); err != nil {
			return fmt.Errorf("registry %q source: %w", reg.Name, err)
		}
		if seen[reg.Name] {
			return fmt.Errorf("duplicate registry %q", reg.Name)
		}
		seen[reg.Name] = true
	}
	return nil
}

// WithConfigLock serializes registry configuration updates.
func WithConfigLock(home string, fn func() error) error {
	lockPath := ConfigPath(home) + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("creating registry lock directory: %w", err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("opening registry lock: %w", err)
	}
	defer lockFile.Close() //nolint:errcheck
	return withExclusiveFileLock(lockFile, "registry lock", fn)
}
