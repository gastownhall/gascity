package main

// Op recording failpoints (OJ-8, X12): GC_OPS_FAILPOINT makes a gc process die
// at, or misbehave at, one crash point (legacyop.CrashPoint) of one of its
// recorded legacy executions, so the X12 kill matrix (test/conversationlane)
// can show what a crash there leaves behind and how a same-key retry, the
// sweeper or a rerun settles it.
//
//	GC_OPS_FAILPOINT=<surface>:<point>[:<kind>][:<n>]
//
// surface is api (the controller's or supervisor's API Servers) or cli (a CLI
// verb's recorder); a gc that records for the other surface never fires. point
// is one of:
//   - k1: the op row is durable and nothing has run (kill);
//   - k2 (api only): the 202 is answered and the delivery has not started
//     (park the request, so the test kills the process holding the 202);
//   - k3: the legacy effect ran and nothing about the op is written (kill);
//   - k4: the op's close landed and nothing is answered (kill);
//   - k5 (api only): every close of one op fails, foreground and background
//     (fault), so the client holds its answer while the op stays open.
//
// kind, optional, counts only ops of that kind (opjournal.Lookup). n, optional
// and last, fires on the n-th op of that surface (and kind) to reach the
// point in this process (default 1); for k5, the n-th distinct op to reach a
// close attempt.
//
// Only a gc linked with test hooks (//cmd/gc:gc_testhooks) honors the
// variable; any other gc ignores it and says so once on stderr. An armed
// failpoint says so once, on the first consultation, with
// clock.BackstopActiveNotice in the line. A malformed value is ignored with a
// notice naming why.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
)

// opsFailpointEnv names the failpoint variable.
const opsFailpointEnv = "GC_OPS_FAILPOINT"

// errOpsFailpointCloseFault is the error every close of a k5 op fails with.
var errOpsFailpointCloseFault = errors.New("ops failpoint k5: injected close failure")

// opsFailpointAction is what a failpoint does when it fires.
type opsFailpointAction int

const (
	opsFailKill opsFailpointAction = iota + 1
	opsFailPark
	opsFailFault
)

// opsFailpointPoints maps each point name to its crash point and action.
var opsFailpointPoints = map[string]struct {
	point  legacyop.CrashPoint
	action opsFailpointAction
}{
	"k1": {legacyop.CrashPointAdmitted, opsFailKill},
	"k2": {legacyop.CrashPointAnswered, opsFailPark},
	"k3": {legacyop.CrashPointEffected, opsFailKill},
	"k4": {legacyop.CrashPointClosed, opsFailKill},
	"k5": {legacyop.CrashPointClosing, opsFailFault},
}

// opsFailpoint is a parsed GC_OPS_FAILPOINT value.
type opsFailpoint struct {
	surface string // legacyop.SurfaceAPI or SurfaceCLI
	name    string // k1..k5
	point   legacyop.CrashPoint
	action  opsFailpointAction
	kind    opjournal.Kind // "" = any
	n       int
}

// parseOpsFailpoint parses <surface>:<point>[:<kind>][:<n>].
func parseOpsFailpoint(raw string) (opsFailpoint, error) {
	parts := strings.Split(raw, ":")
	if len(parts) < 2 || len(parts) > 4 {
		return opsFailpoint{}, fmt.Errorf("not <surface>:<point>[:<kind>][:<n>]")
	}
	fp := opsFailpoint{surface: parts[0], name: parts[1], n: 1}
	switch fp.surface {
	case legacyop.SurfaceAPI, legacyop.SurfaceCLI:
	default:
		return opsFailpoint{}, fmt.Errorf("unknown surface %q (want %s or %s)", fp.surface, legacyop.SurfaceAPI, legacyop.SurfaceCLI)
	}
	p, ok := opsFailpointPoints[fp.name]
	if !ok {
		return opsFailpoint{}, fmt.Errorf("unknown point %q (want k1, k2, k3, k4 or k5)", fp.name)
	}
	fp.point, fp.action = p.point, p.action
	if fp.surface == legacyop.SurfaceCLI && (fp.name == "k2" || fp.name == "k5") {
		return opsFailpoint{}, fmt.Errorf("point %s is the %s surface's only", fp.name, legacyop.SurfaceAPI)
	}
	rest := parts[2:]
	for i, field := range rest {
		if looksLikeHitCount(field) {
			if i != len(rest)-1 {
				return opsFailpoint{}, fmt.Errorf("hit count %q is not the last field", field)
			}
			n, err := strconv.Atoi(field)
			if err != nil || n < 1 || field != strconv.Itoa(n) {
				return opsFailpoint{}, fmt.Errorf("hit count %q is not a whole number >= 1", field)
			}
			fp.n = n
			continue
		}
		if fp.kind != "" {
			return opsFailpoint{}, fmt.Errorf("two kinds %q and %q", fp.kind, field)
		}
		if _, err := opjournal.Lookup(opjournal.Kind(field)); err != nil {
			return opsFailpoint{}, fmt.Errorf("unknown kind %q", field)
		}
		fp.kind = opjournal.Kind(field)
	}
	return fp, nil
}

