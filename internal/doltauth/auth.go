// Package doltauth resolves Dolt credentials from scoped files and env overrides.
package doltauth

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// Resolved holds the effective Dolt auth values for a scope.
type Resolved struct {
	User                    string
	Password                string
	CredentialsFileOverride string
}

// AuthScopeRoot returns the scope root that owns credentials for the target.
func AuthScopeRoot(cityRoot, scopeRoot string, target contract.DoltConnectionTarget) string {
	if filepath.Clean(scopeRoot) == filepath.Clean(cityRoot) {
		return cityRoot
	}
	if target.EndpointOrigin == contract.EndpointOriginExplicit {
		return scopeRoot
	}
	return cityRoot
}

// Resolve returns the effective Dolt auth for a scope and target.
func Resolve(scopeRoot, fallbackUser, host string, port int) Resolved {
	overridePath := strings.TrimSpace(os.Getenv("BEADS_CREDENTIALS_FILE"))
	return Resolved{
		User:                    resolveUser(fallbackUser),
		Password:                resolvePassword(scopeRoot, host, port, overridePath),
		CredentialsFileOverride: overridePath,
	}
}

// ResolveFromEnv returns effective Dolt auth using projected environment values.
func ResolveFromEnv(scopeRoot, fallbackUser string, env map[string]string) Resolved {
	host := strings.TrimSpace(env["GC_DOLT_HOST"])
	port, ok := projectedPort(env)
	if host == "" && ok {
		host = "127.0.0.1"
	}
	if !ok {
		port = 0
	}
	overridePath := strings.TrimSpace(env["BEADS_CREDENTIALS_FILE"])
	if overridePath == "" {
		overridePath = strings.TrimSpace(os.Getenv("BEADS_CREDENTIALS_FILE"))
	}
	envPass := strings.TrimSpace(env["BEADS_DOLT_PASSWORD"])
	return Resolved{
		User:                    resolveUser(fallbackUser),
		Password:                resolvePasswordWithEnv(envPass, scopeRoot, host, port, overridePath),
		CredentialsFileOverride: overridePath,
	}
}

func resolveUser(fallbackUser string) string {
	if user := strings.TrimSpace(os.Getenv("GC_DOLT_USER")); user != "" {
		return user
	}
	return strings.TrimSpace(fallbackUser)
}

func resolvePassword(scopeRoot, host string, port int, overridePath string) string {
	return resolvePasswordWithEnv("", scopeRoot, host, port, overridePath)
}

func resolvePasswordWithEnv(envPass, scopeRoot, host string, port int, overridePath string) string {
	if pass := strings.TrimSpace(os.Getenv("GC_DOLT_PASSWORD")); pass != "" {
		return pass
	}
	if pass := ReadStoreLocalPassword(scopeRoot); pass != "" {
		return pass
	}
	if envPass != "" {
		return envPass
	}
	if pass := strings.TrimSpace(os.Getenv("BEADS_DOLT_PASSWORD")); pass != "" {
		return pass
	}
	host = strings.TrimSpace(host)
	if host == "" || port <= 0 {
		return ""
	}
	lookupPath := overridePath
	if lookupPath == "" {
		lookupPath = DefaultCredentialsPath()
	}
	if lookupPath == "" {
		return ""
	}
	return ReadCredentialsPassword(lookupPath, host, port)
}

// ReadStoreLocalPassword returns the BEADS_DOLT_PASSWORD from a scope-local .beads/.env file.
func ReadStoreLocalPassword(scopeRoot string) string {
	if strings.TrimSpace(scopeRoot) == "" {
		return ""
	}
	return readSimpleEnvValue(filepath.Join(scopeRoot, ".beads", ".env"), "BEADS_DOLT_PASSWORD")
}

func readSimpleEnvValue(path, key string) string {
	f, err := os.Open(path) //nolint:gosec // path is derived from scope roots
	if err != nil {
		return ""
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
	return value
}

func projectedPort(env map[string]string) (int, bool) {
	port := strings.TrimSpace(env["GC_DOLT_PORT"])
	if port == "" {
		return 0, false
	}
	value, err := strconv.Atoi(port)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}

// DefaultCredentialsPath returns the default beads credentials file path for the current OS.
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

// ReadCredentialsPassword returns the password for the given host:port from a beads credentials file.
func ReadCredentialsPassword(path, host string, port int) string {
	f, err := os.Open(path) //nolint:gosec // path comes from env or os.UserHomeDir
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	sectionKey := host + ":" + strconv.Itoa(port)
	inSection := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := line[1 : len(line)-1]
			if section == sectionKey {
				inSection = true
			} else if inSection {
				break
			}
			continue
		}
		if !inSection {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "password" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
