// Package pgauth resolves Postgres credentials for a scope and endpoint,
// mirroring internal/doltauth's resolver chain.
package pgauth

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Endpoint identifies the Postgres target a resolution applies to.
type Endpoint struct {
	Host string
	Port string
	User string
}

// Resolved is the effective credential tuple gc projects to a bd subprocess.
type Resolved struct {
	User     string
	Password string
	Source   Source
}

// Source identifies the resolution tier that supplied Password.
//
// String returns a stable snake_case identifier suitable for events,
// logs, and grep. The order of constants matches the resolution-chain
// reading order (see ResolveFromEnv).
type Source int

// Source values, in resolution-chain reading order; see ResolveFromEnv.
const (
	SourceNone                Source = iota // no tier supplied a value
	SourceProjectedGC                       // envMap["GC_POSTGRES_PASSWORD"]
	SourceProjectedBeads                    // envMap["BEADS_POSTGRES_PASSWORD"]
	SourceProcessEnvGC                      // os.Getenv("GC_POSTGRES_PASSWORD")
	SourceScopeFile                         // <scope>/.beads/.env BEADS_POSTGRES_PASSWORD
	SourceProcessEnvBeads                   // os.Getenv("BEADS_POSTGRES_PASSWORD")
	SourceCredentialsFileEnv                // $BEADS_CREDENTIALS_FILE [host:port] section
	SourceCredentialsFileHome               // ~/.config/beads/credentials [host:port] section
)

// String returns the stable snake_case identifier for s. The eight values
// returned here are the eventing/logging contract — slice 4's payload
// `source` field reads them verbatim, runbooks grep on them, and they
// must not change.
func (s Source) String() string {
	switch s {
	case SourceNone:
		return "none"
	case SourceProjectedGC:
		return "projected_gc"
	case SourceProjectedBeads:
		return "projected_beads"
	case SourceProcessEnvGC:
		return "process_env_gc"
	case SourceScopeFile:
		return "scope_file"
	case SourceProcessEnvBeads:
		return "process_env_beads"
	case SourceCredentialsFileEnv:
		return "credentials_file_env"
	case SourceCredentialsFileHome:
		return "credentials_file_home"
	}
	return "none"
}

// ErrNoPasswordResolvable is returned (wrapped) when every resolution tier
// supplies an empty value. Discriminate with errors.Is.
var ErrNoPasswordResolvable = errors.New("no postgres password resolvable")

// PermissivePermissionError reports a credentials-bearing file whose mode
// permits group/other read/write/execute or owner execute. The resolver
// refuses to read it and stops the chain at that tier rather than falling
// through.
type PermissivePermissionError struct {
	Path string
	Mode os.FileMode
}

func (e *PermissivePermissionError) Error() string {
	return fmt.Sprintf("credentials file %s has mode %#o; refuse to read (require 0600 or 0400; owner-executable modes such as 0700 are rejected)", e.Path, e.Mode.Perm())
}

// CredentialsParseError reports a malformed credentials file with the path
// and 1-indexed line number of the first offending line. The resolver stops
// the chain at that tier rather than falling through.
type CredentialsParseError struct {
	Path   string
	Line   int
	Reason string
}

func (e *CredentialsParseError) Error() string {
	return fmt.Sprintf("parse credentials file %s at line %d: %s", e.Path, e.Line, e.Reason)
}

