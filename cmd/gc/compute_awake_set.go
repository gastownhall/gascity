package main

import (
	"slices"
	"sort"
	"strings"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// defaultOnDemandIdleTimeout is the fallback idle timeout for on-demand
// named sessions that don't configure an explicit idle_timeout. Without
// this, on-demand sessions kept alive by the "on-demand:running" override
// would stay awake indefinitely. 5 minutes is long enough to handle a
// conversation turn, short enough to not waste resources.
const defaultOnDemandIdleTimeout = 5 * time.Minute

// AwakeInput contains all pre-computed state needed to decide which sessions
// should be awake. All external I/O (shell commands, tmux checks, store
// queries) happens before this function is called.
type AwakeInput struct {
	Agents                   []AwakeAgent
	NamedSessions            []AwakeNamedSession
	SessionBeads             []AwakeSessionBead
	WorkBeads                []AwakeWorkBead // in_progress assigned work plus ready open assigned work
	ScaleCheckCounts         map[string]int  // agent template → scale_check count
	NamedSessionDemand       map[string]bool // named-session identity → routed/assigned work demand
	NamedSessionRoutedDemand map[string]bool // named-session identity → pre-suppression routed demand on backing template (wake-only, see DesiredStateResult.NamedSessionRoutedDemand)
	NamedSessionWorkQ        map[string]bool // named-session identity → bridge-carried work_query demand
	WorkSet                  map[string]bool // agent template → work_query found pending work
	RunningSessions          map[string]bool // session name (bead ID under awakeKeyBeadID) → tmux exists
	AttachedSessions         map[string]bool // session name (bead ID under awakeKeyBeadID) → user attached
	PendingSessions          map[string]bool // session name (bead ID under awakeKeyBeadID) → pending interaction
	ReadyWaitSet             map[string]bool // session bead ID → durable wait is ready
	ChatIdleTimeout          time.Duration   // global idle timeout for manual/chat sessions (0 = disabled)
	ManualGracePeriod        time.Duration   // grace period before manual sessions can be idle-slept (0 = disabled)
	Now                      time.Time
	// workIndex indexes WorkBeads by assignee (newAwakeWorkIndex). Nil scans
	// every work bead for every session, as legacy does: only the v2
	// allocator builds one (A3, TestAwakeSetIndexOracle).
	workIndex awakeWorkIndex
}

// awakeWorkIndex is a work bead list's indexes by trimmed assignee.
type awakeWorkIndex map[string][]int

func newAwakeWorkIndex(work []AwakeWorkBead) awakeWorkIndex {
	x := make(awakeWorkIndex)
	for i, wb := range work {
		if assignee := strings.TrimSpace(wb.Assignee); assignee != "" {
			x[assignee] = append(x[assignee], i)
		}
	}
	return x
}

// candidates is the work beads, in order, whose trimmed assignee is one
// sessionAssigneeMatches can accept for bead: every bead without an index.
// Callers still apply the match.
func (x awakeWorkIndex) candidates(work []AwakeWorkBead, named []AwakeNamedSession, bead AwakeSessionBead) []AwakeWorkBead {
	if x == nil {
		return work
	}
	var hits []int
	add := func(identity string) {
		if identity != "" {
			hits = append(hits, x[identity]...)
		}
	}
	add(bead.ID)
	add(bead.SessionName)
	add(bead.Alias)
	add(bead.NamedIdentity)
	if bead.ConfiguredNamedSession {
		for _, ns := range named {
			if ns.RuntimeName != "" && ns.RuntimeName == bead.SessionName {
				add(ns.Identity)
			}
		}
	}
	slices.Sort(hits)
	hits = slices.Compact(hits)
	out := make([]AwakeWorkBead, len(hits))
	for i, h := range hits {
		out[i] = work[h]
	}
	return out
}

// AwakeAgent represents an [[agent]] config entry.
type AwakeAgent struct {
	QualifiedName     string   // e.g. "hello-world/polecat"
	DependsOn         []string // template names this agent depends on
	Suspended         bool
	SleepAfterIdle    time.Duration // 0 = disabled
	MinActiveSessions int           // effective min_active_sessions; 0 = no always-warm guarantee
}

// AwakeNamedSession represents a [[named_session]] config entry.
type AwakeNamedSession struct {
	Identity    string // qualified name, e.g. "hello-world/refinery"
	Template    string // agent template name
	Mode        string // "always" or "on_demand"
	RuntimeName string // computed runtime session_name (e.g. "hello-world--refinery")
}

// AwakeSessionBead represents an open session bead from the store.
type AwakeSessionBead struct {
	ID                        string
	SessionName               string
	Template                  string
	State                     string // "creating", "active", "asleep", "drained", "closed"
	SleepReason               string
	ManualSession             bool
	PendingCreate             bool      // controller claimed this bead for initial start
	ExplicitWake              bool      // explicit durable wake request is pending
	DependencyOnly            bool      // only wakeable via dependency gate
	NamedIdentity             string    // non-empty for named session beads
	Alias                     string    // stable alias the session claims work under; "" for a rebinding pool slot
	ConfiguredNamedSession    bool      // configured_named_session metadata is true
	Pinned                    bool      // pin_awake durable wake reason
	Drained                   bool      // state=="drained" or sleep_reason=="drained"
	WaitHold                  bool      // user-issued gc wait in progress
	HeldUntil                 time.Time // zero = not held
	QuarantinedUntil          time.Time // zero = not quarantined
	IdleSince                 time.Time // zero = unknown/not idle
	CreatedAt                 time.Time // bead creation time (for grace period checks)
	RestartRequested          bool      // restart_requested metadata is still active
	ContinuationResetPending  bool      // continuation_reset_pending metadata is set
	CurrentlyProcessingBeadID string    // work bead the session is currently processing
	PostCreateProtected       bool      // fresh successful pool create; preferred for scaled slots during grace
}

// AwakeWorkBead represents a work bead with an assignee.
type AwakeWorkBead struct {
	ID       string
	Assignee string
	Status   string // "open", "in_progress"
	Ready    bool   // true for open work only after readiness/blocker filtering
	// Blocked is true when an in_progress bead carries an open
	// ready-blocking dependency or gate (bd's IsBlocked projection). It is
	// meaningless for open work, whose blocker state is already folded into
	// Ready. Zero value is false, so every existing in_progress caller that
	// does not populate it keeps today's unconditional-wake behavior.
	//
	// Setting it is not purely suppressive: workBeadHasAwakeDemand also feeds
	// countAssignedScaleSlots, so blocked in_progress work additionally
	// releases the session's scale slot, which can wake a different session
	// as scaled:demand.
	Blocked bool
}

// AwakeDecision is the output for a single session.
type AwakeDecision struct {
	ShouldWake      bool
	Reason          string // human-readable reason for debugging
	HasAssignedWork bool   // underlying assigned-work demand before wake reason overrides
	// AssignedWorkBeadID identifies the work bead that anchored the
	// assigned-work decision for this session, when one applies. Callers
	// use it to persist currently_processing_bead_id and to detect when an
	// alive session has been reassigned to a different bead.
	AssignedWorkBeadID string
	// AssignedWorkClaimed distinguishes an in-progress claim from ready open
	// work. Destructive idle recovery must never recycle a live claim holder.
	AssignedWorkClaimed bool
	// RequiresFreshCycle is true when an alive session's recorded
	// currently_processing_bead_id differs from AssignedWorkBeadID. The
	// reconciler combines this with wake_mode=fresh to trigger a
	// restart-style cycle so the next wake starts a fresh conversation on
	// the newly assigned bead.
	RequiresFreshCycle bool
}

// awakeKey selects how computeAwakeSetKeyed keys its decisions and reads the
// runtime maps of AwakeInput.
type awakeKey uint8

const (
	// awakeKeySessionName keys by session_name. Rows that share a name share
	// one decision, and the last row in slice order wins.
	awakeKeySessionName awakeKey = iota
	// awakeKeyBeadID keys by bead ID, so every row gets its own decision.
	// RunningSessions, AttachedSessions and PendingSessions must then be keyed
	// by bead ID too.
	//
	// This fixes POOL-082 (#8) only partly. Work assigned by session name
	// still matches every row carrying that name, so stale siblings of the
	// live owner get assigned-work too. A configured named session resolves
	// to the first row in slice order that claims its identity, so with two
	// claimants the winner depends on census order. The caller must exclude
	// stale siblings and identity losers, or sort the rows, before it relies
	// on per-row decisions.
	awakeKeyBeadID
)

// ComputeAwakeSet determines which sessions should be awake, keyed by
// session name. It is computeAwakeSetKeyed with awakeKeySessionName.
func ComputeAwakeSet(input AwakeInput) map[string]AwakeDecision {
	return computeAwakeSetKeyed(input, awakeKeySessionName)
}

// computeAwakeSetKeyed determines which sessions should be awake.
//
// Pure function. Algorithm:
//  1. Build desired set from config + demand signals
//  2. Any session in desired set should wake
//  3. Attached/pending/ready-wait override (wake even if not desired)
//  4. Idle sleep suppression
//  5. Hold + quarantine suppression (overrides everything)
//
// Dependency ordering is NOT enforced here — the reconciler's
// executePlannedStarts handles it via wave-based starts.
//
// keyBy changes the keys, and with them where a decision made for a name
// lands: keyed by name it is shared by every row carrying the name; keyed by
// bead ID it lands on the resolved row, or on each row carrying the name when
// no row resolves (desireName, namedBead). With unique session names both
// keyings decide every row the same way.
func computeAwakeSetKeyed(input AwakeInput, keyBy awakeKey) map[string]AwakeDecision {
	key := func(b AwakeSessionBead) string {
		if keyBy == awakeKeyBeadID {
			return b.ID
		}
		return b.SessionName
	}
	// desireName records a desire made under a session name rather than for a
	// row (a configured named session with no row): every row carrying that
	// name.
	desireName := func(desired map[string]string, name, reason string) {
		if keyBy != awakeKeyBeadID {
			desired[name] = reason
			return
		}
		for _, b := range input.SessionBeads {
			if b.SessionName == name {
				desired[b.ID] = reason
			}
		}
	}
	// namedBead is the row a configured named session resolves to. Keyed by
	// name, that is the first row carrying the resolved session name.
	namedBead := func(ns AwakeNamedSession) *AwakeSessionBead {
		bead := resolveNamedSessionBead(input.SessionBeads, ns)
		if bead == nil || keyBy == awakeKeyBeadID {
			return bead
		}
		return findBeadBySessionName(input.SessionBeads, bead.SessionName)
	}

	agentsByName := make(map[string]AwakeAgent, len(input.Agents))
	agentsByBaseName := make(map[string]AwakeAgent, len(input.Agents))
	duplicateBaseNames := make(map[string]bool)
	for _, a := range input.Agents {
		agentsByName[a.QualifiedName] = a
		base := awakeAgentBaseName(a.QualifiedName)
		if existing, ok := agentsByBaseName[base]; ok && existing.QualifiedName != a.QualifiedName {
			duplicateBaseNames[base] = true
			continue
		}
		if !duplicateBaseNames[base] {
			agentsByBaseName[base] = a
		}
	}
	lookupAgent := func(name string) (AwakeAgent, bool) {
		if agent, ok := agentsByName[name]; ok {
			return agent, true
		}
		base := awakeAgentBaseName(name)
		if duplicateBaseNames[base] {
			return AwakeAgent{}, false
		}
		agent, ok := agentsByBaseName[base]
		return agent, ok
	}

	// Step 1: Build desired set.
	// Drained beads are excluded from generic template demand, but explicit
	// compatible wake causes (pending create, named-always, assigned work) may
	// still reuse the same bead.
	desired := make(map[string]string) // key → reason

	// Newly created beads that still carry a controller create claim must be
	// launched at least once, even if the work signal that materialized them
	// is no longer visible on the very next tick.
	for _, bead := range input.SessionBeads {
		if !bead.PendingCreate {
			continue
		}
		desired[key(bead)] = "pending-create"
	}
	for _, bead := range input.SessionBeads {
		if !bead.ExplicitWake || bead.State == "closed" || bead.DependencyOnly {
			continue
		}
		if agent, ok := agentsByName[bead.Template]; ok && !agent.Suspended {
			desired[key(bead)] = "explicit-wake"
		}
	}
	// Named sessions
	for _, ns := range input.NamedSessions {
		if agent, ok := lookupAgent(ns.Identity); ok && agent.Suspended {
			continue
		}
		switch ns.Mode {
		case "always":
			if bead := namedBead(ns); bead != nil {
				if !bead.DependencyOnly {
					desired[key(*bead)] = "named-always"
				}
			} else {
				desireName(desired, ns.Identity, "named-always")
			}
		case "on_demand":
			reason := ""
			switch {
			case input.NamedSessionDemand[ns.Identity]:
				reason = "named-demand"
			case input.NamedSessionRoutedDemand[ns.Identity]:
				reason = "routed-demand"
			case input.NamedSessionWorkQ[ns.Identity]:
				reason = "work-query"
			default:
				continue
			}
			if agent, ok := agentsByName[ns.Template]; ok && agent.Suspended {
				continue
			}
			if bead := namedBead(ns); bead != nil {
				// Drained override. routed-demand wakes even a drained bead
				// (ga-j4lqwa.1): it is real demand for a canonical-singleton
				// holder, the same override strength already given to
				// attached/pending above. named-demand stays gated because
				// NamedSessionDemand (namedWorkReady) does not filter blocked
				// work; exempting it would re-wake sessions that drain-acked on
				// blocked work, the loop the reset-pending guard exists to
				// prevent. Ready assignee-direct work needs no exemption: the
				// assigned-work pass already wakes a drained bead, filtering
				// blocked in_progress work through workBeadHasAwakeDemand.
				// work-query stays gated: it lacks NamedSessionRoutedDemand's
				// deliberate UsesCanonicalSingletonPoolIdentity() scoping, so
				// exempting it would risk a herd-wake on multi-instance pools.
				// Finally, when a drained holder has both signals, named-demand
				// from blocked work wins the reason switch above; promote it to
				// routed-demand so the live routed signal is not masked and the
				// ga-j4lqwa.1 strand cannot survive in the combined case.
				if bead.Drained && reason == "named-demand" && input.NamedSessionRoutedDemand[ns.Identity] {
					reason = "routed-demand"
				}
				drainedExempt := reason == "routed-demand"
				if !bead.DependencyOnly && (!bead.Drained || drainedExempt) && bead.State != "closed" {
					desired[key(*bead)] = reason
				}
			} else {
				desireName(desired, ns.Identity, reason)
			}
		}
	}

	// Agent templates (scaled)
	for template, count := range input.ScaleCheckCounts {
		if count <= 0 {
			continue
		}
		agent, ok := lookupAgent(template)
		if !ok || agent.Suspended {
			continue
		}
		active := collectActiveBeads(input.SessionBeads, template, input.Now)
		filled := countAssignedScaleSlotsIndexed(input.SessionBeads, input.WorkBeads, input.workIndex, input.NamedSessions, template)
		for _, bead := range active {
			if filled >= count {
				break
			}
			if sessionHasAssignedWork(input.workIndex.candidates(input.WorkBeads, input.NamedSessions, bead), input.NamedSessions, bead) {
				continue
			}
			desired[key(bead)] = "scaled:demand"
			filled++
		}
		creating := collectCreatingBeads(input.SessionBeads, template)
		for _, bead := range creating {
			if filled >= count {
				break
			}
			if sessionHasAssignedWork(input.workIndex.candidates(input.WorkBeads, input.NamedSessions, bead), input.NamedSessions, bead) {
				continue
			}
			desired[key(bead)] = "scaled:creating"
			filled++
		}
	}

	// WorkSet: defense-in-depth wake signal from work_query.
	// When work_query sees pending work but ScaleCheckCounts hasn't caught up
	// (count is 0 or absent), wake exactly one session to handle it. This
	// avoids thundering herd — scale_check will catch up on the next tick.
	for template, hasWork := range input.WorkSet {
		if !hasWork {
			continue
		}
		if input.ScaleCheckCounts[template] > 0 {
			continue // ScaleCheck already covers this template
		}
		agent, ok := lookupAgent(template)
		if !ok || agent.Suspended {
			continue
		}
		if isNamedSessionTemplate(input.NamedSessions, template) {
			continue // named sessions are handled in the named-session pass
		}
		// collectActiveBeads already excludes DependencyOnly and Drained
		if active := collectActiveBeads(input.SessionBeads, template, input.Now); len(active) > 0 {
			desired[key(active[0])] = "work-query"
			continue
		}
		if creating := collectCreatingBeads(input.SessionBeads, template); len(creating) > 0 {
			desired[key(creating[0])] = "work-query"
		}
	}

	// Manual sessions
	for _, bead := range input.SessionBeads {
		if !bead.ManualSession || bead.State == "closed" || bead.Drained {
			continue
		}
		if _, ok := agentsByName[bead.Template]; ok {
			desired[key(bead)] = "manual"
		}
	}

	// Sessions with assigned work — a session that has in_progress work or
	// ready open work assigned to it must stay awake. Open work must carry
	// Ready=true so a blocked routed assignment cannot become wake demand if
	// a future caller accidentally broadens the collection query.
	//
	// When the session bead records currently_processing_bead_id, prefer the
	// matching work bead as the anchor so crash recovery brings a session
	// back to the bead it last owned even when other beads share the
	// assignee. If no candidate matches the recorded current bead, fall back
	// to the first matching work bead and flag the divergence — the
	// reconciler reads this to decide whether to cycle the conversation for
	// wake_mode=fresh.
	assignedAnchor := make(map[string]string) // key → matched work bead ID
	for _, bead := range input.SessionBeads {
		if bead.State == "closed" {
			continue
		}
		if agent, ok := lookupAgent(bead.Template); ok && agent.Suspended {
			continue
		}
		var (
			fallback   string
			haveExact  bool
			anchorBead string
			recorded   = bead.CurrentlyProcessingBeadID
			matchedAny = false
		)
		for _, wb := range input.workIndex.candidates(input.WorkBeads, input.NamedSessions, bead) {
			assignee := strings.TrimSpace(wb.Assignee)
			if assignee == "" || !workBeadHasAwakeDemand(wb) {
				continue
			}
			if !sessionAssigneeMatches(input.NamedSessions, bead, assignee) {
				continue
			}
			matchedAny = true
			if recorded != "" && wb.ID == recorded {
				anchorBead = wb.ID
				haveExact = true
				break
			}
			if fallback == "" {
				fallback = wb.ID
			}
		}
		if !matchedAny {
			continue
		}
		if !haveExact {
			anchorBead = fallback
		}
		desired[key(bead)] = "assigned-work"
		assignedAnchor[key(bead)] = anchorBead
	}

	// Min-active-sessions wake: keep min_active_sessions pool sessions warm
	// across a city-stop. A pool agent whose only instance is asleep with
	// sleep_reason=city-stop is neither counted toward the min nor woken by
	// the demand-driven passes above, so without this pass a
	// min_active_sessions=1 agent stays cold indefinitely after gc stop &&
	// gc start until work is explicitly slung to it. We revive the existing
	// asleep city-stop bead rather than relying on a fresh spawn (no
	// orphaned-bead churn), mirroring the named-always same-tick wake (#2367)
	// on the pool min path. Scoped to sleep_reason=city-stop so idle_timeout
	// and wake_mode semantics are unchanged. See #2739.
	for _, agent := range input.Agents {
		if agent.Suspended || agent.MinActiveSessions <= 0 {
			continue
		}
		template := agent.QualifiedName
		covered := countMinActiveCovered(input.SessionBeads, desired, key, template, input.Now)
		if covered >= agent.MinActiveSessions {
			continue
		}
		for _, bead := range cityStopPoolBeads(input.SessionBeads, template) {
			if covered >= agent.MinActiveSessions {
				break
			}
			if _, already := desired[key(bead)]; already {
				continue
			}
			if minActiveHardBlocked(bead, input.Now) {
				continue
			}
			desired[key(bead)] = "min-active"
			covered++
		}
	}

	// continuation_reset_pending means "the next wake must start a fresh
	// conversation" — it is not itself a reason to wake a Drained session.
	// AcknowledgeDrainPatch(freshWake=true) stamps state=drained +
	// continuation_reset_pending=true together when a wake_mode=fresh session
	// drain-acks (e.g. it only has blocked assigned work). Without this guard
	// that pending flag alone re-desires the session every tick, undoing the
	// drain-ack and driving a perpetual wake/drain oscillation (each cycle a
	// full fresh model boot). Mirrors the Drained guard on the pin arm below.
	// A legitimate reset-pending session is asleep-but-not-drained (restart
	// request, config-drift reset) or already carries pending-create/
	// explicit-wake — both remain unaffected by this guard.
	for _, bead := range input.SessionBeads {
		if !bead.ContinuationResetPending || bead.RestartRequested || bead.WaitHold || bead.Drained {
			continue
		}
		switch desired[key(bead)] {
		case "pending-create", "explicit-wake":
			continue
		default:
			desired[key(bead)] = "reset-pending"
		}
	}

	// Step 2-3: Decide awake
	result := make(map[string]AwakeDecision)

	for _, bead := range input.SessionBeads {
		name := key(bead)
		anchor, hasAssignedWork := assignedAnchor[name]
		decision := AwakeDecision{
			HasAssignedWork: hasAssignedWork,
		}
		if hasAssignedWork {
			decision.AssignedWorkBeadID = anchor
			for _, work := range input.workIndex.candidates(input.WorkBeads, input.NamedSessions, bead) {
				if work.Status != "in_progress" {
					continue
				}
				if sessionAssigneeMatches(input.NamedSessions, bead, strings.TrimSpace(work.Assignee)) {
					decision.AssignedWorkClaimed = true
					break
				}
			}
			if bead.CurrentlyProcessingBeadID != "" && anchor != bead.CurrentlyProcessingBeadID {
				decision.RequiresFreshCycle = true
			}
		}

		// Desired set (demand-driven wake). wait_hold suppresses normal
		// demand-driven wake so a session intentionally parked on human
		// input stays asleep until either its durable wait becomes ready
		// or it still needs its initial launch.
		if reason, inDesired := desired[name]; inDesired {
			if !bead.WaitHold || bead.PendingCreate || bead.ExplicitWake {
				decision.ShouldWake = true
				decision.Reason = reason
			}
		}

		// Attached override — even drained beads wake if user is attached
		if input.AttachedSessions[name] && !bead.WaitHold {
			decision.ShouldWake = true
			decision.Reason = "attached"
		}

		// Pending interaction override — even drained beads wake
		if input.PendingSessions[name] && !bead.WaitHold {
			decision.ShouldWake = true
			decision.Reason = "pending"
		}

		// Ready wait — durable wait deadline passed, resume session
		if input.ReadyWaitSet[bead.ID] {
			decision.ShouldWake = true
			decision.Reason = "wait-ready"
		}

		// On-demand running override — on-demand sessions that are
		// currently running stay awake even when demand drops to zero.
		// They drain via idle_timeout, not demand absence. This
		// supports message-driven wake: a message starts the session,
		// it stays alive handling it, then idles until timeout.
		// Drain-ack agents are unaffected — they manage their own
		// lifecycle by calling drain-ack before this check matters.
		if !decision.ShouldWake && !bead.Drained && !bead.WaitHold &&
			bead.SleepReason != string(sessionpkg.SleepReasonIdleTimeout) {
			if input.RunningSessions[name] && isOnDemandSession(input.NamedSessions, bead) {
				decision.ShouldWake = true
				decision.Reason = "on-demand:running"
			}
		}

		// Durable pin override — wakes and keeps the session awake while
		// still respecting hard blockers applied below.
		pinBlockedByState := bead.State == "suspended" || bead.State == "closed" || bead.Drained
		if !decision.ShouldWake && bead.Pinned && !pinBlockedByState && !bead.DependencyOnly && !bead.WaitHold {
			if agent, ok := lookupAgent(bead.Template); ok && !agent.Suspended {
				decision.ShouldWake = true
				decision.Reason = "pin"
			}
		}

		// Idle sleep: desired sessions idle too long should sleep.
		// Attached, pending, pinned, mode=always named, and sessions with
		// assigned demand work are exempt. A claimed in_progress bead also
		// vetoes idle sleep even when blocked: blocked work does not wake an
		// asleep owner, but it must not park the live seat that owns the claim.
		// Blocked open assignments do not prevent idle sleep. Manual sessions
		// within their grace period are also exempt.
		//
		// On_demand named sessions woken by routed/named demand
		// ("named-demand", "routed-demand", "work-query") are also exempt:
		// that demand means there is pending work for this specific session,
		// so an idle window must not put it back to sleep. Without this, an
		// asleep on_demand named session (e.g. a refinery) with routed work
		// that already exists (open_count==desired_count==1) is re-slept every
		// tick and the work is wedged forever — the reconciler reports
		// reason_code=retained indefinitely. A fresh cold-create wakes only
		// because it has no idle reference. The "work done, no demand" drain
		// still fires via the "on-demand:running" reason, which is NOT exempt.
		// See #3413.
		//
		// A durable explicit wake request ("explicit-wake") is exempt for the
		// same reason: it is a standing operator/wake-path demand for this
		// specific session, so an idle window that predates it (e.g. from a
		// supervisor-restart re-projection) must not silently cancel it. See
		// #5739.
		agent, hasAgent := lookupAgent(bead.Template)
		holdsClaimedWork := hasAgent && !agent.Suspended && sessionHasClaimedInProgressWork(input.workIndex.candidates(input.WorkBeads, input.NamedSessions, bead), input.NamedSessions, bead)
		if decision.ShouldWake && !input.AttachedSessions[name] && !input.PendingSessions[name] && !bead.Pinned && !holdsClaimedWork && !bead.IdleSince.IsZero() &&
			!isAlwaysNamedSession(input.NamedSessions, bead) &&
			desired[name] != "assigned-work" && desired[name] != "min-active" &&
			desired[name] != "reset-pending" &&
			desired[name] != "named-demand" && desired[name] != "routed-demand" &&
			desired[name] != "work-query" && desired[name] != "explicit-wake" &&
			!inManualGracePeriod(bead, input.ManualGracePeriod, input.Now) {
			var idleTimeout time.Duration
			switch {
			case bead.ManualSession && input.ChatIdleTimeout > 0:
				idleTimeout = input.ChatIdleTimeout
			case hasAgent && agent.SleepAfterIdle > 0:
				idleTimeout = agent.SleepAfterIdle
			case isOnDemandSession(input.NamedSessions, bead):
				idleTimeout = defaultOnDemandIdleTimeout
			}
			if idleTimeout > 0 && input.Now.Sub(bead.IdleSince) >= idleTimeout {
				decision.ShouldWake = false
				decision.Reason = "idle-sleep"
			}
		}

		// Hold suppression — overrides everything
		if !bead.HeldUntil.IsZero() && input.Now.Before(bead.HeldUntil) {
			decision.ShouldWake = false
			decision.Reason = "held"
		}

		// Quarantine suppression — overrides everything
		if !bead.QuarantinedUntil.IsZero() && input.Now.Before(bead.QuarantinedUntil) {
			decision.ShouldWake = false
			decision.Reason = "quarantined"
		}

		// NOTE: Dependency ordering is NOT enforced here. The reconciler's
		// executePlannedStarts handles dependency-aware wave ordering via
		// allDependenciesAliveForTemplate at wave boundaries. Applying
		// the gate here would prevent candidates from reaching the start
		// list, breaking wave-based starts (where dep starts in wave 0
		// and dependent starts in wave 1).

		result[name] = decision
	}

	return result
}

func countAssignedScaleSlots(beads []AwakeSessionBead, workBeads []AwakeWorkBead, named []AwakeNamedSession, template string) int {
	return countAssignedScaleSlotsIndexed(beads, workBeads, nil, named, template)
}

// countAssignedScaleSlotsIndexed is countAssignedScaleSlots through index;
// nil scans every work bead.
func countAssignedScaleSlotsIndexed(beads []AwakeSessionBead, workBeads []AwakeWorkBead, index awakeWorkIndex, named []AwakeNamedSession, template string) int {
	n := 0
	for _, bead := range beads {
		if bead.Template != template || bead.State == "closed" {
			continue
		}
		if bead.NamedIdentity != "" || bead.ConfiguredNamedSession || bead.ManualSession {
			continue
		}
		if sessionHasAssignedWork(index.candidates(workBeads, named, bead), named, bead) {
			n++
		}
	}
	return n
}

func awakeAgentBaseName(name string) string {
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		return name[idx+1:]
	}
	return name
}

