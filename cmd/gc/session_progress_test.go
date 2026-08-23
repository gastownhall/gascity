package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestOpenPoolSessionCountForTemplateExcludesClosed guards the min-floor scan's
// read-after-close contract: a session whose Info snapshot is Closed (the shape
// the reconciler produces after refreshing a mid-tick close onto infoByID) must
// not count toward the pool's open floor. Only open, same-template sessions are
// counted; a closed same-template session and an open other-template session are
// both excluded.
func TestOpenPoolSessionCountForTemplateExcludesClosed(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Ranged as a map (Step 5e): membership + Closed/template drive the count, not
	// order. Two open workers count; the closed worker and the scout are excluded.
	infoByID := map[string]sessionpkg.Info{
		"s-open-1":        {ID: "s-open-1", Template: "worker"},
		"s-open-2":        {ID: "s-open-2", Template: "worker"},
		"s-closed-worker": {ID: "s-closed-worker", Template: "worker", Closed: true},
		"s-open-scout":    {ID: "s-open-scout", Template: "scout"},
	}

	if got := openPoolSessionCountForTemplate(infoByID, cfg, "worker"); got != 2 {
		t.Fatalf("openPoolSessionCountForTemplate = %d, want 2 (two open workers; the closed worker and the scout must be excluded)", got)
	}
}

// floorCfg builds a one-agent city whose pool has the given min_active_sessions
// floor, matching the shape findAgentByTemplate/EffectiveMinActiveSessions read.
func floorCfg(minFloor int) *config.City {
	agent := config.Agent{Name: "worker"}
	if minFloor > 0 {
		agent.MinActiveSessions = intPtr(minFloor)
	}
	return &config.City{Agents: []config.Agent{agent}}
}

// warm builds an open, active same-template session Info for the floor-exempt
// selection tests, keyed by its bead ID (the deterministic ordering key).
func warm(id string) sessionpkg.Info {
	return sessionpkg.Info{ID: id, Template: "worker", State: sessionpkg.StateActive}
}

