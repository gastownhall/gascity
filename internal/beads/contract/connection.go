// Package contract owns canonical beads/Dolt config and connection resolution.
package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pidutil"
)

// DoltConnectionTarget is the resolved connection info for a beads scope.
type DoltConnectionTarget struct {
	Host           string
	Port           string
	Database       string
	User           string
	EndpointOrigin EndpointOrigin
	EndpointStatus EndpointStatus
	External       bool
}

// ScopeConfigResolutionKind describes how a scope config was resolved.
type ScopeConfigResolutionKind string

// Scope config resolution kinds.
const (
	ScopeConfigMissing       ScopeConfigResolutionKind = "missing"
	ScopeConfigLegacyMinimal ScopeConfigResolutionKind = "legacy_minimal"
	ScopeConfigAuthoritative ScopeConfigResolutionKind = "authoritative"
)

// ScopeConfigResolution reports the authoritative-state resolution for a scope.
type ScopeConfigResolution struct {
	Kind  ScopeConfigResolutionKind
	State ConfigState
}

// InvalidCanonicalConfigError reports invalid canonical scope config.
type InvalidCanonicalConfigError struct {
	Path string
	Err  error
}

// ErrManagedRuntimeUnavailable reports that canonical config expects managed
// Dolt runtime state, but no live runtime state could be resolved.
var ErrManagedRuntimeUnavailable = errors.New("dolt runtime state unavailable")

// IsManagedRuntimeUnavailable reports whether err indicates missing or stale
// managed Dolt runtime state.
func IsManagedRuntimeUnavailable(err error) bool {
	return errors.Is(err, ErrManagedRuntimeUnavailable)
}

func (e *InvalidCanonicalConfigError) Error() string {
	return fmt.Sprintf("invalid canonical endpoint state in %s: %v", e.Path, e.Err)
}

func (e *InvalidCanonicalConfigError) Unwrap() error {
	return e.Err
}

// ResolveDoltConnectionTarget returns the effective Dolt target for a scope.
func ResolveDoltConnectionTarget(fs fsys.FS, cityRoot, scopeRoot string) (DoltConnectionTarget, error) {
	cfgPath := filepath.Join(scopeRoot, ".beads", "config.yaml")
	cfg, ok, err := ReadConfigState(fs, cfgPath)
	if err != nil {
		return DoltConnectionTarget{}, err
	}
	if ok {
		if err := ValidateCanonicalConfigState(fs, cityRoot, scopeRoot, cfg); err != nil {
			return DoltConnectionTarget{}, err
		}
	} else {
		cfg = ConfigState{}
	}
	cfg = deriveLegacyConnectionConfig(fs, cityRoot, scopeRoot, cfg)
	if err := ValidateConnectionConfigState(fs, cityRoot, scopeRoot, cfg); err != nil {
		return DoltConnectionTarget{}, err
	}

	target := DoltConnectionTarget{
		EndpointOrigin: cfg.EndpointOrigin,
		EndpointStatus: cfg.EndpointStatus,
		Database:       "beads",
		User:           strings.TrimSpace(cfg.DoltUser),
	}
	if db, ok, err := ReadDoltDatabase(fs, filepath.Join(scopeRoot, ".beads", "metadata.json")); err != nil {
		return DoltConnectionTarget{}, err
	} else if ok && strings.TrimSpace(db) != "" {
		target.Database = strings.TrimSpace(db)
	}

	switch cfg.EndpointOrigin {
	case EndpointOriginManagedCity:
		port, err := readManagedRuntimePort(fs, cityRoot)
		if err != nil {
			return DoltConnectionTarget{}, err
		}
		target.Host = "127.0.0.1"
		target.Port = port
		return target, nil
	case EndpointOriginCityCanonical, EndpointOriginExplicit:
		return populateExternalTarget(target, cfg)
	case EndpointOriginInheritedCity:
		return resolveInheritedCityConnectionTarget(fs, cityRoot, target, cfg)
	default:
		return DoltConnectionTarget{}, fmt.Errorf("unsupported endpoint origin %q for %s", cfg.EndpointOrigin, cfgPath)
	}
}

