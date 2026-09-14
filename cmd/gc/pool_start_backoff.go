package main

// Pool start-failure backoff and park (gp-r8sk, the minimal re-cut of gp-aqrp):
// a routed work bead whose session start kept failing was re-started every
// tick because the failed-create rollback closes the SESSION bead. The count
// now lives on the WORK bead: commitStartFailure charges it (backoff, then the
// park at max_start_failures), the creation_complete commit clears it,
// workStartDeferral gates the demand inputs, one mail per park goes to
// [session] park_alert_to, and an unpark is a hand act (gc sling --reassign).

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

const (
	// startFailureBackoffBase is the wait after the first consecutive failed
	// start of a routed work bead; each further failure doubles it.
	startFailureBackoffBase = 10 * time.Second
	// startFailureBackoffCap bounds the doubling.
	startFailureBackoffCap = 5 * time.Minute
	// workStartFailureLineLimit bounds the one-line failure record kept on the
	// bead (runes).
	workStartFailureLineLimit = 240

	// workStartMailSender: beadmail passes an unresolvable sender through.
	workStartMailSender = "gc"

	TraceSitePoolStartDeferred    TraceSiteCode   = "reconciler.pool.start_deferred"
	TraceSitePoolWorkStartFailure TraceSiteCode   = "reconciler.pool.work_start_failure"
	TraceReasonParked             TraceReasonCode = "parked"
	TraceReasonStartBackoff       TraceReasonCode = "start_backoff"
)

// startFailureBackoff is the wait before the next start of a routed work bead
// after failures consecutive failed starts: 10s, 20s, 40s, 80s, 160s, then 5m.
// Zero failures means no wait.
func startFailureBackoff(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	backoff := startFailureBackoffBase
	for i := 1; i < failures && backoff < startFailureBackoffCap; i++ {
		backoff *= 2
	}
	if backoff > startFailureBackoffCap {
		return startFailureBackoffCap
	}
	return backoff
}

// workStartDeferral reports whether the pool must not start a session for b
// at now: reason is TraceReasonParked (gc.parked_at set; until is zero) or
// TraceReasonStartBackoff (gc.start_backoff_until still in the future; until
// is that deadline). Any non-blank gc.parked_at parks: an operator releases a
// park by clearing the key, never by editing it.
func workStartDeferral(b beads.Bead, now time.Time) (reason TraceReasonCode, until time.Time, deferred bool) {
	if strings.TrimSpace(b.Metadata[beadmeta.ParkedAtMetadataKey]) != "" {
		return TraceReasonParked, time.Time{}, true
	}
	if deadline, ok := parseRFC3339Metadata(b.Metadata[beadmeta.StartBackoffUntilMetadataKey]); ok && now.Before(deadline) {
		return TraceReasonStartBackoff, deadline, true
	}
	return "", time.Time{}, false
}

// workStartDeferralPass applies workStartDeferral across one demand build: the
// instant the rows are judged against, the trace, the earliest backoff deadline
// it held a row to (the cached demand snapshot expires there) and the ids held.
type workStartDeferralPass struct {
	now       time.Time
	trace     *sessionReconcilerTraceCycle
	recheckAt time.Time
	deferred  map[string]struct{}
}

func newWorkStartDeferralPass(now time.Time, trace *sessionReconcilerTraceCycle) *workStartDeferralPass {
	return &workStartDeferralPass{now: now, trace: trace, deferred: make(map[string]struct{})}
}

// skip reports whether b is held back from pool demand for template, recording
// the trace decision and the recheck deadline when it is.
func (p *workStartDeferralPass) skip(b beads.Bead, template string) bool {
	reason, until, deferred := workStartDeferral(b, p.now)
	if !deferred {
		return false
	}
	p.deferred[b.ID] = struct{}{}
	if !until.IsZero() && (p.recheckAt.IsZero() || until.Before(p.recheckAt)) {
		p.recheckAt = until
	}
	if p.trace != nil {
		payload := traceRecordPayload{"work_bead": b.ID, "reason": string(reason)}
		if !until.IsZero() {
			payload["until"] = until.UTC().Format(time.RFC3339)
		}
		p.trace.RecordDecision(TraceSitePoolStartDeferred, reason, TraceOutcomeDeferred, template, "", payload)
	}
	return true
}

