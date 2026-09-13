package webhookmatch

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

// ExecEnvArgPrefix namespaces webhook-extracted args when they are overlaid onto
// an exec order's environment. Prefixing every key makes it structurally
// impossible for a payload-derived arg to shadow a controller-owned variable
// (GC_CITY, BEADS_DIR, ...): the reserved keys never begin with this prefix.
const ExecEnvArgPrefix = "GC_WEBHOOK_ARG_"

// ExecEnvKey returns the exec-env variable name for a raw arg name.
func ExecEnvKey(argName string) string {
	return ExecEnvArgPrefix + argName
}

// ExecEnvVars returns a copy of vars with every key namespaced via
// [ExecEnvArgPrefix], for overlaying onto an exec order's environment. It
// returns nil for an empty input. The order sink (E6) must route exec-env vars
// through this rather than overlaying the raw [MatchResult.Vars], so a payload
// can never inject a control key. The raw vars are still handed to the formula
// ExpandVars channel and to Order.MissingRequiredParams, which key on the
// declared param name.
func ExecEnvVars(vars map[string]string) map[string]string {
	if len(vars) == 0 {
		return nil
	}
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		out[ExecEnvArgPrefix+k] = v
	}
	return out
}

// extractArgs renders every rule.Args template against the delivery, returning
// the raw-named vars map. A reserved controller env key is skipped as a second
// line of defense — config.ValidateWebhooks rejects such an arg at load time, so
// it should never reach here.
func extractArgs(rule config.WebhookRule, input MatchInput) (map[string]string, error) {
	if len(rule.Args) == 0 {
		return nil, nil
	}
	vars := make(map[string]string, len(rule.Args))
	for name, tmpl := range rule.Args {
		if orders.IsReservedExecEnvKey(name) {
			continue
		}
		val, err := renderTemplate(tmpl, input)
		if err != nil {
			return nil, fmt.Errorf("arg %q: %w", name, err)
		}
		vars[name] = val
	}
	if len(vars) == 0 {
		return nil, nil
	}
	return vars, nil
}

// renderTemplate expands "{{ token }}" placeholders in tmpl, preserving literal
// text. An unterminated "{{" is emitted literally (reference render_template
// parity). Tokens resolve via [resolveToken].
func renderTemplate(tmpl string, input MatchInput) (string, error) {
	var b strings.Builder
	rest := tmpl
	for {
		start := strings.Index(rest, "{{")
		if start < 0 {
			b.WriteString(rest)
			return b.String(), nil
		}
		b.WriteString(rest[:start])
		after := rest[start+2:]
		end := strings.Index(after, "}}")
		if end < 0 {
			// No closing braces: emit the remainder verbatim and stop.
			b.WriteString(rest[start:])
			return b.String(), nil
		}
		token := strings.TrimSpace(after[:end])
		val, err := resolveToken(token, input)
		if err != nil {
			return "", err
		}
		b.WriteString(val)
		rest = after[end+2:]
	}
}

// resolveToken resolves one template token to its string value. "@"-prefixed
// tokens read verified delivery metadata; every other token is a dotted body
// path. A missing body path or unknown metadata token resolves to "".
func resolveToken(token string, input MatchInput) (string, error) {
	if token == "" {
		return "", nil
	}
	if meta, ok := strings.CutPrefix(token, "@"); ok {
		switch meta {
		case "event":
			return input.EventType, nil
		case "delivery", "dedup":
			return input.DedupID, nil
		case "identity":
			return input.Identity, nil
		default:
			return "", nil
		}
	}
	v, ok := resolvePath(input.Body, token)
	if !ok {
		return "", nil
	}
	return coerceToString(v)
}
