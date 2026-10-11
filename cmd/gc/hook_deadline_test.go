package main

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
)

// TestHookRunExportsItsDeadline pins that gc hook run hands its child the
// absolute time its output must beat, close to now + timeout.
func TestHookRunExportsItsDeadline(t *testing.T) {
	exe := buildTurnBoundEnvProbe(t)
	originalExecutable := hookRunExecutable
	t.Cleanup(func() { hookRunExecutable = originalExecutable })
	hookRunExecutable = func() (string, error) { return exe, nil }

	before := time.Now()
	var stdout, stderr bytes.Buffer
	if code := cmdHookRun([]string{"probe"}, hookRunOptions{Timeout: 10 * time.Second}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdHookRun = %d, want 0; stderr=%s", code, stderr.String())
	}
	m := regexp.MustCompile(`(?m)^` + hookDeadlineEnv + `=(.*)$`).FindStringSubmatch(stdout.String())
	if m == nil {
		t.Fatalf("gc hook run child env carries no %s:\n%s", hookDeadlineEnv, stdout.String())
	}
	deadline, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(m[1]))
	if err != nil {
		t.Fatalf("parsing %s=%q: %v", hookDeadlineEnv, m[1], err)
	}
	if deadline.Before(before.Add(10*time.Second)) || deadline.After(time.Now().Add(10*time.Second)) {
		t.Fatalf("%s = %s, want now + 10s (run started %s)", hookDeadlineEnv, deadline, before)
	}
}

