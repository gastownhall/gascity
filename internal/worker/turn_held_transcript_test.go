package worker

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// heldTurnSession is a keyed Codex tmux session whose runtime names the files
// its agent process holds open from ff.HeldOpen.
func heldTurnSession(t *testing.T) (turnSession, runtime.FullFake) {
	t.Helper()
	ff := runtime.FullFake{Fake: runtime.NewFake()}
	s := newTurnSession(t, profiled(runtime.ProfileTmux, ff), "codex", "")
	return s, ff
}

// outsideRolloutPath is where a Codex whose home lies outside the session's
// transcript search roots writes the rollout of conversation id.
func outsideRolloutPath(t *testing.T, id string) string {
	t.Helper()
	now := time.Now()
	home := filepath.Join(t.TempDir(), "codex-city-maintainer", "sessions")
	return filepath.Join(home, now.Format("2006"), now.Format("01"), now.Format("02"), "rollout-"+now.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
}

// A seat whose provider home is outside the search roots still has turns:
// its rollout is found as the file its own agent process holds open
// (ga-crzq6e).
func TestCurrentTurnReadsTheTranscriptItsProcessHoldsOutsideTheSearchRoots(t *testing.T) {
	s, ff := heldTurnSession(t)
	rollout := outsideRolloutPath(t, s.info.SessionKey)
	writeTranscriptLines(t, rollout, codexMeta(s.info.SessionKey, s.info.WorkDir),
		codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142), codexComplete(codexTurn1, "2026-10-08T02:22:30.500Z"))
	ff.HeldOpen = map[string][]string{s.info.SessionName: {"/dev/pts/3", s.info.WorkDir, rollout}}

	got, err := s.handle.CurrentTurn(context.Background())
	if err != nil {
		t.Fatalf("CurrentTurn: %v", err)
	}
	if got.Current.Active() || len(got.Recent) != 1 || got.Recent[0].TurnID != codexTurn1 {
		t.Fatalf("CurrentTurn = %+v, want the held rollout's %s", got, codexTurn1)
	}
}

// The search roots keep deciding where they find the transcript: a held file
// is consulted only when they find none.
func TestCurrentTurnPrefersTheSearchRootsOverHeldFiles(t *testing.T) {
	s, ff := heldTurnSession(t)
	s.writeCompletedTurn(t, "codex")
	elsewhere := outsideRolloutPath(t, s.info.SessionKey)
	writeTranscriptLines(t, elsewhere, codexMeta(s.info.SessionKey, s.info.WorkDir),
		codexStarted(codexTurn2, "2026-10-08T03:00:00.000Z", 1791429600), codexComplete(codexTurn2, "2026-10-08T03:00:09.000Z"))
	ff.HeldOpen = map[string][]string{s.info.SessionName: {elsewhere}}

	got, err := s.handle.CurrentTurn(context.Background())
	if err != nil {
		t.Fatalf("CurrentTurn: %v", err)
	}
	if len(got.Recent) != 1 || got.Recent[0].TurnID != codexTurn1 {
		t.Fatalf("CurrentTurn = %+v, want the search roots' %s", got, codexTurn1)
	}
}

// Without the conversation's rollout among its held files, or with held
// files that cannot be read, a session the search roots cannot place still
// has unknown turns (runtime.ErrRuntimeUnavailable), never a gone session
// and never another conversation's turns.
func TestCurrentTurnWithoutAHeldTranscriptIsUnavailable(t *testing.T) {
	cases := []struct {
		name string
		set  func(t *testing.T, s turnSession, ff runtime.FullFake)
	}{
		{name: "nothing held", set: func(*testing.T, turnSession, runtime.FullFake) {}},
		{name: "another conversation's rollout held", set: func(t *testing.T, s turnSession, ff runtime.FullFake) {
			foreign := outsideRolloutPath(t, otherConversation)
			writeTranscriptLines(t, foreign, codexMeta(otherConversation, s.info.WorkDir), codexStarted(codexTurn2, "2026-10-08T03:00:00.000Z", 1791429600))
			ff.HeldOpen = map[string][]string{s.info.SessionName: {foreign}}
		}},
		{name: "a rollout named for the key recording another conversation", set: func(t *testing.T, s turnSession, ff runtime.FullFake) {
			renamed := outsideRolloutPath(t, s.info.SessionKey)
			writeTranscriptLines(t, renamed, codexMeta(otherConversation, s.info.WorkDir), codexStarted(codexTurn2, "2026-10-08T03:00:00.000Z", 1791429600))
			ff.HeldOpen = map[string][]string{s.info.SessionName: {renamed}}
		}},
		{name: "held files unreadable", set: func(_ *testing.T, s turnSession, ff runtime.FullFake) {
			ff.HeldOpenErrors = map[string]error{s.info.SessionName: errors.New("reading open files of pid 4300: input/output error")}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ff := heldTurnSession(t)
			tc.set(t, s, ff)
			got, err := s.handle.CurrentTurn(context.Background())
			assertNoHistory(t, got, err, runtime.ErrRuntimeUnavailable)
			if errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("CurrentTurn error = %v, want unavailable, not a gone session", err)
			}
		})
	}
}

// Kills a held-file read made apart from the turn read's other runtime probes
// (observeTurnRead): a read of the session's held files in flight, which a
// wedged runtime holds, keeps CurrentTurn, which shares it, only until ctx
// ends, its turns unknown; FreshTurn, whose reads are its own, still reads
// the held rollout.
func TestTurnReadsTheHeldFilesAsARuntimeProbe(t *testing.T) {
	s, ff := heldTurnSession(t)
	rollout := outsideRolloutPath(t, s.info.SessionKey)
	writeTranscriptLines(t, rollout, codexMeta(s.info.SessionKey, s.info.WorkDir),
		codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142), codexComplete(codexTurn1, "2026-10-08T02:22:30.500Z"))
	ff.HeldOpen = map[string][]string{s.info.SessionName: {rollout}}
	startSharedRead(t, turnObservationKey("held", s.handle.turnObservationScope(), s.info.SessionName), heldFileReading{})

	joined, cancelJoined := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelJoined()
	got, err := s.handle.CurrentTurn(joined)
	assertNoHistory(t, got, err, runtime.ErrRuntimeUnavailable)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err = s.handle.FreshTurn(ctx)
	if err != nil || len(got.Recent) != 1 || got.Recent[0].TurnID != codexTurn1 {
		t.Fatalf("FreshTurn beside the held read in flight = %+v, %v; want %s from a read of its own", got, err, codexTurn1)
	}
}
