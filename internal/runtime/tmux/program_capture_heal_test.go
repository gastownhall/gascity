package tmux

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

// healModelPane is one pane of healServer.
type healModelPane struct {
	session, window, id string
	mark                string // agent, orphan or pane
	dead, pipe          bool
	tap, record, text   string
}

// healServer is a scripted tmux server for heal: it answers heal's pane
// reads and the disarm sweep's listing from its panes, applies the commands
// heal runs to them, and keeps the process table's taps (one per armed
// pipe). It records every command.
type healServer struct {
	panes []*healModelPane
	taps  map[string]bool // live tap nonces
	calls [][]string
	// failTarget fails any mutating command aimed at a target ending in it.
	failTarget string
	// pipeOpensBeforeAct opens a foreign pipe on the pane between heal's
	// read and its act.
	pipeOpensBeforeAct bool
	// ps fails the process-table read.
	psFails bool
	// psReads counts the process-table reads.
	psReads int
}

var healArmRandRe = regexp.MustCompile(`'@gc_ps_tap' '([A-Za-z0-9_-]+)\.#\{pane_id\}'`)

func (s *healServer) pane(target string) *healModelPane {
	_, id, ok := strings.Cut(target, ".%")
	if !ok {
		return nil
	}
	for _, p := range s.panes {
		if p.id == "%"+id {
			return p
		}
	}
	return nil
}

func row(fields ...string) string { return strings.Join(fields, "\t") }

