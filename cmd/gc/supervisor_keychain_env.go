package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
)

// supervisorKeychainEnvVar maps supervisor env keys to macOS Keychain items,
// as whitespace/comma/semicolon-separated KEY=SERVICE pairs, e.g.
// "AWS_BEARER_TOKEN_BEDROCK=claude-bedrock-token". The service file persists
// the mapping but never the mapped values; `gc supervisor run` reads each
// value from the generic-password item (account $USER, service SERVICE) at
// startup, so sessions inherit it without the secret ever landing on disk.
const supervisorKeychainEnvVar = "GC_SUPERVISOR_KEYCHAIN_ENV"

// supervisorKeychainEnvEntry is one KEY=SERVICE mapping from
// GC_SUPERVISOR_KEYCHAIN_ENV.
type supervisorKeychainEnvEntry struct {
	Key     string
	Service string
}

// parseSupervisorKeychainEnv parses a GC_SUPERVISOR_KEYCHAIN_ENV value. Each
// malformed, fixed-key, or duplicate-key pair is returned as an error and
// skipped; the valid pairs are returned in order.
func parseSupervisorKeychainEnv(raw string) ([]supervisorKeychainEnvEntry, []error) {
	fields := strings.Fields(strings.NewReplacer(",", " ", ";", " ").Replace(raw))
	var (
		entries []supervisorKeychainEnvEntry
		errs    []error
	)
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		key, service, ok := strings.Cut(field, "=")
		switch {
		case !ok || key == "" || service == "":
			errs = append(errs, fmt.Errorf("%s entry %q: want KEY=SERVICE", supervisorKeychainEnvVar, field))
		case !supervisorServiceEnvNameRE.MatchString(key):
			errs = append(errs, fmt.Errorf("%s entry %q: invalid env var name", supervisorKeychainEnvVar, field))
		case supervisorServiceFixedEnvKeys[key]:
			errs = append(errs, fmt.Errorf("%s entry %q: %s is managed by gc and cannot come from the Keychain", supervisorKeychainEnvVar, field, key))
		case seen[key]:
			errs = append(errs, fmt.Errorf("%s entry %q: %s is already mapped", supervisorKeychainEnvVar, field, key))
		default:
			seen[key] = true
			entries = append(entries, supervisorKeychainEnvEntry{Key: key, Service: service})
		}
	}
	return entries, errs
}

// supervisorKeychainSourcedKeys returns the env keys the service file must not
// persist because GC_SUPERVISOR_KEYCHAIN_ENV sources them from the Keychain.
// Parse errors are reported by loadSupervisorKeychainEnv at supervisor
// startup, where they take effect, so they are not repeated here.
func supervisorKeychainSourcedKeys() map[string]bool {
	entries, _ := parseSupervisorKeychainEnv(os.Getenv(supervisorKeychainEnvVar))
	keys := make(map[string]bool, len(entries))
	for _, entry := range entries {
		keys[entry.Key] = true
	}
	return keys
}

// supervisorKeychainLookup reads a generic-password item's secret. It is a
// variable so tests can substitute a fake Keychain.
var supervisorKeychainLookup = readMacOSKeychainPassword

func readMacOSKeychainPassword(account, service string) (string, error) {
	if goruntime.GOOS != "darwin" {
		return "", fmt.Errorf("the macOS Keychain is not available on %s", goruntime.GOOS)
	}
	out, err := exec.Command("/usr/bin/security", "find-generic-password", "-a", account, "-s", service, "-w").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", err
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

// loadSupervisorKeychainEnv sets each GC_SUPERVISOR_KEYCHAIN_ENV key in this
// process's env from its Keychain item, leaving keys that already have a
// non-empty value alone. Failures are reported on stderr and leave the key
// unset rather than stopping the supervisor: the cities it runs may not all
// depend on the credential.
func loadSupervisorKeychainEnv(stderr io.Writer) {
	raw := os.Getenv(supervisorKeychainEnvVar)
	if strings.TrimSpace(raw) == "" {
		return
	}
	entries, errs := parseSupervisorKeychainEnv(raw)
	for _, err := range errs {
		fmt.Fprintf(stderr, "gc supervisor: %v\n", err) //nolint:errcheck // best-effort stderr
	}
	account := strings.TrimSpace(os.Getenv("USER"))
	for _, entry := range entries {
		if os.Getenv(entry.Key) != "" {
			continue
		}
		if account == "" {
			fmt.Fprintf(stderr, "gc supervisor: cannot load %s from Keychain item %q: USER is not set\n", entry.Key, entry.Service) //nolint:errcheck // best-effort stderr
			continue
		}
		val, err := supervisorKeychainLookup(account, entry.Service)
		if err == nil && val == "" {
			err = errors.New("item is empty")
		}
		if err != nil {
			fmt.Fprintf(stderr, "gc supervisor: loading %s from Keychain item %q (account %q): %v\n", entry.Key, entry.Service, account, err) //nolint:errcheck // best-effort stderr
			continue
		}
		if err := os.Setenv(entry.Key, val); err != nil {
			fmt.Fprintf(stderr, "gc supervisor: setting %s from Keychain item %q: %v\n", entry.Key, entry.Service, err) //nolint:errcheck // best-effort stderr
		}
	}
}
