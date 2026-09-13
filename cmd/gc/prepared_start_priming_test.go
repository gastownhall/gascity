package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestPreparedStartPromptDelivered pins the S19 B0 trap: prepared.promptDelivered
// is the pure delivery decision AND-ed with the fresh-launch condition, so a
// resume incarnation reports false even though the launch path re-sets
// GC_STARTUP_PROMPT_DELIVERED="1" for hook consumption. It also pins promptHash.
func TestPreparedStartPromptDelivered(t *testing.T) {
	const prompt = "do the work"

	cases := []struct {
		name          string
		prompt        string
		startedHash   string // non-empty ⇒ not firstStart
		sessionKey    string // non-empty ⇒ hasResumeKey
		wakeMode      string // "fresh" ⇒ forceFresh
		wantDelivered bool
	}{
		{name: "fresh first start delivers", prompt: prompt, wantDelivered: true},
		{name: "no resume key delivers even with started hash", prompt: prompt, startedHash: "cfg", wantDelivered: true},
		{name: "force fresh delivers despite resume key", prompt: prompt, startedHash: "cfg", sessionKey: "warm", wakeMode: "fresh", wantDelivered: true},
		{name: "resume incarnation does NOT deliver (the trap)", prompt: prompt, startedHash: "cfg", sessionKey: "warm", wantDelivered: false},
		{name: "empty prompt never delivers", prompt: "", startedHash: "", wantDelivered: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			meta := map[string]string{
				"session_name": "worker",
				"template":     "worker",
				"state":        "asleep",
			}
			if tc.startedHash != "" {
				meta["started_config_hash"] = tc.startedHash
			}
			if tc.sessionKey != "" {
				meta["session_key"] = tc.sessionKey
			}
			if tc.wakeMode != "" {
				meta["wake_mode"] = tc.wakeMode
			}
			session, err := store.Create(beads.Bead{
				Title:    "worker",
				Type:     sessionBeadType,
				Labels:   []string{sessionBeadLabel},
				Metadata: meta,
			})
			if err != nil {
				t.Fatalf("Create(session): %v", err)
			}
			candidate := startCandidate{
				info: sessiontest.SeedBead(t, session),
				tp: TemplateParams{
					TemplateName: "worker",
					SessionName:  "worker",
					Command:      "claude",
					Prompt:       tc.prompt,
				},
			}
			prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
			if err != nil {
				t.Fatalf("buildPreparedStart: %v", err)
			}
			if prepared.promptDelivered != tc.wantDelivered {
				t.Errorf("promptDelivered = %v, want %v", prepared.promptDelivered, tc.wantDelivered)
			}
			if got, want := prepared.promptHash, sessionpkg.PromptHash(tc.prompt); got != want {
				t.Errorf("promptHash = %q, want %q", got, want)
			}
			// The env marker choreography is untouched: on the resume row it is
			// still re-set to "1" even though nothing is delivered — the exact
			// reason promptDelivered cannot be inferred from it.
			if tc.name == "resume incarnation does NOT deliver (the trap)" {
				if prepared.cfg.Env[startupPromptDeliveredEnv] != "1" {
					t.Errorf("resume path must still set %s=1 for hooks; got %q", startupPromptDeliveredEnv, prepared.cfg.Env[startupPromptDeliveredEnv])
				}
			}
		})
	}
}