// filter returns rows minus the deferred ones; filterWithRefs and
// filterWithFlags do the same over an index-aligned companion slice.
func (p *workStartDeferralPass) filter(rows []beads.Bead) []beads.Bead {
	kept, _ := filterDeferredAligned(p, rows, []struct{}(nil))
	return kept
}

func (p *workStartDeferralPass) filterWithRefs(rows []beads.Bead, refs []string) ([]beads.Bead, []string) {
	return filterDeferredAligned(p, rows, refs)
}

func (p *workStartDeferralPass) filterWithFlags(rows []beads.Bead, flags []bool) ([]beads.Bead, []bool) {
	return filterDeferredAligned(p, rows, flags)
}

func filterDeferredAligned[T any](p *workStartDeferralPass, rows []beads.Bead, aligned []T) ([]beads.Bead, []T) {
	keptRows := rows[:0:0]
	keptAligned := aligned[:0:0]
	for i, b := range rows {
		if p.skip(b, routedToOrLegacyWorkflowTarget(b)) {
			continue
		}
		keptRows = append(keptRows, b)
		if i < len(aligned) {
			keptAligned = append(keptAligned, aligned[i])
		}
	}
	return keptRows, keptAligned
}

// earliestRecheck folds two recheck deadlines: the earlier non-zero one.
func earliestRecheck(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

// poolDemandAssignedWork is the assigned tier's one input builder: owned is
// the assigned work the pool's sessions hold (the raw pool-demand projection,
// what a session OWNS, unfiltered so a session minted for a parked or
// backed-off bead stays owned by it and is never reused for other work);
// demand is owned minus the rows the start gate holds back, the only slice a
// ComputePoolDesiredStates call may take (codex r3 finding 4).
func poolDemandAssignedWork(
	cfg *config.City,
	cityPath string,
	leading beads.Store,
	sessionInfos []sessionpkg.Info,
	assignedWorkBeads []beads.Bead,
	assignedWorkStoreRefs []string,
	pass *workStartDeferralPass,
) (owned, demand []beads.Bead) {
	owned = filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, leading, sessionInfos, assignedWorkBeads, assignedWorkStoreRefs)
	return owned, pass.filter(owned)
}

// workStartFailurePolicy is what the commit paths need to charge or clear the
// WORK bead a start was for: the stores a trigger's store ref can name, the
// per-agent park threshold, and the park mail target. One value per tick.
type workStartFailurePolicy struct {
	cfg       *config.City
	workStore beads.Store
	rigStores map[string]beads.Store
	mail      mail.Provider
	alertTo   string
	// missLogged keeps the "work bead not found" line to once per bead id for
	// the life of the policy owner (C7: a miss is logged once, not charged).
	missLogged *sync.Map
	// mu serializes the read-modify-write on a work bead across the async start
	// commits of one process. Shared by every policy value the runtime hands out.
	mu *sync.Mutex
	// testBeforeWrite runs between the read and the conditional write (tests
	// only: it lets a test land an operator write in that window).
	testBeforeWrite func()
}

// newWorkStartFailurePolicy builds a policy with its own guards; the runtime
// shares the guards across the values it builds per tick.
func newWorkStartFailurePolicy(cfg *config.City, workStore beads.Store, rigStores map[string]beads.Store, mailer mail.Provider, alertTo string) *workStartFailurePolicy {
	return &workStartFailurePolicy{cfg: cfg, workStore: workStore, rigStores: rigStores, mail: mailer, alertTo: alertTo, missLogged: &sync.Map{}, mu: &sync.Mutex{}}
}

func (p *workStartFailurePolicy) lock() func() {
	if p.mu == nil {
		return func() {}
	}
	p.mu.Lock()
	return p.mu.Unlock
}

// attachWorkStartPolicy pins the tick's policy and the trigger THIS start
// carries (the async commit refreshes candidate.info, which may by then name a
// rebound trigger; the charge and the reset belong to the bead started for).
func (item *preparedStart) attachWorkStartPolicy(p *workStartFailurePolicy) {
	if item == nil {
		return
	}
	item.workStartFailures = p
	item.triggerBeadID = strings.TrimSpace(item.candidate.info.TriggerBeadID)
	item.triggerStoreRef = strings.TrimSpace(item.candidate.info.TriggerBeadStoreRef)
}