func boolField(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func (s *healServer) execute(args []string) (string, error) {
	for len(args) > 0 && (args[0] == "-u" || args[0] == "-L") {
		if args[0] == "-L" {
			args = args[1:]
		}
		args = args[1:]
	}
	s.calls = append(s.calls, slices.Clone(args))
	var out []string
	cmd := []string{}
	flush := func() error {
		if len(cmd) == 0 {
			return nil
		}
		o, err := s.run(cmd)
		cmd = nil
		if o != "" {
			out = append(out, o)
		}
		return err
	}
	for _, a := range args {
		if a == ";" {
			if err := flush(); err != nil {
				return strings.Join(out, "\n"), err
			}
			continue
		}
		cmd = append(cmd, a)
	}
	err := flush()
	return strings.Join(out, "\n"), err
}

func (s *healServer) executeCtx(_ context.Context, args []string) (string, error) {
	return s.execute(args)
}

func (s *healServer) fail(target string) error {
	if s.failTarget != "" && strings.HasSuffix(target, s.failTarget) {
		return fmt.Errorf("tmux: scripted failure on %s", target)
	}
	return nil
}

func (s *healServer) run(cmd []string) (string, error) {
	target := ""
	if i := slices.Index(cmd, "-t"); i >= 0 && i+1 < len(cmd) {
		target = cmd[i+1]
	}
	switch cmd[0] {
	case "list-panes":
		if slices.Contains(cmd, "-a") {
			var rows []string
			for _, p := range s.panes {
				rows = append(rows, row(p.session, p.window, p.id, boolField(p.dead), boolField(p.pipe), p.tap, runtimeInventoryEnd))
			}
			return strings.Join(rows, "\n"), nil
		}
		session := strings.TrimSuffix(strings.TrimPrefix(target, "="), ":")
		var rows []string
		for _, p := range s.panes {
			if p.session == session {
				rows = append(rows, row(boolField(p.dead), p.window, p.mark, p.id, boolField(p.pipe), p.tap, p.record, runtimeInventoryEnd))
			}
		}
		if len(rows) == 0 {
			return "", ErrSessionNotFound
		}
		return strings.Join(rows, "\n"), nil
	case "if-shell":
		p := s.pane(target)
		if err := s.fail(target); err != nil {
			return "", err
		}
		if s.pipeOpensBeforeAct {
			p.pipe = true
		}
		if p.pipe {
			return "", nil
		}
		m := healArmRandRe.FindStringSubmatch(cmd[len(cmd)-1])
		if m == nil {
			return "", fmt.Errorf("if-shell arm list carries no nonce: %q", cmd[len(cmd)-1])
		}
		p.tap, p.record, p.text, p.pipe = m[1]+"."+p.id, "", "", true
		s.taps[p.tap] = true
		return "", nil
	case "display-message":
		return s.pane(target).tap, nil
	case "set-option":
		if err := s.fail(target); err != nil {
			return "", err
		}
		p := s.pane(target)
		opt := cmd[len(cmd)-1]
		if slices.Contains(cmd, "-pu") {
			switch opt {
			case programTapOption:
				p.tap = ""
			case programStatusOption:
				p.record = ""
			case programTextOption:
				p.text = ""
			}
			return "", nil
		}
		opt, value := cmd[len(cmd)-2], cmd[len(cmd)-1]
		if opt == programTapOption {
			p.tap = strings.ReplaceAll(value, "#{pane_id}", p.id)
		}
		return "", nil
	case "pipe-pane":
		if err := s.fail(target); err != nil {
			return "", err
		}
		p := s.pane(target)
		if p.dead {
			return "", fmt.Errorf("tmux: target pane has exited: %s", target)
		}
		for nonce := range s.taps {
			if strings.HasSuffix(nonce, "."+p.id) {
				delete(s.taps, nonce)
			}
		}
		p.pipe = slices.Contains(cmd, "-O")
		if p.pipe {
			s.taps[p.tap] = true
		}
		return "", nil
	}
	return "", fmt.Errorf("healServer: unscripted command %q", cmd)
}

// mutations are the recorded calls that change a pane.
func (s *healServer) mutations() [][]string {
	var out [][]string
	for _, c := range s.calls {
		if c[0] != "list-panes" && c[0] != "display-message" {
			out = append(out, c)
		}
	}
	return out
}

// healProvider is a provider over server, its capture gate passed with the
// test companion, its heal clock at *now.
func healProvider(server *healServer, now *time.Time) *Provider {
	if server.taps == nil {
		server.taps = map[string]bool{}
	}
	tm := &Tmux{cfg: Config{ProgramStatus: runtime.ProgramStatusOn}, exec: server}
	tm.capture.probe = func() (captureCapability, error) {
		return captureCapability{companion: testCompanion, tmux: testTmuxBin}, nil
	}
	snapshot := func(context.Context) (processSnapshot, error) {
		server.psReads++
		if server.psFails {
			return processSnapshot{}, errors.New("ps timed out")
		}
		var nonces []string
		for n := range server.taps {
			nonces = append(nonces, n)
		}
		return tapTable(nonces...), nil
	}
	p := &Provider{tm: tm, cfg: tm.cfg, workDirs: map[string]string{}}
	p.cache = NewStateCache(&tmuxFetcher{tm: tm, snapshot: snapshot}, time.Hour)
	p.heal.now = func() time.Time { return *now }
	return p
}

var healWant = runtime.ProgramStatusArm{Capture: true, App: "codex"}

func mustHeal(t *testing.T, p *Provider, name string, want runtime.ProgramStatusArm, action runtime.ProgramCaptureAction) {
	t.Helper()
	got, err := p.HealProgramCapture(name, want)
	if err != nil {
		t.Fatalf("HealProgramCapture(%q): %v", name, err)
	}
	if got != action {
		t.Fatalf("HealProgramCapture(%q) = %q, want %q", name, got, action)
	}
}

func TestHealRearmsReaderLostAgentPaneOnly(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	agent := &healModelPane{session: "s", window: "@1", id: "%5", mark: "agent", tap: "0ldR4nd.%5", record: "v1|stale"}
	split := &healModelPane{session: "s", window: "@1", id: "%6", mark: "orphan", tap: "spl1t.%6"}
	server := &healServer{panes: []*healModelPane{split, agent}}
	p := healProvider(server, &now)

	mustHeal(t, p, "s", healWant, runtime.ProgramCaptureArmed)

	if agent.tap == "0ldR4nd.%5" || !strings.HasSuffix(agent.tap, ".%5") || !agent.pipe || agent.record != "" {
		t.Fatalf("agent pane after heal: tap %q pipe %v record %q, want a new nonce of %%5, a pipe and no record", agent.tap, agent.pipe, agent.record)
	}
	if split.tap != "spl1t.%6" || split.pipe {
		t.Fatalf("heal touched the human split: %+v", split)
	}
	muts := server.mutations()
	if len(muts) != 1 || muts[0][0] != "if-shell" {
		t.Fatalf("mutations = %q, want one if-shell", muts)
	}
	ifShell := muts[0]
	if want := []string{"if-shell", "-F", "-t", "=s:@1.%5", "#{pane_pipe}", ""}; !slices.Equal(ifShell[:len(want)], want) {
		t.Fatalf("if-shell = %q, want it to lead with %q", ifShell, want)
	}
	armList := ifShell[6]
	for _, part := range []string{"'set-option' '-pu' '-t' '=s:@1.%5' '@gc_ps'", "'set-option' '-pu' '-t' '=s:@1.%5' '@gc_ps_text'", "'pipe-pane' '-O' '-t' '=s:@1.%5'", " -a codex'"} {
		if !strings.Contains(armList, part) {
			t.Fatalf("arm list %q lacks %q", armList, part)
		}
	}
	if strings.Contains(armList, "%6") {
		t.Fatalf("arm list names the split: %q", armList)
	}

	// The re-armed capture is healthy: heal leaves it alone.
	server.calls = nil
	mustHeal(t, p, "s", healWant, runtime.ProgramCaptureUnchanged)
	if muts := server.mutations(); len(muts) != 0 {
		t.Fatalf("a healthy capture was changed: %q", muts)
	}
}

func TestHealArmsNotArmedPane(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pane := &healModelPane{session: "s", window: "@2", id: "%3", mark: "pane"}
	server := &healServer{panes: []*healModelPane{pane}}
	p := healProvider(server, &now)
	mustHeal(t, p, "s", healWant, runtime.ProgramCaptureArmed)
	if !strings.HasSuffix(pane.tap, ".%3") || !pane.pipe {
		t.Fatalf("not_armed pane after heal: %+v", pane)
	}
}

func TestHealNeverTakesOverForeignPipe(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, tc := range []struct {
		name string
		pane healModelPane
		live bool
	}{
		{"our nonce, a foreign reader displaced the tap", healModelPane{tap: "0urs.%5", pipe: true}, false},
		{"no nonce, a foreign reader", healModelPane{pipe: true}, false},
		{"another pane's nonce, a foreign reader", healModelPane{tap: "0th3r.%9", pipe: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pane := tc.pane
			pane.session, pane.window, pane.id, pane.mark = "s", "@1", "%5", "agent"
			server := &healServer{panes: []*healModelPane{&pane}}
			p := healProvider(server, &now)
			mustHeal(t, p, "s", healWant, runtime.ProgramCaptureDeferred)
			if muts := server.mutations(); len(muts) != 0 {
				t.Fatalf("heal acted on a foreign pipe: %q", muts)
			}
		})
	}

	// A pipe opened between the read and the act: if-shell sees it and arms
	// nothing, so heal defers.
	pane := &healModelPane{session: "s", window: "@1", id: "%5", mark: "agent", tap: "0urs.%5"}
	server := &healServer{panes: []*healModelPane{pane}, pipeOpensBeforeAct: true}
	p := healProvider(server, &now)
	mustHeal(t, p, "s", healWant, runtime.ProgramCaptureDeferred)
	if pane.tap != "0urs.%5" {
		t.Fatalf("a pipe that opened in between was taken over: tap %q", pane.tap)
	}
	for _, c := range server.mutations() {
		if c[0] != "if-shell" {
			t.Fatalf("heal ran %q outside the if-shell guard", c)
		}
	}
}

func TestHealDefersWhilePipeOpen(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	// Our tap died but tmux has not closed the pipe yet: heal cannot tell it
	// from a foreign pipe, so it waits for the pipe to close.
	pane := &healModelPane{session: "s", window: "@1", id: "%5", mark: "agent", tap: "0urs.%5", pipe: true}
	server := &healServer{panes: []*healModelPane{pane}}
	p := healProvider(server, &now)
	mustHeal(t, p, "s", healWant, runtime.ProgramCaptureDeferred)

	// With the process table unreadable, our open pipe is left alone.
	server.psFails = true
	mustHeal(t, p, "s", healWant, runtime.ProgramCaptureUnchanged)
	if muts := server.mutations(); len(muts) != 0 {
		t.Fatalf("heal acted on an open pipe: %q", muts)
	}

	// Once the pipe closes, the next heal re-arms it.
	pane.pipe, server.psFails = false, false
	mustHeal(t, p, "s", healWant, runtime.ProgramCaptureArmed)
}

func TestHealReplacesOwnTapAfterCodecChange(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pane := &healModelPane{session: "s", window: "@1", id: "%5", mark: "agent", tap: "0urs.%5", pipe: true, record: "v9|0urs.%5|future"}
	server := &healServer{panes: []*healModelPane{pane}, taps: map[string]bool{"0urs.%5": true}}
	if _, err := programstatus.Decode(pane.record); programstatus.RecordForm(err) != runtime.ProgramRecordUnknownVersion {
		t.Fatalf("fixture record %q does not decode as an unknown codec version: %v", pane.record, err)
	}
	p := healProvider(server, &now)
	mustHeal(t, p, "s", healWant, runtime.ProgramCaptureArmed)
	if pane.tap == "0urs.%5" || !strings.HasSuffix(pane.tap, ".%5") || pane.record != "" || !server.taps[pane.tap] {
		t.Fatalf("tap not replaced: %+v, taps %v", pane, server.taps)
	}
	if server.taps["0urs.%5"] {
		t.Fatal("the old tap still holds the pipe")
	}
}

func TestHealDisarmsWhenSwitchOff(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	off := runtime.ProgramStatusArm{}

	// Our tap is alive: close its pipe, then unset the options.
	live := &healModelPane{session: "s", window: "@1", id: "%5", mark: "agent", tap: "0urs.%5", pipe: true, record: "v1|x", text: "v1|t"}
	server := &healServer{panes: []*healModelPane{live}, taps: map[string]bool{"0urs.%5": true}}
	p := healProvider(server, &now)
	mustHeal(t, p, "s", off, runtime.ProgramCaptureDisarmed)
	if live.tap != "" || live.record != "" || live.text != "" || live.pipe {
		t.Fatalf("after disarm: %+v, want no options and no pipe", live)
	}
	muts := server.mutations()
	if len(muts) != 1 || muts[0][0] != "pipe-pane" || slices.Contains(muts[0][:4], "-O") {
		t.Fatalf("disarm = %q, want one list closing the pipe first", muts)
	}
	mustHeal(t, p, "s", off, runtime.ProgramCaptureUnchanged)

	// The process table shows no tap of ours: the open pipe is not ours to
	// close, so only the options are unset.
	foreign := &healModelPane{session: "s", window: "@1", id: "%5", mark: "agent", tap: "0urs.%5", pipe: true}
	server = &healServer{panes: []*healModelPane{foreign}}
	p = healProvider(server, &now)
	mustHeal(t, p, "s", off, runtime.ProgramCaptureDisarmed)
	if foreign.tap != "" || !foreign.pipe {
		t.Fatalf("after disarm: %+v, want the nonce unset and the pipe left open", foreign)
	}
	for _, c := range server.mutations() {
		if slices.Contains(c, "pipe-pane") {
			t.Fatalf("disarm closed a pipe that may be foreign: %q", c)
		}
	}

	// The process table cannot be read, because ps fails or because its
	// failure gate is still closed after an earlier failure: the open pipe
	// may be our live tap's, and unsetting the nonce that proves it ours
	// would orphan the tap for good, so heal defers. Once the table reads,
	// the disarm closes the pipe and a later declaration arms the pane
	// afresh.
	unread := &healModelPane{session: "s", window: "@1", id: "%5", mark: "agent", tap: "0urs.%5", pipe: true, record: "v1|x"}
	server = &healServer{panes: []*healModelPane{unread}, taps: map[string]bool{"0urs.%5": true}, psFails: true}
	p = healProvider(server, &now)
	mustHeal(t, p, "s", off, runtime.ProgramCaptureDeferred)
	server.psFails = false
	mustHeal(t, p, "s", off, runtime.ProgramCaptureDeferred)
	if server.psReads != 1 {
		t.Fatalf("process-table reads = %d, want 1: the closed gate must defer without reading", server.psReads)
	}
	if unread.tap != "0urs.%5" || unread.record != "v1|x" || !unread.pipe || !server.taps["0urs.%5"] {
		t.Fatalf("after a disarm with the process table unread: %+v, taps %v; want the nonce and record kept on the open pipe", unread, server.taps)
	}
	if muts := server.mutations(); len(muts) != 0 {
		t.Fatalf("a disarm with the process table unread changed the pane: %q", muts)
	}
	p.cache.fetcher.(*tmuxFetcher).snapshotGate.succeeded()
	mustHeal(t, p, "s", off, runtime.ProgramCaptureDisarmed)
	if unread.tap != "" || unread.record != "" || unread.pipe || server.taps["0urs.%5"] {
		t.Fatalf("after the table read: %+v, taps %v; want our tap's pipe closed and the options unset", unread, server.taps)
	}
	mustHeal(t, p, "s", healWant, runtime.ProgramCaptureArmed)

	// No pipe attached: no tap can be orphaned, so the options are unset
	// without reading the process table.
	closed := &healModelPane{session: "s", window: "@1", id: "%5", mark: "agent", tap: "0urs.%5", record: "v1|x"}
	server = &healServer{panes: []*healModelPane{closed}, psFails: true}
	p = healProvider(server, &now)
	mustHeal(t, p, "s", off, runtime.ProgramCaptureDisarmed)
	if closed.tap != "" || closed.record != "" || closed.pipe || server.psReads != 0 {
		t.Fatalf("after disarm: %+v, %d process-table reads; want the options unset without a read", closed, server.psReads)
	}

	// A pane that was never armed is left alone.
	never := &healModelPane{session: "s", window: "@1", id: "%5", mark: "agent", pipe: true}
	server = &healServer{panes: []*healModelPane{never}}
	p = healProvider(server, &now)
	mustHeal(t, p, "s", off, runtime.ProgramCaptureUnchanged)
	if muts := server.mutations(); len(muts) != 0 {
		t.Fatalf("an unarmed pane was changed: %q", muts)
	}
}

func TestDisarmSweepDisarmsOnlyOurPanes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	liveTap := &healModelPane{session: "a", window: "@1", id: "%1", tap: "l1ve.%1", pipe: true, record: "v1|x"}
	lostTap := &healModelPane{session: "b", window: "@2", id: "%2", tap: "l0st.%2", pipe: true}
	inherited := &healModelPane{session: "c", window: "@3", id: "%3", tap: "gl0bal.%42", pipe: true}
	never := &healModelPane{session: "e", window: "@5", id: "%5"}
	closed := &healModelPane{session: "f", window: "@6", id: "%6", tap: "cl0sed.%6", record: "v1|x"}
	// Dead panes follow the live-tap rule too. One whose tap is gone, or
	// whose pipe is closed, has its options unset. One whose live tap still
	// holds its pipe, which pipe-pane refuses to close on a dead pane, keeps
	// its nonce and is deferred: unsetting the nonce would leave the tap
	// reading whatever a respawn runs there, orphaned.
	deadLive := &healModelPane{session: "d", window: "@4", id: "%4", tap: "d3ad.%4", pipe: true, record: "v1|y", text: "t", dead: true}
	deadLost := &healModelPane{session: "g", window: "@7", id: "%7", tap: "d3adl0st.%7", pipe: true, record: "v1|z", dead: true}
	deadClosed := &healModelPane{session: "h", window: "@8", id: "%8", tap: "d3adcl0sed.%8", record: "v1|w", dead: true}
	server := &healServer{
		panes: []*healModelPane{liveTap, lostTap, inherited, deadLive, never, closed, deadLost, deadClosed},
		taps:  map[string]bool{"l1ve.%1": true, "d3ad.%4": true},
	}
	p := healProvider(server, &now)

	n, deferred, err := p.DisarmProgramCaptures()
	if err != nil || n != 5 || deferred != 1 {
		t.Fatalf("DisarmProgramCaptures = %d, %d, %v, want 5 disarmed and 1 deferred", n, deferred, err)
	}
	if got := callsContaining(server.calls, "pipe-pane"); len(got) != 1 || !slices.Contains(got[0], "=a:@1.%1") {
		t.Fatalf("pipe-pane calls %q, want only the live tap's pane closed", got)
	}
	if liveTap.tap != "" || liveTap.record != "" || liveTap.pipe {
		t.Fatalf("live tap's pane: %+v, want disarmed with its pipe closed", liveTap)
	}
	if lostTap.tap != "" || !lostTap.pipe {
		t.Fatalf("lost tap's pane: %+v, want its options unset and its (maybe foreign) pipe left open", lostTap)
	}
	if closed.tap != "" || closed.record != "" {
		t.Fatalf("pipe-less pane: %+v, want its options unset", closed)
	}
	if deadLive.tap != "d3ad.%4" || deadLive.record != "v1|y" || deadLive.text != "t" || !deadLive.pipe || !server.taps["d3ad.%4"] {
		t.Fatalf("dead pane our live tap holds: %+v, want its nonce, record and pipe kept", deadLive)
	}
	if deadLost.tap != "" || deadLost.record != "" || !deadLost.pipe || deadClosed.tap != "" || deadClosed.record != "" {
		t.Fatalf("dead panes no live tap of ours holds: %+v %+v, want their options unset and no pipe touched", deadLost, deadClosed)
	}
	if inherited.tap != "gl0bal.%42" {
		t.Fatalf("the sweep touched an inherited value: %+v", inherited)
	}
	if got := len(callsContaining(server.calls, "list-panes")); got != 1 || server.psReads != 1 {
		t.Fatalf("the sweep listed panes %d times and read the process table %d times, want once each", got, server.psReads)
	}

	// The deferred dead pane is revived by a respawn that does not remove its
	// capture (any but gc's own, whose list removes it under off): the next
	// sweep closes the pipe our live tap still holds, by the live-tap rule,
	// and unsets the options, so the tap never reads the new program.
	deadLive.dead = false
	server.calls = nil
	if n, deferred, err := p.DisarmProgramCaptures(); err != nil || n != 1 || deferred != 0 {
		t.Fatalf("DisarmProgramCaptures after the revive = %d, %d, %v, want 1 disarmed and none deferred", n, deferred, err)
	}
	if deadLive.tap != "" || deadLive.record != "" || deadLive.pipe || server.taps["d3ad.%4"] {
		t.Fatalf("revived pane: %+v, taps %v; want our tap's pipe closed and the options unset", deadLive, server.taps)
	}
	if n, deferred, err := p.DisarmProgramCaptures(); err != nil || n != 0 || deferred != 0 {
		t.Fatalf("a sweep with nothing left = %d, %d, %v, want nothing to do", n, deferred, err)
	}
}

