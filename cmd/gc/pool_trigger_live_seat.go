package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/session"
)

// errPoolSeatRunsElsewhere marks a request whose work runs in a managed
// worktree that every live seat able to take it runs outside of, while no
// fresh session can start: the work is left for a session in its own
// directory (selectOrPlanPoolSessionBeadAvoiding, poolSeatRunsElsewhereError).
// In a full pool a seat retires so that one can start
// (poolSeatsElsewhere.freeSlot).
var errPoolSeatRunsElsewhere = errors.New("work left for a session in its own directory")

// poolSeatElsewhere is a live pool seat whose directory is not the one the
// controller places a request's work in.
type poolSeatElsewhere struct {
	// SeatDir is the directory the seat's row records, where its process
	// runs and which `gc hook --claim` reads as the seat's own
	// (hookClaimPoolSeatWorkDir).
	SeatDir string
	// WorkDir is the directory the controller places the request's work in:
	// its verified managed worktree, or the pool's own directory, routed by
	// pack and workspace, for work bound to none (poolWorkDirUnder).
	WorkDir string
	// Managed reports that WorkDir is the work's managed worktree. A seat
	// anywhere else always leaves such work (hookClaimWorktreeHeldElsewhere);
	// work bound to none it leaves only from inside a managed worktree
	// (hookClaimUnboundWorkOutsideSeat).
	Managed bool
}

// poolSeatPlacedElsewhere reports whether binding request to the pool seat
// info would re-point it at work the controller places in a directory other
// than the one its row records, and where both directories are. Whether the
// seat is alive is the caller's question (poolSeatAlive): a live seat is not
// restarted when its row is re-pointed, because the work dir is not a restart
// input (runtime.ConfigFingerprint) and the warm-bind nudge asks the running
// process to claim the work where it is (deliverWarmBindClaimNudge).
//
// Only a change of trigger re-points a seat; a resume of work assigned to it
// (poolRequestResumesAssignedWorkInfo) keeps computePoolTriggerBindingPatch's
// own rule, where a fresh-mode seat is cycled into the new directory. A
// request whose worktree evidence is unusable is left to the bind, which
// skips it. A row that is not a pool seat's or records no directory, and a
// seat already in the work's directory, are not placed elsewhere.
func poolSeatPlacedElsewhere(bp *agentBuildParams, cfgAgent *config.Agent, info session.Info, request SessionRequest) (poolSeatElsewhere, bool) {
	workID := strings.TrimSpace(request.WorkBeadID)
	if bp == nil || cfgAgent == nil || workID == "" || workID == strings.TrimSpace(info.TriggerBeadID) {
		return poolSeatElsewhere{}, false
	}
	if strings.TrimSpace(request.WorktreeError) != "" || poolRequestResumesAssignedWorkInfo(request, bp.assignedWorkBeads, info) {
		return poolSeatElsewhere{}, false
	}
	seatDir := hookClaimPoolSeatWorkDir(&info, cfgAgent)
	if seatDir == "" {
		return poolSeatElsewhere{}, false
	}
	at := poolSeatElsewhere{SeatDir: seatDir}
	if request.WorktreeSpec != nil {
		at.WorkDir, at.Managed = strings.TrimSpace(request.WorktreeSpec.Path), true
	} else if poolDir := strings.TrimSpace(hookClaimPoolSeatFor(bp.cityPath, bp.cityName, bp.rigs, &info, cfgAgent).PoolDir); poolDir != "" {
		at.WorkDir = poolWorkDirUnder(poolDir, request)
	}
	if at.WorkDir == "" || pathutil.SamePath(resolveWorkDirAgainstCity(bp.cityPath, at.SeatDir), at.WorkDir) {
		return poolSeatElsewhere{}, false
	}
	return at, true
}

// poolSeatAlive reports whether the agent of the pool seat info is alive,
// observed as the reconciler observes it (observeRuntimeProviderLiveness with
// the provider's process names), so a pane whose agent died is not alive and
// the reconciler restarts it where its row points. An observation that fails
// reads as alive: the seat may still run where its row says. No provider, or
// a row with no runtime name, is not alive.
func poolSeatAlive(bp *agentBuildParams, cfgAgent *config.Agent, info session.Info) bool {
	return poolSeatAliveAs(bp, poolSeatProcessNames(bp, cfgAgent), info)
}