func withWorkStartFailurePolicy(p *workStartFailurePolicy) startExecutionOption {
	return func(opts *startExecutionOptions) {
		opts.workStartFailures = p
	}
}

// storeFor resolves the store ref a start request carries: the city work store
// for "", "city", "city:<name>" and a class ref (identical to the work store
// unless the class is relocated, which C7 leaves out), the named rig store for
// "rig:<name>", nil otherwise.
func (p *workStartFailurePolicy) storeFor(ref string) beads.Store {
	norm := normalizeDemandStoreRef(ref)
	switch {
	case norm == "" || norm == "city":
		return p.workStore
	case strings.HasPrefix(norm, "rig:"):
		return p.rigStores[strings.TrimPrefix(norm, "rig:")]
	}
	return nil
}

// workBead reads the trigger work bead of a start; a store or bead the ref does
// not reach is logged once and returns ok=false. An EMPTY ref (the assigned
// tier's requests carry none) means the city work store first, then each rig
// store by id (bead ids are unique within a deployment): the start request's
// own store set, not a sweep for relocated class beads.
func (p *workStartFailurePolicy) workBead(id, ref string, stderr io.Writer) (beads.Store, beads.Bead, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, beads.Bead{}, false
	}
	ref = strings.TrimSpace(ref)
	store := p.storeFor(ref)
	if store == nil {
		p.logMissOnce(stderr, id, fmt.Sprintf("store ref %q not reachable from the reconciler", ref))
		return nil, beads.Bead{}, false
	}
	if ref != "" {
		b, err := store.Get(id)
		if err != nil {
			p.logMissOnce(stderr, id, fmt.Sprintf("read from store ref %q: %v", ref, err))
			return nil, beads.Bead{}, false
		}
		return store, b, true
	}
	// Empty ref: the id must be found in exactly one of the request's stores.
	// Independent stores may share an id (storeScopedBeadKey exists for that);
	// two hits cannot be told apart here, so the charge fails closed.
	candidates := []beads.Store{store}
	names := make([]string, 0, len(p.rigStores))
	for name := range p.rigStores {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if p.rigStores[name] != nil {
			candidates = append(candidates, p.rigStores[name])
		}
	}
	var (
		hitStore beads.Store
		hit      beads.Bead
		hits     int
		lastErr  error
	)
	for _, candidate := range candidates {
		b, err := candidate.Get(id)
		if err != nil {
			if !errors.Is(err, beads.ErrNotFound) {
				lastErr = err
			}
			continue
		}
		hits++
		hitStore, hit = candidate, b
	}
	switch {
	case hits == 1 && lastErr == nil:
		return hitStore, hit, true
	case hits > 1:
		p.logMissOnce(stderr, id, fmt.Sprintf("id found in %d stores with no store ref on the start request; ambiguous", hits))
	case hits == 1:
		// One hit and one store that could not be read: uniqueness was never
		// established, so the hit is not charged (codex r3 finding 2).
		p.logMissOnce(stderr, id, fmt.Sprintf("partial read with no store ref (one hit, one store unreadable): %v", lastErr))
	case lastErr != nil:
		p.logMissOnce(stderr, id, fmt.Sprintf("read with no store ref: %v", lastErr))
	default:
		p.logMissOnce(stderr, id, "not found in the city store or any rig store")
	}
	return nil, beads.Bead{}, false
}

// unconditionalWorkBeadWriteLogged gates the one-per-process line that says
// the charge and reset writes on this build run unconditionally (the store
// offers no revision-conditioned write).
var unconditionalWorkBeadWriteLogged sync.Once