// Kills a sweep that drops the nonce of a pane whose open pipe may still be
// our live tap's while the process table cannot be read (ps fails, or its
// failure gate is still closed): the orphaned tap would read the pane for
// good, its guarded writes succeeding as no-ops, the sweep would report
// nothing left to finish, and no later heal could prove the pipe ours to
// re-arm it.
func TestDisarmSweepDefersOpenPipesWhileProcessTableUnread(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	live := &healModelPane{session: "a", window: "@1", id: "%1", mark: "agent", tap: "l1ve.%1", pipe: true, record: "v1|x"}
	closed := &healModelPane{session: "b", window: "@2", id: "%2", tap: "cl0sed.%2"}
	server := &healServer{panes: []*healModelPane{live, closed}, taps: map[string]bool{"l1ve.%1": true}, psFails: true}
	p := healProvider(server, &now)

	n, deferred, err := p.DisarmProgramCaptures()
	if err != nil || n != 1 || deferred != 1 {
		t.Fatalf("DisarmProgramCaptures = %d, %d, %v, want 1 disarmed and 1 deferred", n, deferred, err)
	}
	if live.tap != "l1ve.%1" || live.record != "v1|x" || !live.pipe || !server.taps["l1ve.%1"] {
		t.Fatalf("open pipe's pane: %+v, taps %v; want its nonce and record kept while the table is unread", live, server.taps)
	}
	if closed.tap != "" {
		t.Fatalf("pipe-less pane: %+v, want its options unset", closed)
	}

	// ps works again, but its failure gate is still closed: the sweep defers
	// without reading the table.
	server.psFails = false
	if n, deferred, err := p.DisarmProgramCaptures(); err != nil || n != 0 || deferred != 1 || server.psReads != 1 {
		t.Fatalf("DisarmProgramCaptures with the gate closed = %d, %d, %v after %d table reads, want 1 deferred and no new read", n, deferred, err, server.psReads)
	}
	if live.tap != "l1ve.%1" || !live.pipe {
		t.Fatalf("open pipe's pane with the gate closed: %+v, want its nonce kept", live)
	}

	// Once the table reads, the next sweep closes our tap's pipe, and a later
	// declaration arms the pane afresh.
	p.cache.fetcher.(*tmuxFetcher).snapshotGate.succeeded()
	if n, deferred, err := p.DisarmProgramCaptures(); err != nil || n != 1 || deferred != 0 {
		t.Fatalf("DisarmProgramCaptures after the table read = %d, %d, %v, want 1 disarmed and none deferred", n, deferred, err)
	}
	if live.tap != "" || live.record != "" || live.pipe || server.taps["l1ve.%1"] {
		t.Fatalf("after the table read: %+v, taps %v; want our tap's pipe closed and the options unset", live, server.taps)
	}
	mustHeal(t, p, "a", healWant, runtime.ProgramCaptureArmed)
	if !strings.HasSuffix(live.tap, ".%1") || live.tap == "l1ve.%1" || !live.pipe || !server.taps[live.tap] {
		t.Fatalf("re-armed pane: %+v, taps %v; want a new nonce of %%1 and its tap on the pipe", live, server.taps)
	}
}