// poolSeatAliveAs is poolSeatAlive with the provider's process names
// resolved (poolSeatProcessNames).
func poolSeatAliveAs(bp *agentBuildParams, processNames []string, info session.Info) bool {
	if bp == nil || bp.sp == nil {
		return false
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	if name == "" {
		return false
	}
	_, alive, err := observeRuntimeProviderLiveness(bp.sp, name, processNames)
	return alive || err != nil
}

// poolSeatProcessNames returns the process names of cfgAgent's provider, by
// which poolSeatAlive tells a live agent from a pane whose agent died; none
// when the provider does not resolve.
func poolSeatProcessNames(bp *agentBuildParams, cfgAgent *config.Agent) []string {
	if bp == nil || bp.lookPath == nil {
		return nil
	}
	resolved, err := config.ResolveProvider(cfgAgent, bp.workspace, bp.providers, bp.lookPath)
	if err != nil {
		return nil
	}
	return resolved.ProcessNames
}

// poolSeatRunsElsewhereError is errPoolSeatRunsElsewhere for one request:
// the live seat it passed over, where that seat runs and where the work runs,
// and why no fresh session can start (cause).
type poolSeatRunsElsewhereError struct {
	seat   session.Info
	at     poolSeatElsewhere
	workID string
	cause  error
}

func (e *poolSeatRunsElsewhereError) Error() string {
	return fmt.Sprintf("%v: session %s runs in %s, not in %s, where trigger bead %s's work runs, and no fresh session can start: %v",
		errPoolSeatRunsElsewhere, e.seat.ID, e.at.SeatDir, e.at.WorkDir, e.workID, e.cause)
}

func (e *poolSeatRunsElsewhereError) Unwrap() []error {
	return []error{errPoolSeatRunsElsewhere, e.cause}
}

// poolSeatsElsewhere selects and plans, for one legacy realization of a pool,
// around the live seats a request would re-point at work placed elsewhere
// (poolSeatPlacedElsewhere and poolSeatAlive), observing each seat at most
// once and resolving the provider's process names once. It stays out of the
// v2 allocator's reach: selectOrPlanPoolSessionBeadAvoiding, which v2 shares,
// must stay pure, so the seats are observed here and passed to it as data.
type poolSeatsElsewhere struct {
	bp           *agentBuildParams
	cfgAgent     *config.Agent
	template     string
	alive        map[string]bool
	processNames []string
	resolved     bool
}

func newPoolSeatsElsewhere(bp *agentBuildParams, cfgAgent *config.Agent, template string) *poolSeatsElsewhere {
	return &poolSeatsElsewhere{bp: bp, cfgAgent: cfgAgent, template: template, alive: map[string]bool{}}
}

// selectOrPlan is selectOrPlanPoolSessionBeadAvoiding for request, passing
// over the live seats elsewhere it could choose (forRequest). When that
// leaves the request waiting at a full pool, it frees a slot (freeSlot) and
// adds the seats retired to the error.
func (s *poolSeatsElsewhere) selectOrPlan(preferred *session.Info, request SessionRequest, decisionTime time.Time, used map[string]bool, usedSlots map[int]bool) (session.Info, int, *poolSessionCreatePlan, error) {
	info, slot, plan, err := selectOrPlanPoolSessionBeadAvoiding(s.bp, s.cfgAgent, s.template, preferred, request, decisionTime, used, usedSlots, s.forRequest(request, used))
	if retired := s.freeSlot(request, err, used); len(retired) > 0 {
		err = fmt.Errorf("%w; freeing a slot: session %s leaves the desired set", err, strings.Join(retired, ", "))
	}
	return info, slot, plan, err
}

// forRequest returns, by session ID, the live seats a selection for request
// could choose that run outside the directory its work is placed in. A
// selection chooses only the seat the request names and the pool's reusable
// seats (reusablePoolSessionInfo), so no other seat is observed, such as a
// busy one no request for other work can reuse.
func (s *poolSeatsElsewhere) forRequest(request SessionRequest, used map[string]bool) map[string]poolSeatElsewhere {
	if s == nil || s.bp == nil || s.bp.sessionBeads == nil || strings.TrimSpace(request.WorkBeadID) == "" {
		return nil
	}
	var seats map[string]poolSeatElsewhere
	for _, info := range s.bp.sessionBeads.OpenInfos() {
		if info.ID != request.SessionBeadID && !reusablePoolSessionInfo(s.bp, s.cfgAgent, s.template, info, used) {
			continue
		}
		at, placed := poolSeatPlacedElsewhere(s.bp, s.cfgAgent, info, request)
		if !placed || !s.isAlive(info) {
			continue
		}
		if seats == nil {
			seats = map[string]poolSeatElsewhere{}
		}
		seats[info.ID] = at
	}
	return seats
}

func (s *poolSeatsElsewhere) isAlive(info session.Info) bool {
	alive, observed := s.alive[info.ID]
	if !observed {
		if !s.resolved {
			s.processNames, s.resolved = poolSeatProcessNames(s.bp, s.cfgAgent), true
		}
		alive = poolSeatAliveAs(s.bp, s.processNames, info)
		s.alive[info.ID] = alive
	}
	return alive
}

// freeSlot frees a slot of a full pool (err is errPoolSessionNoFreeSlot) for
// request's work, which runs in a managed worktree, so that work does not
// wait on seats that never leave, such as those a min_active_sessions floor
// keeps. Each seat it retires is left out of this build's desired set
// (agentBuildParams.retirePoolSeat), and used keeps the pool's later
// requests, the floor fill's included, off it. The reconciler drains a live
// retired seat as orphaned, a drain the floor does not veto, and closes the
// row of one that is not running. A seat already leaving the pool
// (poolSeatLeaving) frees its slot by itself: such seats retire, so that
// their rows close, and no other seat does. Otherwise the live seat the
// request passed over (poolSeatRunsElsewhereError) retires unless it holds
// assigned work. It returns the IDs of the seats retired.
func (s *poolSeatsElsewhere) freeSlot(request SessionRequest, err error, used map[string]bool) []string {
	if s == nil || s.bp == nil || s.bp.sessionBeads == nil || request.WorktreeSpec == nil || !errors.Is(err, errPoolSessionNoFreeSlot) {
		return nil
	}
	var retired []string
	for _, info := range s.bp.sessionBeads.OpenInfos() {
		if !used[info.ID] && s.ofPool(info) && poolSeatLeaving(s.bp, info) {
			retired = append(retired, s.retire(info, used))
		}
	}
	var passed *poolSeatRunsElsewhereError
	if len(retired) == 0 && errors.As(err, &passed) && !sessionBeadHasAssignedWorkInfo(s.bp.assignedWorkBeads, passed.seat, reuseTemplateConfig(s.bp)) {
		retired = append(retired, s.retire(passed.seat, used))
	}
	return retired
}

func (s *poolSeatsElsewhere) retire(info session.Info, used map[string]bool) string {
	if used != nil {
		used[info.ID] = true
	}
	s.bp.retirePoolSeat(info.ID)
	return info.ID
}

// ofPool reports whether info is a pool-managed seat of the pool.
func (s *poolSeatsElsewhere) ofPool(info session.Info) bool {
	return isPoolManagedSessionInfo(info) && !isNamedSessionInfo(info) && !isManualSessionInfoForAgent(info, s.cfgAgent) &&
		resolvedSessionTemplateInfo(info, reuseTemplateConfig(s.bp)) == s.template
}

// poolSeatLeaving reports whether the pool seat info is leaving the pool, its
// slot about to free with no other seat retiring: its runtime is draining, or
// it no longer runs and nothing keeps its slot. That is a seat drained or
// asleep with no hold (isPoolSessionSlotFreeableInfo), or asleep after the
// reconciler drained it as orphaned (CompleteDrainPatch records the drain's
// reason as the sleep reason). A seat an operator suspended, or one holding
// assigned work, is not leaving.
func poolSeatLeaving(bp *agentBuildParams, info session.Info) bool {
	if session.DispositionOfInfo(info).KeepsSlot() || sessionBeadHasAssignedWorkInfo(bp.assignedWorkBeads, info, reuseTemplateConfig(bp)) {
		return false
	}
	switch session.State(strings.TrimSpace(info.MetadataState)) {
	case session.StateDraining:
		return true
	case session.StateAsleep:
		if strings.TrimSpace(info.SleepReason) == drainOrphaned {
			return true
		}
	}
	return isPoolSessionSlotFreeableInfo(info, bp.now())
}

// retirePoolSeat records that legacy realization retired the pool seat id
// this build (poolSeatsElsewhere.freeSlot).
func (bp *agentBuildParams) retirePoolSeat(id string) {
	if bp.retiredPoolSeats == nil {
		bp.retiredPoolSeats = map[string]bool{}
	}
	bp.retiredPoolSeats[id] = true
}

// poolTriggerBindWorkDir is the work dir bindPoolSessionTriggerBead binds the
// pool seat info to for request: the request's verified work dir
// (verifiedPoolTriggerWorkDir), except for a live seat it would re-point at
// work placed in another directory, which keeps the directory its row records.
// Legacy realization hands such a seat work bound to no managed worktree only
// when no fresh session can start (selectOrPlanPoolSessionBeadAvoiding), so
// the seat's claim decides whether it runs that work there: it leaves it from
// inside a managed worktree. The request's evidence is still verified first,
// so unusable evidence skips the item as before.
func poolTriggerBindWorkDir(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string, info session.Info, request SessionRequest) (string, error) {
	workDir, err := verifiedPoolTriggerWorkDir(bp, cfgAgent, qualifiedName, request)
	if err != nil || workDir == "" {
		return workDir, err
	}
	at, placed := poolSeatPlacedElsewhere(bp, cfgAgent, info, request)
	if !placed || !poolSeatAlive(bp, cfgAgent, info) {
		return workDir, nil
	}
	if bp.stderr != nil {
		fmt.Fprintf(bp.stderr, "buildDesiredState: pool %q session %s keeps work dir %s, where it runs, for trigger bead %s, whose work runs in %s; its claim decides whether it runs that work there\n", qualifiedName, info.ID, at.SeatDir, request.WorkBeadID, workDir) //nolint:errcheck // best-effort stderr
	}
	return at.SeatDir, nil
}