// TestIsMinFloorExemptIdleSession pins the deterministic per-session floor
// selection that keeps min_active_sessions sessions warm across idle timeout
// (sc-5mtyhy). It is the acceptance-2/4 core: the minSess lowest-bead-id warm
// sessions are exempt and stay exempt tick over tick; above-floor elastic
// sessions are never exempt and idle-reclaim normally.
func TestIsMinFloorExemptIdleSession(t *testing.T) {
	t.Run("at floor: the single warm session is exempt", func(t *testing.T) {
		infoByID := map[string]sessionpkg.Info{"s-a": warm("s-a")}
		if !isMinFloorExemptIdleSession(infoByID, floorCfg(1), "worker", "s-a") {
			t.Fatal("min=1, one warm session: it must be the exempt floor member")
		}
	})

	t.Run("above floor: only the minSess lowest-id sessions are exempt", func(t *testing.T) {
		// min=1, three warm sessions: only the lowest-id (s-a) stays warm; the
		// two above-floor elastic sessions (s-b, s-c) idle-reclaim (acceptance 2).
		infoByID := map[string]sessionpkg.Info{
			"s-a": warm("s-a"), "s-b": warm("s-b"), "s-c": warm("s-c"),
		}
		cfg := floorCfg(1)
		if !isMinFloorExemptIdleSession(infoByID, cfg, "worker", "s-a") {
			t.Error("s-a (lowest id) must be exempt")
		}
		if isMinFloorExemptIdleSession(infoByID, cfg, "worker", "s-b") {
			t.Error("s-b is above the floor and must NOT be exempt")
		}
		if isMinFloorExemptIdleSession(infoByID, cfg, "worker", "s-c") {
			t.Error("s-c is above the floor and must NOT be exempt")
		}
	})

	t.Run("min=2 exempts the two lowest-id, not the third", func(t *testing.T) {
		infoByID := map[string]sessionpkg.Info{
			"s-a": warm("s-a"), "s-b": warm("s-b"), "s-c": warm("s-c"),
		}
		cfg := floorCfg(2)
		if !isMinFloorExemptIdleSession(infoByID, cfg, "worker", "s-a") ||
			!isMinFloorExemptIdleSession(infoByID, cfg, "worker", "s-b") {
			t.Error("the two lowest-id sessions (s-a, s-b) must both be exempt at min=2")
		}
		if isMinFloorExemptIdleSession(infoByID, cfg, "worker", "s-c") {
			t.Error("s-c is the third session, above a floor of 2, and must NOT be exempt")
		}
	})

	t.Run("no floor: nothing is exempt", func(t *testing.T) {
		infoByID := map[string]sessionpkg.Info{"s-a": warm("s-a")}
		if isMinFloorExemptIdleSession(infoByID, floorCfg(0), "worker", "s-a") {
			t.Fatal("min=0 (no floor): no session may be exempt")
		}
	})

	t.Run("asleep low-id session does not mask a live higher-id one", func(t *testing.T) {
		// s-a is asleep (e.g. max-age-slept awaiting min-fill); the live floor
		// member is s-b. Counting s-a would push s-b above the floor and get the
		// only WARM session idle-killed — the exact regression the state filter
		// prevents. s-b must be exempt.
		asleep := sessionpkg.Info{ID: "s-a", Template: "worker", State: sessionpkg.StateAsleep}
		infoByID := map[string]sessionpkg.Info{"s-a": asleep, "s-b": warm("s-b")}
		if !isMinFloorExemptIdleSession(infoByID, floorCfg(1), "worker", "s-b") {
			t.Fatal("the live session s-b must be exempt; the asleep low-id s-a must not mask it")
		}
	})

	t.Run("a stale low-id session in a non-live state does not mask a live higher-id floor member", func(t *testing.T) {
		// Doc adversarial re-review (sc-sabwwn): the prior isWarmFloorCandidate
		// deny-list excluded only Asleep/Draining/Suspended/Closed, so a stale
		// low-id bead in any of these four non-terminal, non-live states (all
		// persist with Closed==false) inflated the floor rank and masked the live
		// floor member out of the exempt set — reproducing the kill->cold-recreate
		// oscillation the fix eliminates. The allow-list excludes each of them, so
		// the live s-b stays exempt — the same guarantee the Asleep guard proves.
		staleStates := []sessionpkg.State{
			sessionpkg.StateQuarantined,
			sessionpkg.StateFailedCreate,
			sessionpkg.StateDrained,
			sessionpkg.StateArchived,
		}
		for _, st := range staleStates {
			stale := sessionpkg.Info{ID: "s-a", Template: "worker", State: st}
			infoByID := map[string]sessionpkg.Info{"s-a": stale, "s-b": warm("s-b")}
			if !isMinFloorExemptIdleSession(infoByID, floorCfg(1), "worker", "s-b") {
				t.Errorf("state %q: the live s-b must be exempt; a stale low-id s-a in %q must not mask it", st, st)
			}
		}
	})

	t.Run("an awake low-id session IS a live floor member and stays exempt", func(t *testing.T) {
		// Guards the allow-list's inclusion of StateAwake (the healState alive
		// alias): a healed-to-awake floor session is genuinely warm, so at min=1
		// the lowest-id awake session must be the exempt member — omitting Awake
		// would idle-kill the most common warm state and reproduce the oscillation
		// in the opposite (under-exemption) direction.
		awake := sessionpkg.Info{ID: "s-a", Template: "worker", State: sessionpkg.StateAwake}
		infoByID := map[string]sessionpkg.Info{"s-a": awake, "s-b": warm("s-b")}
		if !isMinFloorExemptIdleSession(infoByID, floorCfg(1), "worker", "s-a") {
			t.Fatal("the awake low-id s-a is a live floor member and must be exempt")
		}
		if isMinFloorExemptIdleSession(infoByID, floorCfg(1), "worker", "s-b") {
			t.Fatal("s-b is above the floor (s-a is a live awake occupant) and must NOT be exempt")
		}
	})

	t.Run("other-template and closed sessions are excluded from the count", func(t *testing.T) {
		infoByID := map[string]sessionpkg.Info{
			"s-a": {ID: "s-a", Template: "scout", State: sessionpkg.StateActive},                // other template
			"s-b": {ID: "s-b", Template: "worker", State: sessionpkg.StateActive, Closed: true}, // closed
			"s-c": warm("s-c"),                                                                  // the sole open worker
		}
		if !isMinFloorExemptIdleSession(infoByID, floorCfg(1), "worker", "s-c") {
			t.Fatal("s-c is the only open worker; the scout and the closed worker must not count against it")
		}
	})

	t.Run("deterministic and stable across ticks: elastic reclaim leaves the floor set unchanged", func(t *testing.T) {
		cfg := floorCfg(2)
		// Tick 1: demand spike — 4 warm sessions. Bottom 2 (s-a, s-b) exempt.
		tick1 := map[string]sessionpkg.Info{
			"s-a": warm("s-a"), "s-b": warm("s-b"), "s-c": warm("s-c"), "s-d": warm("s-d"),
		}
		// Tick 2: demand gone — the two above-floor elastic sessions reclaimed.
		tick2 := map[string]sessionpkg.Info{"s-a": warm("s-a"), "s-b": warm("s-b")}
		for _, id := range []string{"s-a", "s-b"} {
			if !isMinFloorExemptIdleSession(tick1, cfg, "worker", id) {
				t.Errorf("tick1: %s must be exempt", id)
			}
			if !isMinFloorExemptIdleSession(tick2, cfg, "worker", id) {
				t.Errorf("tick2: %s must STILL be exempt — the warm floor identity is stable, no oscillation", id)
			}
		}
	})

	t.Run("non-pool identities neither consume a floor rank nor are exempt themselves", func(t *testing.T) {
		// The min_active_sessions guarantee covers pool-managed beads only
		// (isMinActivePoolBead, compute_awake_set.go): configured-named,
		// manual and dependency-only sessions are excluded. A lower-id
		// non-pool session must not push the real pool member (s-b) out of
		// the min=1 exempt set — that would silently no-op the keep-warm fix
		// in a mixed-identity template — and must not become idle-exempt
		// itself, which would extend the exemption beyond the pool floor.
		cases := []struct {
			name string
			info sessionpkg.Info
		}{
			{"configured-named session", sessionpkg.Info{
				ID: "s-a", Template: "worker", State: sessionpkg.StateActive,
				ConfiguredNamedSession: true,
			}},
			{"configured-named identity", sessionpkg.Info{
				ID: "s-a", Template: "worker", State: sessionpkg.StateActive,
				ConfiguredNamedIdentity: "worker#1",
			}},
			{"manual session", sessionpkg.Info{
				ID: "s-a", Template: "worker", State: sessionpkg.StateActive,
				SessionOrigin: "manual",
			}},
			{"dependency-only session", sessionpkg.Info{
				ID: "s-a", Template: "worker", State: sessionpkg.StateActive,
				DependencyOnly: true,
			}},
		}
		for _, tc := range cases {
			infoByID := map[string]sessionpkg.Info{"s-a": tc.info, "s-b": warm("s-b")}
			if !isMinFloorExemptIdleSession(infoByID, floorCfg(1), "worker", "s-b") {
				t.Errorf("%s: the pool member s-b must stay exempt; a lower-id %s must not consume the floor rank", tc.name, tc.name)
			}
			if isMinFloorExemptIdleSession(infoByID, floorCfg(1), "worker", "s-a") {
				t.Errorf("%s: a non-pool session must never be min-floor exempt itself", tc.name)
			}
		}
	})
}