func TestHealSkipsDeadPanes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, want := range []runtime.ProgramStatusArm{healWant, {}} {
		pane := &healModelPane{session: "s", window: "@1", id: "%5", mark: "agent", tap: "0urs.%5", dead: true}
		server := &healServer{panes: []*healModelPane{pane}}
		p := healProvider(server, &now)
		mustHeal(t, p, "s", want, runtime.ProgramCaptureDeferred)
		if muts := server.mutations(); len(muts) != 0 {
			t.Fatalf("heal (want %+v) acted on a dead pane: %q", want, muts)
		}
	}

	// A marked session whose agent pane is gone has no pane heal may name.
	orphan := &healModelPane{session: "s", window: "@1", id: "%6", mark: "orphan"}
	server := &healServer{panes: []*healModelPane{orphan}}
	p := healProvider(server, &now)
	mustHeal(t, p, "s", healWant, runtime.ProgramCaptureDeferred)
	if muts := server.mutations(); len(muts) != 0 {
		t.Fatalf("heal acted on a session without its agent pane: %q", muts)
	}
}

func TestHealBacksOffPerPane(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	failing := &healModelPane{session: "f", window: "@1", id: "%5", mark: "agent"}
	other := &healModelPane{session: "o", window: "@2", id: "%7", mark: "agent"}
	server := &healServer{panes: []*healModelPane{failing, other}, failTarget: ".%5"}
	p := healProvider(server, &now)

	attempts := func() int { return len(callsContaining(server.calls, "=f:@1.%5")) }
	heal := func(wantErr bool, action runtime.ProgramCaptureAction) {
		t.Helper()
		got, err := p.HealProgramCapture("f", healWant)
		if (err != nil) != wantErr || (!wantErr && got != action) {
			t.Fatalf("HealProgramCapture(f) = %q, %v; want error %v, action %q", got, err, wantErr, action)
		}
	}

	heal(true, "")
	if attempts() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts())
	}
	heal(false, runtime.ProgramCaptureDeferred)
	if attempts() != 1 {
		t.Fatalf("a pane in backoff was attempted again: %d attempts", attempts())
	}
	// Another pane is not held back by this one's backoff.
	mustHeal(t, p, "o", healWant, runtime.ProgramCaptureArmed)

	now = now.Add(captureHealBackoffBase)
	heal(true, "")
	if attempts() != 2 {
		t.Fatalf("attempts after the first window = %d, want 2", attempts())
	}
	// The second failure doubles the window.
	now = now.Add(captureHealBackoffBase)
	heal(false, runtime.ProgramCaptureDeferred)
	now = now.Add(captureHealBackoffBase)
	server.failTarget = ""
	heal(false, runtime.ProgramCaptureArmed)
	if attempts() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts())
	}

	var b captureHealBackoff
	b.now = func() time.Time { return now }
	var last time.Duration
	for range 12 {
		last = b.failed("%1")
	}
	if last != captureHealBackoffMax {
		t.Fatalf("backoff after repeated failures = %v, want the %v cap", last, captureHealBackoffMax)
	}
}

