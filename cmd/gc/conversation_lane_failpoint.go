package main

// Conversation lane failpoints (OJ-12x): GC_LANE_FAILPOINT makes a gc process
// kill itself (SIGKILL) at one crash point of one of its lanes, so the X2
// kill matrix (test/conversationlane) can show what a crash at that point
// leaves behind and that the next lane host recovers it.
//
//	GC_LANE_FAILPOINT=<host>:<stage>[:<n>]
//
// host is the lane's host name: ctl (the controller's lane host) or cli (gc
// session nudge's inline pass). A gc that hosts no such lane never fires, so
// a child that inherits the variable is unaffected. stage is one of:
//   - propose (cli only): the CLI's op is durable; nothing is taken or rung;
//   - take: a Take's guarded batch landed and its effect has not started;
//   - effect: the effect returned, link wait included, and its Done is not
//     written;
//   - done: a pass's Done landed accepted.
//
// n fires on the n-th hit of that host and stage in this process (default 1).
//
// Only a gc linked with test hooks (//cmd/gc:gc_testhooks, the
// internal/clock.testHooks stamp) honors the variable; any other gc ignores
// it and says so once on stderr. An armed failpoint says so once, on the
// first consultation, with clock.BackstopActiveNotice in the line, so a test
// can tell a gc that ignored the variable from one that never reached the
// stage. A malformed value is ignored with a notice naming why.

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// laneFailpointEnv names the failpoint variable.
const laneFailpointEnv = "GC_LANE_FAILPOINT"

// laneFailStage is a failpoint's stage.
type laneFailStage string

// Failpoint stages.
const (
	laneFailPropose laneFailStage = "propose"
	laneFailTake    laneFailStage = "take"
	laneFailEffect  laneFailStage = "effect"
	laneFailDone    laneFailStage = "done"
)

// laneFailpoint is a parsed GC_LANE_FAILPOINT value.
type laneFailpoint struct {
	host  string
	stage laneFailStage
	n     int
}

// parseLaneFailpoint parses <host>:<stage>[:<n>]: host ctl or cli, a known
// stage (propose only for cli), and n a whole number >= 1 (default 1).
func parseLaneFailpoint(raw string) (laneFailpoint, error) {
	parts := strings.Split(raw, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return laneFailpoint{}, fmt.Errorf("not <host>:<stage>[:<n>]")
	}
	fp := laneFailpoint{host: parts[0], stage: laneFailStage(parts[1]), n: 1}
	switch fp.host {
	case laneHostName, cliLaneHostName:
	default:
		return laneFailpoint{}, fmt.Errorf("unknown host %q (want %s or %s)", fp.host, laneHostName, cliLaneHostName)
	}
	switch fp.stage {
	case laneFailTake, laneFailEffect, laneFailDone:
	case laneFailPropose:
		if fp.host != cliLaneHostName {
			return laneFailpoint{}, fmt.Errorf("stage propose is the %s host's only", cliLaneHostName)
		}
	default:
		return laneFailpoint{}, fmt.Errorf("unknown stage %q (want propose, take, effect or done)", fp.stage)
	}
	if len(parts) == 3 {
		n, err := strconv.Atoi(parts[2])
		if err != nil || n < 1 || parts[2] != strconv.Itoa(n) {
			return laneFailpoint{}, fmt.Errorf("hit count %q is not a whole number >= 1", parts[2])
		}
		fp.n = n
	}
	return fp, nil
}

// laneFailpointHooksEnabled reports whether this gc honors failpoints; a seam
// for unit tests.
var laneFailpointHooksEnabled = clock.TestHooksEnabled

// laneFailpointKill ends the process at a failpoint: SIGKILL to itself, then
// it blocks so nothing more runs if the signal is late. A seam for unit
// tests.
var laneFailpointKill = func() {
	if p, err := os.FindProcess(os.Getpid()); err == nil {
		_ = p.Kill()
	}
	select {}
}

// laneFailpointNotice is where the one-time notice goes; a seam for tests.
var laneFailpointNotice io.Writer = os.Stderr

// laneFailpointLookupEnv reads the variable; a seam for tests.
var laneFailpointLookupEnv = os.LookupEnv

// laneFailpoints is the process's failpoint state: the variable, read once,
// and the hit counts.
type laneFailpoints struct {
	once  sync.Once
	armed bool
	fp    laneFailpoint
	mu    sync.Mutex
	hits  int
	fired bool
}

// laneFailpointState is the process's failpoint state; tests replace it.
var laneFailpointState = &laneFailpoints{}

// load reads the variable once and prints its notice.
func (s *laneFailpoints) load() (laneFailpoint, bool) {
	s.once.Do(func() {
		raw, set := laneFailpointLookupEnv(laneFailpointEnv)
		if !set || strings.TrimSpace(raw) == "" {
			return
		}
		note := func(msg string) {
			fmt.Fprintf(laneFailpointNotice, "gc: WARNING: %s=%q: %s\n", laneFailpointEnv, raw, msg) //nolint:errcheck // best-effort notice
		}
		if !laneFailpointHooksEnabled() {
			note("ignored: this gc binary is not built with test hooks (test-only knob)")
			return
		}
		fp, err := parseLaneFailpoint(raw)
		if err != nil {
			note("ignored: " + err.Error())
			return
		}
		hit := ""
		if fp.n > 1 {
			hit = fmt.Sprintf(" (hit %d)", fp.n)
		}
		note(fmt.Sprintf("%s: this process kills itself (SIGKILL) at %s%s of its %s lane", clock.BackstopActiveNotice, fp.stage, hit, fp.host))
		s.fp, s.armed = fp, true
	})
	return s.fp, s.armed
}

// armedFor reports whether a failpoint is armed for host.
func (s *laneFailpoints) armedFor(host string) (laneFailpoint, bool) {
	fp, ok := s.load()
	return fp, ok && fp.host == host
}

// hit counts one hit of host and stage and fires on the armed n-th one.
func (s *laneFailpoints) hit(host string, stage laneFailStage) {
	fp, ok := s.armedFor(host)
	if !ok || fp.stage != stage {
		return
	}
	s.mu.Lock()
	s.hits++
	fire := !s.fired && s.hits == fp.n
	if fire {
		s.fired = true
	}
	s.mu.Unlock()
	if fire {
		laneFailpointKill()
	}
}

// laneFailpointHit is a failpoint check at stage of host's lane.
func laneFailpointHit(host string, stage laneFailStage) {
	laneFailpointState.hit(host, stage)
}

// armLaneFailpoints arms lane's take and effect crash points (OnStage) when
// a take or effect failpoint is armed for host; otherwise it does nothing,
// so a production lane pays nothing.
func armLaneFailpoints(lane *opjournal.ConversationLane, host string) {
	fp, ok := laneFailpointState.armedFor(host)
	if !ok || (fp.stage != laneFailTake && fp.stage != laneFailEffect) {
		return
	}
	lane.OnStage(func(s opjournal.LaneStage) {
		switch s {
		case opjournal.LaneStageTaken:
			laneFailpointHit(host, laneFailTake)
		case opjournal.LaneStageEffected:
			laneFailpointHit(host, laneFailEffect)
		}
	})
}

// laneFailpointAfterPass is the done failpoint: a hit for a pass of host's
// lane whose Done landed accepted.
func laneFailpointAfterPass(host string, res opjournal.PassResult) {
	if res.Done == opjournal.DoneAccepted {
		laneFailpointHit(host, laneFailDone)
	}
}
