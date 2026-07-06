package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

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

// (d) A city-level allow_public grant honors public exposure for the matching
// pack webhook.
func TestWebhook_AllowPublicGrantHonorsPublic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "city.toml", `
[workspace]
name = "test"
includes = ["packs/gh"]

[[webhooks.allow_public]]
name = "github"
source = "packs/gh"
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
		t.Fatal("imported-pack webhook must carry SourceDir provenance")
	}
	if w.Publication.Visibility != "public" {
		t.Errorf("visibility = %q, want public (granted by [webhooks].allow_public)", w.Publication.Visibility)
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
