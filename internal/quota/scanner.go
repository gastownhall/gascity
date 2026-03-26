package quota

import (
	"regexp"
	"strings"
)

// Scanner checks session output for rate-limit indicators using
// provider-specific regex patterns.
type Scanner struct {
	// patterns maps provider name to compiled regex patterns.
	patterns map[string][]*regexp.Regexp
}

// NewScanner creates a scanner from provider rate-limit patterns.
// The patterns map keys are provider names and values are regex strings.
func NewScanner(providerPatterns map[string][]string) *Scanner {
	s := &Scanner{
		patterns: make(map[string][]*regexp.Regexp, len(providerPatterns)),
	}
	for provider, pats := range providerPatterns {
		compiled := make([]*regexp.Regexp, 0, len(pats))
		for _, p := range pats {
			if re, err := regexp.Compile(p); err == nil {
				compiled = append(compiled, re)
			}
		}
		if len(compiled) > 0 {
			s.patterns[provider] = compiled
		}
	}
	return s
}

// Scan checks whether output contains any rate-limit pattern for the given
// provider. Returns true if a rate-limit indicator is found.
func (s *Scanner) Scan(output string, provider string) bool {
	pats, ok := s.patterns[provider]
	if !ok {
		return false
	}
	// Scan line by line for efficiency on large outputs.
	for _, line := range strings.Split(output, "\n") {
		for _, re := range pats {
			if re.MatchString(line) {
				return true
			}
		}
	}
	return false
}
