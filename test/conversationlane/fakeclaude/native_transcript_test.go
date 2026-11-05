package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

func TestScriptedClaudePublishesItsOwnKeyedTurns(t *testing.T) {
	for _, flag := range []string{"--session-id", "--resume"} {
		t.Run(flag, func(t *testing.T) {
			home := t.TempDir()
			config := filepath.Join(home, "private-claude")
			t.Setenv("HOME", home)
			t.Setenv("CLAUDE_CONFIG_DIR", config)
			work := t.TempDir()
			t.Chdir(work)
			key := "11d602ef-12da-4ea1-adce-002221ee6001"
			u, err := newTUI(t.TempDir(), io.Discard, flag, key)
			if err != nil {
				t.Fatal(err)
			}
			projects := []string{filepath.Join(config, "projects")}
			if got := sessionlog.FindSessionFileByID(projects, work, key); got != "" {
				t.Fatalf("idle fixture manufactured native turn evidence: %s", got)
			}
			now := time.Now().Add(-10 * time.Second)
			if err := os.WriteFile(filepath.Join(u.dir, "turn"), []byte("1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := u.step(now); err != nil {
				t.Fatal(err)
			}
			path := sessionlog.FindSessionFileByID(projects, work, key)
			if path == "" {
				t.Fatal("scripted control turn has no exact keyed transcript")
			}
			open, err := sessionlog.ScanTurns("claude", path)
			if err != nil || !open.Open || open.Current.ID == "" || open.MissingIDs {
				t.Fatalf("control turn not readable as open: %+v, %v", open, err)
			}
			if err := u.endTurn(now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			closed, err := sessionlog.ScanTurns("claude", path)
			if err != nil || closed.Open || len(closed.Recent) != 1 || closed.Recent[0].ID != open.Current.ID {
				t.Fatalf("control turn not closed under the same id: %+v, %v", closed, err)
			}
			u.input = []byte("actual scripted follow-up")
			if err := u.enter(); err != nil {
				t.Fatal(err)
			}
			next, err := sessionlog.ScanTurns("claude", path)
			if err != nil || !next.Open || next.Current.ID == "" || next.Current.ID == open.Current.ID {
				t.Fatalf("submitted input did not open its own turn: %+v, %v", next, err)
			}
			conversation, err := sessionlog.ReadFile(path, 0)
			if err != nil {
				t.Fatal(err)
			}
			var users []*sessionlog.Entry
			for _, entry := range conversation.Messages {
				if entry.Type == "user" {
					users = append(users, entry)
				}
			}
			if conversation.ID != key || len(users) != 2 || users[0].UUID == users[1].UUID || !strings.Contains(string(users[1].Message), "actual scripted follow-up") {
				t.Fatalf("scripted inputs not readable under the exact key: %+v", conversation)
			}
			data, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(data), "actual scripted follow-up") {
				t.Fatalf("actual input absent: %s, %v", data, err)
			}
			if err := u.endTurn(time.Now()); err != nil {
				t.Fatal(err)
			}
			final, err := sessionlog.ScanTurns("claude", path)
			if err != nil || final.Open || len(final.Recent) != 2 || final.MalformedRecords != 0 || final.UntimedRecords != 0 {
				t.Fatalf("ended scripted turns are incomplete: %+v, %v", final, err)
			}
		})
	}
}

func TestScriptedClaudeRejectsMissingOrUnsafeSessionKey(t *testing.T) {
	for _, args := range [][]string{nil, {"--session-id", "../outside"}, {"--resume", "."}, {"--session-id", ""}, {"--session-id", "first", "--resume", "second"}} {
		if _, err := newTUI(t.TempDir(), io.Discard, args...); err == nil {
			t.Errorf("accepted session arguments %q", args)
		}
	}
}

func TestScriptedClaudeResumePreservesTheEarlierInputHistory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	work := t.TempDir()
	t.Chdir(work)
	state := t.TempDir()
	key := "11d602ef-12da-4ea1-adce-002221ee6002"
	first, err := newTUI(state, io.Discard, "--session-id", key)
	if err != nil {
		t.Fatal(err)
	}
	first.input = []byte("before scripted restart")
	if err := first.enter(); err != nil {
		t.Fatal(err)
	}
	if err := first.endTurn(time.Now()); err != nil {
		t.Fatal(err)
	}
	path := sessionlog.FindSessionFileByID([]string{filepath.Join(home, ".claude", "projects")}, work, key)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newTUI(state, io.Discard, "--resume", key)
	if err != nil {
		t.Fatal(err)
	}
	idle, err := os.ReadFile(path)
	if err != nil || string(idle) != string(before) {
		t.Fatalf("idle resume changed native evidence: %v", err)
	}
	second.input = []byte("after scripted restart")
	if err := second.enter(); err != nil {
		t.Fatal(err)
	}
	conversation, err := sessionlog.ReadFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, entry := range conversation.Messages {
		if entry.Type == "user" {
			texts = append(texts, string(entry.Message))
		}
	}
	if len(texts) != 2 || !strings.Contains(texts[0], "before scripted restart") || !strings.Contains(texts[1], "after scripted restart") || conversation.HasBranches {
		t.Fatalf("resume lost or forked earlier scripted input history: %v branches=%v", texts, conversation.HasBranches)
	}
}

func TestScriptedClaudeResumeRefusesAnUnterminatedRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Chdir(t.TempDir())
	state := t.TempDir()
	key := "11d602ef-12da-4ea1-adce-002221ee6003"
	first, err := newTUI(state, io.Discard, "--session-id", key)
	if err != nil {
		t.Fatal(err)
	}
	first.input = []byte("retain this existing input")
	if err := first.enter(); err != nil {
		t.Fatal(err)
	}
	path := first.native.path
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A valid JSON record can still be an incomplete append without its LF.
	data = data[:len(data)-1]
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newTUI(state, io.Discard, "--resume", key); err == nil {
		t.Fatal("accepted an unterminated existing transcript record")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatalf("rejected resume changed existing evidence: %v", err)
	}
}
