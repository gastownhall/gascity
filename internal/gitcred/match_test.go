package gitcred

import "testing"

// ruleset builds a single-layer Rules value from rules for matcher tests.
func ruleset(rules ...Rule) *Rules {
	lyr := layer{}
	for _, r := range rules {
		lyr.rules = append(lyr.rules, LoadedRule{Rule: r, Origin: "test"})
	}
	return &Rules{layers: []layer{lyr}}
}

func layered(layers ...[]Rule) *Rules {
	r := &Rules{}
	for _, rs := range layers {
		lyr := layer{}
		for _, rule := range rs {
			lyr.rules = append(lyr.rules, LoadedRule{Rule: rule, Origin: "test"})
		}
		r.layers = append(r.layers, lyr)
	}
	return r
}

func TestMatchRequestProtocolGatesGitHubDefault(t *testing.T) {
	// The built-in ambient github.com default is httpsOnly: MatchRequest serves
	// it for protocol=https but withholds it for plaintext protocol=http so the
	// bearer token is never sent over cleartext.
	clearCredEnv(t)
	t.Setenv("GITHUB_TOKEN", "ghp_example")
	rules, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := rules.MatchRequest(Request{Protocol: "https", Host: "github.com", Path: "org/repo"}); !ok {
		t.Fatalf("https request must match the github.com default")
	}
	if _, ok := rules.MatchRequest(Request{Protocol: "http", Host: "github.com", Path: "org/repo"}); ok {
		t.Fatalf("plaintext http request must not match the httpsOnly github.com default")
	}
}

func TestMatchRequestFileRuleStillServesHTTP(t *testing.T) {
	// A user-authored file rule is not httpsOnly, so plaintext http still matches
	// it — the HTTPS-only restriction applies only to the built-in ambient default.
	rules := ruleset(Rule{Match: "example.com", Helper: "token"})
	if _, ok := rules.MatchRequest(Request{Protocol: "http", Host: "example.com", Path: "o/r"}); !ok {
		t.Fatalf("plaintext http must still match a user file rule")
	}
}

func TestMatchSourceLongestPrefixWithinLayer(t *testing.T) {
	rules := ruleset(
		Rule{Match: "github.com", Helper: "broad"},
		Rule{Match: "github.com/gascity", Helper: "narrow"},
	)
	got, ok := rules.MatchSource("https://github.com/gascity/gas-city-inc")
	if !ok || got.Helper != "narrow" {
		t.Fatalf("want narrow rule, got %+v ok=%v", got, ok)
	}
}

func TestMatchLayerOrderBeatsPrefixLength(t *testing.T) {
	// City layer (first) has only a broad host rule; home layer (second) has a
	// longer prefix. Layer order must win.
	rules := layered(
		[]Rule{{Match: "github.com", Helper: "city-broad"}},
		[]Rule{{Match: "github.com/gascity", Helper: "home-narrow"}},
	)
	got, ok := rules.MatchSource("https://github.com/gascity/repo")
	if !ok || got.Helper != "city-broad" {
		t.Fatalf("layer order must win, got %+v ok=%v", got, ok)
	}
}

func TestMatchHostCaseInsensitive(t *testing.T) {
	rules := ruleset(Rule{Match: "GitHub.com/Org", Helper: "x"})
	if _, ok := rules.MatchSource("https://github.com/org/repo"); !ok {
		t.Fatalf("host/path match should be case-insensitive")
	}
}

func TestMatchSegmentBoundary(t *testing.T) {
	rules := ruleset(Rule{Match: "github.com/gas", Helper: "x"})
	if _, ok := rules.MatchSource("https://github.com/gascity/repo"); ok {
		t.Fatalf("prefix must match on segment boundaries, not substrings")
	}
}

func TestMatchDotGitStripped(t *testing.T) {
	rules := ruleset(Rule{Match: "github.com/org/repo", Helper: "x"})
	if _, ok := rules.MatchSource("https://github.com/org/repo.git"); !ok {
		t.Fatalf("trailing .git must be stripped before matching")
	}
}

func TestMatchSCPForm(t *testing.T) {
	rules := ruleset(Rule{Match: "github.com/org", SSHKeyFile: "~/.ssh/id"})
	got, ok := rules.MatchSource("git@github.com:org/repo.git")
	if !ok || got.SSHKeyFile == "" {
		t.Fatalf("scp-form host/path extraction failed: %+v ok=%v", got, ok)
	}
}

