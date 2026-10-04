package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// stubSupervisorKeychainLookup swaps the Keychain reader for the test's
// lifetime and records every (account, service) pair it was asked for.
func stubSupervisorKeychainLookup(t *testing.T, fn func(account, service string) (string, error)) *[]string {
	t.Helper()
	var calls []string
	orig := supervisorKeychainLookup
	supervisorKeychainLookup = func(account, service string) (string, error) {
		calls = append(calls, account+"/"+service)
		return fn(account, service)
	}
	t.Cleanup(func() { supervisorKeychainLookup = orig })
	return &calls
}

func TestParseSupervisorKeychainEnv(t *testing.T) {
	got, errs := parseSupervisorKeychainEnv(
		"AWS_BEARER_TOKEN_BEDROCK=claude-bedrock-token, OTHER_TOKEN=other-item;" +
			" no-equals BAD-NAME=item EMPTY_SERVICE= =no-key PATH=path-item" +
			" AWS_BEARER_TOKEN_BEDROCK=duplicate-item")

	want := []supervisorKeychainEnvEntry{
		{Key: "AWS_BEARER_TOKEN_BEDROCK", Service: "claude-bedrock-token"},
		{Key: "OTHER_TOKEN", Service: "other-item"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %#v, want %#v", got, want)
	}
	var msgs []string
	for _, err := range errs {
		msgs = append(msgs, err.Error())
	}
	joined := strings.Join(msgs, "\n")
	for _, bad := range []string{"no-equals", "BAD-NAME=item", "EMPTY_SERVICE=", "=no-key", "PATH=path-item", "AWS_BEARER_TOKEN_BEDROCK=duplicate-item"} {
		if !strings.Contains(joined, "\""+bad+"\"") {
			t.Errorf("no error reported for %q; errors:\n%s", bad, joined)
		}
	}
}

func TestParseSupervisorKeychainEnvEmpty(t *testing.T) {
	got, errs := parseSupervisorKeychainEnv("  ")
	if len(got) != 0 || len(errs) != 0 {
		t.Fatalf("parse(blank) = %#v, %v; want nothing", got, errs)
	}
}

// TestBuildSupervisorServiceDataOmitsKeychainSourcedValues is the point of
// the feature: a credential mapped to a Keychain item must never be written
// into the service file, even though it is exported in the calling shell and
// would otherwise persist as a provider credential. The mapping itself must
// persist, so the service-managed supervisor knows what to load.
func TestBuildSupervisorServiceDataOmitsKeychainSourcedValues(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "secret-from-shell")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-not-mapped")
	t.Setenv(supervisorKeychainEnvVar, "AWS_BEARER_TOKEN_BEDROCK=claude-bedrock-token")
	writeSupervisorSecretsEnvFile(t, "AWS_BEARER_TOKEN_BEDROCK=secret-from-file\n")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}

	got := supervisorServiceEnvMap(data.ExtraEnv)
	if v, ok := got["AWS_BEARER_TOKEN_BEDROCK"]; ok {
		t.Fatalf("ExtraEnv persisted Keychain-sourced AWS_BEARER_TOKEN_BEDROCK=%q", v)
	}
	if got[supervisorKeychainEnvVar] != "AWS_BEARER_TOKEN_BEDROCK=claude-bedrock-token" {
		t.Fatalf("ExtraEnv[%s] = %q, want the mapping persisted (all env: %#v)", supervisorKeychainEnvVar, got[supervisorKeychainEnvVar], got)
	}
	if got["ANTHROPIC_AUTH_TOKEN"] != "sk-not-mapped" {
		t.Fatalf("unmapped provider credential should still persist; ExtraEnv[ANTHROPIC_AUTH_TOKEN] = %q", got["ANTHROPIC_AUTH_TOKEN"])
	}
}

