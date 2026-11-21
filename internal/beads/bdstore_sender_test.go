package beads

import (
	"encoding/json"
	"testing"
)

func TestBdIssueToBeadReadsSenderColumn(t *testing.T) {
	var issue bdIssue
	if err := json.Unmarshal([]byte(`{"id":"gc-1","title":"mail","status":"open","issue_type":"message","sender":"human"}`), &issue); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := issue.toBead().From; got != "human" {
		t.Errorf("From = %q, want %q from bd's sender field", got, "human")
	}
}