func TestMatchSSHURL(t *testing.T) {
	rules := ruleset(Rule{Match: "github.com/org", SSHKeyFile: "~/.ssh/id"})
	if _, ok := rules.MatchSource("ssh://git@github.com:22/org/repo"); !ok {
		t.Fatalf("ssh:// URL host/path extraction failed")
	}
}

func TestMatchTransportGateHTTPSkipsSSHRule(t *testing.T) {
	// An ssh_key_file rule must not serve an https URL; matching continues to
	// the next-longest compatible rule.
	rules := ruleset(
		Rule{Match: "github.com/org/repo", SSHKeyFile: "~/.ssh/id"},
		Rule{Match: "github.com/org", Helper: "token"},
	)
	got, ok := rules.MatchSource("https://github.com/org/repo")
	if !ok || got.Helper != "token" {
		t.Fatalf("https must skip ssh rule and fall to token rule, got %+v ok=%v", got, ok)
	}
}

func TestMatchTransportGateSSHSkipsTokenRule(t *testing.T) {
	rules := ruleset(
		Rule{Match: "github.com/org/repo", Helper: "token"},
		Rule{Match: "github.com/org", SSHKeyFile: "~/.ssh/id"},
	)
	got, ok := rules.MatchSource("git@github.com:org/repo.git")
	if !ok || got.SSHKeyFile == "" {
		t.Fatalf("ssh must skip token rule and fall to ssh rule, got %+v ok=%v", got, ok)
	}
}

func TestMatchFileAndLocalNeverMatch(t *testing.T) {
	rules := ruleset(Rule{Match: "example.com", Helper: "x"})
	for _, src := range []string{
		"file:///home/u/repo",
		"/home/u/repo",
		"./packs/review",
	} {
		if _, ok := rules.MatchSource(src); ok {
			t.Fatalf("source %q must never match", src)
		}
	}
}

func TestMatchTrailingWildcardNormalization(t *testing.T) {
	rules := ruleset(Rule{Match: "github.com/gascity/*", Helper: "x"})
	if _, ok := rules.MatchSource("https://github.com/gascity/repo"); !ok {
		t.Fatalf("trailing /* must normalize to a host/prefix match")
	}
}

func TestMatchHostOnlyRuleMatchesAnyPath(t *testing.T) {
	rules := ruleset(Rule{Match: "github.com", Helper: "x"})
	if _, ok := rules.MatchSource("https://github.com/any/repo"); !ok {
		t.Fatalf("host-only rule should match any path on that host")
	}
}

// TestMatchPortHostSymmetry guards the parent/helper agreement for a
// non-default-port host. The parent resolves the rule via MatchSource, which
// strips the port from the URL host; the helper resolves via Match, and git
// supplies the request host WITH the port under credential.useHttpPath=true. A
// credential is host-scoped, not port-scoped, so a single rule authored as the
// bare host must match both call sites for the same URL.
func TestMatchPortHostSymmetry(t *testing.T) {
	rules := ruleset(Rule{Match: "git.corp.example", Helper: "token"})

	parent, parentOK := rules.MatchSource("https://git.corp.example:8443/o/r")
	if !parentOK {
		t.Fatalf("parent MatchSource should match the bare-host rule for a :port URL")
	}

	// The helper receives req.Host with the port, exactly as git supplies it.
	helper, helperOK := rules.Match("git.corp.example:8443", "o/r")
	if !helperOK {
		t.Fatalf("helper Match should match the bare-host rule when git supplies host:port")
	}

	if parent.Helper != helper.Helper || parent.Match != helper.Match {
		t.Fatalf("parent/helper disagreement: parent=%+v helper=%+v", parent, helper)
	}
}

// TestMatchRuleHostWithPortNormalized guards the other direction: a rule
// authored with an explicit :port still matches both a bare-host URL and a
// with-port URL, since the credential is host-scoped.
func TestMatchRuleHostWithPortNormalized(t *testing.T) {
	rules := ruleset(Rule{Match: "git.corp.example:8443/o", Helper: "token"})
	if _, ok := rules.MatchSource("https://git.corp.example/o/r"); !ok {
		t.Fatalf("a rule authored with :port should still match a bare-host URL")
	}
	if _, ok := rules.Match("git.corp.example:8443", "o/r"); !ok {
		t.Fatalf("a rule authored with :port should match a with-port request host")
	}
}