func findNamedSessionBead(beads []AwakeSessionBead, identity string) *AwakeSessionBead {
	for i := range beads {
		if beads[i].NamedIdentity == identity {
			return &beads[i]
		}
	}
	return nil
}

// resolveNamedSessionBeadName locates the session_name of the bead that
// represents a configured named session. The primary match is the bead's
// NamedIdentity (configured_named_identity metadata). The fallback matches
// a configured_named_session bead by its session_name when that matches the
// identity's deterministic runtime name AND its template matches.
//
// The fallback recovers a configured named session whose NamedIdentity is
// MISSING on its bead — for example, a bead minted before
// configured_named_identity was added. Beads with a non-empty NamedIdentity
// that doesn't match any [[named_session]] identity are NOT recovered by
// this fallback (those return "" at the bead.NamedIdentity != ns.Identity
// check below); a config-change migration that leaves a stale NamedIdentity
// must be handled separately. Without this fallback, ComputeAwakeSet
// silently drops the bead from `desired` and the session stays asleep
// forever even though the config says mode=always. See #1493.
func resolveNamedSessionBeadName(beads []AwakeSessionBead, ns AwakeNamedSession) string {
	if bead := resolveNamedSessionBead(beads, ns); bead != nil {
		return bead.SessionName
	}
	return ""
}

