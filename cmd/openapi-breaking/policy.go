package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// policy is the checked-in breaking-change policy
// (internal/api/openapi-breaking.toml).
type policy struct {
	Severity []severityOverride `toml:"severity"`
	Waivers  []waiver           `toml:"waiver"`
}

// severityOverride re-levels one oasdiff check.
type severityOverride struct {
	Check  string `toml:"check"`
	Level  string `toml:"level"`
	Reason string `toml:"reason"`
}

// waiver acknowledges one intentional breaking change.
type waiver struct {
	Fingerprint string `toml:"fingerprint"`
	Check       string `toml:"check"`
	Target      string `toml:"target"`
	Reason      string `toml:"reason"`
	PR          string `toml:"pr"`
}

var (
	fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)
	prPattern          = regexp.MustCompile(`^(#[0-9]+|https://github\.com/[^/\s]+/[^/\s]+/pull/[0-9]+)$`)
	validLevels        = map[string]bool{"err": true, "warn": true, "info": true, "none": true}
)

func loadPolicy(path string) (policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return policy{}, fmt.Errorf("reading policy %s: %w", path, err)
	}
	p, err := parsePolicy(data)
	if err != nil {
		return policy{}, fmt.Errorf("policy %s: %w", path, err)
	}
	return p, nil
}

func parsePolicy(data []byte) (policy, error) {
	var p policy
	md, err := toml.Decode(string(data), &p)
	if err != nil {
		return policy{}, fmt.Errorf("parsing: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return policy{}, fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
	}
	if err := p.validate(); err != nil {
		return policy{}, err
	}
	return p, nil
}

func (p policy) validate() error {
	seenChecks := map[string]bool{}
	for i, s := range p.Severity {
		where := fmt.Sprintf("severity[%d]", i)
		if s.Check == "" || s.Reason == "" {
			return fmt.Errorf("%s: check and reason are required", where)
		}
		if !validLevels[s.Level] {
			return fmt.Errorf("%s (%s): level %q must be one of err, warn, info, none", where, s.Check, s.Level)
		}
		if seenChecks[s.Check] {
			return fmt.Errorf("%s: duplicate severity override for %q", where, s.Check)
		}
		seenChecks[s.Check] = true
	}
	seenFingerprints := map[string]bool{}
	for i, w := range p.Waivers {
		where := fmt.Sprintf("waiver[%d]", i)
		var missing []string
		for _, f := range []struct{ name, value string }{
			{"fingerprint", w.Fingerprint},
			{"check", w.Check},
			{"target", w.Target},
			{"reason", w.Reason},
			{"pr", w.PR},
		} {
			if strings.TrimSpace(f.value) == "" {
				missing = append(missing, f.name)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("%s: missing required fields: %s", where, strings.Join(missing, ", "))
		}
		if !fingerprintPattern.MatchString(w.Fingerprint) {
			return fmt.Errorf("%s: fingerprint %q must be 12 lowercase hex characters", where, w.Fingerprint)
		}
		if !prPattern.MatchString(w.PR) {
			return fmt.Errorf("%s (%s): pr %q must be #N or a https://github.com/<owner>/<repo>/pull/N URL", where, w.Fingerprint, w.PR)
		}
		if seenFingerprints[w.Fingerprint] {
			return fmt.Errorf("%s: duplicate waiver for fingerprint %s", where, w.Fingerprint)
		}
		seenFingerprints[w.Fingerprint] = true
	}
	return nil
}

// severityLevelsFile renders the overrides in oasdiff's --severity-levels
// format ("<check> <level>" per line; the format has no comments, which is
// why the reasons live in the TOML policy instead).
func (p policy) severityLevelsFile() string {
	var b strings.Builder
	for _, s := range p.Severity {
		fmt.Fprintf(&b, "%s %s\n", s.Check, s.Level)
	}
	return b.String()
}
