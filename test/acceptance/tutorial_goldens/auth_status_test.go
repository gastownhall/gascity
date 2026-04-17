//go:build acceptance_c

package tutorialgoldens

import "testing"

func TestClaudeStatusOutputLoggedIn(t *testing.T) {
	t.Parallel()

	if !claudeStatusOutputLoggedIn([]byte(`{"loggedIn":true}`)) {
		t.Fatal("expected loggedIn=true to be accepted")
	}
	if claudeStatusOutputLoggedIn([]byte(`{"loggedIn":false}`)) {
		t.Fatal("expected loggedIn=false to be rejected")
	}
	if claudeStatusOutputLoggedIn([]byte(`not json`)) {
		t.Fatal("expected invalid JSON to be rejected")
	}
}

func TestCodexStatusOutputLoggedIn(t *testing.T) {
	t.Parallel()

	if !codexStatusOutputLoggedIn([]byte("Logged in using ChatGPT\n")) {
		t.Fatal("expected successful login text to be accepted")
	}
	if codexStatusOutputLoggedIn([]byte("Not logged in\n")) {
		t.Fatal("expected unauthenticated output to be rejected")
	}
}