func TestLoadSupervisorKeychainEnvSetsUnsetKeys(t *testing.T) {
	t.Setenv("USER", "operator")
	t.Setenv(supervisorKeychainEnvVar, "AWS_BEARER_TOKEN_BEDROCK=claude-bedrock-token")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	calls := stubSupervisorKeychainLookup(t, func(string, string) (string, error) {
		return "token-from-keychain", nil
	})

	var stderr bytes.Buffer
	loadSupervisorKeychainEnv(&stderr)

	if got := os.Getenv("AWS_BEARER_TOKEN_BEDROCK"); got != "token-from-keychain" {
		t.Fatalf("AWS_BEARER_TOKEN_BEDROCK = %q, want the Keychain value", got)
	}
	if want := []string{"operator/claude-bedrock-token"}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("lookups = %v, want %v", *calls, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestLoadSupervisorKeychainEnvKeepsExplicitValue(t *testing.T) {
	t.Setenv("USER", "operator")
	t.Setenv(supervisorKeychainEnvVar, "AWS_BEARER_TOKEN_BEDROCK=claude-bedrock-token")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "already-set")
	calls := stubSupervisorKeychainLookup(t, func(string, string) (string, error) {
		return "token-from-keychain", nil
	})

	loadSupervisorKeychainEnv(io.Discard)

	if got := os.Getenv("AWS_BEARER_TOKEN_BEDROCK"); got != "already-set" {
		t.Fatalf("AWS_BEARER_TOKEN_BEDROCK = %q, want the explicit value kept", got)
	}
	if len(*calls) != 0 {
		t.Fatalf("Keychain should not be read for an already-set key; lookups = %v", *calls)
	}
}

func TestLoadSupervisorKeychainEnvReportsFailures(t *testing.T) {
	t.Setenv("USER", "operator")
	t.Setenv(supervisorKeychainEnvVar, "MISSING_TOKEN=missing-item EMPTY_TOKEN=empty-item bad-entry")
	for _, key := range []string{"MISSING_TOKEN", "EMPTY_TOKEN"} {
		t.Setenv(key, "")
	}
	stubSupervisorKeychainLookup(t, func(_, service string) (string, error) {
		if service == "missing-item" {
			return "", errors.New("item not found")
		}
		return "", nil
	})

	var stderr bytes.Buffer
	loadSupervisorKeychainEnv(&stderr)

	for _, key := range []string{"MISSING_TOKEN", "EMPTY_TOKEN"} {
		if v := os.Getenv(key); v != "" {
			t.Fatalf("%s = %q after a failed lookup, want it left empty", key, v)
		}
	}
	out := stderr.String()
	for _, want := range []string{
		`MISSING_TOKEN from Keychain item "missing-item"`,
		"item not found",
		`EMPTY_TOKEN from Keychain item "empty-item"`,
		`"bad-entry"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q:\n%s", want, out)
		}
	}
}

func TestLoadSupervisorKeychainEnvRequiresUser(t *testing.T) {
	t.Setenv("USER", "")
	t.Setenv(supervisorKeychainEnvVar, "AWS_BEARER_TOKEN_BEDROCK=claude-bedrock-token")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	calls := stubSupervisorKeychainLookup(t, func(string, string) (string, error) {
		return "token-from-keychain", nil
	})

	var stderr bytes.Buffer
	loadSupervisorKeychainEnv(&stderr)

	if len(*calls) != 0 {
		t.Fatalf("Keychain read without an account; lookups = %v", *calls)
	}
	if !strings.Contains(stderr.String(), "USER") {
		t.Fatalf("stderr should explain the missing USER:\n%s", stderr.String())
	}
}

// TestDoSupervisorRunLoadsKeychainEnv covers the call site: the Keychain
// values must be in the process env before the run loop starts, because
// sessions and bd subprocesses inherit that env.
func TestDoSupervisorRunLoadsKeychainEnv(t *testing.T) {
	t.Setenv("USER", "operator")
	t.Setenv(supervisorKeychainEnvVar, "AWS_BEARER_TOKEN_BEDROCK=claude-bedrock-token")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	stubSupervisorKeychainLookup(t, func(string, string) (string, error) {
		return "token-from-keychain", nil
	})

	origRun := runSupervisorFunc
	var seen string
	runSupervisorFunc = func(io.Writer, io.Writer) int {
		seen = os.Getenv("AWS_BEARER_TOKEN_BEDROCK")
		return 0
	}
	t.Cleanup(func() { runSupervisorFunc = origRun })

	if rc := doSupervisorRun(io.Discard, io.Discard); rc != 0 {
		t.Fatalf("doSupervisorRun = %d, want 0", rc)
	}
	if seen != "token-from-keychain" {
		t.Fatalf("run loop saw AWS_BEARER_TOKEN_BEDROCK = %q, want the Keychain value", seen)
	}
}