// TestIsWarmFloorCandidate pins the explicit ALLOW-LIST of states that occupy a
// warm floor slot: only genuinely-live sessions count — StateNone (fresh),
// Active, Awake (the healState alive alias), Creating, StartPending. Every other
// state, including the four Doc's adversarial re-review flagged as leaking under
// the prior deny-list (Quarantined, FailedCreate, Drained, Archived) plus
// Asleep/Draining/Suspended/Closed, is NOT a warm occupant (sc-sabwwn). An
// allow-list fails closed: a state absent from the table is excluded by default.
func TestIsWarmFloorCandidate(t *testing.T) {
	cases := []struct {
		state sessionpkg.State
		want  bool
	}{
		// Live — count toward the warm floor.
		{sessionpkg.StateActive, true},
		{sessionpkg.StateAwake, true}, // healState alias for Active — must count, else healed-alive floor sessions get idle-killed
		{sessionpkg.StateCreating, true},
		{sessionpkg.StateStartPending, true},
		{sessionpkg.StateNone, true}, // freshly stamped, not yet active
		{"", true},                   // StateNone spelled literally — same admit
		// Dormant / non-runnable — must NOT count.
		{sessionpkg.StateAsleep, false},
		{sessionpkg.StateDraining, false},
		{sessionpkg.StateSuspended, false},
		{sessionpkg.StateClosed, false},
		// The four states the prior deny-list forgot (Doc HOLD, sc-sabwwn): each
		// persists with Closed==false but is stale/non-live, so counting it would
		// mask the live floor member and reproduce the kill->recreate oscillation.
		{sessionpkg.StateQuarantined, false},
		{sessionpkg.StateFailedCreate, false},
		{sessionpkg.StateDrained, false},
		{sessionpkg.StateArchived, false},
	}
	for _, tc := range cases {
		if got := isWarmFloorCandidate(sessionpkg.Info{State: tc.state}); got != tc.want {
			t.Errorf("isWarmFloorCandidate(state=%q) = %v, want %v", tc.state, got, tc.want)
		}
	}
	if isWarmFloorCandidate(sessionpkg.Info{State: sessionpkg.StateActive, Closed: true}) {
		t.Error("a Closed session is never a warm floor candidate")
	}
	// The Closed guard must reject even when State=="" (StateNone) — a closed
	// bead's state is blanked to "", which the allow-list would otherwise admit.
	if isWarmFloorCandidate(sessionpkg.Info{State: sessionpkg.StateNone, Closed: true}) {
		t.Error("a Closed session with a blanked (StateNone) state must still be rejected by the Closed guard")
	}
}

