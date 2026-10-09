package credsource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secret = "s3cr3t-T0KEN-value"

func TestParseForms(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want Source
	}{
		{"", Source{}},
		{"env:CITY_TOKEN", Source{Kind: KindEnv, Env: "CITY_TOKEN"}},
		{" env: CITY_TOKEN ", Source{Kind: KindEnv, Env: "CITY_TOKEN"}},
		{`command:vault read -field=token "secret/gc city"`, Source{Kind: KindCommand, Argv: []string{"vault", "read", "-field=token", "secret/gc city"}}},
		{"file:/run/secrets/beads", Source{Kind: KindFile, Path: "/run/secrets/beads"}},
		{"file:.gc/beads-token", Source{Kind: KindFile, Path: ".gc/beads-token"}},
	} {
		got, err := Parse(tc.raw)
		if err != nil {
			t.Errorf("Parse(%q) error = %v", tc.raw, err)
			continue
		}
		if got.Key() != tc.want.Key() {
			t.Errorf("Parse(%q) = %#v, want %#v", tc.raw, got, tc.want)
		}
	}
}

// TestParseRefusesInlineSecretsWithoutEchoingThem: anything that is not a
// source form is refused, and the refusal never carries the value.
func TestParseRefusesInlineSecretsWithoutEchoingThem(t *testing.T) {
	for _, raw := range []string{
		secret,
		"Bearer " + secret,
		"token:" + secret,
		"env:" + secret + "-has-dashes",
		"env:",
		"command:",
		`command:"unbalanced ` + secret,
		"file:",
	} {
		_, err := Parse(raw)
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("Parse(%q) error = %v, want ErrMalformed", raw, err)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("Parse error %q echoes the value", err)
		}
	}
}

func TestStringRedactsCommandArguments(t *testing.T) {
	src, err := Parse("command:helper --token " + secret)
	if err != nil {
		t.Fatal(err)
	}
	if got := src.String(); got != "command:helper" {
		t.Fatalf("String() = %q, want only the program", got)
	}
}

func TestResolveEnvFileAndCommand(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tok"), []byte("  file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "helper.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho command-token\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	opts := ResolveOptions{Getenv: func(k string) string {
		if k == "CITY_TOKEN" {
			return "env-token"
		}
		return ""
	}, Dir: dir}
	for raw, want := range map[string]string{
		"env:CITY_TOKEN":    "env-token",
		"file:tok":          "file-token",
		"command:" + script: "command-token",
	} {
		src, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, err := src.Resolve(context.Background(), opts)
		if err != nil || got != want {
			t.Errorf("Resolve(%s) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

// TestResolveErrorsAreTypedAndNameTheSourceWithoutTheValue covers every
// failure class: each is a *ResolveError naming the (redacted) source, and no
// error carries a token, a helper's stdout or its stderr.
func TestResolveErrorsAreTypedAndNameTheSourceWithoutTheValue(t *testing.T) {
	dir := t.TempDir()
	failing := filepath.Join(dir, "failing.sh")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\necho "+secret+"\necho "+secret+" >&2\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spaced := filepath.Join(dir, "spaced")
	if err := os.WriteFile(spaced, []byte(secret+" "+secret), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		raw     string
		want    error
		wantSrc string
	}{
		{"env:UNSET_CITY_TOKEN", ErrEmpty, "env:UNSET_CITY_TOKEN"},
		{"file:" + empty, ErrEmpty, "file:" + empty},
		{"file:" + filepath.Join(dir, "missing"), ErrUnreadable, "file:"},
		{"file:" + spaced, ErrInvalidToken, "file:" + spaced},
		{"command:" + failing + " --arg " + secret, ErrCommandFailed, "command:" + failing},
	} {
		src, err := Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		_, err = src.Resolve(context.Background(), ResolveOptions{Getenv: func(string) string { return "" }, Dir: dir})
		var resolveErr *ResolveError
		if !errors.As(err, &resolveErr) || !errors.Is(err, tc.want) {
			t.Errorf("Resolve(%s) error = %v, want a *ResolveError wrapping %v", src, err, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSrc) {
			t.Errorf("error %q does not name the source %q", err, tc.wantSrc)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q leaks the credential", err)
		}
	}
}