// ValidateCanonicalConfigState validates canonical scope config invariants.
func ValidateCanonicalConfigState(fs fsys.FS, cityRoot, scopeRoot string, cfg ConfigState) error {
	if err := ValidateConnectionConfigState(fs, cityRoot, scopeRoot, cfg); err != nil {
		return err
	}

	if sameScope(scopeRoot, cityRoot) {
		switch cfg.EndpointOrigin {
		case "":
			return nil
		case EndpointOriginCityCanonical:
			if strings.TrimSpace(cfg.DoltHost) == "" || strings.TrimSpace(cfg.DoltPort) == "" {
				return fmt.Errorf("canonical %s config requires both dolt.host and dolt.port", cfg.EndpointOrigin)
			}
		}
		return nil
	}

	switch cfg.EndpointOrigin {
	case "":
		return nil
	case EndpointOriginExplicit:
		if strings.TrimSpace(cfg.DoltHost) == "" || strings.TrimSpace(cfg.DoltPort) == "" {
			return fmt.Errorf("canonical explicit rig config requires both dolt.host and dolt.port")
		}
	case EndpointOriginInheritedCity:
		cityResolved, err := ResolveScopeConfigState(fs, cityRoot, cityRoot, "")
		if err != nil {
			return err
		}
		if cityResolved.Kind != ScopeConfigAuthoritative {
			return nil
		}
		cityState := cityResolved.State
		switch cityState.EndpointOrigin {
		case EndpointOriginManagedCity:
			if configTracksEndpoint(cfg) {
				return fmt.Errorf("inherited rig under managed city must not track dolt.host, dolt.port, or dolt.user")
			}
		case EndpointOriginCityCanonical:
			if strings.TrimSpace(cfg.DoltHost) == "" || strings.TrimSpace(cfg.DoltPort) == "" {
				return fmt.Errorf("canonical inherited rig config requires both dolt.host and dolt.port")
			}
			if !sameExternalEndpoint(cityState, cfg) {
				return fmt.Errorf("canonical inherited rig config must mirror the city endpoint")
			}
		default:
			return fmt.Errorf("invalid city endpoint origin %q for inherited rig config", cityState.EndpointOrigin)
		}
	}
	return nil
}

