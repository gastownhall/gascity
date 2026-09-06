package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// MailCrossCityConfig enables city-qualified mail addressing: recipients of
// the form <city>/<address>, where the first path segment names a city and
// the remainder is that city's own address form. Authoring the section is the
// feature flag; omitting it keeps every recipient resolving exactly as today.
type MailCrossCityConfig struct {
	// City is this city's own segment in city-qualified addresses. Empty
	// defaults to the effective city name (workspace name, else the city
	// directory's base name). <City>/<address> and <address> are one mailbox.
	City string `toml:"city,omitempty"`
	// Cities lists the peer cities addressable as <city>/<address>. A
	// recipient naming a listed city resolves against the roster and is never
	// looked up in the local session store.
	Cities []string `toml:"cities"`
}

// MailCityRoster returns the local city name and peer cities for
// cross-city mail addressing, or ("", nil) when the section is absent.
// fallbackCityName seeds the local name when neither [mail.crosscity] city
// nor the workspace identity names it — callers pass the city directory's
// base name, mirroring every other effective-city-name derivation.
func (c *City) MailCityRoster(fallbackCityName string) (string, []string) {
	if c == nil || c.Mail.CrossCity == nil {
		return "", nil
	}
	cc := c.Mail.CrossCity
	local := strings.TrimSpace(cc.City)
	if local == "" {
		local = EffectiveCityName(c, fallbackCityName)
	}
	return local, cc.Cities
}

// ValidateMailCrossCity validates the [mail.crosscity] section of a composed
// config: a present section must list at least one syntactically valid,
// duplicate-free peer city; the local city must not list itself; and no rig
// may share a name with a listed city — the first mail-address segment must
// bind unambiguously to either a rig or a city, never both (a collision here
// would silently rebind existing rig-qualified mail). cityRoot supplies the
// directory-derived fallback for the effective local city name.
func ValidateMailCrossCity(cfg *City, cityRoot string) error {
	if cfg == nil || cfg.Mail.CrossCity == nil {
		return nil
	}
	cc := cfg.Mail.CrossCity
	if len(cc.Cities) == 0 {
		return fmt.Errorf("[mail.crosscity] cities must list at least one peer city")
	}
	fallback := ""
	if cityRoot != "" {
		fallback = filepath.Base(filepath.Clean(cityRoot))
	}
	local, _ := cfg.MailCityRoster(fallback)
	if err := validateMailCityName(local); err != nil {
		return fmt.Errorf("[mail.crosscity] city: %w", err)
	}
	seen := make(map[string]bool, len(cc.Cities))
	for _, city := range cc.Cities {
		if err := validateMailCityName(city); err != nil {
			return fmt.Errorf("[mail.crosscity] cities: %w", err)
		}
		if seen[city] {
			return fmt.Errorf("[mail.crosscity] cities lists duplicate city %q", city)
		}
		seen[city] = true
		if city == local {
			return fmt.Errorf("[mail.crosscity] cities must not list this city's own city %q", city)
		}
	}
	for i := range cfg.Rigs {
		if seen[cfg.Rigs[i].Name] || cfg.Rigs[i].Name == local {
			return fmt.Errorf("rig %q collides with a [mail.crosscity] city of the same name: the first mail-address segment must name either a rig or a city, never both", cfg.Rigs[i].Name)
		}
	}
	return nil
}

// RigNames returns the configured rig names. Cross-city mail resolution uses
// them as the first-segment scopes that stay local, so a failed rig-qualified
// lookup keeps its ordinary error instead of an unknown-city refusal.
func (c *City) RigNames() []string {
	if c == nil || len(c.Rigs) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.Rigs))
	for i := range c.Rigs {
		if c.Rigs[i].Name != "" {
			names = append(names, c.Rigs[i].Name)
		}
	}
	return names
}

// validateMailCityName enforces the city-segment syntax: non-empty, and
// composed of letters, digits, "-", "_", or "." — above all no "/", which is
// the address separator, and no whitespace.
func validateMailCityName(name string) error {
	if name == "" {
		return fmt.Errorf("city name is empty")
	}
	if strings.IndexFunc(name, func(r rune) bool { return !isMailCityNameRune(r) }) >= 0 {
		return fmt.Errorf("invalid city name %q: use letters, digits, '-', '_', or '.'", name)
	}
	return nil
}

func isMailCityNameRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
}