// updateWorkBead writes patch through a revision-conditioned write when the
// store offers one (a write since the read is a conflict, never overwritten).
// The store is resolved through the wrappers gc puts around it (the policy
// wrapper embeds beads.Store, which promotes no optional capability, and
// declares a resolve target instead), so the production store reaches the backend's UpdateIfMatch (codex r3
// finding 1). A store with no conditional write at all writes unconditionally
// and says so once per process.
func updateWorkBead(store beads.Store, b beads.Bead, patch map[string]string, stderr io.Writer) (conflict bool, err error) {
	writer, ok := workBeadConditionalWriter(store)
	if !ok {
		logUnconditionalWorkBeadWrite(stderr, b.ID, "no conditional writer resolves from the store")
		return false, store.Update(b.ID, beads.UpdateOpts{Metadata: patch})
	}
	err = writer.UpdateIfMatch(b.ID, b.Revision, beads.UpdateOpts{Metadata: patch})
	var precondition *beads.PreconditionFailedError
	switch {
	case err == nil:
		return false, nil
	case errors.As(err, &precondition):
		return true, err
	case errors.Is(err, beads.ErrConditionalWriteUnsupported):
		logUnconditionalWorkBeadWrite(stderr, b.ID, err.Error())
		return false, store.Update(b.ID, beads.UpdateOpts{Metadata: patch})
	}
	return false, err
}

// workBeadConditionalWriter resolves the store's conditional writer through
// the wrappers gc puts around it: the policy wrapper embeds the Store
// interface, which promotes no optional capability, so it declares a resolve
// target instead; follow it (bounded, cycle-safe).
func workBeadConditionalWriter(store beads.Store) (beads.ConditionalWriter, bool) {
	for depth := 0; store != nil && depth < 8; depth++ {
		if writer, ok := beads.ConditionalWriterFor(store); ok {
			return writer, true
		}
		target, ok := store.(beads.ConditionalWritesResolveTargeter)
		if !ok {
			return nil, false
		}
		next := target.ConditionalWritesResolveTarget()
		if next == nil || next == store {
			return nil, false
		}
		store = next
	}
	return nil, false
}

func logUnconditionalWorkBeadWrite(stderr io.Writer, beadID, detail string) {
	unconditionalWorkBeadWriteLogged.Do(func() {
		fmt.Fprintf(stderr, "session reconciler: start-failure writes on work beads run UNCONDITIONALLY on this store (first on %s): %s; a concurrent --reassign reset can be overwritten\n", beadID, detail) //nolint:errcheck // best-effort stderr
	})
}

// ceilSecond rounds up to the next whole second (RFC3339 drops the fraction).
func ceilSecond(t time.Time) time.Time {
	if t.Nanosecond() == 0 {
		return t
	}
	return t.Truncate(time.Second).Add(time.Second)
}

func (p *workStartFailurePolicy) logMissOnce(stderr io.Writer, id, detail string) {
	if p.missLogged != nil {
		if _, seen := p.missLogged.LoadOrStore(id, struct{}{}); seen {
			return
		}
	}
	fmt.Fprintf(stderr, "session reconciler: work bead %s start failure not charged: %s\n", id, detail) //nolint:errcheck // best-effort stderr
}

