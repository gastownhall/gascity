package gitcred

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/gchome"
)

// Env var names — the single source of truth referenced by cmd/gc.
const (
	// EnvCredentialsFile names an explicit rules file that REPLACES the city
	// and GC_HOME file layers when set.
	EnvCredentialsFile = "GC_GIT_CREDENTIALS_FILE"
	// EnvCredentialCommand names an external helper command used as a
	// last-resort layer when no file rule matches.
	EnvCredentialCommand = "GC_GIT_CREDENTIAL_COMMAND"
	// EnvCredentialCity is the non-secret city-root reference injection sets on
	// the git subprocess so the git-credential helper can re-load the city
	// layer.
	EnvCredentialCity = "GC_CREDENTIAL_CITY"
)

// credentialsFileName is the fixed basename of a per-city or per-home rules
// file.
const credentialsFileName = "credentials.toml"

// commandLayerOrigin is the LoadedRule.Origin marker for the command-layer
// fallback.
const commandLayerOrigin = "$" + EnvCredentialCommand

// ErrInsecurePermissions reports a credentials file readable by group or other.
var ErrInsecurePermissions = errors.New("credentials file is group/world accessible")

// Rule is one [[credential]] entry. Exactly one pointer field (Helper,
// TokenFile, TokenEnv, or SSHKeyFile) must be set.
type Rule struct {
	Match      string `toml:"match"`
	Username   string `toml:"username,omitempty"`
	Helper     string `toml:"helper,omitempty"`
	TokenFile  string `toml:"token_file,omitempty"`
	TokenEnv   string `toml:"token_env,omitempty"`
	SSHKeyFile string `toml:"ssh_key_file,omitempty"`
}

// LoadedRule pairs a Rule with the file (or command-layer marker) that declared
// it.
type LoadedRule struct {
	Rule
	// Origin is the absolute path of the declaring file, or
	// "$GC_GIT_CREDENTIAL_COMMAND" for the command-layer fallback.
	Origin string
	// httpsOnly withholds this rule from plaintext http transport. The built-in
	// ambient github.com token default sets it so a bearer token is never served
	// over http://; file and command rules leave it false.
	httpsOnly bool
}

// layer is one resolution tier's rules, kept separate so Match can apply
// layer-order-outer / longest-prefix-inner precedence.
type layer struct {
	rules []LoadedRule
}

// Rules is the resolved, layered credential rule set. The zero value matches
// nothing.
type Rules struct {
	layers       []layer
	commandLayer bool
}

type credentialsFile struct {
	Credential []map[string]any `toml:"credential"`
}

// Load resolves credential rules from the layered sources, in order:
//
//  1. $GC_GIT_CREDENTIALS_FILE — when set, REPLACES the city and GC_HOME file
//     layers.
//  2. <cityRoot>/.gc/credentials.toml — skipped when cityRoot is "".
//  3. $GC_HOME/credentials.toml — gchome.Default().
//  4. $GC_GIT_CREDENTIAL_COMMAND — recorded as a rule-less fallback layer.
//
// Every file present must be 0600/0400 (no group/other bits; the check is
// skipped on Windows) or Load returns ErrInsecurePermissions wrapping the path.
// Missing files are not errors. A literal "token"/"password" key, or a rule
// with zero or more than one pointer field, is a hard parse error.
func Load(cityRoot string) (*Rules, error) {
	rules := &Rules{}

	if explicit := strings.TrimSpace(os.Getenv(EnvCredentialsFile)); explicit != "" {
		lyr, err := loadFileLayer(explicit)
		if err != nil {
			return nil, err
		}
		if lyr != nil {
			rules.layers = append(rules.layers, *lyr)
		}
	} else {
		if strings.TrimSpace(cityRoot) != "" {
			cityFile := filepath.Join(cityRoot, ".gc", credentialsFileName)
			lyr, err := loadFileLayer(cityFile)
			if err != nil {
				return nil, err
			}
			if lyr != nil {
				rules.layers = append(rules.layers, *lyr)
			}
		}
		homeFile := filepath.Join(gchome.Default(), credentialsFileName)
		lyr, err := loadFileLayer(homeFile)
		if err != nil {
			return nil, err
		}
		if lyr != nil {
			rules.layers = append(rules.layers, *lyr)
		}
	}

	// A configured command layer (GC_GIT_CREDENTIAL_COMMAND) is a deliberate
	// operator choice and must outrank the built-in convenience default below,
	// so detect it first.
	commandLayerConfigured := strings.TrimSpace(os.Getenv(EnvCredentialCommand)) != ""
	rules.commandLayer = commandLayerConfigured

	// Built-in default layer, appended LAST so any explicit file rule for the
	// same host wins. It lets `gc` clone a private github.com pack over HTTPS
	// using the ambient GitHub token — the ubiquitous convention used by CI and
	// the gh CLI — without requiring a hand-written credentials.toml. Scoped
	// strictly to the github.com host so the token is never offered to any other
	// (possibly attacker-supplied) pack source, marked httpsOnly so the matcher
	// withholds it from plaintext http github.com sources (no bearer token over
	// cleartext), and skipped entirely when a command layer is configured so the
	// explicitly-configured helper keeps precedence over this ambient-token
	// convenience: the command layer is a no-rule fallback consulted only when no
	// rule matches, so a default rule here would otherwise silently shadow it.
	if lyr := githubDefaultTokenLayer(commandLayerConfigured); lyr != nil {
		rules.layers = append(rules.layers, *lyr)
	}
	return rules, nil
}

