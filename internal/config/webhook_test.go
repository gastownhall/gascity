package config

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// ghPublicPackTOML is a pack that contributes a public github webhook, used by
// the allow_public content-digest tests.
const ghPublicPackTOML = `
[pack]
name = "gh"
schema = 1

[[webhook]]
name = "github"

[webhook.publication]
visibility = "public"
hostname = "hooks"

[webhook.verify]
scheme = "github-hmac-sha256"
secret_env = "GC_WEBHOOK_GITHUB_SECRET"

[[webhook.rule]]
event = "pull_request"
order = "pr-review-request"
`

// (a) A full [[webhook]] with every sub-table parses and validates.
func TestWebhook_ParsesAllSubTables(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "city.toml", `
[workspace]
name = "test"

[[webhook]]
name = "github"
scope = "city"

[webhook.publication]
visibility = "public"
hostname = "hooks"

[webhook.verify]
scheme = "github-hmac-sha256"
secret_env = "GC_WEBHOOK_GITHUB_SECRET"
secret_key = "primary"
event_header = "X-GitHub-Event"
dedup_header = "X-GitHub-Delivery"

[[webhook.rule]]
event = "pull_request"
match = { action = "labeled", "label.name" = "status/needs-review" }
order = "pr-review-request"
rig = "maintainer"
[webhook.rule.args]
repo = "{{repository.full_name}}"
pr = "{{pull_request.number}}"

[[webhook.rule]]
event = "issues"
match = { action = "labeled" }
order = "triage-patrol"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.Webhooks) != 1 {
		t.Fatalf("want 1 webhook, got %d", len(cfg.Webhooks))
	}
	w := cfg.Webhooks[0]
	if w.Name != "github" {
		t.Errorf("name = %q, want github", w.Name)
	}
	if w.ScopeOrDefault() != "city" {
		t.Errorf("scope = %q, want city", w.ScopeOrDefault())
	}
	// Root city.toml authorship is operator-trusted: public survives the guard.
	if w.SourceDir != "" {
		t.Errorf("root webhook SourceDir = %q, want empty", w.SourceDir)
	}
	if w.Publication.Visibility != "public" {
		t.Errorf("visibility = %q, want public (root-authored, untouched by guard)", w.Publication.Visibility)
	}
	if w.Publication.Hostname != "hooks" {
		t.Errorf("hostname = %q, want hooks", w.Publication.Hostname)
	}
	if w.Verify.Scheme != "github-hmac-sha256" {
		t.Errorf("verify.scheme = %q", w.Verify.Scheme)
	}
	if w.Verify.SecretEnv != "GC_WEBHOOK_GITHUB_SECRET" {
		t.Errorf("verify.secret_env = %q", w.Verify.SecretEnv)
	}
	if w.Verify.SecretKey != "primary" {
		t.Errorf("verify.secret_key = %q", w.Verify.SecretKey)
	}
	if w.Verify.EventHeader != "X-GitHub-Event" || w.Verify.DedupHeader != "X-GitHub-Delivery" {
		t.Errorf("verify headers = %q/%q", w.Verify.EventHeader, w.Verify.DedupHeader)
	}
	if len(w.Rules) != 2 {
		t.Fatalf("want 2 rules, got %d", len(w.Rules))
	}
	r0 := w.Rules[0]
	if r0.Event != "pull_request" || r0.Order != "pr-review-request" || r0.Rig != "maintainer" {
		t.Errorf("rule[0] = %+v", r0)
	}
	if r0.TargetOrDefault() != "order" {
		t.Errorf("rule[0] target = %q, want order (default)", r0.TargetOrDefault())
	}
	if r0.Match["action"] != "labeled" || r0.Match["label.name"] != "status/needs-review" {
		t.Errorf("rule[0].match = %v", r0.Match)
	}
	if r0.Args["repo"] != "{{repository.full_name}}" || r0.Args["pr"] != "{{pull_request.number}}" {
		t.Errorf("rule[0].args = %v", r0.Args)
	}
	if err := ValidateWebhooks(cfg.Webhooks); err != nil {
		t.Fatalf("ValidateWebhooks: %v", err)
	}
}

// (b) An imported-pack public webhook with NO allow_public grant is capped to tenant.
func TestWebhook_ImportedPackPublicCappedToTenant(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "city.toml", `
[workspace]
name = "test"
includes = ["packs/gh"]
`)
	writeFile(t, dir, "packs/gh/pack.toml", `
[pack]
name = "gh"
schema = 1

[[webhook]]
name = "github"

[webhook.publication]
visibility = "public"
hostname = "hooks"

[webhook.verify]
scheme = "github-hmac-sha256"
secret_env = "GC_WEBHOOK_GITHUB_SECRET"

[[webhook.rule]]
event = "pull_request"
order = "pr-review-request"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.Webhooks) != 1 {
		t.Fatalf("want 1 webhook, got %d", len(cfg.Webhooks))
	}
	w := cfg.Webhooks[0]
	if w.SourceDir == "" {
		t.Fatal("imported-pack webhook must carry non-empty SourceDir provenance")
	}
	if want := filepath.Join(dir, "packs/gh"); !sameDir(w.SourceDir, want) {
		t.Errorf("SourceDir = %q, want %q", w.SourceDir, want)
	}
	if w.Publication.Visibility != "tenant" {
		t.Errorf("visibility = %q, want tenant (public capped by default-closed pack-guard)", w.Publication.Visibility)
	}
}

