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

// TestNestedHookRunKeepsTheEarlierDeadline pins a gc hook run inside another:
// the child gets the earlier of the inherited deadline and its own, once, so
// an inner timeout longer than the outer one cannot extend the time the outer
// run keeps output for, and a shorter one still applies.
func TestNestedHookRunKeepsTheEarlierDeadline(t *testing.T) {
	exe := buildTurnBoundEnvProbe(t)
	originalExecutable := hookRunExecutable
	t.Cleanup(func() { hookRunExecutable = originalExecutable })
	hookRunExecutable = func() (string, error) { return exe, nil }
	deadlineLine := regexp.MustCompile(`(?m)^` + hookDeadlineEnv + `=(.*)$`)

	for _, tc := range []struct {
		name          string
		inheritedIn   time.Duration
		timeout       time.Duration
		wantInherited bool
	}{
		{name: "inherited earlier", inheritedIn: 2 * time.Second, timeout: 10 * time.Second, wantInherited: true},
		{name: "own earlier", inheritedIn: time.Minute, timeout: 10 * time.Second, wantInherited: false},
		// An inherited deadline already past cannot be the outer run's live
		// one; it is stale, and honoring it would skip every archive and
		// release every nudge.
		{name: "inherited already passed", inheritedIn: -time.Second, timeout: 10 * time.Second, wantInherited: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inherited := hookDeadlineEnvValue(time.Now().Add(tc.inheritedIn))
			t.Setenv(hookDeadlineEnv, inherited)
			before := time.Now()
			var stdout, stderr bytes.Buffer
			if code := cmdHookRun([]string{"probe"}, hookRunOptions{Timeout: tc.timeout}, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("cmdHookRun = %d; stderr=%s", code, stderr.String())
			}
			lines := deadlineLine.FindAllStringSubmatch(stdout.String(), -1)
			if len(lines) != 1 {
				t.Fatalf("child env carries %d %s entries, want 1:\n%s", len(lines), hookDeadlineEnv, stdout.String())
			}
			got := strings.TrimSpace(lines[0][1])
			if tc.wantInherited {
				if got != inherited {
					t.Fatalf("%s = %s, want the inherited %s", hookDeadlineEnv, got, inherited)
				}
				return
			}
			deadline, err := time.Parse(time.RFC3339Nano, got)
			if err != nil {
				t.Fatalf("parsing %s=%q: %v", hookDeadlineEnv, got, err)
			}
			if deadline.Before(before.Add(tc.timeout)) || deadline.After(time.Now().Add(tc.timeout)) {
				t.Fatalf("%s = %s, want its own now + %s", hookDeadlineEnv, deadline, tc.timeout)
			}
		})
	}
}

// TestDeadlineSkipWarnsOnlyForASkippedHandoff pins the skipped-archive
// warning: past the margin, a mail check that injected only ordinary mail has
// nothing to archive and says nothing, while one that injected an auto-handoff
// says the handoff is left for the next hook.
func TestDeadlineSkipWarnsOnlyForASkippedHandoff(t *testing.T) {
	for _, tc := range []struct {
		name     string
		seed     func(t *testing.T, f hookBudgetFixture)
		wantWarn bool
	}{
		{name: "ordinary mail only", seed: func(t *testing.T, f hookBudgetFixture) { f.sendMail(t, "status please") }, wantWarn: false},
		{name: "auto-handoff", seed: func(t *testing.T, f hookBudgetFixture) { f.autoHandoff(t) }, wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookBudgetFixture(t)
			tc.seed(t, f)
			setHookProcessStart(t, time.Now().Add(-time.Minute))
			t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(time.Now().Add(time.Second)))
			run := recordHookCommand(t, func(stdout, stderr io.Writer) int { return mailCheckInject(t, f, stdout, stderr) })
			if run.code != 0 || !strings.Contains(run.stdout, "unread message") {
				t.Fatalf("exit %d, nothing injected; stdout=%q stderr=%q", run.code, run.stdout, run.stderr)
			}
			if warned := strings.Contains(run.stderr, "deadline"); warned != tc.wantWarn {
				t.Fatalf("deadline warning = %v, want %v; stderr=%q", warned, tc.wantWarn, run.stderr)
			}
		})
	}
}

// setHookProcessStart pins when this process counts as started, for the
// rest of the test.
func setHookProcessStart(t *testing.T, start time.Time) {
	t.Helper()
	prev := hookProcessStart
	hookProcessStart = start
	t.Cleanup(func() { hookProcessStart = prev })
}

