package webhookmatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestRenderTemplate(t *testing.T) {
	in := MatchInput{
		EventType: "pull_request",
		DedupID:   "delivery-42",
		Identity:  "svc-plane",
		Body: mustParse(t, `{
			"repository": {"full_name": "octo/hello"},
			"pull_request": {"number": 1347},
			"draft": false,
			"nested": {"obj": {"a": 1}}
		}`),
	}
	cases := []struct {
		tmpl string
		want string
	}{
		{"{{repository.full_name}}", "octo/hello"},
		{"{{pull_request.number}}", "1347"},
		{"pr-{{pull_request.number}}", "pr-1347"},
		{"{{repository.full_name}}#{{pull_request.number}}", "octo/hello#1347"},
		{"{{ repository.full_name }}", "octo/hello"}, // inner whitespace trimmed
		{"literal only", "literal only"},
		{"", ""},
		{"{{draft}}", "false"},
		{"{{missing.path}}", ""},       // missing -> empty
		{"a{{missing}}b", "ab"},        // missing between literals
		{"{{@event}}", "pull_request"}, // metadata tokens
		{"{{@delivery}}", "delivery-42"},
		{"{{@dedup}}", "delivery-42"},
		{"{{@identity}}", "svc-plane"},
		{"{{@unknown}}", ""},                 // unknown metadata token -> empty
		{"{{nested.obj}}", `{"a":1}`},        // nested object -> compact JSON
		{"{{unterminated", "{{unterminated"}, // no closing braces -> literal
		{"ok {{unterminated", "ok {{unterminated"},
	}
	for _, tc := range cases {
		got, err := renderTemplate(tc.tmpl, in)
		if err != nil {
			t.Errorf("renderTemplate(%q): %v", tc.tmpl, err)
			continue
		}
		if got != tc.want {
			t.Errorf("renderTemplate(%q) = %q, want %q", tc.tmpl, got, tc.want)
		}
	}
}

func TestExecEnvVars(t *testing.T) {
	if got := ExecEnvVars(nil); got != nil {
		t.Errorf("ExecEnvVars(nil) = %v, want nil", got)
	}
	got := ExecEnvVars(map[string]string{"repo": "octo/hello", "pr": "1347"})
	if got["GC_WEBHOOK_ARG_repo"] != "octo/hello" || got["GC_WEBHOOK_ARG_pr"] != "1347" {
		t.Errorf("ExecEnvVars = %v", got)
	}
	if ExecEnvKey("repo") != "GC_WEBHOOK_ARG_repo" {
		t.Errorf("ExecEnvKey(repo) = %q", ExecEnvKey("repo"))
	}
}

// planeIssueCreated is a trimmed Plane issue-created webhook (the first-consumer
// order-sink target: backlog-patrol).
const planeIssueCreated = `{
  "event": "issue",
  "action": "created",
  "workspace_id": "ws_abc123",
  "data": {"id": "iss_987", "name": "Fix the thing", "priority": "high"}
}`

func TestMatch_PlaneIssueCreated_OrderSink(t *testing.T) {
	rule := config.WebhookRule{
		Event: "issue",
		Match: map[string]string{"action": "created"},
		Order: "backlog-patrol",
		Args: map[string]string{
			"workspace": "{{workspace_id}}",
			"issue":     "{{data.id}}",
		},
	}
	in := MatchInput{EventType: "issue", DedupID: "plane-1", Body: mustParse(t, planeIssueCreated)}
	res, ok, err := Match(in, []config.WebhookRule{rule})
	if err != nil || !ok {
		t.Fatalf("Match ok=%v err=%v", ok, err)
	}
	if res.Target != "order" || res.Order != "backlog-patrol" {
		t.Errorf("target/order = %q/%q, want order/backlog-patrol", res.Target, res.Order)
	}
	if res.Vars["workspace"] != "ws_abc123" || res.Vars["issue"] != "iss_987" {
		t.Errorf("vars = %v", res.Vars)
	}
}

// slackMessageEvent is a trimmed Slack Events API message callback (conversation
// sink). Args still extract; E6/E7 decides the sink from Target.
const slackMessageEvent = `{
  "type": "event_callback",
  "team_id": "T012AB3CD",
  "event": {
    "type": "message",
    "channel": "C0EAQDV4Z",
    "user": "U061F7AUR",
    "text": "hey @gc can you review this?",
    "ts": "1355517523.000005",
    "thread_ts": "1355517523.000004"
  }
}`

func TestMatch_SlackMessage_ConversationSink(t *testing.T) {
	rule := config.WebhookRule{
		Event:  "message",
		Match:  map[string]string{"event.type": "message"},
		Target: "conversation",
		Args: map[string]string{
			"channel": "{{event.channel}}",
			"thread":  "{{event.thread_ts}}",
			"text":    "{{event.text}}",
		},
	}
	in := MatchInput{EventType: "message", DedupID: "Ev0PV52K21", Body: mustParse(t, slackMessageEvent)}
	res, ok, err := Match(in, []config.WebhookRule{rule})
	if err != nil || !ok {
		t.Fatalf("Match ok=%v err=%v", ok, err)
	}
	if res.Target != "conversation" {
		t.Fatalf("target = %q, want conversation", res.Target)
	}
	if res.Order != "" {
		t.Errorf("order = %q, want empty for conversation rule", res.Order)
	}
	want := map[string]string{
		"channel": "C0EAQDV4Z",
		"thread":  "1355517523.000004",
		"text":    "hey @gc can you review this?",
	}
	for k, v := range want {
		if res.Vars[k] != v {
			t.Errorf("vars[%q] = %q, want %q", k, res.Vars[k], v)
		}
	}
}

// discordInteraction is a trimmed Discord APPLICATION_COMMAND interaction
// (type 2) — a "/gc fix" slash command routed to the conversation sink.
const discordInteraction = `{
  "type": 2,
  "id": "1043730023456789012",
  "channel_id": "1030662936234567890",
  "guild_id": "1030662936000000000",
  "data": {"name": "gc", "options": [{"name": "fix", "value": "flaky test"}]},
  "member": {"user": {"id": "80351110224678912", "username": "octo"}}
}`

func TestMatch_DiscordInteraction_ConversationSink(t *testing.T) {
	rule := config.WebhookRule{
		Event:  "interaction",
		Match:  map[string]string{"type": "2", "data.name": "gc"},
		Target: "conversation",
		Args: map[string]string{
			"channel":     "{{channel_id}}",
			"user":        "{{member.user.username}}",
			"subcommand":  "{{data.options.0.name}}",
			"delivery_id": "{{@delivery}}",
		},
	}
	in := MatchInput{EventType: "interaction", DedupID: "1043730023456789012", Body: mustParse(t, discordInteraction)}
	res, ok, err := Match(in, []config.WebhookRule{rule})
	if err != nil || !ok {
		t.Fatalf("Match ok=%v err=%v", ok, err)
	}
	if res.Target != "conversation" {
		t.Fatalf("target = %q, want conversation", res.Target)
	}
	want := map[string]string{
		"channel":     "1030662936234567890",
		"user":        "octo",
		"subcommand":  "fix",
		"delivery_id": "1043730023456789012",
	}
	for k, v := range want {
		if res.Vars[k] != v {
			t.Errorf("vars[%q] = %q, want %q", k, res.Vars[k], v)
		}
	}
}
