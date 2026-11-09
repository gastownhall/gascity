package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// These tests drive the REAL start-failure recorders, legacy's
// commitStartFailure and the v2 failure Call (readStartFailure), to a
// startup-health threshold crossing, then run the escalator over what they
// recorded. They pin that a start that died during startup is recorded as
// FailureKindStartupDeath, so the escalation carries the redacted
// start-stderr.log tail, and that the tail is read from the directory the
// runtime wrote it to: the runtime session name of the failed attempt, which
// for a pool row is not the episode key (ga-o04bfr.1.2, round 2: B2, S1).

const deathTestSecret = "sk-live-0123456789abcdef"

// startAttempt is one failed start: the error the provider layer returned and
// whether the start died during startup (startResult.diedDuringStartup).
type startAttempt struct {
	err  error
	died bool
}

var (
	attemptDied  = startAttempt{err: errors.New(`session "x" died during startup`), died: true}
	attemptOther = startAttempt{err: errors.New("readiness probe failed")}
	attemptSlow  = startAttempt{err: fmt.Errorf("starting: %w", context.DeadlineExceeded)}
)

// deathScenario is the state after a run of failed starts of one pool slot.
type deathScenario struct {
	store    beads.Store
	cityPath string
	key      string // the episode key
	lastName string // the runtime session name of the last attempt
}

// writeAttemptStderr writes the start-stderr.log the tmux adapter leaves for
// the runtime session name, under the city's session diagnostics directory.
func writeAttemptStderr(t *testing.T, cityPath, runtimeName, content string) {
	t.Helper()
	dir := filepath.Join(citylayout.SessionDiagnosticsDir(cityPath), runtimeName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "start-stderr.log"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func attemptMarker(n int) string { return fmt.Sprintf("CRASH-OF-ATTEMPT-%d", n) }

// failLegacyAttempts fails one pool slot's start once per attempt through
// commitStartFailure, each attempt a new bead with a new bead-scoped runtime
// name, as a failed pool create is.
func failLegacyAttempts(t *testing.T, attempts ...startAttempt) deathScenario {
	t.Helper()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	sc := deathScenario{store: store, cityPath: t.TempDir()}
	for i, at := range attempts {
		now := time.Date(2026, 8, 15, 0, 0, i+1, 0, time.UTC)
		open, err := loadSessionBeads(store)
		if err != nil {
			t.Fatalf("attempt %d: loadSessionBeads: %v", i+1, err)
		}
		info, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, newSessionBeadSnapshot(open), now, poolChurnIdentity(), "")
		if err != nil {
			t.Fatalf("attempt %d: createPoolSessionBeadWithAlias: %v", i+1, err)
		}
		name := strings.TrimSpace(info.SessionNameMetadata)
		sc.key, sc.lastName = startupHealthEpisodeKey(info, name), name
		secret := ""
		if i == len(attempts)-1 {
			secret = " token " + deathTestSecret
		}
		writeAttemptStderr(t, sc.cityPath, name, attemptMarker(i+1)+secret+"\n")
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("provisioning box %q: %v", name, err)
		}
		commitStartFailure(startResult{
			prepared: preparedStart{candidate: startCandidate{
				info: info,
				tp:   TemplateParams{SessionName: name, TemplateName: poolChurnTemplate, Command: "true"},
			}},
			err:               at.err,
			outcome:           TraceOutcomeProviderError,
			started:           now,
			finished:          now,
			rollbackPending:   true,
			provider:          sp,
			diedDuringStartup: at.died,
		}, sessionFrontDoor(store), &clock.Fake{Time: now}, events.Discard, 0, io.Discard, nil)
	}
	return sc
}

// failV2Attempts fails one pool slot's start once per attempt through the v2
// failure Call, a pending create that did not commit.
func failV2Attempts(t *testing.T, attempts ...startAttempt) deathScenario {
	t.Helper()
	store := beads.NewMemStore()
	sc := deathScenario{store: store, cityPath: t.TempDir()}
	info, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, newSessionBeadSnapshot(nil), time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC), poolChurnIdentity(), "")
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithAlias: %v", err)
	}
	base := strings.TrimSpace(info.SessionNameMetadata)
	caps := txCaps{
		episode: startupHealthRecord{store: store},
		start:   startCaps{clock: newFakePlannerClock(plannerT0)},
	}
	for i, at := range attempts {
		// A v2 pending create that does not commit keeps its row; the
		// runtime name stands for the attempt's own bead-scoped name.
		name := fmt.Sprintf("%s-attempt%d", base, i+1)
		sc.key, sc.lastName = startupHealthEpisodeKey(info, name), name
		secret := ""
		if i == len(attempts)-1 {
			secret = " token " + deathTestSecret
		}
		writeAttemptStderr(t, sc.cityPath, name, attemptMarker(i+1)+secret+"\n")
		cause := causeStartError
		if at.died {
			cause = causeDiedDuringStartup
		}
		readStartFailure(context.Background(), caps, startFailure{
			cause: cause, err: at.err, pending: true, died: at.died,
			row: info, name: name, episode: sc.key,
		})
	}
	return sc
}