// ResolveFromEnv reads the projected env map plus on-disk sources to derive
// the credentials gc should project to a bd subprocess targeting endpoint
// within scopeRoot.
//
// Resolution order (first non-empty wins):
//
//  1. envMap["GC_POSTGRES_PASSWORD"]
//  2. envMap["BEADS_POSTGRES_PASSWORD"]
//  3. os.Getenv("GC_POSTGRES_PASSWORD")
//  4. scopeRoot/.beads/.env BEADS_POSTGRES_PASSWORD (chmod-checked)
//  5. os.Getenv("BEADS_POSTGRES_PASSWORD")
//  6. $BEADS_CREDENTIALS_FILE [host:port] section (chmod-checked, parse-checked)
//  7. ~/.config/beads/credentials [host:port] section (chmod-checked, parse-checked)
//
// Pass envMap == nil to skip tiers 1 and 2.
//
// Returns ErrNoPasswordResolvable (wrapped, identifiable via errors.Is) when
// every tier returns empty. Returns *PermissivePermissionError when an
// on-disk source's mode permits group or other read; the chain stops at
// that tier rather than falling through. Returns *CredentialsParseError
// when a credentials file is malformed at tier 6 or 7.
func ResolveFromEnv(envMap map[string]string, scopeRoot string, endpoint Endpoint) (Resolved, error) {
	user := strings.TrimSpace(endpoint.User)

	// Tier 1: envMap["GC_POSTGRES_PASSWORD"]
	if envMap != nil {
		if value := strings.TrimSpace(envMap["GC_POSTGRES_PASSWORD"]); value != "" {
			return Resolved{User: user, Password: value, Source: SourceProjectedGC}, nil
		}
		// Tier 2: envMap["BEADS_POSTGRES_PASSWORD"]
		if value := strings.TrimSpace(envMap["BEADS_POSTGRES_PASSWORD"]); value != "" {
			return Resolved{User: user, Password: value, Source: SourceProjectedBeads}, nil
		}
	}

	// Tier 3: os.Getenv("GC_POSTGRES_PASSWORD")
	if value := strings.TrimSpace(os.Getenv("GC_POSTGRES_PASSWORD")); value != "" {
		return Resolved{User: user, Password: value, Source: SourceProcessEnvGC}, nil
	}

	// Tier 4: <scope>/.beads/.env BEADS_POSTGRES_PASSWORD (chmod-checked)
	if value, err := readEnvValueChecked(storeLocalEnvPath(scopeRoot), "BEADS_POSTGRES_PASSWORD"); err != nil {
		return Resolved{}, err
	} else if value != "" {
		return Resolved{User: user, Password: value, Source: SourceScopeFile}, nil
	}

	// Tier 5: os.Getenv("BEADS_POSTGRES_PASSWORD")
	if value := strings.TrimSpace(os.Getenv("BEADS_POSTGRES_PASSWORD")); value != "" {
		return Resolved{User: user, Password: value, Source: SourceProcessEnvBeads}, nil
	}

	// Tier 6: $BEADS_CREDENTIALS_FILE [host:port] section
	if path := strings.TrimSpace(os.Getenv("BEADS_CREDENTIALS_FILE")); path != "" {
		value, err := readCredentialsFilePassword(path, endpoint.Host, endpoint.Port)
		if err != nil {
			return Resolved{}, err
		}
		if value != "" {
			return Resolved{User: user, Password: value, Source: SourceCredentialsFileEnv}, nil
		}
	}

	// Tier 7: platform-default credentials file [host:port] section
	if path := DefaultCredentialsPath(); path != "" {
		value, err := readCredentialsFilePassword(path, endpoint.Host, endpoint.Port)
		if err != nil {
			return Resolved{}, err
		}
		if value != "" {
			return Resolved{User: user, Password: value, Source: SourceCredentialsFileHome}, nil
		}
	}

	return Resolved{User: user, Source: SourceNone}, fmt.Errorf("no postgres password resolvable for %s@%s:%s: %w", endpoint.User, endpoint.Host, endpoint.Port, ErrNoPasswordResolvable)
}

// ReadStoreLocalPassword returns the BEADS_POSTGRES_PASSWORD value from
// scopeRoot/.beads/.env. Returns ("", nil) when the file does not exist
// or the key is absent. Returns ("", *PermissivePermissionError) when the
// file's mode permits group or other read.
//
// gc doctor (slice 4) calls this helper to probe the on-disk steady state
// without invoking the full resolver chain.
func ReadStoreLocalPassword(scopeRoot string) (string, error) {
	return readEnvValueChecked(storeLocalEnvPath(scopeRoot), "BEADS_POSTGRES_PASSWORD")
}