// ResolveAuthoritativeConfigState returns a normalized authoritative scope config when present.
func ResolveAuthoritativeConfigState(fs fsys.FS, cityRoot, scopeRoot, issuePrefix string) (ConfigState, bool, error) {
	existing, ok, err := ReadConfigState(fs, filepath.Join(scopeRoot, ".beads", "config.yaml"))
	if err != nil || !ok {
		return ConfigState{}, ok, err
	}
	existing.IssuePrefix = issuePrefix
	if err := ValidateCanonicalConfigState(fs, cityRoot, scopeRoot, existing); err != nil {
		return ConfigState{}, false, err
	}

	port := strings.TrimSpace(existing.DoltPort)
	rawHost := strings.TrimSpace(existing.DoltHost)
	host := canonicalExternalHost(existing.DoltHost, port)
	if sameScope(scopeRoot, cityRoot) {
		switch existing.EndpointOrigin {
		case EndpointOriginManagedCity:
			existing.EndpointStatus = EndpointStatusVerified
			existing.DoltHost = ""
			existing.DoltPort = ""
			existing.DoltUser = ""
			return existing, true, nil
		case EndpointOriginCityCanonical:
			existing.DoltHost = host
			existing.DoltPort = port
			if existing.EndpointStatus == "" {
				existing.EndpointStatus = EndpointStatusUnverified
			}
			return existing, true, nil
		case "":
			if host == "" && port == "" {
				return ConfigState{}, false, nil
			}
			existing.EndpointOrigin = EndpointOriginCityCanonical
			existing.EndpointStatus = EndpointStatusUnverified
			existing.DoltHost = host
			existing.DoltPort = port
			return existing, true, nil
		default:
			return ConfigState{}, false, nil
		}
	}

	switch existing.EndpointOrigin {
	case EndpointOriginExplicit:
		existing.DoltHost = host
		existing.DoltPort = port
		if existing.EndpointStatus == "" {
			existing.EndpointStatus = EndpointStatusUnverified
		}
		return existing, true, nil
	case EndpointOriginInheritedCity:
		cityResolved, err := ResolveScopeConfigState(fs, cityRoot, cityRoot, "")
		if err != nil {
			return ConfigState{}, false, err
		}
		if cityResolved.Kind == ScopeConfigAuthoritative {
			return inheritedAuthoritativeRigConfigState(issuePrefix, cityResolved.State), true, nil
		}
		existing.DoltHost = host
		existing.DoltPort = port
		if host == "" && port == "" {
			existing.DoltUser = ""
			if existing.EndpointStatus == "" {
				existing.EndpointStatus = EndpointStatusVerified
			}
			return existing, true, nil
		}
		if existing.EndpointStatus == "" {
			existing.EndpointStatus = EndpointStatusUnverified
		}
		return existing, true, nil
	case "":
		if rawHost == "" && port == "" {
			return ConfigState{}, false, nil
		}
		if rawHost == "" && port != "" {
			cityResolved, err := ResolveScopeConfigState(fs, cityRoot, cityRoot, "")
			if err != nil {
				return ConfigState{}, false, err
			}
			if cityResolved.Kind == ScopeConfigAuthoritative && cityResolved.State.EndpointOrigin == EndpointOriginCityCanonical {
				return ConfigState{}, false, nil
			}
			return ConfigState{IssuePrefix: issuePrefix, EndpointOrigin: EndpointOriginInheritedCity, EndpointStatus: EndpointStatusVerified}, true, nil
		}
		cityResolved, err := ResolveScopeConfigState(fs, cityRoot, cityRoot, "")
		if err != nil {
			return ConfigState{}, false, err
		}
		if cityResolved.Kind == ScopeConfigAuthoritative && cityResolved.State.EndpointOrigin == EndpointOriginCityCanonical && sameExternalEndpoint(cityResolved.State, existing) {
			return inheritedAuthoritativeRigConfigState(issuePrefix, cityResolved.State), true, nil
		}
		existing.EndpointOrigin = EndpointOriginExplicit
		existing.EndpointStatus = EndpointStatusUnverified
		existing.DoltHost = host
		existing.DoltPort = port
		return existing, true, nil
	}
	return ConfigState{}, false, nil
}

// ScopeUsesExplicitEndpoint reports whether a scope owns an explicit endpoint.
func ScopeUsesExplicitEndpoint(fs fsys.FS, cityRoot, scopeRoot string) (bool, error) {
	resolved, err := ResolveScopeConfigState(fs, cityRoot, scopeRoot, "")
	if err != nil {
		return false, err
	}
	return resolved.Kind == ScopeConfigAuthoritative && resolved.State.EndpointOrigin == EndpointOriginExplicit, nil
}

// AllowsInvalidInheritedCityFallback reports whether inherited-city fallback is permitted.
func AllowsInvalidInheritedCityFallback(fs fsys.FS, cityRoot, scopeRoot string) (bool, error) {
	cfg, ok, err := ReadConfigState(fs, filepath.Join(scopeRoot, ".beads", "config.yaml"))
	if err != nil || !ok {
		return false, err
	}
	if cfg.EndpointOrigin != EndpointOriginInheritedCity {
		return false, nil
	}
	cityResolved, err := ResolveScopeConfigState(fs, cityRoot, cityRoot, "")
	if err != nil {
		return false, nil
	}
	return cityResolved.Kind == ScopeConfigAuthoritative && cityResolved.State.EndpointOrigin == EndpointOriginCityCanonical, nil
}