// (c) A fragment-contributed public webhook is capped (provenance stamped at the
// mergeFragment site, not read as trusted — closes redteam attack #9).
func TestWebhook_FragmentPublicCappedToTenant(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "city.toml", `
include = ["frag.toml"]

[workspace]
name = "test"
`)
	writeFile(t, dir, "frag.toml", `
[[webhook]]
name = "planehook"

[webhook.publication]
visibility = "public"

[webhook.verify]
scheme = "hmac-sha256"
secret_env = "GC_WEBHOOK_PLANE_SECRET"
signature_header = "X-Plane-Signature"

[[webhook.rule]]
event = "issue"
order = "backlog-patrol"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.Webhooks) != 1 {
		t.Fatalf("want 1 webhook, got %d", len(cfg.Webhooks))
	}
	w := cfg.Webhooks[0]
	if w.SourceDir == "" {
		t.Fatal("fragment webhook must be stamped with SourceDir (attack #9): empty reads as trusted root")
	}
	if w.Publication.Visibility != "tenant" {
		t.Errorf("visibility = %q, want tenant (fragment public capped)", w.Publication.Visibility)
	}
}

// (d) A city-level allow_public grant with NO digest is default-closed: the pack
// webhook is capped to tenant even though name+source match, because a name-only
// grant would silently re-honor a content swap (R3 content-scoped consent).
func TestWebhook_AllowPublicWithoutDigestCapped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "city.toml", `
[workspace]
name = "test"
includes = ["packs/gh"]

[[webhooks.allow_public]]
name = "github"
source = "packs/gh"
`)
	writeFile(t, dir, "packs/gh/pack.toml", ghPublicPackTOML)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.Webhooks) != 1 {
		t.Fatalf("want 1 webhook, got %d", len(cfg.Webhooks))
	}
	if w := cfg.Webhooks[0]; w.SourceDir == "" {
		t.Fatal("imported-pack webhook must carry SourceDir provenance")
	}
	if got := cfg.Webhooks[0].Publication.Visibility; got != "tenant" {
		t.Errorf("visibility = %q, want tenant (a name+source grant with no digest must not honor public)", got)
	}
}

// (d') A grant whose digest matches the webhook's current content honors public
// exposure; a stale/placeholder digest does not (content-scoped consent, R3).
func TestWebhook_AllowPublicWithMatchingDigestHonored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gh/pack.toml", ghPublicPackTOML)
	grantWith := func(digest string) string {
		return fmt.Sprintf(`
[workspace]
name = "test"
includes = ["packs/gh"]

[[webhooks.allow_public]]
name = "github"
source = "packs/gh"
digest = %q
`, digest)
	}

	// A placeholder (stale) digest is still capped to tenant; the composed webhook
	// then yields the real digest (visibility is excluded from the digest, so the
	// capped value is fine to compute from).
	writeFile(t, dir, "city.toml", grantWith("sha256:stale"))
	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes (stale): %v", err)
	}
	if got := cfg.Webhooks[0].Publication.Visibility; got != "tenant" {
		t.Fatalf("stale-digest grant: visibility = %q, want tenant", got)
	}
	digest := WebhookContentDigest(cfg.Webhooks[0])

	// Re-consent with the correct digest → public honored.
	writeFile(t, dir, "city.toml", grantWith(digest))
	cfg2, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes (matching): %v", err)
	}
	if got := cfg2.Webhooks[0].Publication.Visibility; got != "public" {
		t.Errorf("matching-digest grant: visibility = %q, want public", got)
	}
}

// (d”) Duplicate allow_public grants for the same name+source must not let a
// stale-digest entry shadow a later valid re-consent: authorization holds when
// ANY matching grant pins the current digest, regardless of grant order.
func TestWebhookPublicDenyReason_StaleGrantDoesNotShadowValid(t *testing.T) {
	const cityRoot = "/city"
	w := &Webhook{
		Name:      "github",
		SourceDir: "/city/packs/gh",
		Verify:    WebhookVerify{Scheme: "github-hmac-sha256", SecretEnv: "GC_WEBHOOK_GITHUB_SECRET"},
		Rules:     []WebhookRule{{Event: "pull_request", Order: "pr-review-request"}},
	}
	digest := WebhookContentDigest(*w)
	stale := WebhookAllowPublic{Name: "github", Source: "/city/packs/gh", Digest: "sha256:stale"}
	valid := WebhookAllowPublic{Name: "github", Source: "/city/packs/gh", Digest: digest}

	// Stale grant FIRST, valid re-consent SECOND → authorized (the shadowing bug:
	// the stale first match used to cap the hook despite the later valid grant).
	if reason := webhookPublicDenyReason(w, cityRoot, []WebhookAllowPublic{stale, valid}); reason != "" {
		t.Errorf("stale-then-valid: got deny reason %q, want authorized (empty)", reason)
	}
	// Order-independent: valid FIRST, stale SECOND → still authorized.
	if reason := webhookPublicDenyReason(w, cityRoot, []WebhookAllowPublic{valid, stale}); reason != "" {
		t.Errorf("valid-then-stale: got deny reason %q, want authorized (empty)", reason)
	}
	// Only stale duplicates (none pin the current digest) → capped, and the reason
	// is the content-changed re-consent prompt (not the no-digest or no-match one).
	onlyStale := []WebhookAllowPublic{
		{Name: "github", Source: "/city/packs/gh", Digest: "sha256:stale-a"},
		{Name: "github", Source: "/city/packs/gh", Digest: "sha256:stale-b"},
	}
	reason := webhookPublicDenyReason(w, cityRoot, onlyStale)
	if reason == "" {
		t.Fatal("only-stale duplicates: got authorized, want a content-changed deny reason")
	}
	if !strings.Contains(reason, "content changed") {
		t.Errorf("only-stale duplicates: reason = %q, want it to mention content change", reason)
	}
}

// FIX 5: allow_public provenance matching is by canonical path (exact or true
// subtree) only — never by shared basename or unanchored suffix. A foreign pack
// whose SourceDir merely ends in the same leaf segment as an operator grant must
// NOT be authorized, or R3's provenance-scoped default-closed guard is defeated.
func TestWebhookSourceMatches_CanonicalPathOnly(t *testing.T) {
	const cityRoot = "/city"
	cases := []struct {
		name       string
		sourceDir  string
		allow      string
		wantMatch  bool
		wantReason string
	}{
		{"exact absolute", "/city/packs/trusted/github", "/city/packs/trusted/github", true, "identical path"},
		{"true subtree", "/city/packs/trusted/github", "/city/packs/trusted", true, "SourceDir under the granted dir"},
		{"basename collision rejected", "/city/packs/evil/github", "/city/packs/trusted/github", false, "same leaf, different pack"},
		{"suffix collision rejected", "/city/other/github", "/city/packs/trusted/github", false, "unanchored suffix must not match"},
		{"sibling-prefix not subtree", "/city/packs/trusted-evil/github", "/city/packs/trusted", false, "prefix string but not a path subtree"},
		{"relative grant resolves against city root", "/city/packs/trusted/github", "packs/trusted/github", true, "relative source joined to cityRoot"},
		{"relative grant basename spoof rejected", "/city/packs/evil/github", "packs/trusted/github", false, "relative source must still be canonical"},
		{"empty grant never matches", "/city/packs/trusted/github", "", false, "default-closed"},
		{"empty source never matches", "", "/city/packs/trusted/github", false, "unstamped provenance is not trusted here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := webhookSourceMatches(tc.sourceDir, tc.allow, cityRoot); got != tc.wantMatch {
				t.Fatalf("webhookSourceMatches(%q, %q, %q) = %v, want %v (%s)",
					tc.sourceDir, tc.allow, cityRoot, got, tc.wantMatch, tc.wantReason)
			}
		})
	}
}

// FIX 5 (integration): a grant scoped to one pack must not re-home a DIFFERENT
// pack that shares the webhook name + leaf segment. The basename-collision pack
// stays capped to tenant; only the exact granted pack keeps public.
func TestWebhook_AllowPublicRejectsBasenameSpoof(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "city.toml", `
[workspace]
name = "test"
includes = ["packs/evil/github"]

[[webhooks.allow_public]]
name = "github"
source = "packs/trusted/github"
`)
	writeFile(t, dir, "packs/evil/github/pack.toml", `
[pack]
name = "evil"
schema = 1

[[webhook]]
name = "github"

[webhook.publication]
visibility = "public"

[webhook.verify]
scheme = "github-hmac-sha256"
secret_env = "GC_WEBHOOK_GITHUB_SECRET"

[[webhook.rule]]
event = "pull_request"
order = "pr-review-request"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.Webhooks) != 1 {
		t.Fatalf("want 1 webhook, got %d", len(cfg.Webhooks))
	}
	if got := cfg.Webhooks[0].Publication.Visibility; got != "tenant" {
		t.Errorf("visibility = %q, want tenant: a grant for packs/trusted/github must NOT authorize packs/evil/github (basename spoof)", got)
	}
}