// DefaultCredentialsPath returns the default beads credentials file path
// for the current OS. Returns "" when no home directory is discoverable.
func DefaultCredentialsPath() string {
	if runtime.GOOS == "windows" {
		if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
			return filepath.Join(appData, "beads", "credentials")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".config", "beads", "credentials")
}

// storeLocalEnvPath returns the canonical scope-local .beads/.env path.
func storeLocalEnvPath(scopeRoot string) string {
	if strings.TrimSpace(scopeRoot) == "" {
		return ""
	}
	return filepath.Join(scopeRoot, ".beads", ".env")
}

// isPermissive returns true when mode permits group/other read/write/execute
// or owner execute.
func isPermissive(mode os.FileMode) bool {
	return mode.Perm()&0o177 != 0
}

// readEnvValueChecked opens path, applies the chmod predicate, and returns
// the value of key. ENOENT returns ("", nil). A permissive mode returns
// ("", *PermissivePermissionError).
func readEnvValueChecked(path, key string) (string, error) {
	if path == "" {
		return "", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	if isPermissive(info.Mode()) {
		return "", &PermissivePermissionError{Path: path, Mode: info.Mode()}
	}
	f, err := os.Open(path) //nolint:gosec // path is derived from scope roots
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	value := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		name, raw, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		value = strings.TrimSpace(raw)
		if len(value) >= 2 {
			if (value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"') {
				if unquoted, err := strconv.Unquote(value); err == nil {
					value = unquoted
				} else {
					value = value[1 : len(value)-1]
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan %s: %w", path, err)
	}
	return strings.TrimSpace(value), nil
}

// readCredentialsFilePassword opens path, applies the chmod predicate, and
// returns the password value from the [host:port] section. ENOENT returns
// ("", nil). A permissive mode returns ("", *PermissivePermissionError). A
// malformed file returns ("", *CredentialsParseError) at the first offending
// line.
func readCredentialsFilePassword(path, host, port string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	if isPermissive(info.Mode()) {
		return "", &PermissivePermissionError{Path: path, Mode: info.Mode()}
	}
	f, err := os.Open(path) //nolint:gosec // path is derived from env or os.UserHomeDir
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	sectionKey := host + ":" + port
	inSection := false
	matchedPassword := ""
	matchedSectionSeen := false
	passwordSeenInSection := false
	matched := false
	lineNum := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return "", &CredentialsParseError{Path: path, Line: lineNum, Reason: "unterminated section header (expected ']')"}
			}
			section := strings.TrimSpace(line[1 : len(line)-1])
			if section == "" {
				return "", &CredentialsParseError{Path: path, Line: lineNum, Reason: "empty section header"}
			}
			if section == sectionKey {
				if matchedSectionSeen {
					return "", &CredentialsParseError{Path: path, Line: lineNum, Reason: "duplicate credentials section for " + sectionKey}
				}
				inSection = true
				matchedSectionSeen = true
				passwordSeenInSection = false
			} else if inSection {
				// Past our section; we already finished scanning it.
				inSection = false
			}
			continue
		}
		if !inSection {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return "", &CredentialsParseError{Path: path, Line: lineNum, Reason: "missing '=' in key/value line"}
		}
		if strings.TrimSpace(key) != "password" {
			// Unknown keys inside a matching section are silently ignored.
			continue
		}
		if passwordSeenInSection {
			return "", &CredentialsParseError{Path: path, Line: lineNum, Reason: "duplicate password key in credentials section for " + sectionKey}
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"') {
				if unquoted, err := strconv.Unquote(value); err == nil {
					value = unquoted
				} else {
					value = value[1 : len(value)-1]
				}
			}
		}
		matchedPassword = value
		passwordSeenInSection = true
		matched = true
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan %s: %w", path, err)
	}
	if matched {
		return strings.TrimSpace(matchedPassword), nil
	}
	return "", nil
}