// ValidateInheritedCityEndpointMirror checks that an inherited rig mirrors the city endpoint.
func ValidateInheritedCityEndpointMirror(fs fsys.FS, cityRoot, scopeRoot string) error {
	resolved, err := ResolveScopeConfigState(fs, cityRoot, scopeRoot, "")
	if err != nil {
		return err
	}
	if resolved.Kind != ScopeConfigAuthoritative || resolved.State.EndpointOrigin != EndpointOriginInheritedCity {
		return nil
	}
	rigCfg := resolved.State
	cityTarget, err := ResolveDoltConnectionTarget(fs, cityRoot, cityRoot)
	if err != nil {
		return nil
	}
	if cityTarget.EndpointOrigin != EndpointOriginCityCanonical {
		return nil
	}
	if strings.TrimSpace(rigCfg.DoltHost) != strings.TrimSpace(cityTarget.Host) || strings.TrimSpace(rigCfg.DoltPort) != strings.TrimSpace(cityTarget.Port) || strings.TrimSpace(rigCfg.DoltUser) != strings.TrimSpace(cityTarget.User) {
		return fmt.Errorf("local inherited endpoint mirror drifts from canonical city endpoint")
	}
	return nil
}

// ResolveScopeConfigState resolves a scope config into canonical, legacy, or missing state.
func ResolveScopeConfigState(fs fsys.FS, cityRoot, scopeRoot, issuePrefix string) (ScopeConfigResolution, error) {
	cfgPath := filepath.Join(scopeRoot, ".beads", "config.yaml")
	existing, ok, err := ReadConfigState(fs, cfgPath)
	if err != nil {
		return ScopeConfigResolution{}, err
	}
	if !ok {
		return ScopeConfigResolution{Kind: ScopeConfigMissing}, nil
	}
	if IsLegacyMinimalEndpointConfig(existing) {
		return ScopeConfigResolution{Kind: ScopeConfigLegacyMinimal}, nil
	}
	state, ok, err := ResolveAuthoritativeConfigState(fs, cityRoot, scopeRoot, issuePrefix)
	if err != nil {
		return ScopeConfigResolution{}, &InvalidCanonicalConfigError{Path: cfgPath, Err: err}
	}
	if !ok {
		return ScopeConfigResolution{}, &InvalidCanonicalConfigError{Path: cfgPath, Err: fmt.Errorf("unrecognized endpoint authority")}
	}
	return ScopeConfigResolution{Kind: ScopeConfigAuthoritative, State: state}, nil
}

func inheritedAuthoritativeRigConfigState(prefix string, cityState ConfigState) ConfigState {
	state := ConfigState{
		IssuePrefix:    prefix,
		EndpointOrigin: EndpointOriginInheritedCity,
	}
	if cityState.EndpointOrigin == EndpointOriginCityCanonical {
		state.DoltHost = cityState.DoltHost
		state.DoltPort = cityState.DoltPort
		state.DoltUser = strings.TrimSpace(cityState.DoltUser)
		state.EndpointStatus = cityState.EndpointStatus
		return state
	}
	state.EndpointStatus = EndpointStatusVerified
	return state
}

