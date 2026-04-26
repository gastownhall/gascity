package doctor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeRunner records invocations and returns canned output.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []fakeRunnerCall
	respond func(query string) (string, error)
}

type fakeRunnerCall struct {
	Query    string
	Host     string
	Port     string
	User     string
	Password string
}

func (f *fakeRunner) Run(_ context.Context, query, host, port, user, password string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeRunnerCall{
		Query: query, Host: host, Port: port, User: user, Password: password,
	})
	f.mu.Unlock()
	if f.respond == nil {
		return "", nil
	}
	return f.respond(query)
}

func newCheckWithRunner(label, prefix, db, port string, fr *fakeRunner) *IssuePrefixConfigCheck {
	c := NewIssuePrefixConfigCheck(label, prefix, db, "127.0.0.1", port, "root", "", false)
	c.runner = fr.Run
	return c
}

func TestIssuePrefixConfig_NameUsesLabel(t *testing.T) {
	c := NewIssuePrefixConfigCheck("city", "ga", "ga", "127.0.0.1", "3306", "root", "", false)
	if got := c.Name(); got != "issue-prefix-config:city" {
		t.Errorf("Name() = %q, want issue-prefix-config:city", got)
	}
}

func TestIssuePrefixConfig_RunSkippedWhenDisabled(t *testing.T) {
	c := NewIssuePrefixConfigCheck("city", "ga", "ga", "127.0.0.1", "3306", "root", "", true)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("Status = %v, want OK", r.Status)
	}
	if !strings.Contains(r.Message, "skipped") {
		t.Errorf("expected skip message, got %q", r.Message)
	}
}

func TestIssuePrefixConfig_RunSkippedWhenNoPort(t *testing.T) {
	fr := &fakeRunner{respond: func(string) (string, error) { return "", nil }}
	c := newCheckWithRunner("city", "ga", "ga", "", fr)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("Status = %v, want OK (server not running)", r.Status)
	}
	if !strings.Contains(r.Message, "managed dolt not running") {
		t.Errorf("expected not-running message, got %q", r.Message)
	}
	if len(fr.calls) != 0 {
		t.Errorf("runner should not be invoked when no port; got %d calls", len(fr.calls))
	}
}

func TestIssuePrefixConfig_RunOKWhenRowMatches(t *testing.T) {
	fr := &fakeRunner{respond: func(query string) (string, error) {
		// Mimic dolt sql output: header bar, column name, divider, value, footer bar.
		return "+-------+\n| value |\n+-------+\n| ga    |\n+-------+", nil
	}}
	c := newCheckWithRunner("city", "ga", "ga", "3306", fr)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("Status = %v, want OK; message %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "issue_prefix=ga") {
		t.Errorf("expected confirmation message, got %q", r.Message)
	}
}

func TestIssuePrefixConfig_RunErrorWhenRowMissing(t *testing.T) {
	fr := &fakeRunner{respond: func(query string) (string, error) {
		// Empty result — no data rows.
		return "Empty set (0.001 sec)", nil
	}}
	c := newCheckWithRunner("city", "ga", "ga", "3306", fr)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Errorf("Status = %v, want Error", r.Status)
	}
	if !strings.Contains(r.Message, "issue_prefix row missing") {
		t.Errorf("expected missing-row message, got %q", r.Message)
	}
	if !strings.Contains(r.FixHint, "gc doctor --fix") {
		t.Errorf("FixHint should mention gc doctor --fix, got %q", r.FixHint)
	}
	if !strings.Contains(r.FixHint, "#1232") {
		t.Errorf("FixHint should reference the issue, got %q", r.FixHint)
	}
}

func TestIssuePrefixConfig_RunErrorWhenRowDrifted(t *testing.T) {
	fr := &fakeRunner{respond: func(query string) (string, error) {
		return "+-------+\n| value |\n+-------+\n| stale |\n+-------+", nil
	}}
	c := newCheckWithRunner("city", "ga", "ga", "3306", fr)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Errorf("Status = %v, want Error", r.Status)
	}
	if !strings.Contains(r.Message, "issue_prefix=stale") {
		t.Errorf("expected drift message naming observed value, got %q", r.Message)
	}
	if !strings.Contains(r.Message, "expected ga") {
		t.Errorf("expected drift message naming expected value, got %q", r.Message)
	}
}

func TestIssuePrefixConfig_RunErrorWhenQueryFails(t *testing.T) {
	fr := &fakeRunner{respond: func(string) (string, error) {
		return "", errors.New("connection refused")
	}}
	c := newCheckWithRunner("city", "ga", "ga", "3306", fr)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Errorf("Status = %v, want Error", r.Status)
	}
	if !strings.Contains(r.Message, "connection refused") {
		t.Errorf("expected upstream error in message, got %q", r.Message)
	}
	if !strings.Contains(r.FixHint, "gc doctor --fix") {
		t.Errorf("FixHint should still suggest --fix, got %q", r.FixHint)
	}
}

func TestIssuePrefixConfig_FixWritesAndVerifies(t *testing.T) {
	state := "" // tracks current value
	fr := &fakeRunner{respond: func(query string) (string, error) {
		// Naive simulator: INSERT updates state; SELECT returns it.
		if strings.Contains(query, "INSERT INTO config") {
			state = "ga"
			return "", nil
		}
		if strings.Contains(query, "SELECT value FROM config") {
			if state == "" {
				return "Empty set (0.001 sec)", nil
			}
			return "+-------+\n| value |\n+-------+\n| " + state + "    |\n+-------+", nil
		}
		return "", nil
	}}
	c := newCheckWithRunner("city", "ga", "ga", "3306", fr)
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}
	// Re-run should now report OK.
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("Run after Fix: Status = %v, want OK; message %q", r.Status, r.Message)
	}
}