func TestHookDeadlineLeaves(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		value   string
		want    bool
		wantErr bool
	}{
		{name: "no deadline", value: "", want: true},
		{name: "ample", value: hookDeadlineEnvValue(now.Add(time.Minute)), want: true},
		{name: "exactly the margin", value: hookDeadlineEnvValue(now.Add(hookDeadlineArchiveMargin)), want: true},
		{name: "inside the margin", value: hookDeadlineEnvValue(now.Add(hookDeadlineArchiveMargin - time.Millisecond)), want: false},
		{name: "passed", value: hookDeadlineEnvValue(now.Add(-time.Second)), want: false},
		{name: "malformed", value: "soon", want: false, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(hookDeadlineEnv, tc.value)
			got, err := hookDeadlineLeaves(hookDeadlineArchiveMargin, now)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("hookDeadlineLeaves = (%v, %v), want (%v, error=%v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// TestInjectedAutoHandoffIsArchivedOnlyWhenTheHookCanFinish runs the two hooks
// that inject auto-handoff mail and archive it after writing. With time left
// before the gc hook run deadline the handoff is injected and archived — read
// and closed — exactly as without a deadline. Past it, the handoff is still
// injected but left unread and open, so the next hook delivers it again
// instead of it being consumed by output gc hook run discards.
func TestInjectedAutoHandoffIsArchivedOnlyWhenTheHookCanFinish(t *testing.T) {
	hooks := map[string]func(t *testing.T, f hookBudgetFixture, stdout, stderr io.Writer) int{
		"mail check --inject": mailCheckInject,
		"prime --hook":        primeHook,
	}
	for hookName, hook := range hooks {
		for _, tc := range []struct {
			name        string
			deadline    func() string
			wantArchive bool
		}{
			{name: "no deadline", deadline: func() string { return "" }, wantArchive: true},
			{name: "time left", deadline: func() string { return hookDeadlineEnvValue(time.Now().Add(time.Minute)) }, wantArchive: true},
			{name: "deadline near", deadline: func() string { return hookDeadlineEnvValue(time.Now().Add(time.Second)) }, wantArchive: false},
			{name: "deadline malformed", deadline: func() string { return "not-a-time" }, wantArchive: false},
		} {
			t.Run(hookName+"/"+tc.name, func(t *testing.T) {
				f := newHookBudgetFixture(t)
				f.autoHandoff(t)
				handoffID := onlyAutoHandoffID(t, f)
				t.Setenv(hookDeadlineEnv, tc.deadline())
				var stdout, stderr bytes.Buffer
				endStoreMemo := beginInvocationStoreMemo()
				code := hook(t, f, &stdout, &stderr)
				if err := endStoreMemo(); err != nil {
					t.Fatalf("releasing stores: %v", err)
				}
				resetHookBudgetProcessState(t)
				if code != 0 {
					t.Fatalf("exit %d; stderr=%s", code, stderr.String())
				}
				if !strings.Contains(stdout.String(), "context cycle") {
					t.Fatalf("the handoff was not injected:\n%s", stdout.String())
				}
				b := getBead(t, f, handoffID)
				archived := b.Status == "closed" && b.Metadata[mail.ReadMetadataKey] == "true"
				if archived != tc.wantArchive {
					t.Fatalf("handoff archived = %v (status %q, %s=%q), want %v; stderr=%s",
						archived, b.Status, mail.ReadMetadataKey, b.Metadata[mail.ReadMetadataKey], tc.wantArchive, stderr.String())
				}
				if !tc.wantArchive {
					if b.Status != "open" || hasBeadLabel(b.Labels, "read") || b.Metadata["close_reason"] != "" {
						t.Fatalf("unarchived handoff was touched: status %q labels %v metadata %v", b.Status, b.Labels, b.Metadata)
					}
					if !strings.Contains(stderr.String(), "auto-handoff") {
						t.Fatalf("skipping the archive was silent; stderr=%q", stderr.String())
					}
				} else if !hasBeadLabel(b.Labels, "read") || b.Metadata["close_reason"] == "" {
					t.Fatalf("archived handoff lacks the read label or close_reason: labels %v metadata %v", b.Labels, b.Metadata)
				}
			})
		}
	}
}

// onlyAutoHandoffID returns the id of the fixture's one auto-handoff message.
func onlyAutoHandoffID(t *testing.T, f hookBudgetFixture) string {
	t.Helper()
	store, err := openCityStoreAt(f.cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	rows, err := store.List(beads.ListQuery{Type: "message", Label: mail.AutoHandoffLabel, TierMode: beads.TierBoth})
	if err != nil || len(rows) != 1 {
		t.Fatalf("auto-handoff messages = %d (err %v), want 1", len(rows), err)
	}
	resetHookBudgetProcessState(t)
	return rows[0].ID
}

func getBead(t *testing.T, f hookBudgetFixture, id string) beads.Bead {
	t.Helper()
	store, err := openCityStoreAt(f.cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	resetHookBudgetProcessState(t)
	return b
}

// TestInboxArchiveRetiresExactlyTheInjectedWindow pins that archiving from the
// inbox rows retires exactly what the per-recipient path retires: the
// auto-handoff mail inside the injection window, never one clamped out of it,
// and never ordinary mail.
func TestInboxArchiveRetiresExactlyTheInjectedWindow(t *testing.T) {
	type outcome map[string]string
	run := func(t *testing.T, withRoutes bool) outcome {
		store := beads.NewMemStore()
		mp := beadmail.New(store)
		seed := func(title string, labels ...string) string {
			b, err := store.Create(beads.Bead{Title: title, Type: "message", Assignee: "mayor", From: "mayor", Labels: labels})
			if err != nil {
				t.Fatalf("Create %s: %v", title, err)
			}
			return b.ID
		}
		ids := []string{seed("clamped handoff", mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel)}
		for i := 0; i < mailInjectMaxMessages-1; i++ {
			ids = append(ids, seed(fmt.Sprintf("ordinary-%d", i)))
		}
		ids = append(ids, seed("windowed handoff", mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel))
		target := resolvedMailTarget{display: "mayor", recipients: []string{"mayor"}}
		if withRoutes {
			target.session = &session.Info{ID: "mayor"}
		}
		var stdout, stderr bytes.Buffer
		if code := doMailCheckTargetWithFormat(mp, target, true, "", &stdout, &stderr); code != 0 {
			t.Fatalf("doMailCheckTargetWithFormat = %d; stderr=%s", code, stderr.String())
		}
		out := outcome{}
		for i, id := range ids {
			b, err := store.Get(id)
			if err != nil {
				t.Fatalf("Get(%s): %v", id, err)
			}
			out[fmt.Sprint(i)] = b.Status + "/" + b.Metadata[mail.ReadMetadataKey]
		}
		return out
	}
	perRecipient := run(t, false)
	fromInbox := run(t, true)
	if fmt.Sprint(perRecipient) != fmt.Sprint(fromInbox) {
		t.Fatalf("inbox archive = %v, per-recipient archive = %v", fromInbox, perRecipient)
	}
	if perRecipient["0"] != "open/" {
		t.Fatalf("the handoff clamped out of the window was archived: %v", perRecipient)
	}
	if last := fmt.Sprint(mailInjectMaxMessages); perRecipient[last] != "closed/true" {
		t.Fatalf("the handoff inside the window was not archived: %v", perRecipient)
	}
}