// ValidateConnectionConfigState validates config needed to build a connection target.
func ValidateConnectionConfigState(fs fsys.FS, cityRoot, scopeRoot string, cfg ConfigState) error {
	hasTrackedEndpoint := configTracksEndpoint(cfg)
	if sameScope(scopeRoot, cityRoot) {
		switch cfg.EndpointOrigin {
		case EndpointOriginManagedCity:
			if hasTrackedEndpoint {
				return fmt.Errorf("managed city config must not track dolt.host, dolt.port, or dolt.user")
			}
		case EndpointOriginCityCanonical:
			if strings.TrimSpace(cfg.DoltPort) == "" {
				return fmt.Errorf("city_canonical config requires dolt.port")
			}
			if err := validateExternalHostValue(cfg.DoltHost, cfg.DoltPort); err != nil {
				return err
			}
		case EndpointOriginInheritedCity, EndpointOriginExplicit:
			return fmt.Errorf("%s endpoint origin is invalid for city scope", cfg.EndpointOrigin)
		}
		return nil
	}
	switch cfg.EndpointOrigin {
	case EndpointOriginManagedCity, EndpointOriginCityCanonical:
		return fmt.Errorf("%s endpoint origin is invalid for rig scope", cfg.EndpointOrigin)
	case EndpointOriginExplicit:
		if strings.TrimSpace(cfg.DoltPort) == "" {
			return fmt.Errorf("explicit rig config requires dolt.port")
		}
		if err := validateExternalHostValue(cfg.DoltHost, cfg.DoltPort); err != nil {
			return err
		}
	case EndpointOriginInheritedCity:
		cityResolved, err := ResolveScopeConfigState(fs, cityRoot, cityRoot, "")
		if err != nil {
			return err
		}
		if cityResolved.Kind != ScopeConfigAuthoritative {
			return nil
		}
		cityState := cityResolved.State
		switch cityState.EndpointOrigin {
		case EndpointOriginManagedCity:
			if hasTrackedEndpoint {
				return fmt.Errorf("inherited rig under managed city must not track dolt.host, dolt.port, or dolt.user")
			}
		case EndpointOriginCityCanonical:
			if err := validateExternalHostValue(cfg.DoltHost, cfg.DoltPort); err != nil {
				return err
			}
			return nil
		case EndpointOriginInheritedCity, EndpointOriginExplicit:
			return fmt.Errorf("invalid city endpoint origin %q for inherited rig config", cityState.EndpointOrigin)
		}
	}
	return nil
}

func deriveLegacyConnectionConfig(fs fsys.FS, cityRoot, scopeRoot string, cfg ConfigState) ConfigState {
	if cfg.EndpointOrigin != "" && cfg.EndpointStatus != "" {
		return cfg
	}
	derived := cfg
	hasExternalEndpoint := strings.TrimSpace(cfg.DoltHost) != "" || strings.TrimSpace(cfg.DoltPort) != ""
	scopeIsCity := sameScope(scopeRoot, cityRoot)

	if derived.EndpointOrigin == "" {
		switch {
		case scopeIsCity && hasExternalEndpoint:
			derived.EndpointOrigin = EndpointOriginCityCanonical
		case scopeIsCity:
			derived.EndpointOrigin = EndpointOriginManagedCity
		case hasExternalEndpoint:
			derived.EndpointOrigin = deriveRigLegacyExternalOrigin(fs, cityRoot, cfg)
		default:
			derived.EndpointOrigin = EndpointOriginInheritedCity
		}
	}
	if derived.EndpointStatus == "" {
		switch derived.EndpointOrigin {
		case EndpointOriginManagedCity:
			derived.EndpointStatus = EndpointStatusVerified
		case EndpointOriginInheritedCity:
			if hasExternalEndpoint {
				if cityCfg, ok, err := ReadConfigState(fs, filepath.Join(cityRoot, ".beads", "config.yaml")); err == nil && ok {
					cityCfg = deriveLegacyConnectionConfig(fs, cityRoot, cityRoot, cityCfg)
					if cityCfg.EndpointStatus != "" {
						derived.EndpointStatus = cityCfg.EndpointStatus
						break
					}
				}
				derived.EndpointStatus = EndpointStatusUnverified
			} else {
				derived.EndpointStatus = EndpointStatusVerified
			}
		default:
			derived.EndpointStatus = EndpointStatusUnverified
		}
	}
	if !scopeIsCity && derived.EndpointOrigin == EndpointOriginInheritedCity && strings.TrimSpace(cfg.DoltHost) == "" && strings.TrimSpace(cfg.DoltPort) != "" {
		cityResolved, err := ResolveScopeConfigState(fs, cityRoot, cityRoot, "")
		if err != nil || cityResolved.Kind != ScopeConfigAuthoritative || cityResolved.State.EndpointOrigin != EndpointOriginCityCanonical {
			derived.DoltHost = ""
			derived.DoltPort = ""
			derived.DoltUser = ""
			derived.EndpointStatus = EndpointStatusVerified
		}
	}
	return derived
}