// resolveNamedSessionBead returns the bead resolveNamedSessionBeadName names,
// or nil.
func resolveNamedSessionBead(beads []AwakeSessionBead, ns AwakeNamedSession) *AwakeSessionBead {
	if bead := findNamedSessionBead(beads, ns.Identity); bead != nil && bead.SessionName != "" {
		return bead
	}
	if ns.RuntimeName == "" {
		return nil
	}
	bead := findBeadBySessionName(beads, ns.RuntimeName)
	if bead == nil || !bead.ConfiguredNamedSession {
		return nil
	}
	if ns.Template != "" && bead.Template != ns.Template {
		return nil
	}
	if bead.NamedIdentity != "" && bead.NamedIdentity != ns.Identity {
		return nil
	}
	return bead
}

func findBeadBySessionName(beads []AwakeSessionBead, name string) *AwakeSessionBead {
	for i := range beads {
		if beads[i].SessionName == name {
			return &beads[i]
		}
	}
	return nil
}

// isMinActivePoolBead reports whether a bead is a pool-managed instance of
// template that may participate in the min_active_sessions guarantee. Named
// and manual sessions are excluded (they carry their own keep-awake rules),
// as are drained and closed beads (not live, not revivable here).
// Dependency-only beads are excluded too: they wake exclusively via the
// dependency gate, so they neither count toward the min nor are eligible for
// min-active revival — matching collectActiveBeads.
func isMinActivePoolBead(b AwakeSessionBead, template string) bool {
	return b.Template == template &&
		b.NamedIdentity == "" && !b.ConfiguredNamedSession &&
		!b.ManualSession && !b.Drained && !b.DependencyOnly && b.State != "closed"
}