// githubDefaultTokenEnvVars are the environment variables, in priority order,
// that the built-in github.com default rule consults for a token. GH_TOKEN is
// the gh CLI convention and takes precedence: it is the usual cross-repo
// override — a broad PAT set alongside the workflow's repo-scoped GITHUB_TOKEN,
// which is the GitHub Actions auto-provisioned fallback.
var githubDefaultTokenEnvVars = []string{"GH_TOKEN", "GITHUB_TOKEN"}

// githubDefaultTokenLayer returns a single-rule layer that authenticates
// https://github.com clones from the ambient GitHub token, or nil when no such
// token is set (the byte-identical guarantee — no rule, no injection) or when a
// command layer is configured (which takes precedence). The rule binds to
// whichever env var is populated so the existing TokenEnv resolution path
// (resolve.go) reads it unchanged. It is marked httpsOnly so the matcher never
// offers the bearer token to a plaintext http github.com source, where it would
// travel in cleartext.
func githubDefaultTokenLayer(commandLayerConfigured bool) *layer {
	if commandLayerConfigured {
		return nil
	}
	for _, envVar := range githubDefaultTokenEnvVars {
		if strings.TrimSpace(os.Getenv(envVar)) == "" {
			continue
		}
		return &layer{rules: []LoadedRule{{
			Rule:      Rule{Match: "github.com", TokenEnv: envVar},
			Origin:    "$" + envVar,
			httpsOnly: true,
		}}}
	}
	return nil
}

// loadFileLayer reads and validates a single credentials file. A missing file
// yields (nil, nil).
func loadFileLayer(path string) (*layer, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading credentials file %q: %w", path, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s", ErrInsecurePermissions, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading credentials file %q: %w", path, err)
	}
	var file credentialsFile
	if _, err := toml.Decode(string(data), &file); err != nil {
		return nil, fmt.Errorf("parsing credentials file %q: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	lyr := &layer{}
	for i, raw := range file.Credential {
		rule, err := ruleFromRaw(raw)
		if err != nil {
			return nil, fmt.Errorf("credentials file %q credential #%d: %w", path, i+1, err)
		}
		lyr.rules = append(lyr.rules, LoadedRule{Rule: rule, Origin: abs})
	}
	return lyr, nil
}

// ruleFromRaw converts a decoded [[credential]] table into a validated Rule. It
// rejects literal secret keys and enforces exactly-one-pointer cardinality.
func ruleFromRaw(raw map[string]any) (Rule, error) {
	for _, forbidden := range []string{"token", "password", "secret"} {
		if _, ok := raw[forbidden]; ok {
			return Rule{}, fmt.Errorf("literal %q key is not allowed; use a pointer (helper, token_file, token_env, or ssh_key_file)", forbidden)
		}
	}
	var rule Rule
	rule.Match = stringField(raw, "match")
	rule.Username = stringField(raw, "username")
	rule.Helper = stringField(raw, "helper")
	rule.TokenFile = stringField(raw, "token_file")
	rule.TokenEnv = stringField(raw, "token_env")
	rule.SSHKeyFile = stringField(raw, "ssh_key_file")
	if strings.TrimSpace(rule.Match) == "" {
		return Rule{}, errors.New("match is required")
	}
	pointers := 0
	for _, p := range []string{rule.Helper, rule.TokenFile, rule.TokenEnv, rule.SSHKeyFile} {
		if strings.TrimSpace(p) != "" {
			pointers++
		}
	}
	if pointers == 0 {
		return Rule{}, errors.New("no credential pointer set; exactly one of helper, token_file, token_env, or ssh_key_file is required")
	}
	if pointers > 1 {
		return Rule{}, errors.New("more than one credential pointer set; exactly one of helper, token_file, token_env, or ssh_key_file is required")
	}
	return rule, nil
}

func stringField(raw map[string]any, key string) string {
	if v, ok := raw[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// All returns every loaded rule, in layer order (highest-precedence layer
// first), for listing.
func (r *Rules) All() []LoadedRule {
	var out []LoadedRule
	for _, lyr := range r.layers {
		out = append(out, lyr.rules...)
	}
	return out
}

// HasCommandLayer reports whether $GC_GIT_CREDENTIAL_COMMAND supplied a
// last-resort fallback layer.
func (r *Rules) HasCommandLayer() bool {
	return r.commandLayer
}