// looksLikeHitCount reports whether a field is meant as a hit count: it
// starts with a digit or a sign. No op kind does.
func looksLikeHitCount(field string) bool {
	return field != "" && strings.ContainsRune("0123456789+-", rune(field[0]))
}

// describe is the armed notice's account of fp.
func (fp opsFailpoint) describe() string {
	var does string
	switch fp.action {
	case opsFailKill:
		does = "kills itself (SIGKILL)"
	case opsFailPark:
		does = "parks the request"
	case opsFailFault:
		does = "fails every close of one op"
	}
	of := ""
	if fp.kind != "" {
		of = " of kind " + string(fp.kind)
	}
	hit := ""
	if fp.n > 1 {
		hit = fmt.Sprintf(" (hit %d)", fp.n)
	}
	return fmt.Sprintf("%s: this process %s at %s (%s) of its %s ops%s%s",
		clock.BackstopActiveNotice, does, fp.name, fp.point, fp.surface, of, hit)
}

// opsFailpoints is a process's op failpoint state: the variable, read once,
// and the hits. Its seams are fields so unit tests build their own.
type opsFailpoints struct {
	lookupEnv    func(string) (string, bool)
	hooksEnabled func() bool
	notice       io.Writer
	kill, park   func()

	once  sync.Once
	raw   string
	fp    opsFailpoint
	armed bool

	mu      sync.Mutex
	hits    int
	fired   bool
	seen    map[string]bool // k5: ops that reached a close attempt
	faulted string          // k5: the op every close of which fails
}

// newOpsFailpoints is the production state: the process environment, the
// test-hooks stamp, stderr, SIGKILL and a forever block.
func newOpsFailpoints() *opsFailpoints {
	return &opsFailpoints{
		lookupEnv:    os.LookupEnv,
		hooksEnabled: clock.TestHooksEnabled,
		notice:       os.Stderr,
		kill:         killSelfAtFailpoint,
		park:         func() { select {} },
	}
}

// opsFailpointState is the process's op failpoint state.
var opsFailpointState = newOpsFailpoints()

// load reads the variable once and prints its notice.
func (s *opsFailpoints) load() (opsFailpoint, bool) {
	s.once.Do(func() {
		raw, set := s.lookupEnv(opsFailpointEnv)
		if !set || strings.TrimSpace(raw) == "" {
			return
		}
		s.raw = raw
		if !s.hooksEnabled() {
			failpointNotice(s.notice, opsFailpointEnv, raw, "ignored: this gc binary is not built with test hooks (test-only knob)")
			return
		}
		fp, err := parseOpsFailpoint(raw)
		if err != nil {
			failpointNotice(s.notice, opsFailpointEnv, raw, "ignored: "+err.Error())
			return
		}
		failpointNotice(s.notice, opsFailpointEnv, raw, fp.describe())
		s.fp, s.armed = fp, true
	})
	return s.fp, s.armed
}

// hook is the crash-point hook for a recorder of surface: nil unless a
// failpoint is armed for that surface, so a production recorder carries none.
func (s *opsFailpoints) hook(surface string) legacyop.CrashPointFunc {
	if fp, ok := s.load(); !ok || fp.surface != surface {
		return nil
	}
	return s.at
}

// at is the armed hook: it counts the matching hits and fires on the armed
// one. Only a k5 fault returns an error.
func (s *opsFailpoints) at(p legacyop.CrashPoint, op opjournal.Op) error {
	fp := s.fp
	if p != fp.point || (fp.kind != "" && op.Kind != fp.kind) {
		return nil
	}
	if fp.action == opsFailFault {
		return s.fault(op)
	}
	s.mu.Lock()
	s.hits++
	fire := !s.fired && s.hits == fp.n
	if fire {
		s.fired = true
	}
	s.mu.Unlock()
	if !fire {
		return nil
	}
	if fp.action == opsFailPark {
		failpointNotice(s.notice, opsFailpointEnv, s.raw, fmt.Sprintf("%s: op %s parked after its answer, before its effect", clock.BackstopActiveNotice, op.ID))
		s.park()
		return nil
	}
	s.kill()
	return nil
}

// fault is k5: the n-th distinct op to reach a close attempt becomes the
// faulted op, and every close of it fails from then on.
func (s *opsFailpoints) fault(op opjournal.Op) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.faulted != "" {
		if op.ID == s.faulted {
			return errOpsFailpointCloseFault
		}
		return nil
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	s.seen[op.ID] = true
	if len(s.seen) != s.fp.n {
		return nil
	}
	s.faulted = op.ID
	failpointNotice(s.notice, opsFailpointEnv, s.raw, fmt.Sprintf("%s: op %s: every close of it fails from now on", clock.BackstopActiveNotice, op.ID))
	return errOpsFailpointCloseFault
}

// OpsCrashPoints implements api.OpsCrashPointsProvider: the API recorders'
// GC_OPS_FAILPOINT hook, nil unless one is armed for the api surface.
func (cs *controllerState) OpsCrashPoints() legacyop.CrashPointFunc {
	return opsFailpointState.hook(legacyop.SurfaceAPI)
}

var _ api.OpsCrashPointsProvider = (*controllerState)(nil)