// recordWorkStartFailure charges the work bead a failed start was for: one
// Update carrying the counter and backoff deadline, or, at the threshold, the
// park. Only a provider-error outcome counts (the class a pre_start failure
// lands in); the startup rate-limit screen is excluded because it holds an
// EXISTING session under its own quarantine. A parked bead is not charged again.
func recordWorkStartFailure(result startResult, clk clock.Clock, stderr io.Writer, trace *sessionReconcilerTraceCycle) {
	p := result.prepared.workStartFailures
	if p == nil || result.err == nil || result.outcome != TraceOutcomeProviderError || result.rateLimitScreen {
		return
	}
	info := result.prepared.candidate.info
	template := result.prepared.candidate.tp.TemplateName
	maxFailures := findAgentByTemplate(p.cfg, template).EffectiveMaxStartFailures()
	now := clk.Now().UTC()
	stamp := now.Format(time.RFC3339)
	line := lastErrorLine(result.err)
	unlock := p.lock()
	var (
		store    beads.Store
		beadID   string
		failures int
		parked   bool
		written  bool
	)
	// One re-read on a revision conflict (an operator's --reassign reset landed
	// in between); a second conflict drops this charge rather than overwrite.
	for attempt := 0; attempt < 2 && !written; attempt++ {
		var b beads.Bead
		var ok bool
		store, b, ok = p.workBead(result.prepared.triggerBeadID, result.prepared.triggerStoreRef, stderr)
		if !ok || strings.TrimSpace(b.Metadata[beadmeta.ParkedAtMetadataKey]) != "" {
			unlock()
			return
		}
		if p.testBeforeWrite != nil {
			p.testBeforeWrite()
		}
		beadID = b.ID
		failures = metadataInt(b.Metadata[beadmeta.StartFailuresMetadataKey]) + 1
		parked = maxFailures > 0 && failures >= maxFailures
		patch := map[string]string{
			beadmeta.StartFailuresMetadataKey:     strconv.Itoa(failures),
			beadmeta.StartFailedAtMetadataKey:     stamp,
			beadmeta.StartFailureMetadataKey:      line,
			beadmeta.StartBackoffUntilMetadataKey: ceilSecond(now.Add(startFailureBackoff(failures))).Format(time.RFC3339),
		}
		if parked {
			patch[beadmeta.ParkedAtMetadataKey] = stamp
			patch[beadmeta.ParkReasonMetadataKey] = line
			patch[beadmeta.ParkFailuresMetadataKey] = strconv.Itoa(failures)
			patch[beadmeta.StartFailuresMetadataKey] = ""
			patch[beadmeta.StartBackoffUntilMetadataKey] = ""
		}
		conflict, err := updateWorkBead(store, b, patch, stderr)
		if conflict {
			continue
		}
		if err != nil {
			fmt.Fprintf(stderr, "session reconciler: recording start failure on work bead %s: %v\n", b.ID, err) //nolint:errcheck // best-effort stderr
			unlock()
			return
		}
		written = true
	}
	unlock()
	if !written {
		fmt.Fprintf(stderr, "session reconciler: work bead %s changed twice under a start-failure write; this failure is not recorded\n", beadID) //nolint:errcheck // best-effort stderr
		return
	}
	if parked {
		fmt.Fprintf(stderr, "session reconciler: PARKED work bead %s after %d start failures (agent %s): %s\n", beadID, failures, template, line) //nolint:errcheck // best-effort stderr
		if trace != nil {
			trace.RecordDecision(TraceSitePoolWorkStartFailure, TraceReasonParked, TraceOutcomeHeld, template, info.SessionNameMetadata, traceRecordPayload{
				"work_bead": beadID,
				"failures":  failures,
				"error":     line,
			})
		}
		// Sent after the lock is released: a slow provider holds no other bead.
		p.sendParkMail(store, beadID, template, failures, line, stamp, stderr)
		return
	}
	backoff := startFailureBackoff(failures)
	fmt.Fprintf(stderr, "session reconciler: work bead %s start failure %d/%d (agent %s), next start no sooner than %s: %s\n", beadID, failures, maxFailures, template, backoff, line) //nolint:errcheck // best-effort stderr
	if trace != nil {
		trace.RecordDecision(TraceSitePoolWorkStartFailure, TraceReasonStartBackoff, TraceOutcomeRetry, template, info.SessionNameMetadata, traceRecordPayload{
			"work_bead": beadID,
			"failures":  failures,
			"backoff":   backoff.String(),
			"error":     line,
		})
	}
}