func TestValidateWebhooks_Rejects(t *testing.T) {
	base := func(w Webhook) Webhook {
		if w.Verify.Scheme == "" {
			w.Verify.Scheme = "hmac-sha256"
		}
		if w.Verify.SecretEnv == "" {
			w.Verify.SecretEnv = "GC_WEBHOOK_X"
		}
		if len(w.Rules) == 0 {
			w.Rules = []WebhookRule{{Event: "e", Order: "o"}}
		}
		return w
	}
	cases := []struct {
		name    string
		hook    Webhook
		wantErr string
	}{
		{"empty name", base(Webhook{}), "name is required"},
		{"bad name", base(Webhook{Name: "bad name"}), "name must match"},
		{"unknown scheme", func() Webhook {
			w := base(Webhook{Name: "h"})
			w.Verify.Scheme = "sha512-wut"
			return w
		}(), "not a known scheme"},
		{"bad secret_env", func() Webhook {
			w := base(Webhook{Name: "h"})
			w.Verify.SecretEnv = "1BAD-ENV"
			return w
		}(), "secret_env must be an environment variable name"},
		{"missing secret for hmac", func() Webhook {
			return Webhook{Name: "h", Verify: WebhookVerify{Scheme: "hmac-sha256"}, Rules: []WebhookRule{{Event: "e", Order: "o"}}}
		}(), "secret_env is required"},
		{"no rules", func() Webhook {
			w := base(Webhook{Name: "h"})
			w.Rules = nil
			return w
		}(), "at least one"},
		{"rule missing event", func() Webhook {
			w := base(Webhook{Name: "h"})
			w.Rules = []WebhookRule{{Order: "o"}}
			return w
		}(), "event is required"},
		{"order-rule missing order", func() Webhook {
			w := base(Webhook{Name: "h"})
			w.Rules = []WebhookRule{{Event: "e"}}
			return w
		}(), "order is required"},
		{"bad target", func() Webhook {
			w := base(Webhook{Name: "h"})
			w.Rules = []WebhookRule{{Event: "e", Order: "o", Target: "bogus"}}
			return w
		}(), "target must be"},
		{"bad arg key", func() Webhook {
			w := base(Webhook{Name: "h"})
			w.Rules = []WebhookRule{{Event: "e", Order: "o", Args: map[string]string{"bad-key": "x"}}}
			return w
		}(), "args key"},
		{"reserved arg key (R4)", func() Webhook {
			w := base(Webhook{Name: "h"})
			w.Rules = []WebhookRule{{Event: "e", Order: "o", Args: map[string]string{"GC_CITY": "{{action}}"}}}
			return w
		}(), "reserved controller-owned env key"},
		{"secret_env outside operator namespace", func() Webhook {
			w := base(Webhook{Name: "h"})
			w.Verify.SecretEnv = "MY_SECRET"
			return w
		}(), "operator namespace"},
		{"discord requires secret_env", func() Webhook {
			return Webhook{Name: "h", Verify: WebhookVerify{Scheme: "discord-ed25519"}, Rules: []WebhookRule{{Event: "e", Order: "o"}}}
		}(), "secret_env is required"},
		{"bearer_env outside operator namespace", func() Webhook {
			w := base(Webhook{Name: "h"})
			w.Verify.BearerEnv = "SOME_TOKEN"
			return w
		}(), "operator namespace"},
		{"malformed allowed_cidr", func() Webhook {
			w := base(Webhook{Name: "h"})
			w.Verify.AllowedCIDRs = []string{"not-a-cidr"}
			return w
		}(), "allowed_cidrs"},
		{"rig scope without rig", func() Webhook {
			return base(Webhook{Name: "h", Scope: "rig"})
		}(), "requires a rig binding"},
		{"city scope with rig", func() Webhook {
			return base(Webhook{Name: "h", Rig: "maintainer"})
		}(), "rig is only valid for scope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWebhooks([]Webhook{tc.hook})
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// A rig-scoped webhook with its authoritative rig binding validates; the sink
// uses that rig to constrain dispatch (R4).
func TestValidateWebhooks_RigScopedValid(t *testing.T) {
	w := Webhook{
		Name:   "maintainer-hook",
		Scope:  "rig",
		Rig:    "maintainer",
		Verify: WebhookVerify{Scheme: "hmac-sha256", SecretEnv: "GC_WEBHOOK_MAINT"},
		Rules:  []WebhookRule{{Event: "issue", Order: "triage", Rig: "maintainer"}},
	}
	if err := ValidateWebhooks([]Webhook{w}); err != nil {
		t.Fatalf("valid rig-scoped webhook rejected: %v", err)
	}
}

// Valid operator-namespaced bearer_env and well-formed allowed_cidrs (CIDR and
// bare-IP forms) are accepted.
func TestValidateWebhooks_BearerAndCIDRAccepted(t *testing.T) {
	w := Webhook{
		Name: "gh",
		Verify: WebhookVerify{
			Scheme: "github-hmac-sha256", SecretEnv: "GC_WEBHOOK_GH",
			BearerEnv:    "GC_WEBHOOK_GH_BEARER",
			AllowedCIDRs: []string{"192.30.252.0/22", "203.0.113.7"},
		},
		Rules: []WebhookRule{{Event: "push", Order: "build", Args: map[string]string{"ref": "{{ref}}"}}},
	}
	if err := ValidateWebhooks([]Webhook{w}); err != nil {
		t.Fatalf("valid bearer_env/allowed_cidrs rejected: %v", err)
	}
}

// A bare IPv4-mapped IPv6 allowlist entry is unmapped to its IPv4 form so it
// matches the unmapped request IP the source check compares against. Without the
// unmap it would parse as an IPv6 /128 and fail-close (403) a legitimate IPv4
// caller, diverging from the request-time normalization the parser promises to
// share.
func TestParseWebhookCIDRs_UnmapsBareIPv4Mapped(t *testing.T) {
	prefixes, err := ParseWebhookCIDRs([]string{"::ffff:192.0.2.1"})
	if err != nil {
		t.Fatalf("ParseWebhookCIDRs(::ffff:192.0.2.1) error: %v", err)
	}
	if len(prefixes) != 1 {
		t.Fatalf("got %d prefixes, want 1", len(prefixes))
	}
	p := prefixes[0]
	if !p.Addr().Is4() {
		t.Errorf("parsed prefix addr = %v, want unmapped IPv4 form", p.Addr())
	}
	// webhookRemoteIP unmaps 4-in-6 request IPs, so a request from 192.0.2.1 must
	// fall inside the parsed mapped-form entry.
	if req := netip.MustParseAddr("192.0.2.1"); !p.Contains(req) {
		t.Errorf("prefix %v does not contain unmapped request IP %v", p, req)
	}
}

// A CIDR-form IPv4-mapped IPv6 allowlist entry ("::ffff:192.0.2.0/120") is
// normalized to its equivalent IPv4 prefix ("192.0.2.0/24"), matching the
// unmapped request IP the source check compares against. Before the fix the
// ParsePrefix branch appended the prefix without unmapping, so it stayed an IPv6
// prefix that fail-closed (403) a legitimate IPv4 caller — the same divergence
// the bare-address case above closes, but for CIDR notation.
func TestParseWebhookCIDRs_UnmapsMappedCIDRPrefix(t *testing.T) {
	prefixes, err := ParseWebhookCIDRs([]string{"::ffff:192.0.2.0/120"})
	if err != nil {
		t.Fatalf("ParseWebhookCIDRs(::ffff:192.0.2.0/120) error: %v", err)
	}
	if len(prefixes) != 1 {
		t.Fatalf("got %d prefixes, want 1", len(prefixes))
	}
	p := prefixes[0]
	if !p.Addr().Is4() {
		t.Errorf("parsed prefix addr = %v, want unmapped IPv4 form", p.Addr())
	}
	if p.Bits() != 24 {
		t.Errorf("parsed prefix bits = %d, want 24 (a /120 mapped prefix is a /24 IPv4 range)", p.Bits())
	}
	// A request from inside the range matches; one just outside it does not — proving
	// the prefix width survived the conversion rather than collapsing to a host route.
	if in := netip.MustParseAddr("192.0.2.200"); !p.Contains(in) {
		t.Errorf("prefix %v does not contain in-range IPv4 %v", p, in)
	}
	if out := netip.MustParseAddr("192.0.3.1"); p.Contains(out) {
		t.Errorf("prefix %v wrongly contains out-of-range IPv4 %v", p, out)
	}
}

// A mapped-form prefix shorter than /96 spans beyond the IPv4-mapped range, so it
// cannot represent an IPv4 allowlist entry and is rejected at parse (and thus at
// config load) rather than silently never matching an unmapped IPv4 peer.
func TestParseWebhookCIDRs_RejectsSub96MappedPrefix(t *testing.T) {
	if _, err := ParseWebhookCIDRs([]string{"::ffff:0.0.0.0/64"}); err == nil {
		t.Fatal("ParseWebhookCIDRs(::ffff:0.0.0.0/64) = nil error, want rejection of a sub-/96 mapped prefix")
	}
}

// WebhookContentDigest is stable across equivalent content and changes when a
// security-relevant field (here the target order) changes, but ignores the
// excluded fields (name, SourceDir, visibility, max_per_minute).
func TestWebhookContentDigest_StableAndSensitive(t *testing.T) {
	w := Webhook{
		Name:        "github",
		Publication: ServicePublicationConfig{Visibility: "public"},
		Verify:      WebhookVerify{Scheme: "github-hmac-sha256", SecretEnv: "GC_WEBHOOK_GH"},
		Rules:       []WebhookRule{{Event: "pull_request", Order: "pr-review", Args: map[string]string{"repo": "{{repo}}"}}},
	}
	base := WebhookContentDigest(w)

	// Excluded fields do not change the digest.
	ignored := w
	ignored.Name = "renamed"
	ignored.SourceDir = "/packs/elsewhere"
	ignored.Publication.Visibility = "tenant"
	ignored.MaxPerMinute = 99
	if got := WebhookContentDigest(ignored); got != base {
		t.Errorf("digest changed on an excluded-field edit: %q != %q", got, base)
	}

	// A security-relevant change (target order) changes the digest.
	swapped := w
	swapped.Rules = []WebhookRule{{Event: "pull_request", Order: "attacker-order", Args: map[string]string{"repo": "{{repo}}"}}}
	if got := WebhookContentDigest(swapped); got == base {
		t.Error("digest must change when a rule's target order changes (content-swap detection)")
	}
}

func TestValidateWebhooks_ConversationRuleNeedsNoOrder(t *testing.T) {
	w := Webhook{
		Name:   "slack",
		Verify: WebhookVerify{Scheme: "slack-v0", SecretEnv: "GC_WEBHOOK_SLACK"},
		Rules:  []WebhookRule{{Event: "message", Target: "conversation"}},
	}
	if err := ValidateWebhooks([]Webhook{w}); err != nil {
		t.Fatalf("conversation rule should not require order: %v", err)
	}
}

// sameDir reports whether two paths resolve to the same absolute directory.
func sameDir(a, b string) bool {
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	return aa == bb
}

// TestEffectiveRateLimit_PackCannotRaiseCeiling is the E8 trust assertion: a
// pack authors the whole [[webhook]] block, so its MaxPerMinute is untrusted and
// may only LOWER the operator ceiling — never raise it.
func TestEffectiveRateLimit_PackCannotRaiseCeiling(t *testing.T) {
	pol := WebhookPolicyConfig{RateLimit: &WebhookRateLimitConfig{PerMinute: 100, Burst: 10}}

	// A pack-contributed webhook tries to grant itself a huge limit.
	raise := Webhook{Name: "evil", SourceDir: "packs/evil", MaxPerMinute: 1_000_000}
	if pm, burst := pol.EffectiveRateLimit(raise); pm != 100 || burst != 10 {
		t.Fatalf("pack raise attempt: got (%d,%d), want operator ceiling (100,10)", pm, burst)
	}

	// A pack lowering its own limit is honored (self-restriction is safe).
	lower := Webhook{Name: "polite", MaxPerMinute: 5}
	if pm, _ := pol.EffectiveRateLimit(lower); pm != 5 {
		t.Fatalf("pack self-lower: got %d, want 5", pm)
	}
}

func TestEffectiveRateLimit_DefaultsAndOverride(t *testing.T) {
	// No operator policy → built-in defaults.
	var empty WebhookPolicyConfig
	if pm, burst := empty.EffectiveRateLimit(Webhook{Name: "x"}); pm != defaultWebhookRateLimitPerMinute || burst != defaultWebhookRateLimitBurst {
		t.Fatalf("defaults: got (%d,%d), want (%d,%d)", pm, burst, defaultWebhookRateLimitPerMinute, defaultWebhookRateLimitBurst)
	}

	// An operator per-webhook override has operator authority: it may raise above
	// the fleet default. A pack's MaxPerMinute still clamps below it.
	pol := WebhookPolicyConfig{RateLimit: &WebhookRateLimitConfig{
		PerMinute: 50,
		Burst:     5,
		Overrides: []WebhookRateLimitOverride{{Name: "github", PerMinute: 600, Burst: 120}},
	}}
	if pm, burst := pol.EffectiveRateLimit(Webhook{Name: "github"}); pm != 600 || burst != 120 {
		t.Fatalf("operator override: got (%d,%d), want (600,120)", pm, burst)
	}
	if pm, _ := pol.EffectiveRateLimit(Webhook{Name: "github", MaxPerMinute: 30}); pm != 30 {
		t.Fatalf("override + pack self-lower: got %d, want 30", pm)
	}
	// A different webhook falls back to the fleet default.
	if pm, _ := pol.EffectiveRateLimit(Webhook{Name: "plane"}); pm != 50 {
		t.Fatalf("no override: got %d, want fleet default 50", pm)
	}
}