func minActiveHardBlocked(b AwakeSessionBead, now time.Time) bool {
	return b.WaitHold ||
		(!b.HeldUntil.IsZero() && now.Before(b.HeldUntil)) ||
		(!b.QuarantinedUntil.IsZero() && now.Before(b.QuarantinedUntil))
}

// countMinActiveCovered counts pool session beads for template that already
// satisfy the min_active_sessions guarantee: non-asleep live beads
// (active/creating) plus any bead an earlier pass already marked
// desired-awake this tick. An asleep bead with no wake reason does not count —
// that is precisely the deficit the min-active pass fills.
func countMinActiveCovered(beads []AwakeSessionBead, desired map[string]string, key func(AwakeSessionBead) string, template string, now time.Time) int {
	n := 0
	for _, b := range beads {
		if !isMinActivePoolBead(b, template) {
			continue
		}
		if minActiveHardBlocked(b, now) {
			continue
		}
		if b.State == "asleep" {
			if _, awake := desired[key(b)]; awake {
				n++
			}
			continue
		}
		// Only live beads (active/creating) count as covering the guarantee.
		// Transitional or non-runnable states (suspended, draining,
		// quarantined, failed-create, stopped, ...) do not — counting them
		// would mask a real deficit and leave the pool cold when there are
		// zero live sessions.
		if b.State == "active" || b.State == "creating" {
			n++
		}
	}
	return n
}