func resolveInheritedCityConnectionTarget(fs fsys.FS, cityRoot string, target DoltConnectionTarget, rigCfg ConfigState) (DoltConnectionTarget, error) {
	cityState, err := resolveCityTopologyState(fs, cityRoot)
	if err != nil {
		return DoltConnectionTarget{}, err
	}
	switch cityState.EndpointOrigin {
	case EndpointOriginCityCanonical:
		target.User = strings.TrimSpace(cityState.DoltUser)
		if cityState.EndpointStatus != "" {
			target.EndpointStatus = cityState.EndpointStatus
		}
		return populateExternalTarget(target, cityState)
	case EndpointOriginManagedCity:
		if cityState.EndpointStatus != "" {
			target.EndpointStatus = cityState.EndpointStatus
		}
		port, err := readManagedRuntimePort(fs, cityRoot)
		if err != nil {
			return DoltConnectionTarget{}, err
		}
		target.Host = "127.0.0.1"
		target.Port = port
		return target, nil
	}
	if strings.TrimSpace(rigCfg.DoltHost) != "" {
		return populateExternalTarget(target, rigCfg)
	}
	return DoltConnectionTarget{}, fmt.Errorf("unsupported city endpoint origin %q for inherited rig config", cityState.EndpointOrigin)
}

func deriveRigLegacyExternalOrigin(fs fsys.FS, cityRoot string, rigCfg ConfigState) EndpointOrigin {
	cityState, err := resolveCityTopologyState(fs, cityRoot)
	if err != nil {
		if strings.TrimSpace(rigCfg.DoltHost) == "" && strings.TrimSpace(rigCfg.DoltPort) != "" {
			return EndpointOriginInheritedCity
		}
		return EndpointOriginExplicit
	}
	if strings.TrimSpace(rigCfg.DoltHost) == "" && strings.TrimSpace(rigCfg.DoltPort) != "" && cityState.EndpointOrigin == EndpointOriginManagedCity {
		return EndpointOriginInheritedCity
	}
	if cityState.EndpointOrigin == EndpointOriginCityCanonical && sameExternalEndpoint(cityState, rigCfg) {
		return EndpointOriginInheritedCity
	}
	return EndpointOriginExplicit
}

func sameExternalEndpoint(a, b ConfigState) bool {
	if strings.TrimSpace(a.DoltPort) != strings.TrimSpace(b.DoltPort) {
		return false
	}
	if canonicalExternalHost(strings.TrimSpace(a.DoltHost), strings.TrimSpace(a.DoltPort)) != canonicalExternalHost(strings.TrimSpace(b.DoltHost), strings.TrimSpace(b.DoltPort)) {
		return false
	}
	return strings.TrimSpace(a.DoltUser) == strings.TrimSpace(b.DoltUser)
}

func canonicalExternalHost(host, port string) string {
	host = strings.TrimSpace(host)
	if host == "" && strings.TrimSpace(port) != "" {
		return "127.0.0.1"
	}
	return host
}

func validateExternalHostValue(host, port string) error {
	host = canonicalExternalHost(host, port)
	switch strings.Trim(host, "[]") {
	case "", "127.0.0.1", "localhost":
		return nil
	case "0.0.0.0", "::":
		return fmt.Errorf("external endpoint host %q is invalid; use a concrete host, not a bind address", host)
	default:
		return nil
	}
}

func sameScope(a, b string) bool {
	return normalizeScopePathForCompare(a) == normalizeScopePathForCompare(b)
}