// escalate runs the escalator over the scenario's store and returns the one
// mail it sent.
func (sc deathScenario) escalate(t *testing.T) (session.StartupHealthEpisode, mail.Message) {
	t.Helper()
	front := sessionFrontDoor(sc.store)
	inbox := mail.NewFake()
	e := &startupHealthEscalator{
		store:    front,
		mail:     inbox,
		to:       escalationTestRecipient,
		cityPath: sc.cityPath,
		now:      func() time.Time { return escalationTestNow },
	}
	e.sweep()
	ep, err := front.LoadStartupHealthEpisode(sc.key)
	if err != nil {
		t.Fatalf("loading episode %q: %v", sc.key, err)
	}
	msgs := inbox.Messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want exactly 1 (episode %+v)", len(msgs), ep)
	}
	return ep, msgs[0]
}

type deathPath struct {
	name string
	fail func(t *testing.T, attempts ...startAttempt) deathScenario
}

var deathPaths = []deathPath{
	{"legacy", failLegacyAttempts},
	{"v2", failV2Attempts},
}

func repeatAttempt(at startAttempt, n int) []startAttempt {
	out := make([]startAttempt, n)
	for i := range out {
		out[i] = at
	}
	return out
}

// A start that died during startup records startup_death, and the
// escalation carries the redacted tail of the log of the attempt that died:
// under the pool attempt's own runtime name, not the episode key.
func TestStartDeathEpisodeEscalatesWithTheRedactedStartStderrTail(t *testing.T) {
	t.Setenv("GC_TEST_ESCALATION_API_TOKEN", deathTestSecret)
	for _, p := range deathPaths {
		t.Run(p.name, func(t *testing.T) {
			sc := p.fail(t, repeatAttempt(attemptDied, defaultMaxWakeAttempts)...)
			if sc.key == sc.lastName {
				t.Fatalf("fixture: episode key %q equals the runtime name; want a pool row whose key differs", sc.key)
			}

			ep, msg := sc.escalate(t)

			if ep.Kind != session.FailureKindStartupDeath {
				t.Fatalf("episode kind = %q, want %q", ep.Kind, session.FailureKindStartupDeath)
			}
			if !strings.Contains(msg.Body, attemptMarker(defaultMaxWakeAttempts)) {
				t.Fatalf("mail body lacks the last attempt's start-stderr.log tail:\n%s", msg.Body)
			}
			if strings.Contains(msg.Body, attemptMarker(1)) {
				t.Fatalf("mail body carries an earlier attempt's log:\n%s", msg.Body)
			}
			if strings.Contains(msg.Body, deathTestSecret) {
				t.Fatalf("secret leaked into the mail body:\n%s", msg.Body)
			}
		})
	}
}

// A plain start error records other and attaches no tail, a deadline records
// timeout and attaches none, even with a log on disk.
func TestPlainAndDeadlineStartErrorsKeepTheirKindAndAttachNoTail(t *testing.T) {
	cases := []struct {
		name string
		at   startAttempt
		kind session.FailureKind
	}{
		{"plain error", attemptOther, session.FailureKindOther},
		{"deadline", attemptSlow, session.FailureKindTimeout},
	}
	for _, p := range deathPaths {
		for _, c := range cases {
			t.Run(p.name+"/"+c.name, func(t *testing.T) {
				sc := p.fail(t, repeatAttempt(c.at, defaultMaxWakeAttempts)...)

				ep, msg := sc.escalate(t)

				if ep.Kind != c.kind {
					t.Fatalf("episode kind = %q, want %q", ep.Kind, c.kind)
				}
				if strings.Contains(msg.Body, "CRASH-OF-ATTEMPT") {
					t.Fatalf("a %s episode attached a start-stderr.log tail:\n%s", ep.Kind, msg.Body)
				}
			})
		}
	}
}

// A mixed episode ends with the kind of its LAST failure: the log on disk is
// the last crash's, so a death followed by another failure attaches no stale
// tail, and a failure followed by a death attaches the death's.
func TestMixedEpisodeEndsWithTheLastFailureKind(t *testing.T) {
	for _, p := range deathPaths {
		t.Run(p.name+"/death then other", func(t *testing.T) {
			sc := p.fail(t, append(repeatAttempt(attemptDied, defaultMaxWakeAttempts-1), attemptOther)...)
			ep, msg := sc.escalate(t)
			if ep.Kind != session.FailureKindOther {
				t.Fatalf("episode kind = %q, want %q", ep.Kind, session.FailureKindOther)
			}
			if strings.Contains(msg.Body, "CRASH-OF-ATTEMPT") {
				t.Fatalf("a stale death's tail was attached:\n%s", msg.Body)
			}
		})
		t.Run(p.name+"/other then death", func(t *testing.T) {
			sc := p.fail(t, append(repeatAttempt(attemptOther, defaultMaxWakeAttempts-2), attemptSlow, attemptDied)...)
			ep, msg := sc.escalate(t)
			if ep.Kind != session.FailureKindStartupDeath {
				t.Fatalf("episode kind = %q, want %q", ep.Kind, session.FailureKindStartupDeath)
			}
			if !strings.Contains(msg.Body, attemptMarker(defaultMaxWakeAttempts)) {
				t.Fatalf("mail body lacks the dying attempt's tail:\n%s", msg.Body)
			}
		})
	}
}