func TestSessionProgressStalled(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-time.Hour)    // well past any sane threshold
	recent := now.Add(-time.Second) // within threshold
	const threshold = 30 * time.Minute

	tests := []struct {
		name            string
		threshold       time.Duration
		holdsClaim      bool
		providerHealthy bool
		exempt          bool
		lastProgress    time.Time
		want            bool
	}{
		{"stalled: alive, no claim, healthy, not exempt, old progress", threshold, false, true, false, stale, true},
		{"disabled when threshold is zero", 0, false, true, false, stale, false},
		{"not stalled when progress is recent", threshold, false, true, false, recent, false},
		{"holds a claim -> reaper's job, not recycled", threshold, true, true, false, stale, false},
		{"provider unhealthy -> never recycle into a dead provider", threshold, false, false, false, stale, false},
		{"exempt (attached/interactive/startup) -> left alone", threshold, false, true, true, stale, false},
		{"unknown progress (zero) -> conservative, not recycled", threshold, false, true, false, time.Time{}, false},
		{"exactly at threshold is not yet stalled", threshold, false, true, false, now.Add(-threshold), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionProgressStalled(tc.threshold, tc.holdsClaim, tc.providerHealthy, tc.exempt, tc.lastProgress, now)
			if got != tc.want {
				t.Errorf("sessionProgressStalled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSessionClaimHolderStalled(t *testing.T) {
	now := time.Date(2026, 7, 25, 20, 0, 0, 0, time.UTC)
	const threshold = 20 * time.Minute

	tests := []struct {
		name            string
		threshold       time.Duration
		holdsClaim      bool
		providerHealthy bool
		exempt          bool
		lastProgress    time.Time
		want            bool
	}{
		{"stale confirmed holder is recyclable", threshold, true, true, false, now.Add(-time.Hour), true},
		{"disabled", 0, true, true, false, now.Add(-time.Hour), false},
		{"recent activity", threshold, true, true, false, now.Add(-time.Second), false},
		{"claimless session belongs to other recycler", threshold, false, true, false, now.Add(-time.Hour), false},
		{"unhealthy provider", threshold, true, false, false, now.Add(-time.Hour), false},
		{"protected session", threshold, true, true, true, now.Add(-time.Hour), false},
		{"unknown activity", threshold, true, true, false, time.Time{}, false},
		{"at threshold", threshold, true, true, false, now.Add(-threshold), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionClaimHolderStalled(tc.threshold, tc.holdsClaim, tc.providerHealthy, tc.exempt, tc.lastProgress, now); got != tc.want {
				t.Fatalf("sessionClaimHolderStalled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMinPositiveDuration(t *testing.T) {
	tests := []struct {
		first, second time.Duration
		want          time.Duration
	}{
		{0, 0, 0},
		{0, time.Minute, time.Minute},
		{time.Minute, 0, time.Minute},
		{time.Minute, 2 * time.Minute, time.Minute},
		{2 * time.Minute, time.Minute, time.Minute},
	}
	for _, tc := range tests {
		if got := minPositiveDuration(tc.first, tc.second); got != tc.want {
			t.Errorf("minPositiveDuration(%s, %s) = %s, want %s", tc.first, tc.second, got, tc.want)
		}
	}
}

// TestProgressStall_MinFloorIdleWorker_NotRecycled verifies that a pool worker
// sitting below the min_active_sessions floor is exempt from the stall recycler.
func TestProgressStall_MinFloorIdleWorker_NotRecycled(t *testing.T) {
	tests := []struct {
		name       string
		min        int
		open       int
		wantExempt bool
	}{
		// pool with min=1, exactly 1 open session → at floor, exempt
		{"at floor: open == min", 1, 1, true},
		// pool with min=2, 1 open session → below floor, exempt
		{"below floor: open < min", 2, 1, true},
		// pool with min=1, 2 open sessions → above floor, not exempt
		{"above floor: open > min", 1, 2, false},
		// pool with min=0 (no floor) → not exempt regardless of open count
		{"no floor: min == 0", 0, 1, false},
		// pool with min=0, open=0 → also not exempt
		{"no floor, empty pool", 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isMinFloorIdleWorker(tc.min, tc.open)
			if got != tc.wantExempt {
				t.Errorf("isMinFloorIdleWorker(%d, %d) = %v, want %v", tc.min, tc.open, got, tc.wantExempt)
			}
		})
	}
}

// TestProgressStall_DemandWorkerLostClaim_IsRecycled verifies that a demand
// worker (pool with no floor, or pool above its floor) that holds no claim
// and has stale progress IS recycled by sessionProgressStalled.
func TestProgressStall_DemandWorkerLostClaim_IsRecycled(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-time.Hour)
	const threshold = 30 * time.Minute

	tests := []struct {
		name        string
		min         int
		open        int
		wantRecycle bool
	}{
		// min=0: no floor at all, demand worker is recycled
		{"demand pool: min=0, open=1", 0, 1, true},
		// min=1 but 2 open sessions: above floor, demand worker is recycled
		{"above floor: min=1, open=2", 1, 2, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			floorExempt := isMinFloorIdleWorker(tc.min, tc.open)
			recycled := sessionProgressStalled(threshold, false, true, floorExempt, stale, now)
			if recycled != tc.wantRecycle {
				t.Errorf("demand worker: isMinFloorIdleWorker(%d,%d)=%v; sessionProgressStalled=%v, want %v",
					tc.min, tc.open, floorExempt, recycled, tc.wantRecycle)
			}
		})
	}
}
