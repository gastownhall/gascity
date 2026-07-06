package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/orders"
)

var (
	validWebhookName   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
	validWebhookArgKey = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	// validWebhookSecretEnv reuses the GitHubPRMonitor identifier shape
	// (github_pr_monitor.go): a webhook secret is referenced by env var name,
	// never stored inline in TOML.
	validWebhookSecretEnv = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// knownWebhookSchemes is the set of built-in verifier scheme identifiers. E2
// validates the string only; the verifier implementations land in E4.
var knownWebhookSchemes = map[string]bool{
	"github-hmac-sha256": true,
	"hmac-sha256":        true,
	"slack-v0":           true,
	"discord-ed25519":    true,
	"jwt-jwks":           true,
}

// hmacFamilyWebhookSchemes require a shared secret referenced via secret_env.
// discord-ed25519 (public key) and jwt-jwks (JWKS trust anchor) do not.
var hmacFamilyWebhookSchemes = map[string]bool{
	"github-hmac-sha256": true,
	"hmac-sha256":        true,
	"slack-v0":           true,
}

// Webhook declares a city- or rig-scoped inbound HTTP receiver mounted under
// /v0/city/{city}/hook/{name}. It mirrors the [[service]] declaration shape:
// generic publication intent plus pack provenance, so the same edge routing
// and pack-guard rules apply. The verifier and dispatch mechanics live in
// later phases (E3/E4/E5/E6); this type carries the config surface only.
type Webhook struct {
	// Name is the unique webhook identifier and mount segment.
	Name string `toml:"name" jsonschema:"required"`
	// Scope selects city- or rig-scoped dispatch semantics, mirroring
	// Order.Scope. Empty defaults to city.
	Scope string `toml:"scope,omitempty" jsonschema:"enum=city,enum=rig"`
	// Publication declares generic publication intent, reusing the service
	// publication contract. Pack/fragment-contributed public webhooks are
	// capped to tenant unless the city grants them via [webhooks].allow_public.
	Publication ServicePublicationConfig `toml:"publication,omitempty"`
	// Verify declares the signature verification scheme and its inputs.
	Verify WebhookVerify `toml:"verify,omitempty"`
	// Rules maps verified provider events to dispatch targets.
	Rules []WebhookRule `toml:"rule,omitempty"`
	// MaxPerMinute is an optional per-webhook self-imposed sustained request
	// ceiling for the E8 rate limiter. SECURITY: a [[webhook]] block may be
	// pack-contributed, and a pack must never be able to weaken the operator's
	// flood defense, so this value may only LOWER a webhook's effective limit —
	// it is min-clamped to the operator-owned ceiling and can never raise it (see
	// WebhookPolicyConfig.EffectiveRateLimit). Leave unset to inherit the operator
	// default/override.
	MaxPerMinute int `toml:"max_per_minute,omitempty"`
	// SourceDir records pack/fragment provenance for pack-stamped webhooks.
	// Empty means the webhook was authored directly in the root city.toml and
	// is therefore operator-trusted. Runtime-only; never authored in TOML.
	SourceDir string `toml:"-" json:"-"`
}

// WebhookVerify declares how an inbound delivery is authenticated. Fields only;
// the verification logic is E4. Secrets are always referenced by env var name
// (secret_env) and never stored inline.
type WebhookVerify struct {
	// Scheme selects the built-in verifier (see knownWebhookSchemes).
	Scheme string `toml:"scheme,omitempty"`
	// SecretEnv names the environment variable holding the HMAC/shared secret.
	SecretEnv string `toml:"secret_env,omitempty"`
	// SecretKey is an optional stable rotation-slot identifier. Empty defaults
	// to SecretEnv.
	SecretKey string `toml:"secret_key,omitempty"`
	// SignatureHeader overrides the request header carrying the signature for
	// generic HMAC schemes (e.g. X-Plane-Signature).
	SignatureHeader string `toml:"signature_header,omitempty"`
	// EventHeader names the request header carrying the provider event type.
	EventHeader string `toml:"event_header,omitempty"`
	// DedupHeader names the request header carrying the delivery id used for
	// at-least-once dedup.
	DedupHeader string `toml:"dedup_header,omitempty"`
	// TimestampHeader optionally names a request header carrying a signed
	// timestamp for replay defense.
	TimestampHeader string `toml:"timestamp_header,omitempty"`
	// ReplayWindow bounds the accepted signed-timestamp skew (Go duration).
	ReplayWindow string `toml:"replay_window,omitempty"`
	// Issuer, JWKSURL, and Audience pin the jwt-jwks trust anchor. Per the
	// security review (R1) these are operator-owned and must be declared in
	// city.toml, never in pack TOML.
	Issuer   string `toml:"issuer,omitempty"`
	JWKSURL  string `toml:"jwks_url,omitempty"`
	Audience string `toml:"audience,omitempty"`
	// BearerEnv optionally names an env var holding an additional per-source
	// bearer token checked alongside the signature.
	BearerEnv string `toml:"bearer_env,omitempty"`
	// AllowedCIDRs optionally restricts accepted source addresses (e.g. the
	// GitHub webhook CIDR allowlist).
	AllowedCIDRs []string `toml:"allowed_cidrs,omitempty"`
}

// WebhookRule maps one verified provider event to a dispatch target. Matching
// and arg extraction are E5; this type carries the declaration only.
type WebhookRule struct {
	// Event is the provider event type this rule matches (e.g. pull_request).
	Event string `toml:"event"`
	// Match is an exact-equality dotted-path predicate over the payload.
	Match map[string]string `toml:"match,omitempty"`
	// Order is the target order name for target="order" rules.
	Order string `toml:"order,omitempty"`
	// Rig optionally scopes the dispatched order to a rig.
	Rig string `toml:"rig,omitempty"`
	// Target selects the dispatch sink: "order" (default) or "conversation".
	Target string `toml:"target,omitempty" jsonschema:"enum=order,enum=conversation"`
	// Args maps declared order params to {{payload.path}} projections.
	Args map[string]string `toml:"args,omitempty"`
}

// WebhookPolicyConfig holds city-level webhook governance authored in the root
// city.toml under [webhooks]. It is intentionally never merged from packs or
// fragments so a pack cannot grant itself public exposure.
type WebhookPolicyConfig struct {
	// AllowPublic lists {name, source} grants that permit a pack/fragment
	// webhook to keep publication.visibility="public". Default-closed: a
	// pack/fragment public webhook with no matching grant is capped to tenant.
	AllowPublic []WebhookAllowPublic `toml:"allow_public,omitempty"`
	// JWTPolicies pins the operator-owned trust anchor for each jwt-jwks webhook,
	// keyed by webhook name. Per security review R1, a jwt-jwks webhook's issuer,
	// audience, and JWKS URL are operator-owned and must come from here (the root
	// city.toml [webhooks] block), never from a pack-authored [webhook.verify]
	// table — otherwise a pack could point the trust root at an attacker-controlled
	// issuer/JWKS. The receiver (E3) reads this, not WebhookVerify.Issuer/etc.,
	// when constructing the jwt-jwks verifier.
	JWTPolicies []WebhookJWTPolicy `toml:"jwt_policy,omitempty"`
	// RateLimit holds the operator-owned E8 per-webhook rate-limit governance:
	// the fleet default plus optional per-webhook overrides. Because the whole
	// [webhooks] table is never merged from packs or fragments, a pack cannot
	// touch these values — it can only LOWER its own limit via Webhook.MaxPerMinute
	// (clamped in EffectiveRateLimit). This is the trust boundary for the flood
	// defense: the operator sets the ceiling; packs may only self-restrict below it.
	//
	// A pointer so an absent [webhooks].rate_limit round-trips cleanly (a zero-value
	// nested table is not suppressed by BurntSushi's omitempty); nil means "use the
	// built-in defaults".
	RateLimit *WebhookRateLimitConfig `toml:"rate_limit,omitempty"`
}

// Built-in webhook rate-limit defaults, applied when the operator declares no
// [webhooks].rate_limit. A sustained few-per-second sustained rate with a burst
// covers legitimate provider delivery (GitHub can batch a handful at once) while
// capping a compromised-secret flood or a runaway sender.
const (
	defaultWebhookRateLimitPerMinute = 300
	defaultWebhookRateLimitBurst     = 60
)

// WebhookRateLimitConfig is the operator-owned rate-limit policy authored under
// the root city.toml [webhooks].rate_limit table. It is never composed from packs.
type WebhookRateLimitConfig struct {
	// PerMinute is the default sustained request ceiling applied to every webhook
	// that declares no lower self-limit. 0 uses defaultWebhookRateLimitPerMinute.
	PerMinute int `toml:"per_minute,omitempty"`
	// Burst is the token-bucket burst allowance. 0 uses defaultWebhookRateLimitBurst.
	Burst int `toml:"burst,omitempty"`
	// Overrides pins an operator-chosen limit for a specific webhook by name.
	// Operator authority: an override may raise OR lower that webhook's limit — it
	// is the operator, not a pack, declaring it. A pack's own MaxPerMinute can then
	// only clamp further downward, never above the override.
	Overrides []WebhookRateLimitOverride `toml:"override,omitempty"`
}

// WebhookRateLimitOverride is one operator-authored per-webhook rate-limit pin.
type WebhookRateLimitOverride struct {
	Name      string `toml:"name"`
	PerMinute int    `toml:"per_minute,omitempty"`
	Burst     int    `toml:"burst,omitempty"`
}

// override returns the operator-declared limit for the named webhook, if any.
func (c WebhookRateLimitConfig) override(name string) (WebhookRateLimitOverride, bool) {
	name = strings.TrimSpace(name)
	for _, o := range c.Overrides {
		if strings.EqualFold(strings.TrimSpace(o.Name), name) {
			return o, true
		}
	}
	return WebhookRateLimitOverride{}, false
}

// EffectiveRateLimit resolves the sustained per-minute rate and burst the E8
// limiter should enforce for w, applying the operator-owned policy and then the
// pack/city self-limit clamp:
//
//  1. start from the built-in default;
//  2. apply the operator fleet default ([webhooks].rate_limit);
//  3. apply the operator per-webhook override (operator authority: may raise or lower);
//  4. apply the webhook's own MaxPerMinute, which may ONLY lower the result — a
//     pack-contributed webhook cannot raise the limit it is subject to.
//
// Step 4 is the security-relevant clamp: a pack authors the whole [[webhook]]
// block, so MaxPerMinute is untrusted and is honored only when it is stricter
// than the operator ceiling.
func (c WebhookPolicyConfig) EffectiveRateLimit(w Webhook) (perMinute, burst int) {
	perMinute = defaultWebhookRateLimitPerMinute
	burst = defaultWebhookRateLimitBurst
	if rl := c.RateLimit; rl != nil {
		if rl.PerMinute > 0 {
			perMinute = rl.PerMinute
		}
		if rl.Burst > 0 {
			burst = rl.Burst
		}
		if ov, ok := rl.override(w.Name); ok {
			if ov.PerMinute > 0 {
				perMinute = ov.PerMinute
			}
			if ov.Burst > 0 {
				burst = ov.Burst
			}
		}
	}
	// Pack/city self-limit: clamp downward only. A value at or above the operator
	// ceiling is ignored, so a pack can never widen its own limit.
	if w.MaxPerMinute > 0 && w.MaxPerMinute < perMinute {
		perMinute = w.MaxPerMinute
	}
	return perMinute, burst
}

// WebhookJWTPolicy is one operator-owned jwt-jwks trust anchor. It mirrors
// webhookverify.JWTVerifierPolicy but lives in config so the operator declares
// it in city.toml; the receiver copies it into the verifier options at request
// time. Authoring it in the root [webhooks] block (never in a pack) is what makes
// the R1 boundary enforceable: a pack cannot supply these fields.
type WebhookJWTPolicy struct {
	// Name is the webhook this policy applies to (matched against Webhook.Name).
	Name string `toml:"name"`
	// Issuer is the required "iss" claim, pinned exactly.
	Issuer string `toml:"issuer"`
	// Audience is the required "aud" claim.
	Audience string `toml:"audience"`
	// JWKSURL is the https endpoint publishing the signing keys.
	JWKSURL string `toml:"jwks_url"`
}

// WebhookAllowPublic is one operator-authored public-exposure grant.
type WebhookAllowPublic struct {
	// Name is the webhook name being granted public exposure.
	Name string `toml:"name"`
	// Source is the pack/fragment provenance the grant is scoped to. Matched
	// against the webhook's stamped SourceDir.
	Source string `toml:"source"`
	// Digest optionally pins the content digest of the granted webhook's
	// security-relevant fields.
	//
	// TODO(R3): compute and enforce this digest over
	// {visibility, verify scheme/secret_env/secret_key/trust-root, each rule's
	// event/match/order/rig/target} so a content-swap upgrade auto-downgrades
	// to tenant until the operator re-consents. E2 matches on {name, source}
	// only; the digest field is reserved for that follow-up.
	Digest string `toml:"digest,omitempty"`
}

// OperatorJWTPolicy returns the operator-owned jwt-jwks trust anchor declared for
// the named webhook in the root city.toml [webhooks].jwt_policy list, or ok=false
// when none is declared. It is the R1 seam: a jwt-jwks webhook's trust root comes
// only from here, never from a pack-authored [webhook.verify].
func (c WebhookPolicyConfig) OperatorJWTPolicy(webhookName string) (WebhookJWTPolicy, bool) {
	name := strings.TrimSpace(webhookName)
	for _, p := range c.JWTPolicies {
		if strings.EqualFold(strings.TrimSpace(p.Name), name) {
			return p, true
		}
	}
	return WebhookJWTPolicy{}, false
}

// ScopeOrDefault returns the normalized webhook scope.
func (w Webhook) ScopeOrDefault() string {
	if s := strings.TrimSpace(strings.ToLower(w.Scope)); s != "" {
		return s
	}
	return "city"
}

// MountPathOrDefault returns the webhook mount path.
func (w Webhook) MountPathOrDefault() string {
	return "/hook/" + w.Name
}

// TargetOrDefault returns the normalized rule dispatch sink.
func (r WebhookRule) TargetOrDefault() string {
	if t := strings.TrimSpace(strings.ToLower(r.Target)); t != "" {
		return t
	}
	return "order"
}

// ValidateWebhooks checks webhook declarations for configuration errors that
// would prevent runtime activation. It mirrors ValidateServices.
func ValidateWebhooks(webhooks []Webhook) error {
	seen := make(map[string]bool, len(webhooks))
	for i, w := range webhooks {
		if w.Name == "" {
			return fmt.Errorf("webhook[%d]: name is required", i)
		}
		if !validWebhookName.MatchString(w.Name) {
			return fmt.Errorf("webhook %q: name must match [a-zA-Z0-9][a-zA-Z0-9_-]*", w.Name)
		}
		if seen[w.Name] {
			if w.SourceDir != "" {
				return fmt.Errorf("webhook %q: duplicate name (from %q)", w.Name, w.SourceDir)
			}
			return fmt.Errorf("webhook %q: duplicate name", w.Name)
		}
		seen[w.Name] = true

		switch w.ScopeOrDefault() {
		case "city", "rig":
		default:
			return fmt.Errorf("webhook %q: scope must be \"city\" or \"rig\", got %q", w.Name, w.Scope)
		}

		switch strings.TrimSpace(strings.ToLower(w.Publication.Visibility)) {
		case "", "private", "public", "tenant":
		default:
			return fmt.Errorf("webhook %q: publication.visibility must be \"private\", \"public\", or \"tenant\", got %q", w.Name, w.Publication.Visibility)
		}
		if hostname := strings.TrimSpace(strings.ToLower(w.Publication.Hostname)); hostname != "" && !validPublicationLabel.MatchString(hostname) {
			return fmt.Errorf("webhook %q: publication.hostname must be a single DNS label, got %q", w.Name, w.Publication.Hostname)
		}
		if w.MaxPerMinute < 0 {
			return fmt.Errorf("webhook %q: max_per_minute must be >= 0, got %d", w.Name, w.MaxPerMinute)
		}

		if err := validateWebhookVerify(w); err != nil {
			return err
		}

		if len(w.Rules) == 0 {
			return fmt.Errorf("webhook %q: at least one [[webhook.rule]] is required", w.Name)
		}
		for j, rule := range w.Rules {
			if err := validateWebhookRule(w.Name, j, rule); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateWebhookVerify(w Webhook) error {
	scheme := strings.TrimSpace(w.Verify.Scheme)
	if scheme == "" {
		return fmt.Errorf("webhook %q: verify.scheme is required", w.Name)
	}
	if !knownWebhookSchemes[scheme] {
		return fmt.Errorf("webhook %q: verify.scheme %q is not a known scheme (github-hmac-sha256, hmac-sha256, slack-v0, discord-ed25519, jwt-jwks)", w.Name, scheme)
	}
	if env := strings.TrimSpace(w.Verify.SecretEnv); env != "" && !validWebhookSecretEnv.MatchString(env) {
		return fmt.Errorf("webhook %q: verify.secret_env must be an environment variable name, got %q", w.Name, w.Verify.SecretEnv)
	}
	if hmacFamilyWebhookSchemes[scheme] && strings.TrimSpace(w.Verify.SecretEnv) == "" {
		return fmt.Errorf("webhook %q: verify.secret_env is required for scheme %q", w.Name, scheme)
	}
	if env := strings.TrimSpace(w.Verify.BearerEnv); env != "" && !validWebhookSecretEnv.MatchString(env) {
		return fmt.Errorf("webhook %q: verify.bearer_env must be an environment variable name, got %q", w.Name, w.Verify.BearerEnv)
	}
	return nil
}

func validateWebhookRule(webhookName string, idx int, rule WebhookRule) error {
	ctx := fmt.Sprintf("webhook %q: rule[%d]", webhookName, idx)
	if strings.TrimSpace(rule.Event) == "" {
		return fmt.Errorf("%s: event is required", ctx)
	}
	switch rule.TargetOrDefault() {
	case "order":
		if strings.TrimSpace(rule.Order) == "" {
			return fmt.Errorf("%s: order is required for target=\"order\"", ctx)
		}
	case "conversation":
		// conversation rules route into the extmsg path; no order target.
	default:
		return fmt.Errorf("%s: target must be \"order\" or \"conversation\", got %q", ctx, rule.Target)
	}
	for key := range rule.Args {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("%s: args key is required", ctx)
		}
		if !validWebhookArgKey.MatchString(key) {
			return fmt.Errorf("%s: args key %q must match [a-zA-Z_][a-zA-Z0-9_]*", ctx, key)
		}
		// R4 (security review): a webhook arg is untrusted payload data; it must
		// never be able to set a controller-owned execution env key. The runtime
		// namespaces extracted args under GC_WEBHOOK_ARG_ so a collision is
		// structurally impossible, but rejecting a reserved name here fails the
		// misconfiguration closed at load time rather than silently dropping it.
		if orders.IsReservedExecEnvKey(key) {
			return fmt.Errorf("%s: args key %q is a reserved controller-owned env key and cannot be set from a webhook payload", ctx, key)
		}
	}
	return nil
}

// applyWebhookPackGuard enforces the default-closed pack-guard: a public
// webhook contributed by a pack or fragment (non-empty SourceDir) is capped to
// tenant unless the root city.toml grants it via [webhooks].allow_public.
// Root-authored webhooks (empty SourceDir) are operator-trusted and untouched.
//
// This is the load-bearing control the security review flagged (R3): it runs
// once over the fully-composed webhook set — after every merge site has stamped
// SourceDir — so provenance is centralized and cannot leak through an
// unstamped path. It returns the downgrade warnings for the caller to surface.
func applyWebhookPackGuard(cfg *City, cityRoot string) []string {
	if cfg == nil {
		return nil
	}
	var warnings []string
	for i := range cfg.Webhooks {
		w := &cfg.Webhooks[i]
		if !strings.EqualFold(strings.TrimSpace(w.Publication.Visibility), "public") {
			continue
		}
		// Empty provenance means the literal root city.toml authored it, which
		// is operator-trusted. Every non-root merge site stamps SourceDir, so
		// treating "" as trusted is default-closed for pack/fragment content.
		if w.SourceDir == "" {
			continue
		}
		if webhookPublicGranted(w.Name, w.SourceDir, cityRoot, cfg.WebhookPolicy.AllowPublic) {
			continue
		}
		w.Publication.Visibility = "tenant"
		warnings = append(warnings, fmt.Sprintf(
			"webhook %q: pack/fragment-contributed publication.visibility=\"public\" capped to \"tenant\" (no matching [webhooks].allow_public grant for source %q)",
			w.Name, w.SourceDir))
	}
	return warnings
}

// webhookPublicGranted reports whether an operator-authored allow_public entry
// grants public exposure to the named webhook from the given provenance. A
// relative grant Source is resolved against cityRoot (the directory of the root
// city.toml).
func webhookPublicGranted(name, sourceDir, cityRoot string, grants []WebhookAllowPublic) bool {
	for _, g := range grants {
		if !strings.EqualFold(strings.TrimSpace(g.Name), strings.TrimSpace(name)) {
			continue
		}
		if webhookSourceMatches(sourceDir, g.Source, cityRoot) {
			return true
		}
	}
	return false
}

// webhookSourceMatches reports whether a stamped provenance directory satisfies
// an operator-declared allow_public source. The grant is default-closed: an
// empty source never matches. Matching is by CANONICAL filesystem path — exact
// equality or true subtree containment — so a foreign pack whose SourceDir
// merely shares the same final path segment (basename), or ends in the grant
// string as an unanchored suffix, can NOT satisfy a grant scoped to a different
// pack (that spoof defeated R3's provenance-scoped default-closed guard). A
// relative grant source is resolved against cityRoot; the stamped SourceDir is
// already absolute (each merge site stamps it with an absolute pack dir).
func webhookSourceMatches(sourceDir, allowSource, cityRoot string) bool {
	src := strings.TrimSpace(allowSource)
	sd := strings.TrimSpace(sourceDir)
	if src == "" || sd == "" {
		return false
	}
	if !filepath.IsAbs(src) {
		src = filepath.Join(strings.TrimSpace(cityRoot), src)
	}
	absSD, err := filepath.Abs(sd)
	if err != nil {
		return false
	}
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return false
	}
	return absSD == absSrc || strings.HasPrefix(absSD, absSrc+string(filepath.Separator))
}

// stampWebhookSource returns a copy of webhooks with SourceDir set to source.
// Used at each pack/fragment merge site to centralize provenance stamping.
func stampWebhookSource(webhooks []Webhook, source string) []Webhook {
	if len(webhooks) == 0 {
		return nil
	}
	out := make([]Webhook, len(webhooks))
	copy(out, webhooks)
	for i := range out {
		out[i].SourceDir = source
	}
	return out
}

// deepCopyWebhooks returns a deep copy of the webhook slice, used by the pack
// load cache so cached results are not mutated by later binding stamps.
func deepCopyWebhooks(in []Webhook) []Webhook {
	if in == nil {
		return nil
	}
	out := make([]Webhook, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Rules = deepCopyWebhookRules(in[i].Rules)
		out[i].Verify.AllowedCIDRs = append([]string(nil), in[i].Verify.AllowedCIDRs...)
	}
	return out
}

func deepCopyWebhookRules(in []WebhookRule) []WebhookRule {
	if in == nil {
		return nil
	}
	out := make([]WebhookRule, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Match = deepCopyStringMap(in[i].Match)
		out[i].Args = deepCopyStringMap(in[i].Args)
	}
	return out
}

// filterWebhooksBySourceDir keeps only webhooks declared at or under sourceDir —
// the non-transitive import surface. Mirrors filterServicesBySourceDir.
func filterWebhooksBySourceDir(webhooks []Webhook, sourceDir string) []Webhook {
	absSource, _ := filepath.Abs(sourceDir)
	var out []Webhook
	for _, w := range webhooks {
		absDir, _ := filepath.Abs(w.SourceDir)
		if absDir == absSource || strings.HasPrefix(absDir, absSource+string(filepath.Separator)) {
			out = append(out, w)
		}
	}
	return out
}

// cachedPackWebhooks returns the webhook declarations accumulated for a loaded
// pack directory (the pack's own plus its include/import closure). Mirrors
// cachedPackRuntimes/cachedPackSkills.
func cachedPackWebhooks(cache *packLoadCache, topoDir string) []Webhook {
	if cache == nil {
		return nil
	}
	absDir, err := filepath.Abs(topoDir)
	if err != nil {
		absDir = topoDir
	}
	result, ok := cache.results[absDir]
	if !ok {
		return nil
	}
	return deepCopyWebhooks(result.webhooks)
}
