package nudgepoller

import "testing"

func TestTargetFromArgv(t *testing.T) {
	city := "/tmp/city"
	cases := []struct {
		name        string
		argv        []string
		wantSession string
		wantAgent   string
		wantOK      bool
	}{
		{
			"canonical poller argv",
			append([]string{"gc"}, CommandArgs(city, "s-gc-wisp-abc", "gc-wisp-abc")...),
			"s-gc-wisp-abc", "gc-wisp-abc", true,
		},
		{
			"equals-form flags",
			[]string{"gc", "nudge", "poll", "--city=" + city, "--session=mayor", "gc-wisp-wdg"},
			"mayor", "gc-wisp-wdg", true,
		},
		{
			"test-binary prefix still matches",
			[]string{"gc.test", "-test.run=TestHelper", "--", "nudge", "poll", "--city", city, "--session", "s1", "agent1"},
			"s1", "agent1", true,
		},
		{"wrong city", append([]string{"gc"}, CommandArgs("/tmp/other", "s1", "a1")...), "", "", false},
		{"not a poller", []string{"gc", "mail", "check", "s1"}, "", "", false},
		{"missing target", []string{"gc", "nudge", "poll", "--city", city, "--session", "s1"}, "", "", false},
		{"empty argv", nil, "", "", false},
	}
	for _, tc := range cases {
		gotSession, gotAgent, gotOK := TargetFromArgv(city, tc.argv)
		if gotSession != tc.wantSession || gotAgent != tc.wantAgent || gotOK != tc.wantOK {
			t.Errorf("%s: TargetFromArgv() = (%q, %q, %v), want (%q, %q, %v)",
				tc.name, gotSession, gotAgent, gotOK, tc.wantSession, tc.wantAgent, tc.wantOK)
		}
	}
}