// TestHookDeadlineLeavesArchiveTime pins the archive margin: a third of the
// time the process was given, capped at hookDeadlineArchiveMarginMax — so a
// hook under the default timeout keeps the full margin, and one under a short
// timeout still archives when it is fast.
func TestHookDeadlineLeavesArchiveTime(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	long := now.Add(-time.Minute)
	for _, tc := range []struct {
		name    string
		start   time.Time
		value   string
		want    bool
		wantErr bool
	}{
		{name: "no deadline", start: long, value: "", want: true},
		{name: "ample", start: long, value: hookDeadlineEnvValue(now.Add(time.Minute)), want: true},
		{name: "exactly the full margin", start: long, value: hookDeadlineEnvValue(now.Add(hookDeadlineArchiveMarginMax)), want: true},
		{name: "inside the full margin", start: long, value: hookDeadlineEnvValue(now.Add(hookDeadlineArchiveMarginMax - time.Millisecond)), want: false},
		{name: "default timeout, just started", start: now, value: hookDeadlineEnvValue(now.Add(defaultHookRunTimeout)), want: true},
		{name: "short timeout, fast hook", start: now.Add(-100 * time.Millisecond), value: hookDeadlineEnvValue(now.Add(800 * time.Millisecond)), want: true},
		{name: "short timeout, exactly a third left", start: now.Add(-600 * time.Millisecond), value: hookDeadlineEnvValue(now.Add(300 * time.Millisecond)), want: true},
		{name: "short timeout, under a third left", start: now.Add(-700 * time.Millisecond), value: hookDeadlineEnvValue(now.Add(200 * time.Millisecond)), want: false},
		{name: "passed", start: long, value: hookDeadlineEnvValue(now.Add(-time.Second)), want: false},
		{name: "malformed", start: long, value: "soon", want: false, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(hookDeadlineEnv, tc.value)
			got, err := hookDeadlineLeavesArchiveTime(now, tc.start)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("hookDeadlineLeavesArchiveTime = (%v, %v), want (%v, error=%v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// TestShortHookTimeoutStillArchives runs an auto-handoff hook twice under a
// timeout shorter than the full archive margin. The first run injects the
// handoff and archives it, so the second does not inject it again: a short
// timeout cannot re-inject the same handoff forever.
func TestShortHookTimeoutStillArchives(t *testing.T) {
	hooks := map[string]func(t *testing.T, f hookBudgetFixture, stdout, stderr io.Writer) int{
		"mail check --inject": mailCheckInject,
		"prime --hook":        primeHook,
	}
	const timeout = 2 * time.Second
	if timeout >= hookDeadlineArchiveMarginMax {
		t.Fatalf("timeout %s does not undercut the full margin %s", timeout, hookDeadlineArchiveMarginMax)
	}
	for hookName, hook := range hooks {
		t.Run(hookName, func(t *testing.T) {
			f := newHookBudgetFixture(t)
			f.autoHandoff(t)
			for run := 1; run <= 2; run++ {
				start := time.Now()
				setHookProcessStart(t, start)
				t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(start.Add(timeout)))
				var stdout, stderr bytes.Buffer
				endStoreMemo := beginInvocationStoreMemo()
				code := hook(t, f, &stdout, &stderr)
				if err := endStoreMemo(); err != nil {
					t.Fatalf("releasing stores: %v", err)
				}
				resetHookBudgetProcessState(t)
				if code != 0 {
					t.Fatalf("run %d: exit %d; stderr=%s", run, code, stderr.String())
				}
				injected := strings.Contains(stdout.String(), "context cycle")
				if want := run == 1; injected != want {
					t.Fatalf("run %d injected the handoff = %v, want %v; stderr=%s", run, injected, want, stderr.String())
				}
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
				setHookProcessStart(t, time.Now().Add(-time.Minute))
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
		// The window keeps the newest mail, so the messages are seeded a second
		// apart rather than created: MemStore.Create stamps time.Now, which a
		// coarse clock (macOS reads microseconds) repeats for back-to-back
		// creates.
		base := time.Date(2026, 8, 13, 14, 0, 0, 0, time.UTC)
		var seeded []beads.Bead
		seed := func(title string, labels ...string) {
			seeded = append(seeded, beads.Bead{
				ID:        fmt.Sprintf("gc-%d", len(seeded)+1),
				Title:     title,
				Type:      "message",
				Status:    "open",
				Assignee:  "mayor",
				From:      "mayor",
				Labels:    labels,
				CreatedAt: base.Add(time.Duration(len(seeded)) * time.Second),
			})
		}
		seed("clamped handoff", mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel)
		for i := 0; i < mailInjectMaxMessages-1; i++ {
			seed(fmt.Sprintf("ordinary-%d", i))
		}
		seed("windowed handoff", mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel)
		store := beads.NewMemStoreFrom(len(seeded), seeded, nil)
		mp := beadmail.New(store)
		target := resolvedMailTarget{display: "mayor", recipients: []string{"mayor"}}
		if withRoutes {
			target.session = &session.Info{ID: "mayor"}
		}
		var stdout, stderr bytes.Buffer
		if code := doMailCheckTargetWithFormat(mp, target, true, "", &stdout, &stderr); code != 0 {
			t.Fatalf("doMailCheckTargetWithFormat = %d; stderr=%s", code, stderr.String())
		}
		out := outcome{}
		for i, s := range seeded {
			b, err := store.Get(s.ID)
			if err != nil {
				t.Fatalf("Get(%s): %v", s.ID, err)
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

// TestNudgeDrainInjectAcksOnlyWhenTheHookCanFinish pins the nudge half of the
// hook deadline: gc hook run discards the child's output when it overruns, so
// an injecting drain acks the nudges it wrote only while the margin remains.
// Past it, the claims are released back to pending — the next drain delivers
// them again — and the release is reported, never a silent ack of output that
// may be thrown away.
func TestNudgeDrainInjectAcksOnlyWhenTheHookCanFinish(t *testing.T) {
	for _, tc := range []struct {
		name        string
		deadlineIn  time.Duration
		wantPending int
	}{
		{name: "ample time acks", deadlineIn: time.Hour, wantPending: 0},
		{name: "deadline close releases", deadlineIn: time.Second, wantPending: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookBudgetFixture(t)
			f.queueNudge(t)
			setHookProcessStart(t, time.Now().Add(-time.Minute))
			t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(time.Now().Add(tc.deadlineIn)))
			run := recordHookCommand(t, func(stdout, stderr io.Writer) int { return nudgeDrainInject(t, f, stdout, stderr) })
			if run.code != 0 || !strings.Contains(run.stdout, "review queued work") {
				t.Fatalf("exit %d, nudge not injected; stdout=%q stderr=%q", run.code, run.stdout, run.stderr)
			}
			pending, inFlight, dead, err := listQueuedNudges(f.cityDir, hookBudgetIdentity, time.Now())
			if err != nil {
				t.Fatalf("listQueuedNudges: %v", err)
			}
			if len(pending) != tc.wantPending || len(inFlight) != 0 || len(dead) != 0 {
				t.Fatalf("queue after drain = %d pending, %d in flight, %d dead; want %d, 0, 0",
					len(pending), len(inFlight), len(dead), tc.wantPending)
			}
			if warned := strings.Contains(run.stderr, "deadline"); warned != (tc.wantPending > 0) {
				t.Fatalf("deadline warning = %v, want %v; stderr=%q", warned, tc.wantPending > 0, run.stderr)
			}
		})
	}
}

// deadlineAfterCloseProvider is a beadmail provider whose archive runs out of
// hook time between its close and label phases: before the first label it
// moves the hook deadline inside the margin, as a slow close would.
type deadlineAfterCloseProvider struct {
	*beadmail.Provider
	t            *testing.T
	gateAnswered []bool
}

func (p *deadlineAfterCloseProvider) ArchiveInjectedAutoHandoffsFromInbox(inbox *beadmail.Inbox, ids []string, mayLabel func() bool) error {
	return p.Provider.ArchiveInjectedAutoHandoffsFromInbox(inbox, ids, func() bool {
		p.t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(time.Now().Add(time.Second)))
		ok := mayLabel()
		p.gateAnswered = append(p.gateAnswered, ok)
		return ok
	})
}

// TestArchiveSkipsTheLabelWhenTheDeadlineNearsAfterTheClose pins that the
// injecting hook checks its deadline again before the archive's label write:
// once the close has used up the margin, the handoff stays closed and read
// through mail.read, and the label — which adds nothing to a closed handoff —
// is not written past the deadline.
func TestArchiveSkipsTheLabelWhenTheDeadlineNearsAfterTheClose(t *testing.T) {
	store := beads.NewMemStore()
	mp := &deadlineAfterCloseProvider{Provider: beadmail.New(store), t: t}
	handoff, err := store.Create(beads.Bead{
		Title: "context cycle", Type: "message", Assignee: "mayor", From: "mayor",
		Labels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	setHookProcessStart(t, time.Now().Add(-time.Minute))
	t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(time.Now().Add(time.Hour)))
	target := resolvedMailTarget{display: "mayor", recipients: []string{"mayor"}, session: &session.Info{ID: "mayor"}}
	var stdout, stderr bytes.Buffer
	if code := doMailCheckTargetWithFormat(mp, target, true, "", &stdout, &stderr); code != 0 {
		t.Fatalf("doMailCheckTargetWithFormat = %d; stderr=%s", code, stderr.String())
	}
	if len(mp.gateAnswered) != 1 || mp.gateAnswered[0] {
		t.Fatalf("label gate answered %v, want one refusal", mp.gateAnswered)
	}
	b, err := store.Get(handoff.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b.Status != "closed" || b.Metadata[mail.ReadMetadataKey] != "true" || hasBeadLabel(b.Labels, "read") {
		t.Fatalf("handoff = status %q labels %v metadata %v; want closed, read through mail.read, no label", b.Status, b.Labels, b.Metadata)
	}
}
