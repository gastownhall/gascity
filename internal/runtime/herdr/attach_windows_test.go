//go:build windows

package herdr

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestAttachCommandFocusesTheSessionsTab pins Windows attach up to the client
// it opens. herdr refuses direct attach on Windows, so attach looks up the tab
// holding the session's pane, focuses it, and opens herdr's own client on the
// session.
func TestAttachCommandFocusesTheSessionsTab(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	bindTestPane(t, p, "gastown__worker", bindModeShell)

	cmd, err := p.attachCommand(context.Background(), "gastown__worker")
	if err != nil {
		t.Fatalf("attachCommand: %v", err)
	}
	if want := []string{p.c.bin, "--session", p.c.session}; !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("attach runs %q; want herdr's client on the session, %q", cmd.Args, want)
	}
	calls := fakeCalls(t, state)
	get := strings.Index(calls, "pane get %5")
	focus := strings.Index(calls, "tab focus t1")
	if get < 0 || focus < get {
		t.Errorf("attach did not look up the pane's tab and then focus it; calls:\n%s", calls)
	}
	if strings.Contains(calls, "agent attach") {
		t.Errorf("attach used direct attach, which herdr refuses on Windows; calls:\n%s", calls)
	}
}

// TestAttachRefusesASessionWithoutAPane pins that attach opens nothing, and
// moves no focus, for a session herdr has no pane for.
func TestAttachRefusesASessionWithoutAPane(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	if cmd, err := p.attachCommand(context.Background(), "gastown__nobody"); err == nil {
		t.Fatalf("attachCommand for a session with no pane returned %q", cmd.Args)
	}
	if calls := fakeCalls(t, state); strings.Contains(calls, "tab focus") {
		t.Errorf("attach focused a tab for a session with no pane; calls:\n%s", calls)
	}
}

// TestAttachRefusesAPaneWithoutATab pins that attach opens nothing, and moves
// no focus, when herdr's reply names no tab for the session's pane.
func TestAttachRefusesAPaneWithoutATab(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	bindTestPane(t, p, "gastown__worker", bindModeShell)
	setState(t, state, "pane_no_tab")
	if cmd, err := p.attachCommand(context.Background(), "gastown__worker"); err == nil {
		t.Fatalf("attachCommand for a pane with no tab returned %q", cmd.Args)
	}
	if calls := fakeCalls(t, state); strings.Contains(calls, "tab focus") {
		t.Errorf("attach focused a tab it had no id for; calls:\n%s", calls)
	}
}
