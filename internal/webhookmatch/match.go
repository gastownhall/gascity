package webhookmatch

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// EventWildcard is the rule.Event value that matches any delivery event type.
const EventWildcard = "*"

// MatchInput is one verified delivery presented to the matcher. The receiver
// populates it from the E4 [webhookverify.VerifyResult] plus the parsed body.
// EventType is supplied by the caller — from the verified event header, or a
// caller-derived value for schemes that carry the type in the payload; this
// package does not read request headers.
type MatchInput struct {
	// EventType is the resolved provider event type (e.g. "pull_request").
	EventType string
	// DedupID is the stable per-delivery id, surfaced via the @delivery token.
	DedupID string
	// Identity is the verified principal (jwt-jwks subject), via @identity.
	Identity string
	// Body is the parsed JSON body. Build it with [ParseBody] so numbers keep
	// their exact source text.
	Body map[string]any
}

// MatchResult is the resolved dispatch intent for the first matching rule.
type MatchResult struct {
	// Rule is the matched rule, carried whole for the sink layer.
	Rule config.WebhookRule
	// RuleIndex is the matched rule's position in the input slice.
	RuleIndex int
	// Target is the normalized sink: "order" or "conversation". E6/E7 route on
	// it; E5 only carries it through.
	Target string
	// Order is the target order name (empty for conversation rules).
	Order string
	// Rig optionally scopes the dispatched order.
	Rig string
	// Vars maps each declared arg name to its extracted string value, in the
	// order-param namespace. Route through [ExecEnvVars] before overlaying an
	// exec order's environment.
	Vars map[string]string
	// Reason is a short human-readable explanation of the match.
	Reason string
}

// Match selects the first rule whose Event and Match predicate both hold for
// input, extracts that rule's args, and returns the resolved [MatchResult]. The
// second return is false when no rule matches (Reason on the zero result
// explains why). An error is returned only when a matched rule's arg extraction
// fails structurally (e.g. a value that cannot be encoded) — never for a plain
// no-match or a missing payload path.
func Match(input MatchInput, rules []config.WebhookRule) (MatchResult, bool, error) {
	for i, rule := range rules {
		if !eventMatches(rule.Event, input.EventType) {
			continue
		}
		if !matchPredicate(rule.Match, input.Body) {
			continue
		}
		vars, err := extractArgs(rule, input)
		if err != nil {
			return MatchResult{}, false, fmt.Errorf("webhookmatch: rule[%d]: %w", i, err)
		}
		return MatchResult{
			Rule:      rule,
			RuleIndex: i,
			Target:    rule.TargetOrDefault(),
			Order:     rule.Order,
			Rig:       rule.Rig,
			Vars:      vars,
			Reason:    fmt.Sprintf("rule[%d] matched event %q", i, rule.Event),
		}, true, nil
	}
	return MatchResult{Reason: fmt.Sprintf("no rule matched event %q", input.EventType)}, false, nil
}

// eventMatches reports whether a rule's Event selector matches the delivery
// event type: the "*" wildcard matches any event, otherwise exact equality.
func eventMatches(ruleEvent, eventType string) bool {
	re := strings.TrimSpace(ruleEvent)
	if re == EventWildcard {
		return true
	}
	return re == eventType
}

// matchPredicate reports whether every dotted-path entry in match holds against
// body. A missing path never matches. An empty/nil match map is vacuously true.
func matchPredicate(match map[string]string, body map[string]any) bool {
	for path, expected := range match {
		actual, ok := resolvePath(body, path)
		if !ok {
			return false
		}
		s, err := coerceToString(actual)
		if err != nil {
			return false
		}
		if !valueMatches(s, expected) {
			return false
		}
	}
	return true
}

// valueMatches compares a coerced payload value against an expected match value.
// It is deliberately minimal and injection-free: "*" is a presence check (the
// path already resolved), a trailing "*" is a literal prefix match, and every
// other value is exact string equality.
func valueMatches(actual, expected string) bool {
	if expected == EventWildcard {
		return true
	}
	if strings.HasSuffix(expected, EventWildcard) {
		return strings.HasPrefix(actual, strings.TrimSuffix(expected, EventWildcard))
	}
	return actual == expected
}