// sendParkMail sends the one park mail. The park is already written: a missing
// target or a failed send stamps gc.park_mail_failed and logs one loud line.
func (p *workStartFailurePolicy) sendParkMail(store beads.Store, beadID, template string, failures int, line, stamp string, stderr io.Writer) {
	to := strings.TrimSpace(p.alertTo)
	var err error
	switch {
	case to == "" || p.mail == nil:
		err = fmt.Errorf("[session] park_alert_to unset or no mail provider")
	default:
		subject := fmt.Sprintf("PARKED %s after %d start failures", beadID, failures)
		body := fmt.Sprintf("Work bead %s is PARKED: the pool stopped starting sessions for it.\n\nAgent:    %s\nFailures: %d consecutive failed starts\nLast:     %s\nParked:   %s\n\nThe bead keeps its status, assignee and gc.routed_to; nothing unparks it automatically.\nTo re-dispatch: gc sling --reassign %s %s\nTo release it in place: gc bd update %s --unset-metadata %s --unset-metadata %s --unset-metadata %s --unset-metadata %s\n",
			beadID, template, failures, line, stamp,
			template, beadID,
			beadID, beadmeta.ParkedAtMetadataKey, beadmeta.ParkReasonMetadataKey, beadmeta.ParkFailuresMetadataKey, beadmeta.ParkMailFailedMetadataKey)
		_, err = p.mail.Send(workStartMailSender, to, subject, body)
	}
	if err == nil {
		return
	}
	fmt.Fprintf(stderr, "session reconciler: PARK MAIL FAILED for work bead %s (to %q): %v\n", beadID, to, err) //nolint:errcheck // best-effort stderr
	if updateErr := store.Update(beadID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.ParkMailFailedMetadataKey: stamp}}); updateErr != nil {
		fmt.Fprintf(stderr, "session reconciler: recording park mail failure on work bead %s: %v\n", beadID, updateErr) //nolint:errcheck // best-effort stderr
	}
}

// recordWorkStartSuccess clears the start-failure counter on the work bead a
// confirmed start was for, after the CommitStartedPatch batch (the
// creation_complete stamp) has landed. Park keys are never touched here (C6).
func recordWorkStartSuccess(result startResult, stderr io.Writer) {
	recordWorkStartSuccessFor(result.prepared.workStartFailures, result.prepared.triggerBeadID, result.prepared.triggerStoreRef, stderr)
}

// recordWorkStartSuccessFor is the reset for a confirmed start of the session
// whose trigger is (id, ref): the ordinary commit and the pending-create
// recovery commit (recoverRunningPendingCreate), both of which stamp
// creation_complete_at.
func recordWorkStartSuccessFor(p *workStartFailurePolicy, id, ref string, stderr io.Writer) {
	if p == nil {
		return
	}
	defer p.lock()()
	// One re-read on a revision conflict (an unrelated edit landed between
	// the read and the write), the charge's own two-attempt shape; a second
	// conflict logs and leaves the row (codex r3 finding 6).
	for attempt := 0; attempt < 2; attempt++ {
		store, b, ok := p.workBead(id, ref, stderr)
		if !ok {
			return
		}
		patch := map[string]string{}
		for _, key := range []string{
			beadmeta.StartFailuresMetadataKey,
			beadmeta.StartFailedAtMetadataKey,
			beadmeta.StartFailureMetadataKey,
			beadmeta.StartBackoffUntilMetadataKey,
		} {
			if strings.TrimSpace(b.Metadata[key]) != "" {
				patch[key] = ""
			}
		}
		if len(patch) == 0 {
			return
		}
		if p.testBeforeWrite != nil {
			p.testBeforeWrite()
		}
		conflict, err := updateWorkBead(store, b, patch, stderr)
		switch {
		case conflict && attempt == 0:
			continue
		case conflict:
			fmt.Fprintf(stderr, "session reconciler: start-failure reset on work bead %s skipped: row changed twice since the read\n", b.ID) //nolint:errcheck // best-effort stderr
		case err != nil:
			fmt.Fprintf(stderr, "session reconciler: clearing start failures on work bead %s: %v\n", b.ID, err) //nolint:errcheck // best-effort stderr
		}
		return
	}
}

// metadataInt reads a non-negative integer metadata value; blank reads as zero.
func metadataInt(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// lastErrorLine is the one-line failure record kept on the bead: the last
// non-blank line of the error text, bounded to workStartFailureLineLimit runes.
func lastErrorLine(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	// The setup runner folds "...; stderr: <tail>; stdout: <tail>", stdout last.
	if i := strings.LastIndex(text, "stderr: "); i >= 0 {
		text = text[i+len("stderr: "):]
		if j := strings.Index(text, "; stdout: "); j >= 0 {
			text = text[:j]
		}
	}
	line := ""
	for _, candidate := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			line = trimmed
		}
	}
	if runes := []rune(line); len(runes) > workStartFailureLineLimit {
		line = string(runes[:workStartFailureLineLimit])
	}
	return line
}