func TestHealMissingSessionIsNotFound(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	server := &healServer{}
	p := healProvider(server, &now)
	if _, err := p.HealProgramCapture("gone", healWant); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("heal of a missing session = %v, want an error wrapping runtime.ErrSessionNotFound", err)
	}
}

func TestProgramCaptureFloorNeverStartsAServer(t *testing.T) {
	bin, _ := writeFakeCompanion(t, "gc-osc7501 dev codec=1 relay=1")
	cfg := DefaultConfig()
	cfg.ProgramStatusBin = bin
	for _, tc := range []struct {
		name string
		out  string
		err  error
		want func(error) bool
	}{
		{"current tmux", "3.4|/tmp/tmux-1000/city", nil, func(err error) bool { return err == nil }},
		{"old tmux", "3.1|/tmp/tmux-1000/city", nil, func(err error) bool { return err != nil && strings.Contains(err.Error(), "older than 3.2") }},
		{"no server", "", ErrNoServer, func(err error) bool { return errors.Is(err, runtime.ErrRuntimeUnavailable) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeExecutor{out: tc.out, err: tc.err}
			tm := NewTmuxWithConfig(cfg)
			tm.exec = fe
			p := &Provider{tm: tm, cfg: cfg}
			if err := p.ProgramCaptureFloor(); !tc.want(err) {
				t.Fatalf("ProgramCaptureFloor = %v", err)
			}
			for _, c := range fe.calls {
				if slices.Contains(c, "start-server") {
					t.Fatalf("the floor check started a server: %q", c)
				}
			}
		})
	}

	missing := DefaultConfig()
	missing.ProgramStatusBin = t.TempDir() + "/absent/gc-osc7501"
	tm := NewTmuxWithConfig(missing)
	tm.exec = &fakeExecutor{out: "3.4|/tmp/s"}
	if err := (&Provider{tm: tm, cfg: missing}).ProgramCaptureFloor(); err == nil {
		t.Fatal("ProgramCaptureFloor passed without a companion")
	}
}