func TestIssuePrefixConfig_FixSurfacesWriteError(t *testing.T) {
	fr := &fakeRunner{respond: func(query string) (string, error) {
		if strings.Contains(query, "INSERT INTO config") {
			return "", errors.New("server is read-only")
		}
		return "", nil
	}}
	c := newCheckWithRunner("city", "ga", "ga", "3306", fr)
	err := c.Fix(&CheckContext{})
	if err == nil {
		t.Fatal("Fix should surface write errors")
	}
	if !strings.Contains(err.Error(), "server is read-only") {
		t.Errorf("error should wrap upstream message, got %v", err)
	}
}

func TestIssuePrefixConfig_FixSurfacesVerifyMismatch(t *testing.T) {
	fr := &fakeRunner{respond: func(query string) (string, error) {
		if strings.Contains(query, "INSERT INTO config") {
			return "", nil
		}
		// Verify SELECT returns wrong value — simulate uncommitted write.
		return "+-------+\n| value |\n+-------+\n| stale |\n+-------+", nil
	}}
	c := newCheckWithRunner("city", "ga", "ga", "3306", fr)
	err := c.Fix(&CheckContext{})
	if err == nil {
		t.Fatal("Fix should detect verify mismatch")
	}
	if !strings.Contains(err.Error(), "verification failed") {
		t.Errorf("expected verification-failed message, got %v", err)
	}
}

func TestIssuePrefixConfig_FixUsesParameterizedSQL(t *testing.T) {
	fr := &fakeRunner{respond: func(string) (string, error) {
		return "+-------+\n| value |\n+-------+\n| ga    |\n+-------+", nil
	}}
	c := newCheckWithRunner("city", "ga", "ga", "3306", fr)
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}
	if len(fr.calls) < 2 {
		t.Fatalf("expected at least 2 SQL calls (write + verify), got %d", len(fr.calls))
	}
	writeQuery := fr.calls[0].Query
	for _, want := range []string{
		"USE `ga`",
		"INSERT INTO config",
		"`key`",
		"'issue_prefix'",
		"'ga'",
		"ON DUPLICATE KEY UPDATE",
		"DOLT_COMMIT",
	} {
		if !strings.Contains(writeQuery, want) {
			t.Errorf("write query missing %q\n%s", want, writeQuery)
		}
	}
}

func TestIssuePrefixConfig_RejectsInvalidIdentifiers(t *testing.T) {
	cases := map[string]struct{ db, prefix string }{
		"sql injection in db":     {"bad`name", "ga"},
		"space in db":             {"my db", "ga"},
		"semicolon in prefix":     {"ga", "ga; DROP TABLE"},
		"empty db":                {"", "ga"},
		"empty prefix":            {"ga", ""},
		"quote in prefix":         {"ga", "ga'pwn"},
		"backtick in prefix":      {"ga", "ga`pwn"},
		"dot in db":               {"ga.config", "ga"},
		"control char in prefix":  {"ga", "ga\nDROP"},
		"unicode in prefix":       {"ga", "café"},
	}
	fr := &fakeRunner{respond: func(string) (string, error) {
		t.Errorf("runner must not be invoked for invalid identifiers")
		return "", nil
	}}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newCheckWithRunner("city", tc.prefix, tc.db, "3306", fr)
			r := c.Run(&CheckContext{})
			if r.Status != StatusWarning {
				t.Errorf("Run: Status = %v, want Warning", r.Status)
			}
			if err := c.Fix(&CheckContext{}); err == nil {
				t.Errorf("Fix should reject invalid identifier (db=%q prefix=%q)", tc.db, tc.prefix)
			}
		})
	}
}

func TestExtractIssuePrefixValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty result", "Empty set (0.001 sec)", ""},
		{"single row table", "+-------+\n| value |\n+-------+\n| ga    |\n+-------+", "ga"},
		{"with surrounding whitespace", "+-----+\n| value |\n+-----+\n|   abc   |\n+-----+", "abc"},
		{"multiline output skips header", "  +-----+\n  | value |\n  +-----+\n  | xyz |\n  +-----+\n", "xyz"},
		{"truly empty string", "", ""},
		{"only header rows", "+-----+\n| value |\n+-----+", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractIssuePrefixValue(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidIssuePrefixIdent(t *testing.T) {
	good := []string{"a", "ga", "my-rig", "rig_1", "abc123", "X-Y_Z"}
	bad := []string{"", " ", "a b", "a.b", "a;b", "a'b", "a\"b", "a`b", "café", "a\nb"}
	for _, s := range good {
		if !validIssuePrefixIdent(s) {
			t.Errorf("validIssuePrefixIdent(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if validIssuePrefixIdent(s) {
			t.Errorf("validIssuePrefixIdent(%q) = true, want false", s)
		}
	}
}

func TestIssuePrefixConfig_RunPassesConnectionParams(t *testing.T) {
	fr := &fakeRunner{respond: func(string) (string, error) {
		return "+-------+\n| value |\n+-------+\n| ga    |\n+-------+", nil
	}}
	c := NewIssuePrefixConfigCheck("city", "ga", "ga", "192.168.0.1", "13306", "alice", "secret", false)
	c.runner = fr.Run
	if r := c.Run(&CheckContext{}); r.Status != StatusOK {
		t.Fatalf("expected OK, got %v: %s", r.Status, r.Message)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(fr.calls))
	}
	got := fr.calls[0]
	if got.Host != "192.168.0.1" || got.Port != "13306" || got.User != "alice" || got.Password != "secret" {
		t.Errorf("connection params not propagated: %+v", got)
	}
}
