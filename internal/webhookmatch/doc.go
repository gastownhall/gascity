// Package webhookmatch maps a verified webhook delivery to a dispatch target.
//
// It is the E5 stage of the supervisor webhook receiver: pure match + arg
// extraction, with no HTTP, no goroutines, no config-file IO, and no dispatch.
// The receiver verifies a delivery (E4), parses its JSON body, and hands this
// package the resolved event type, delivery metadata, the parsed body, and the
// webhook's ordered [config.WebhookRule] list. This package answers two
// questions and nothing more:
//
//  1. Which rule (if any) does this delivery match? — [Match]
//  2. What named args does that rule extract from the body? — the returned
//     [MatchResult.Vars]
//
// The order/conversation sink split (E6/E7) is downstream; [Match] carries the
// rule's Target through so the caller can route, but never dispatches.
//
// # Match semantics
//
// Rules are evaluated in declaration order; the first match wins (TOML order is
// significant, exactly as the github-intake matching_rules prototype behaves).
// A rule matches when both hold:
//
//   - Event: rule.Event equals the delivery EventType, or rule.Event is the
//     wildcard "*" (any event).
//   - Match: every entry of rule.Match holds. A key is a dotted path into the
//     body (e.g. "action", "pull_request.state", "issue.labels.0.name"); the
//     value at that path is coerced to a string and compared to the expected
//     string. A missing path never matches (and never panics). Comparison is
//     exact-equality by default, with two minimal, predictable extensions and
//     no regex/injection surface:
//   - expected "*" matches any present value (presence check);
//   - expected ending in "*" (e.g. "status/*") is a literal prefix match
//     via strings.HasPrefix over the text before the star.
//
// # Extraction syntax
//
// Each rule.Args entry maps an arg name to a value template. A template is
// literal text with zero or more "{{ token }}" placeholders (the github-intake
// {{payload.path}} scheme). Whitespace inside the braces is trimmed. A token
// resolves as:
//
//   - "@event"           -> the delivery EventType
//   - "@delivery"/"@dedup" -> the delivery DedupID
//   - "@identity"        -> the verified Identity (jwt-jwks subject, else empty)
//   - any other token    -> a dotted path into the body, coerced to a string
//
// A missing body path or unknown "@" token resolves to the empty string (the
// reference render_template's behavior); an unterminated "{{" is emitted
// literally. Values are coerced deterministically: strings verbatim, JSON
// numbers by their exact source text (body is parsed with UseNumber, so
// pull_request.number renders "1347", not "1347.000000"), bools as
// "true"/"false", JSON null as "", and nested objects/arrays as their compact
// JSON encoding. The arg names come from operator-installed pack TOML, but the
// values come from the untrusted payload — so an extracted value is always an
// opaque string. It is never re-interpreted as a path, template, or command; a
// value of "$(id)" or "; rm -rf /" is just those literal bytes.
//
// # R4 — arg namespacing + reserved-key guard (security review)
//
// [MatchResult.Vars] is keyed by the raw declared arg name, which is the order
// param namespace consumed by orders.Order.MissingRequiredParams and by the
// formula ExpandVars channel. For the exec-env overlay (where an un-namespaced
// key could shadow a controller-owned variable like GC_CITY), the caller must
// route vars through [ExecEnvVars], which prefixes every key with
// [ExecEnvArgPrefix] so a payload can never inject a control key. As a second
// line of defense, config.ValidateWebhooks rejects a rule arg whose name is a
// reserved controller env key at load time, and [Match] additionally skips any
// such name during extraction.
package webhookmatch
