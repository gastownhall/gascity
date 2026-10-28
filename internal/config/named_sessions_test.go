package config

import "testing"

func TestNamedSessionBareIdentity(t *testing.T) {
	lane := NamedSession{Template: "golden-hon", Dir: "contentbuild", BindingName: "lane"}
	ops := NamedSession{Template: "golden-hon", Dir: "contentbuild", BindingName: "ops"}
	top := NamedSession{Template: "golden-hon"}
	named := NamedSession{Name: "hon", Template: "golden-hon", BindingName: "lane"}
	tests := []struct {
		name     string
		sessions []NamedSession
		want     string
		cands    int
	}{
		{"unique binding", []NamedSession{lane}, "golden-hon", 1},
		{"two bindings share the leaf", []NamedSession{lane, ops}, "", 2},
		{"a top-level entry owns the leaf", []NamedSession{lane, top}, "", 1},
		{"no binding", []NamedSession{top}, "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &City{NamedSessions: tc.sessions}
			if got := NamedSessionBareIdentity(cfg, &cfg.NamedSessions[0]); got != tc.want {
				t.Errorf("NamedSessionBareIdentity = %q, want %q", got, tc.want)
			}
			if got := len(NamedSessionBareCandidates(cfg, "golden-hon")); got != tc.cands {
				t.Errorf("len(NamedSessionBareCandidates) = %d, want %d", got, tc.cands)
			}
		})
	}
	cfg := &City{NamedSessions: []NamedSession{named}}
	if got := NamedSessionBareIdentity(cfg, &cfg.NamedSessions[0]); got != "hon" {
		t.Errorf("NamedSessionBareIdentity(named) = %q, want hon", got)
	}
	if got := NamedSessionBareCandidates(cfg, "lane.hon"); got != nil {
		t.Errorf("NamedSessionBareCandidates(lane.hon) = %v, want nil for a binding-prefixed identity", got)
	}
}