// cityStopPoolBeads returns the asleep, city-stop pool beads for template in
// deterministic order (by bead ID). These are the revival candidates for the
// min_active_sessions wake — restricting to sleep_reason=city-stop keeps
// idle_timeout / wake_mode semantics untouched.
func cityStopPoolBeads(beads []AwakeSessionBead, template string) []AwakeSessionBead {
	var out []AwakeSessionBead
	for _, b := range beads {
		if isMinActivePoolBead(b, template) && b.State == "asleep" && b.SleepReason == string(sessionpkg.SleepReasonCityStop) {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func isNamedSessionTemplate(named []AwakeNamedSession, template string) bool {
	for _, ns := range named {
		if ns.Template == template {
			return true
		}
	}
	return false
}

func collectActiveBeads(beads []AwakeSessionBead, template string, now time.Time) []AwakeSessionBead {
	var result []AwakeSessionBead
	for _, b := range beads {
		// Exclude both NamedIdentity-tagged beads AND ConfiguredNamedSession
		// beads whose NamedIdentity happens to be missing — the latter are
		// still configured named sessions (recovered via the runtime-name
		// fallback in namedSessionMatches / resolveNamedSessionBeadName).
		// Treating them as generic pool candidates would re-introduce the
		// #1493 failure mode in a different shape: a configured named
		// session getting woken by generic template scale_check demand.
		if b.Template == template && b.State == "active" &&
			b.NamedIdentity == "" && !b.ConfiguredNamedSession &&
			!b.ManualSession && !b.Drained && !b.DependencyOnly &&
			!minActiveHardBlocked(b, now) {
			result = append(result, b)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].PostCreateProtected != result[j].PostCreateProtected {
			return result[i].PostCreateProtected
		}
		if !result[i].PostCreateProtected {
			return false
		}
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func sessionHasAssignedWork(workBeads []AwakeWorkBead, named []AwakeNamedSession, bead AwakeSessionBead) bool {
	for _, wb := range workBeads {
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" || !workBeadHasAwakeDemand(wb) {
			continue
		}
		if sessionAssigneeMatches(named, bead, assignee) {
			return true
		}
	}
	return false
}

func sessionHasClaimedInProgressWork(workBeads []AwakeWorkBead, named []AwakeNamedSession, bead AwakeSessionBead) bool {
	for _, wb := range workBeads {
		if wb.Status == "in_progress" && sessionAssigneeMatches(named, bead, strings.TrimSpace(wb.Assignee)) {
			return true
		}
	}
	return false
}

func workBeadHasAwakeDemand(bead AwakeWorkBead) bool {
	switch bead.Status {
	case "in_progress":
		return !bead.Blocked
	case "open":
		return bead.Ready
	default:
		return false
	}
}

func sessionAssigneeMatches(named []AwakeNamedSession, bead AwakeSessionBead, assignee string) bool {
	if assignee == "" {
		return false
	}
	if assignee == bead.ID || assignee == bead.SessionName {
		return true
	}
	// A session claims work under its alias first (session.AssigneeIdentifier),
	// so a namepool member holding "rig/furiosa" work owns it. The bridge fills
	// Alias only for stable aliases (stableAssignmentAliasForConfigInfo), never
	// for a rebinding pool slot.
	if bead.Alias != "" && assignee == bead.Alias {
		return true
	}
	if bead.NamedIdentity != "" {
		return assignee == bead.NamedIdentity
	}
	if !bead.ConfiguredNamedSession {
		return false
	}
	// This configured-named fallback mirrors sessionAssignmentIdentifiersForConfig
	// so awake decisions and cleanup guards recognize the same identities.
	for _, ns := range named {
		if ns.RuntimeName == "" || ns.RuntimeName != bead.SessionName {
			continue
		}
		if ns.Template != "" && ns.Template != bead.Template {
			continue
		}
		if assignee == ns.Identity {
			return true
		}
	}
	return false
}

func isOnDemandSession(named []AwakeNamedSession, bead AwakeSessionBead) bool {
	return namedSessionMatches(named, bead, "on_demand")
}

func isAlwaysNamedSession(named []AwakeNamedSession, bead AwakeSessionBead) bool {
	return namedSessionMatches(named, bead, "always")
}

// namedSessionMatches reports whether bead represents a configured named
// session of the given mode. The fallback path (bead.ConfiguredNamedSession
// + matching runtime name + template) mirrors resolveNamedSessionBeadName
// so a bead with missing/stale NamedIdentity is still recognized as named —
// otherwise idle-sleep suppression and on-demand keep-awake silently lose
// their exemption for affected beads. See #1493.
func namedSessionMatches(named []AwakeNamedSession, bead AwakeSessionBead, mode string) bool {
	for _, ns := range named {
		if ns.Mode != mode {
			continue
		}
		if bead.NamedIdentity != "" && ns.Identity == bead.NamedIdentity {
			return true
		}
		if bead.NamedIdentity == "" && bead.ConfiguredNamedSession &&
			ns.RuntimeName != "" && ns.RuntimeName == bead.SessionName &&
			(ns.Template == "" || ns.Template == bead.Template) {
			return true
		}
	}
	return false
}

func collectCreatingBeads(beads []AwakeSessionBead, template string) []AwakeSessionBead {
	var result []AwakeSessionBead
	for _, b := range beads {
		// See collectActiveBeads above for why ConfiguredNamedSession beads
		// must be excluded even when NamedIdentity is empty.
		if b.Template == template && isCreatingCandidateState(b.State) &&
			b.NamedIdentity == "" && !b.ConfiguredNamedSession &&
			!b.ManualSession && !b.Drained && !b.DependencyOnly {
			result = append(result, b)
		}
	}
	return result
}

func isCreatingCandidateState(state string) bool {
	switch sessionpkg.State(state) {
	case sessionpkg.StateStartPending, sessionpkg.StateCreating:
		return true
	default:
		return false
	}
}

// inManualGracePeriod returns true if the session is a manual session
// created recently enough to be protected from idle sleep.
func inManualGracePeriod(bead AwakeSessionBead, gracePeriod time.Duration, now time.Time) bool {
	return bead.ManualSession && gracePeriod > 0 && !bead.CreatedAt.IsZero() &&
		now.Sub(bead.CreatedAt) < gracePeriod
}