func normalizeScopePathForCompare(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

func resolveCityTopologyState(fs fsys.FS, cityRoot string) (ConfigState, error) {
	target, err := ResolveDoltConnectionTarget(fs, cityRoot, cityRoot)
	if err != nil {
		return ConfigState{}, err
	}
	return configStateFromDoltTarget(target), nil
}

func configStateFromDoltTarget(target DoltConnectionTarget) ConfigState {
	if target.External {
		return ConfigState{
			EndpointOrigin: EndpointOriginCityCanonical,
			EndpointStatus: target.EndpointStatus,
			DoltHost:       target.Host,
			DoltPort:       target.Port,
			DoltUser:       target.User,
		}
	}
	return ConfigState{
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: target.EndpointStatus,
	}
}

// ConfigHasEndpointAuthority reports whether config carries endpoint authority.
func ConfigHasEndpointAuthority(cfg ConfigState) bool {
	return cfg.EndpointOrigin != "" || strings.TrimSpace(cfg.DoltHost) != "" || strings.TrimSpace(cfg.DoltPort) != ""
}

// IsLegacyMinimalEndpointConfig reports whether config only carries legacy minimal endpoint data.
func IsLegacyMinimalEndpointConfig(cfg ConfigState) bool {
	return cfg.EndpointOrigin == "" && cfg.EndpointStatus == "" && strings.TrimSpace(cfg.DoltHost) == "" && strings.TrimSpace(cfg.DoltPort) == "" && strings.TrimSpace(cfg.DoltUser) == ""
}

func configTracksEndpoint(cfg ConfigState) bool {
	return strings.TrimSpace(cfg.DoltHost) != "" || strings.TrimSpace(cfg.DoltPort) != "" || strings.TrimSpace(cfg.DoltUser) != ""
}

func populateExternalTarget(target DoltConnectionTarget, cfg ConfigState) (DoltConnectionTarget, error) {
	port := strings.TrimSpace(cfg.DoltPort)
	if port == "" {
		return DoltConnectionTarget{}, fmt.Errorf("missing dolt.port for external scope")
	}
	if _, err := strconv.Atoi(port); err != nil {
		return DoltConnectionTarget{}, fmt.Errorf("invalid dolt.port %q: %w", port, err)
	}
	host := strings.TrimSpace(cfg.DoltHost)
	if host == "" {
		host = "127.0.0.1"
	}
	if err := validateExternalHostValue(host, port); err != nil {
		return DoltConnectionTarget{}, err
	}
	target.Host = host
	target.Port = port
	target.External = true
	return target, nil
}

func readManagedRuntimePort(fs fsys.FS, cityRoot string) (string, error) {
	state, err := readManagedRuntimeState(fs, cityRoot)
	if err != nil {
		return "", err
	}
	if !validManagedRuntimeState(state, cityRoot) {
		return "", fmt.Errorf("%w", ErrManagedRuntimeUnavailable)
	}
	return strconv.Itoa(state.Port), nil
}

type managedRuntimeState struct {
	Running bool   `json:"running"`
	PID     int    `json:"pid"`
	Port    int    `json:"port"`
	DataDir string `json:"data_dir"`
}

func readManagedRuntimeState(fs fsys.FS, cityRoot string) (managedRuntimeState, error) {
	data, err := fs.ReadFile(filepath.Join(cityRoot, ".gc", "runtime", "packs", "dolt", "dolt-state.json"))
	if err != nil {
		return managedRuntimeState{}, fmt.Errorf("read dolt runtime state: %w", err)
	}
	var state managedRuntimeState
	if err := json.Unmarshal(data, &state); err != nil {
		return managedRuntimeState{}, fmt.Errorf("parse dolt runtime state: %w", err)
	}
	return state, nil
}

func validManagedRuntimeState(state managedRuntimeState, cityRoot string) bool {
	if !state.Running || state.Port <= 0 || state.PID <= 0 {
		return false
	}
	expectedDataDir := filepath.Join(cityRoot, ".beads", "dolt")
	if filepath.Clean(strings.TrimSpace(state.DataDir)) != filepath.Clean(expectedDataDir) {
		return false
	}
	if !contractPIDAlive(state.PID) {
		return false
	}
	return contractPortReachable(strconv.Itoa(state.Port))
}

func contractPIDAlive(pid int) bool {
	return pidutil.Alive(pid)
}

func contractPortReachable(port string) bool {
	if strings.TrimSpace(port) == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
