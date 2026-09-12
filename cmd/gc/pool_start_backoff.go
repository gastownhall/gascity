package main

// Per-work-bead start backoff and park.
//
// A routed work bead with no live session is pool demand on EVERY reconciler
// tick: an in_progress bead assigned to the bare pool template takes the
// wake-known-identity tier (pool_desired_state.go), an open unassigned routed
// bead the scale_check tier (build_desired_state.go). Each tier creates a
// brand-new session bead. When that session's start fails (a failing
// pre_start included) the session bead is rolled back and closed as
// failed-create, and — deliberately — no wake failure accrues on it: the
// rollback comment in commitStartFailure says "closed and recreated fresh on
// the next tick". Every per-session damper (wake_attempts, churn_count,
// quarantined_until) therefore restarts from zero with the next session bead,
// while the WORK bead, untouched by the rollback, is counted again. The
// result measured on citadel 2026-09-11 02:41Z–03:04Z: about 160 sessions in
// 23 minutes for one bead whose pre_start could not resolve its branch,
// visible only in supervisor.log (papercut pc_b969af2a45eb).
//
// The durable operand is the work bead's own metadata (the supervisor
// restarts, a handoff restarts the reconciler; process memory does not
// survive either):
//
//   - gc.start_failures / gc.start_failed_at / gc.start_failure — the
//     consecutive failed-start count, the last failure time, the last failure
//     line;
//   - gc.start_backoff_until — the time before which no start is planned for
//     the bead (10s doubling per failure, capped at 5m: startFailureBackoff);
//   - gc.parked_at / gc.park_reason / gc.park_failures / gc.park_id — the
//     park, written once the count reaches the agent's max_start_failures
//     (default 5): no further start, the bead keeps its status and route so
//     no other pool claims it, and ONE mail goes to the mayor. gc.park_id is
//     the park's random identity: it names the mail (subject tag), fences the
//     delivered stamp (gc.park_mailed_at) so a delivery for an older park
//     never marks a newer one mailed, and lets a retry find a mail that
//     landed before a restart could stamp it — an undelivered park mail is
//     retried each tick until it lands.
//
// A successful start (creation_complete) clears the whole record. The unpark
// is a designed surface, never a hand edit: gc sling --reassign clears every
// key above before routing (internal/sling reopenForReassign), and unsetting
// gc.park_reason, gc.parked_at and gc.park_failures does the same for a
// hold-in-place fix — at park time the count folds into gc.park_failures and
// gc.start_failures is cleared, so the three-key unpark starts a clean count.
//
// The gate sits where demand is counted (workStartDeferral): a parked or
// backed-off bead is not demand for either tier, so the pool never plans a
// start for it. The record is written where the start result is known
// (commitStartFailure / commitStartResultTraced) through the
// workStartFailurePolicy the controller threads in with the start options.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
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

	// workStartDeferralBackoff and workStartDeferralParked are the two reasons
	// workStartDeferral can defer a start; they double as trace reason codes.
	workStartDeferralBackoff = "start_backoff"
	workStartDeferralParked  = "parked"
)

// startFailureBackoff is the wait before the next start of a routed work bead
// after failures consecutive failed starts: 10s, 20s, 40s, 80s, 160s, then 5m.
func startFailureBackoff(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	backoff := startFailureBackoffBase
	for i := 1; i < failures; i++ {
		backoff *= 2
		if backoff >= startFailureBackoffCap {
			return startFailureBackoffCap
		}
	}
	if backoff > startFailureBackoffCap {
		return startFailureBackoffCap
	}
	return backoff
}

// workStartFailureState is the typed read of the start-failure and park keys
// on one work bead. Absent, empty and unparseable values read as zero: cmd/gc
// clears metadata by empty value (clearedSessionAffinityMetadata), and bd's
// --unset-metadata deletes the key, so both spellings of "no park" agree.
type workStartFailureState struct {
	Failures     int
	FailedAt     time.Time
	Failure      string
	BackoffUntil time.Time
	ParkedAt     time.Time
	ParkReason   string
	ParkFailures int
	ParkID       string
	ParkMailedAt time.Time
}

// Parked reports whether the bead carries a park.
func (s workStartFailureState) Parked() bool {
	return !s.ParkedAt.IsZero()
}

func readWorkStartFailureState(meta map[string]string) workStartFailureState {
	return workStartFailureState{
		Failures:     metadataInt(meta[beadmeta.StartFailuresMetadataKey]),
		FailedAt:     metadataTime(meta[beadmeta.StartFailedAtMetadataKey]),
		Failure:      strings.TrimSpace(meta[beadmeta.StartFailureMetadataKey]),
		BackoffUntil: metadataTime(meta[beadmeta.StartBackoffUntilMetadataKey]),
		ParkedAt:     metadataTime(meta[beadmeta.ParkedAtMetadataKey]),
		ParkReason:   strings.TrimSpace(meta[beadmeta.ParkReasonMetadataKey]),
		ParkFailures: metadataInt(meta[beadmeta.ParkFailuresMetadataKey]),
		ParkID:       strings.TrimSpace(meta[beadmeta.ParkIDMetadataKey]),
		ParkMailedAt: metadataTime(meta[beadmeta.ParkMailedAtMetadataKey]),
	}
}

// parkIdentity is what a delivery must match to acknowledge a park: the
// random gc.park_id, or — for a park written before that key existed — the
// bead's id and its gc.parked_at together: two legacy parks stamped in the
// same second on different beads must not share a mail receipt, or one
// bead's landed mail would stamp the other's park mailed without a send.
func (s workStartFailureState) parkIdentity(beadID string) string {
	if s.ParkID != "" {
		return s.ParkID
	}
	if s.ParkedAt.IsZero() {
		return ""
	}
	return strings.TrimSpace(beadID) + "@" + s.ParkedAt.UTC().Format(time.RFC3339)
}

// newParkID mints a park's random identity (8 bytes, hex). A failed read of
// the random source falls back to the nanosecond clock — unique enough to
// tell two parks of one bead apart, which is all the fence needs.
func newParkID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func metadataInt(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func metadataTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return t
}

// workStartDeferral reports whether the pool must not plan a start for the
// bead at now: parked (deferred until the unpark; until is the park time) or
// inside its backoff window (until is the window's end).
func workStartDeferral(meta map[string]string, now time.Time) (deferred bool, reason string, until time.Time) {
	state := readWorkStartFailureState(meta)
	if state.Parked() {
		return true, workStartDeferralParked, state.ParkedAt
	}
	if !state.BackoffUntil.IsZero() && now.Before(state.BackoffUntil) {
		return true, workStartDeferralBackoff, state.BackoffUntil
	}
	return false, "", time.Time{}
}

// workStartFailurePatch is the metadata patch for one more failed start at
// now. Below the limit it advances the count and sets the next backoff window;
// at the limit (limit > 0) it parks: the count folds into gc.park_failures,
// gc.start_failures and gc.start_backoff_until clear, gc.park_mailed_at clears
// so the mail is owed. A bead that is already parked keeps its park (and its
// mail stamp): a further failure is recorded as the last failure, is not a
// second park, and does not count toward the next attempt.
func workStartFailurePatch(state workStartFailureState, now time.Time, failure string, limit int) (map[string]string, bool) {
	failures := state.Failures + 1
	stamp := now.UTC().Format(time.RFC3339)
	patch := map[string]string{
		beadmeta.StartFailedAtMetadataKey: stamp,
		beadmeta.StartFailureMetadataKey:  failure,
	}
	if state.Parked() {
		// The park holds the count that earned it (gc.park_failures); a
		// failure that lands on a parked bead (a start already in flight
		// when the park was written) is recorded as the last failure only —
		// the counter stays cleared, so the unpark that unsets the three park
		// keys starts the next attempt's count from zero, as documented.
		patch[beadmeta.StartFailuresMetadataKey] = ""
		patch[beadmeta.StartBackoffUntilMetadataKey] = ""
		return patch, false
	}
	if limit > 0 && failures >= limit {
		patch[beadmeta.StartFailuresMetadataKey] = ""
		patch[beadmeta.StartBackoffUntilMetadataKey] = ""
		patch[beadmeta.ParkedAtMetadataKey] = stamp
		patch[beadmeta.ParkReasonMetadataKey] = failure
		patch[beadmeta.ParkFailuresMetadataKey] = strconv.Itoa(failures)
		patch[beadmeta.ParkIDMetadataKey] = newParkID()
		patch[beadmeta.ParkMailedAtMetadataKey] = ""
		return patch, true
	}
	patch[beadmeta.StartFailuresMetadataKey] = strconv.Itoa(failures)
	patch[beadmeta.StartBackoffUntilMetadataKey] = now.UTC().Add(startFailureBackoff(failures)).Format(time.RFC3339)
	return patch, false
}

// workStartFailureClearPatch clears every record/park key
// (beadmeta.WorkStartFailureMetadataKeys) the bead carries a value for. Empty
// when nothing is set, so the steady state writes nothing.
func workStartFailureClearPatch(meta map[string]string) map[string]string {
	patch := map[string]string{}
	for _, key := range beadmeta.WorkStartFailureMetadataKeys {
		if strings.TrimSpace(meta[key]) != "" {
			patch[key] = ""
		}
	}
	return patch
}

// workStartFailureLine reduces a start error to the one line the bead keeps:
// the last non-empty line (a pre_start failure ends in its stderr tail, so
// that is the diagnostic line), bounded to workStartFailureLineLimit runes.
func workStartFailureLine(err error) string {
	if err == nil {
		return ""
	}
	line := ""
	for _, candidate := range strings.Split(err.Error(), "\n") {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			line = candidate
		}
	}
	if runes := []rune(line); len(runes) > workStartFailureLineLimit {
		line = string(runes[:workStartFailureLineLimit-1]) + "…"
	}
	return line
}

// parkedWorkNotice is what the park mail names.
type parkedWorkNotice struct {
	BeadID   string
	Title    string
	Agent    string
	Failures int
	Reason   string
	ParkedAt time.Time
	// ParkID is the park's identity (workStartFailureState.parkIdentity); it
	// rides in the subject as Tag so a landed mail can be recognized later.
	ParkID string
}

// Tag is the subject token that identifies the park the mail is for.
func (n parkedWorkNotice) Tag() string {
	return fmt.Sprintf("[park %s]", n.ParkID)
}

// Subject and Body are the one mail the mayor gets per park.
func (n parkedWorkNotice) Subject() string {
	return fmt.Sprintf("PARKED %s: %d consecutive failed session starts on %s %s", n.BeadID, n.Failures, n.Agent, n.Tag())
}

func (n parkedWorkNotice) Body() string {
	var b strings.Builder
	fmt.Fprintf(&b, "The pool parked work bead %s after %d consecutive failed session starts for agent %s.\n", n.BeadID, n.Failures, n.Agent)
	if title := strings.TrimSpace(n.Title); title != "" {
		fmt.Fprintf(&b, "Title: %s\n", title)
	}
	fmt.Fprintf(&b, "Parked at: %s\n", n.ParkedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Last failure: %s\n", n.Reason)
	b.WriteString("\nNo further session is started for this bead until it is unparked; it keeps its status and route so no other pool claims it.\n")
	fmt.Fprintf(&b, "Unpark by re-dispatching it (gc sling --reassign <agent> %s), or hold it in place and clear the park with:\n", n.BeadID)
	fmt.Fprintf(&b, "  gc bd update %s --unset-metadata %s --unset-metadata %s --unset-metadata %s\n", n.BeadID, beadmeta.ParkReasonMetadataKey, beadmeta.ParkedAtMetadataKey, beadmeta.ParkFailuresMetadataKey)
	b.WriteString("The park keys are visible in gc bd show.\n")
	return b.String()
}

// workStartFailurePolicy is the reconciler-side wiring for the record: where
// the trigger work bead of a start lives, the per-agent park limit, and how a
// park is mailed. nil disables the record (tests that do not care, callers
// without a work store); a nil notify parks without mail and says so on
// stderr.
type workStartFailurePolicy struct {
	workStore beads.Store
	rigStores map[string]beads.Store
	// extraStores are the relocated class-binding stores a work bead can also
	// live in (storageRoutes.relocatedStores); probed by id after the work and
	// rig stores.
	extraStores []beads.Store
	// classStoreByRef resolves a "class:<token>" trigger store ref to the
	// relocated class store it names (storageRoutes.storeForClassRef); nil, or
	// a nil answer, falls back to the work store.
	classStoreByRef func(ref string) beads.Store
	// templateOf returns the pool template a work bead is routed to now; nil
	// skips the re-route check. A failure of a session started for template T
	// is charged only while the bead is still routed to T — a bead
	// re-dispatched elsewhere in the meantime (gc sling --reassign) keeps its
	// fresh record.
	templateOf func(beads.Bead) string
	// canonicalTemplate maps the template a session was started for to the
	// same canonical identity templateOf returns for a bead's route, so a
	// legacy bound identity left on a bead by a bound→unbound migration
	// ("rig/old.worker" for the agent "rig/worker") is not mistaken for a
	// re-route. Nil compares the template as given.
	canonicalTemplate func(template string) string
	// limitFor returns the agent's effective max_start_failures for a session
	// template; nil means the default.
	limitFor func(template string) int
	// sweeps counts the park-mail retry sweeps this policy handed to the
	// background (retryUnmailedParks); awaitParkMailRetries waits on it —
	// tests only, the tick never waits for a mail.
	sweeps sync.WaitGroup
	// notify sends the park mail; it returns an error when the mail did not
	// land, in which case the park stays unmailed and is retried each tick.
	notify func(parkedWorkNotice) error
	// lookup reports whether a mail carrying the notice's Tag already exists
	// in the recipient's mail (read or unread): a delivery that landed before
	// the stamp could persist (a restart in between) is then stamped, never
	// sent again. Nil skips the check.
	lookup func(parkedWorkNotice) (bool, error)
	// retry throttles the re-send of an unlanded park mail (nil = every tick).
	retry  *parkMailRetryState
	stderr io.Writer
	// resolveWriter yields the conditional writer the record write fences on
	// (nil writer = unfenced, the plain re-read path); nil uses the designed
	// seam, beads.ResolveConditionalWriter — which follows a wrapper's declared
	// resolution target and honors the city's beads.conditional_writes mode —
	// and tests inject a bare capability probe to drive the fenced arms on a
	// store that carries no mode.
	resolveWriter func(beads.Store) (beads.ConditionalWriter, error)
	// requireFenced reports whether a store is stamped beads.conditional_writes
	// = "require" (nil = beads.ConditionalWritesRequired); a fenced write the
	// store refuses as unsupported at write time then fails closed instead of
	// taking the plain path. Tests inject it on a store that carries no mode.
	requireFenced func(beads.Store) bool
}

// fencedWritesRequired resolves requireFenced.
func (p *workStartFailurePolicy) fencedWritesRequired(store beads.Store) bool {
	if p != nil && p.requireFenced != nil {
		return p.requireFenced(store)
	}
	return beads.ConditionalWritesRequired(store)
}

// conditionalWriter resolves the writer writeWorkRecord fences on.
func (p *workStartFailurePolicy) conditionalWriter(store beads.Store) (beads.ConditionalWriter, error) {
	if p != nil && p.resolveWriter != nil {
		return p.resolveWriter(store)
	}
	writer, diag, err := beads.ResolveConditionalWriter(store)
	if err != nil {
		return nil, err
	}
	if diag != nil {
		p.logf("session reconciler: work-record writes on this store are unfenced: %v\n", diag)
	}
	return writer, nil
}

// parkMailRetryEvery bounds how often an unlanded park mail is re-sent per
// bead, so a city whose mayor cannot be resolved does not log every tick.
const parkMailRetryEvery = 5 * time.Minute

// parkMailRetryState is the in-memory retry throttle and single-flight guard:
// a restart retries sooner, never later, and the durable fact
// (gc.park_mailed_at empty) is what says a mail is still owed. inFlight keeps
// the park's own async send and the tick's retry from both sending for one
// bead at the same time.
type parkMailRetryState struct {
	mu       sync.Mutex
	last     map[string]time.Time
	inFlight map[string]bool
	// sweptAt is when the demand-independent park sweep last listed the
	// stores (sweepUnmailedParks); bounded by parkMailRetryEvery.
	sweptAt time.Time
}

// sweepDue reports whether the store sweep may run at now, and claims it.
func (r *parkMailRetryState) sweepDue(now time.Time) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.sweptAt.IsZero() && now.Before(r.sweptAt.Add(parkMailRetryEvery)) {
		return false
	}
	r.sweptAt = now
	return true
}

// begin claims the single-flight slot for a bead; false when another send is
// in progress.
func (r *parkMailRetryState) begin(beadID string) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inFlight[beadID] {
		return false
	}
	if r.inFlight == nil {
		r.inFlight = make(map[string]bool)
	}
	r.inFlight[beadID] = true
	return true
}

func (r *parkMailRetryState) end(beadID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inFlight, beadID)
}

func (r *parkMailRetryState) due(beadID string, now time.Time) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	last, ok := r.last[beadID]
	return !ok || !now.Before(last.Add(parkMailRetryEvery))
}

func (r *parkMailRetryState) mark(beadID string, now time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = make(map[string]time.Time)
	}
	r.last[beadID] = now
}

// forget drops every throttle entry for a bead (its keys are
// "<bead>@<park identity>": two copies of one bead owing different parks
// throttle separately, so one copy's failed stamp never silences the other).
func (r *parkMailRetryState) forget(beadID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.last {
		if key == beadID || strings.HasPrefix(key, beadID+"@") {
			delete(r.last, key)
		}
	}
}

func (p *workStartFailurePolicy) limit(template string) int {
	if p != nil && p.limitFor != nil {
		return p.limitFor(template)
	}
	return defaultMaxStartFailures()
}

func (p *workStartFailurePolicy) logf(format string, args ...any) {
	if p == nil || p.stderr == nil {
		return
	}
	fmt.Fprintf(p.stderr, format, args...) //nolint:errcheck
}

// storeForTriggerBead resolves the trigger work bead of a session and the
// store it lives in. The session's gc.trigger_bead_store_ref names the store
// the demand side counted the bead in ("city", a bare rig name, "rig:NAME",
// or "class:TOKEN" for a relocated class binding); a ref that answers
// not-found falls back to a probe of every store by id, the work store first,
// so a re-homed bead is still found. A ref whose store FAILS to answer is an
// error, never absence: the sweep would find a migration's retained copy and
// charge (or clear) the inactive row while the active bead stays untouched.
func (p *workStartFailurePolicy) storeForTriggerBead(id, ref string) (beads.Store, beads.Bead, bool) {
	store, b, ok, err := p.findTriggerBead(id, ref)
	if err != nil {
		p.logf("session reconciler: work bead %s could not be read (%v); this start is not accounted for on it\n", strings.TrimSpace(id), err)
	}
	return store, b, ok
}

// findTriggerBead is storeForTriggerBead's lookup: found, or not found
// together with every read error (other than not-found) the probes met — a
// transient store failure is not the bead's absence, and a caller that
// would otherwise drop a charge or a reset in silence says so, or retries.
func (p *workStartFailurePolicy) findTriggerBead(id, ref string) (beads.Store, beads.Bead, bool, error) {
	id = strings.TrimSpace(id)
	if p == nil || id == "" {
		return nil, beads.Bead{}, false, nil
	}
	var errs []error
	probe := func(store beads.Store) (beads.Bead, bool) {
		b, err := store.Get(id)
		if err == nil {
			return b, true
		}
		if !errors.Is(err, beads.ErrNotFound) {
			errs = append(errs, err)
		}
		return beads.Bead{}, false
	}
	if store := p.storeByRef(ref); store != nil {
		if b, ok := probe(store); ok {
			return store, b, true, nil
		}
		if len(errs) > 0 {
			return nil, beads.Bead{}, false, fmt.Errorf("store %q named by the trigger: %w", strings.TrimSpace(ref), errors.Join(errs...))
		}
	}
	for _, store := range p.orderedStores() {
		if b, ok := probe(store); ok {
			return store, b, true, nil
		}
	}
	return nil, beads.Bead{}, false, errors.Join(errs...)
}

func (p *workStartFailurePolicy) storeByRef(ref string) beads.Store {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "", ref == "city", strings.HasPrefix(ref, "city:"):
		return p.workStore
	case storeref.IsClassRef(ref):
		// A relocated class binding: the ACTIVE copy lives in the class
		// store; a migration retains the original row in the work store, and
		// charging that retained copy (or skipping it as "routed elsewhere")
		// would leave the active bead eligible and spawning.
		if p.classStoreByRef != nil {
			if store := p.classStoreByRef(ref); store != nil {
				return store
			}
		}
		return p.workStore
	}
	name := strings.TrimSpace(strings.TrimPrefix(ref, "rig:"))
	if store, ok := p.rigStores[name]; ok && store != nil {
		return store
	}
	return nil
}

func (p *workStartFailurePolicy) orderedStores() []beads.Store {
	stores := make([]beads.Store, 0, 1+len(p.rigStores)+len(p.extraStores))
	add := func(store beads.Store) {
		if store == nil {
			return
		}
		for _, seen := range stores {
			if seen == store {
				return
			}
		}
		stores = append(stores, store)
	}
	add(p.workStore)
	names := make([]string, 0, len(p.rigStores))
	for name := range p.rigStores {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		add(p.rigStores[name])
	}
	for _, store := range p.extraStores {
		add(store)
	}
	return stores
}

// workRecordWriteAttempts bounds the fenced read-modify-write of the record.
const workRecordWriteAttempts = 3

// writeWorkRecord applies compute(bead) to the bead's current state under a
// revision fence: with a ConditionalWriter the write lands only if the bead
// is unchanged since the read (a concurrent reassign or reset wins and the
// attempt recomputes from the fresh row, up to workRecordWriteAttempts). A
// store with no conditional write — one that offers none, and one whose
// writer answers ErrConditionalWriteUnsupported (DoltliteReadStore
// implements the interface and cannot serve it) — takes ONE plain path: a
// second read immediately before the write, refused when the record or the
// route moved in between; the residual read→write window is the store's,
// documented. compute returns the patch; an empty patch writes nothing. The
// returned bead is the row the patch was computed from.
func (p *workStartFailurePolicy) writeWorkRecord(store beads.Store, id string, compute func(beads.Bead) map[string]string) (beads.Bead, map[string]string, error) {
	// The designed seam, not a bare interface probe: it follows a wrapper's
	// declared resolution target (the controller's beadPolicyStore, the typed
	// class wrappers) to the store that holds the writer, and honors the
	// city's beads.conditional_writes mode — off/unset takes the plain path,
	// require refuses (no write) rather than write unfenced.
	writer, err := p.conditionalWriter(store)
	if err != nil {
		return beads.Bead{}, nil, err
	}
	fenced := writer != nil
	// Every read is LIVE, fenced or not: a cache-served row that says "no
	// record" while the backing holds four failures computes an EMPTY patch —
	// no write, no fence, the four failures kept and the clear reported done —
	// and an unfenced re-read that cannot see a concurrent reassign or clear
	// fences nothing. liveWorkBead reads a caching store's backing (the
	// revision the fence is checked against is the backing's, where the
	// conditional writer checks it); a plain store answers as it does for Get.
	read := func() (beads.Bead, error) {
		return liveWorkBead(store, id)
	}
	var lastErr error
	for attempt := 0; attempt < workRecordWriteAttempts; attempt++ {
		bead, err := read()
		if err != nil {
			return beads.Bead{}, nil, err
		}
		patch := compute(bead)
		if len(patch) == 0 {
			return bead, nil, nil
		}
		if fenced {
			err = writer.UpdateIfMatch(id, bead.Revision, beads.UpdateOpts{Metadata: patch})
			if err == nil {
				return bead, patch, nil
			}
			if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
				// Only a proven precondition failure (the row moved under us)
				// is retried from a fresh read. Any other error — an ambiguous
				// timeout included, where the write may already have landed —
				// is reported and NOT retried: recomputing on top of a landed
				// write would charge one failure twice.
				var precondition *beads.PreconditionFailedError
				if !errors.As(err, &precondition) {
					return beads.Bead{}, nil, err
				}
				lastErr = err
				continue
			}
			// The writer cannot fence after all (the capability probe passed at
			// resolve time; the backend refused the fence now). Under
			// beads.conditional_writes = "require" that is a refusal, never a
			// fall-back: the record is not written. Otherwise this store is
			// unfenced from here on, and THIS read takes the plain path below
			// like any other.
			if p.fencedWritesRequired(store) {
				return beads.Bead{}, nil, fmt.Errorf("beads.conditional_writes=require: the fenced write of work bead %s was refused as unsupported at write time (%w); the record is not written unfenced", id, err)
			}
			fenced = false
		}
		again, err := liveWorkBead(store, id)
		if err != nil {
			return beads.Bead{}, nil, err
		}
		if !sameWorkRecord(bead, again) {
			lastErr = fmt.Errorf("work bead %s changed between read and write (no conditional write on this store)", id)
			continue
		}
		return bead, patch, writeLiveWorkRecord(store, id, patch)
	}
	return beads.Bead{}, nil, fmt.Errorf("after %d attempts: %w", workRecordWriteAttempts, lastErr)
}

// sameWorkRecord reports whether two reads of a bead agree on everything the
// record depends on: the route and every record/park key.
func sameWorkRecord(a, b beads.Bead) bool {
	if a.Revision != 0 || b.Revision != 0 {
		return a.Revision == b.Revision
	}
	if strings.TrimSpace(a.Metadata[beadmeta.RoutedToMetadataKey]) != strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) {
		return false
	}
	for _, key := range beadmeta.WorkStartFailureMetadataKeys {
		if strings.TrimSpace(a.Metadata[key]) != strings.TrimSpace(b.Metadata[key]) {
			return false
		}
	}
	return true
}

// workTrigger names the work bead a session start was PREPARED for. It is
// captured off the session's Info at prepare time and carried on the
// prepared start, never re-read at commit: a demand pass can re-point a
// session's gc.trigger_bead_id to other work while its start is in flight
// (bindPoolSessionTriggerBead), and the start that then fails or succeeds is
// still the one that ran with the first bead's environment.
type workTrigger struct {
	BeadID   string
	StoreRef string
}

func workTriggerFromInfo(info sessionpkg.Info) workTrigger {
	return workTrigger{BeadID: strings.TrimSpace(info.TriggerBeadID), StoreRef: strings.TrimSpace(info.TriggerBeadStoreRef)}
}

// workTriggerForStart is the work bead a planned start is charged to: the
// trigger the session bead carries when the start is prepared — the same
// row the start's trigger env is read from (sessionTriggerBeadEnv) and the
// row recoverRunningPendingCreate reads to clear before it confirms — so
// the bead the start RUNS for, the bead a failure charges and the bead a
// confirmation clears are one operand. The build writes that trigger before
// any start (bindNamedSessionWakeTrigger / bindPoolSessionTriggerBead) and
// leaves it alone while a start is in flight; a bind that did not land
// leaves the previous trigger, and the start then runs for and is charged to
// that. TemplateParams.TriggerBeadID mirrors the same value for the create
// and reopen paths (namedSessionTriggerMetadata) and is not read here.
func workTriggerForStart(info sessionpkg.Info) workTrigger {
	return workTriggerFromInfo(info)
}

// startDeferred re-proves the demand gate on the work bead a start is about
// to run for, read live at start time: parked, or inside its backoff, means
// the start is not made; so does a record that cannot be read (a store that
// fails to answer is not the bead's absence). A bead in no store is not
// deferred. The planning gate ran on
// a snapshot; between the plan and the start the bead's own record can move
// (another seat's failure parked it, a queued seat outlived it, a holder's
// clear-bind did not land), and a start made past a park would, on success,
// lift a park it was never planned past.
func (p *workStartFailurePolicy) startDeferred(trigger workTrigger, now time.Time) (bool, string) {
	if p == nil || strings.TrimSpace(trigger.BeadID) == "" {
		return false, ""
	}
	store, bead, ok, err := p.findTriggerBead(trigger.BeadID, trigger.StoreRef)
	if err != nil {
		// A store that fails to answer is not the bead's absence: a start
		// made past an unreadable record could, on success, clear a park
		// the record holds. Deferred until it can be read (nothing charged).
		return true, "unreadable (" + err.Error() + ")"
	}
	if !ok {
		return false, ""
	}
	// The row the verdict is read from is LIVE (a caching store's backing):
	// the plan read a snapshot, and a park written since — by another
	// seat's failure through the live write path — is invisible to a cache.
	live, err := liveWorkBead(store, bead.ID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return false, ""
		}
		return true, "unreadable (" + err.Error() + ")"
	}
	deferred, reason, _ := workStartDeferral(live.Metadata, now)
	return deferred, reason
}

// recordStartFailure charges one failed start to the trigger work bead the
// start was prepared for: the record advances, the next start backs off, and
// at the limit the bead is parked and the park mailed. Called on every
// failed-start arm of commitStartFailure (the fresh-create rollback, the
// terminal-provider arm, the kept-session resume arm). Best-effort: a store
// failure is logged, never fatal to the tick.
func (p *workStartFailurePolicy) recordStartFailure(trigger workTrigger, template string, err error, now time.Time) {
	if p == nil || err == nil {
		return
	}
	// A controller stop mid-start is not the lane failing: the pending create
	// is rolled back and retried by the next controller, and must not count.
	if errors.Is(err, context.Canceled) {
		return
	}
	store, first, ok := p.storeForTriggerBead(trigger.BeadID, trigger.StoreRef)
	if !ok {
		return
	}
	failure := workStartFailureLine(err)
	limit := p.limit(template)
	var (
		state  workStartFailureState
		parked bool
		skip   string
	)
	bead, patch, writeErr := p.writeWorkRecord(store, first.ID, func(current beads.Bead) map[string]string {
		parked, skip = false, ""
		if p.templateOf != nil {
			if routed := p.templateOf(current); routed != "" && routed != p.canonical(template) {
				skip = routed
				return nil
			}
		}
		state = readWorkStartFailureState(current.Metadata)
		var next map[string]string
		next, parked = workStartFailurePatch(state, now, failure, limit)
		return next
	})
	if writeErr != nil {
		p.logf("session reconciler: recording failed start on work bead %s: %v\n", first.ID, writeErr)
		return
	}
	if skip != "" {
		p.logf("session reconciler: failed start of %s for work bead %s not charged: the bead is now routed to %s\n", template, first.ID, skip)
		return
	}
	next := readWorkStartFailureState(withMetadataPatch(bead.Metadata, patch))
	switch {
	case parked:
		p.logf("session reconciler: PARKED work bead %s after %d consecutive failed starts on %s: %s\n", bead.ID, next.ParkFailures, template, failure)
		p.mailPark(store, bead.ID, template, true)
	case state.Parked():
		p.logf("session reconciler: failed start on parked work bead %s (%s): %s\n", bead.ID, template, failure)
	default:
		p.logf("session reconciler: failed start %d/%d on work bead %s (%s): next start not before %s: %s\n", next.Failures, limit, bead.ID, template, next.BackoffUntil.UTC().Format(time.RFC3339), failure)
	}
}

// recordStartSuccess clears the record on the work bead a start ran for once
// that start confirmed (creation_complete): a lane that starts is not backed
// off, and a stale park a successful start proves wrong is lifted with it.
// Writes nothing when the bead carries no record. Reports whether the reset
// is SETTLED — the clear landed, or there was nothing to clear (no record,
// no such bead, a bead since routed to another template) — as opposed to
// failed: a read or write error. It runs BEFORE the batch that confirms the
// start (commitStartResultTraced, recoverRunningPendingCreate), and a failed
// clear fails that commit, so the session stays pending-create and the next
// tick clears again before confirming.
func (p *workStartFailurePolicy) recordStartSuccess(trigger workTrigger, template string) bool {
	if p == nil {
		return true
	}
	store, first, ok, lookupErr := p.findTriggerBead(trigger.BeadID, trigger.StoreRef)
	if !ok {
		if lookupErr != nil {
			p.logf("session reconciler: work bead %s could not be read (%v); its start-failure record is cleared before the start is confirmed\n", strings.TrimSpace(trigger.BeadID), lookupErr)
			return false
		}
		return true
	}
	skip := ""
	bead, patch, err := p.writeWorkRecord(store, first.ID, func(current beads.Bead) map[string]string {
		skip = ""
		// The same re-route check as the failure side: a success of a session
		// started for template T says nothing about a bead since re-dispatched
		// to (and possibly parked by) another template.
		if p.templateOf != nil {
			if routed := p.templateOf(current); routed != "" && routed != p.canonical(template) {
				skip = routed
				return nil
			}
		}
		return workStartFailureClearPatch(current.Metadata)
	})
	if err != nil {
		p.logf("session reconciler: clearing start failures on work bead %s: %v\n", first.ID, err)
		return false
	}
	if skip != "" {
		p.logf("session reconciler: start of %s for work bead %s confirmed; record left alone: the bead is now routed to %s\n", template, first.ID, skip)
		return true
	}
	if len(patch) == 0 {
		return true
	}
	p.retry.forget(bead.ID)
	p.logf("session reconciler: work bead %s started; start-failure record cleared\n", bead.ID)
	return true
}

// mailPark sends the one park mail and stamps gc.park_mailed_at when it lands.
// An unlanded mail leaves the stamp empty so retryUnmailedParks sends it again
// (throttled by parkMailRetryEvery; the park's own first attempt is never
// throttled). Every send — the park's own and a retry — first re-reads the
// row under the single-flight slot and stands down if the park is already
// stamped or lifted, whichever of the two paths got there first; before any
// send it looks the park's tag ([park <gc.park_id>]) up in the mayor's mail,
// open or archived, and a mail that already landed (a restart lost the stamp)
// is stamped, never sent again; the stamp itself is fenced on the park's
// identity, so a delivery for an older park can never mark a newer one
// mailed. The one window left is the read-mail retention purge deleting a
// landed mail before a restart that lost its stamp.
func (p *workStartFailurePolicy) mailPark(store beads.Store, beadID, template string, first bool) {
	now := time.Now()
	// LIVE: a cache-served row can still say "parked, unmailed" after another
	// process lifted the park — the notice would then announce a park the
	// operator already unparked.
	current, err := liveWorkBead(store, beadID)
	if err != nil {
		p.logf("session reconciler: re-reading parked work bead %s before its mail: %v\n", beadID, err)
		return
	}
	state := readWorkStartFailureState(current.Metadata)
	if !state.Parked() || !state.ParkMailedAt.IsZero() {
		return
	}
	// The throttle and the single-flight slot are per PARK (bead + park
	// identity), not per bead id: a retained copy and the active copy of a
	// migrated bead owe different parks, and one copy's failed stamp must not
	// silence the other's mail.
	key := beadID + "@" + state.parkIdentity(beadID)
	if !first && !p.retry.due(key, now) {
		return
	}
	if !p.retry.begin(key) {
		return
	}
	defer p.retry.end(key)
	title := current.Title
	p.retry.mark(key, now)
	notice := parkedWorkNotice{
		BeadID:   beadID,
		Title:    title,
		Agent:    template,
		Failures: state.ParkFailures,
		Reason:   state.ParkReason,
		ParkedAt: state.ParkedAt,
		ParkID:   state.parkIdentity(beadID),
	}
	if p.notify == nil {
		p.logf("session reconciler: no mail route configured; park of %s not mailed\n", beadID)
		return
	}
	// A mail for THIS park that already landed (the stamp did not persist
	// before a restart, say) is acknowledged, never sent again.
	landed := false
	if p.lookup != nil {
		found, err := p.lookup(notice)
		if err != nil {
			p.logf("session reconciler: checking for an earlier park mail for %s (retried next tick): %v\n", beadID, err)
			return
		}
		landed = found
	}
	if !landed {
		if err := p.notify(notice); err != nil {
			p.logf("session reconciler: park mail for %s did not land (retried next tick): %v\n", beadID, err)
			return
		}
	} else {
		p.logf("session reconciler: park mail for %s %s already landed; stamping without a second send\n", beadID, notice.Tag())
	}
	parkID := notice.ParkID
	_, stamped, err := p.writeWorkRecord(store, beadID, func(row beads.Bead) map[string]string {
		state := readWorkStartFailureState(row.Metadata)
		// Lifted meanwhile (the three-key unpark leaves gc.park_id behind, so
		// the identity alone would still match), or a different park by now:
		// this delivery does not acknowledge it.
		if !state.Parked() || state.parkIdentity(beadID) != parkID {
			return nil
		}
		return map[string]string{beadmeta.ParkMailedAtMetadataKey: now.UTC().Format(time.RFC3339)}
	})
	if err != nil {
		p.logf("session reconciler: park mail for %s sent but the stamp did not persist: %v\n", beadID, err)
		return
	}
	if len(stamped) == 0 {
		p.logf("session reconciler: park mail for %s sent for a park that has since changed; stamp skipped\n", beadID)
		return
	}
	p.retry.forget(beadID)
}

// retryUnmailedParks re-sends the park mail for every parked bead in a tick
// snapshot whose mail has not landed. workBeads and storeRefs are
// index-aligned (the snapshot convention); the store is resolved the way a
// trigger bead's is, so a ref the snapshot spells differently still finds
// the row. Called for both the assigned-work and the open-routed snapshots:
// an open unassigned routed bead can park before any session claims it.
//
// The owed rows are picked off the snapshot here; the sends run on ONE
// background sweep, off the tick: a landed mail nudges the mayor and waits
// for the nudge to be delivered (sendMailNotifyWithWorker, up to 30s of idle
// wait each), and ten parks owed after a messaging outage would otherwise
// hold the whole tick — every unrelated session reconcile behind them — for
// minutes. The per-bead single-flight slot and the 5-minute throttle in
// mailPark keep a sweep still running when the next tick's sweep starts from
// sending twice; the park's own first send (recordStartFailure) is the same
// mailPark under the same slot.
func (p *workStartFailurePolicy) retryUnmailedParks(workBeads []beads.Bead, storeRefs []string) {
	if p == nil || len(workBeads) != len(storeRefs) {
		return
	}
	var owed []owedPark
	for i, wb := range workBeads {
		if !parkStillOwesMail(wb) {
			continue
		}
		owed = append(owed, owedPark{id: wb.ID, ref: storeRefs[i]})
	}
	p.retryOwedParks(owed)
}

// owedPark is one unmailed park handed to the background retry: the bead,
// and where it lives — the store it was FOUND in when the finder had it in
// hand (the sweep), else the demand side's store ref, resolved the way a
// trigger's is. A relocated class store's row is mailed from that store,
// never from a retained migration copy the ref would resolve to.
type owedPark struct {
	id, ref string
	store   beads.Store
}

func parkStillOwesMail(b beads.Bead) bool {
	state := readWorkStartFailureState(b.Metadata)
	return state.Parked() && state.ParkMailedAt.IsZero()
}

func (p *workStartFailurePolicy) retryOwedParks(owed []owedPark) {
	if p == nil || len(owed) == 0 {
		return
	}
	p.sweeps.Add(1)
	go func() {
		defer p.sweeps.Done()
		for _, o := range owed {
			store, current := o.store, beads.Bead{}
			if store != nil {
				live, err := liveWorkBead(store, o.id)
				if err != nil {
					p.logf("session reconciler: park-mail retry: re-reading %s in its store: %v\n", o.id, err)
					continue
				}
				current = live
			} else {
				var ok bool
				store, current, ok = p.storeForTriggerBead(o.id, o.ref)
				if !ok {
					continue
				}
			}
			template := ""
			if p.templateOf != nil {
				template = p.templateOf(current)
			}
			p.mailPark(store, current.ID, template, false)
		}
	}()
}

// sweepUnmailedParks is the delivery obligation's own discovery, independent
// of demand: every parked bead whose mail has not landed, in every store the
// policy knows (the work store, the rig stores, the relocated class stores),
// is handed to retryUnmailedParks — a bead that stopped being demand (its
// agent suspended, a dependency added, a claim elsewhere) still owes its one
// mail. The stores are listed at most every parkMailRetryEvery (the retry
// state's clock; no retry state, as in tests, sweeps on every call); a store
// that fails to list is said and skipped for this sweep.
func (p *workStartFailurePolicy) sweepUnmailedParks(now time.Time) {
	if p == nil || !p.retry.sweepDue(now) {
		return
	}
	// Off the tick: the listings (a stalled uncached store answers at its
	// read timeout) and the sends both run on the background sweep; the tick
	// only decides the sweep is due.
	p.sweeps.Add(1)
	go func() {
		defer p.sweeps.Done()
		p.retryOwedParks(p.collectUnmailedParks())
	}()
}

// collectUnmailedParks lists every store the policy knows for parked beads
// whose mail has not landed. A closed bead is not listed: its park is moot
// (nothing will start it), and so is its mail.
func (p *workStartFailurePolicy) collectUnmailedParks() []owedPark {
	var owed []owedPark
	collect := func(store beads.Store, ref string) {
		if store == nil {
			return
		}
		// Both tiers: an ephemeral (wisp-tier) work bead parks like any
		// other, and a relocated class store has no policy wrapper widening
		// the default tier for it.
		rows, err := store.List(beads.ListQuery{AllowScan: true, TierMode: beads.FederatedReadTier})
		if err != nil {
			p.logf("session reconciler: park-mail sweep: listing store %q: %v (parks there are retried next sweep)\n", ref, err)
			return
		}
		for _, row := range rows {
			if !parkStillOwesMail(row) {
				continue
			}
			// The store the park was found in rides with it: a class store's
			// active row is mailed from the class store, not from a retained
			// copy a ref would resolve to.
			owed = append(owed, owedPark{id: row.ID, ref: ref, store: store})
		}
	}
	collect(p.workStore, "city")
	names := make([]string, 0, len(p.rigStores))
	for name := range p.rigStores {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if p.rigStores[name] == p.workStore {
			continue
		}
		collect(p.rigStores[name], "rig:"+name)
	}
	for _, store := range p.extraStores {
		collect(store, "class")
	}
	return owed
}

// awaitParkMailRetries blocks until every retry sweep retryUnmailedParks has
// handed to the background is done. Tests only: the tick never waits for a
// mail to land.
func (p *workStartFailurePolicy) awaitParkMailRetries() {
	if p == nil {
		return
	}
	p.sweeps.Wait()
}

// excludeStartDeferredWork drops the assigned-work rows the pool must not plan
// a start for at now — parked, or inside the backoff window after a failed
// start — recording one trace decision per skipped row. The scale_check tier
// applies the same gate inside defaultScaleCheckCountsAndDemand.
func excludeStartDeferredWork(workBeads []beads.Bead, now time.Time, trace *sessionReconcilerTraceCycle) []beads.Bead {
	kept, _ := excludeStartDeferredWorkAligned(workBeads, nil, now, trace)
	return kept
}

// excludeStartDeferredWorkAligned is excludeStartDeferredWork over an
// index-aligned (beads, storeRefs) pair; a nil storeRefs returns nil refs.
func excludeStartDeferredWorkAligned(workBeads []beads.Bead, storeRefs []string, now time.Time, trace *sessionReconcilerTraceCycle) ([]beads.Bead, []string) {
	if len(workBeads) == 0 {
		return workBeads, storeRefs
	}
	kept := make([]beads.Bead, 0, len(workBeads))
	var keptRefs []string
	if storeRefs != nil {
		keptRefs = make([]string, 0, len(storeRefs))
	}
	for i, wb := range workBeads {
		deferred, reason, until := workStartDeferral(wb.Metadata, now)
		if !deferred {
			kept = append(kept, wb)
			if storeRefs != nil && i < len(storeRefs) {
				keptRefs = append(keptRefs, storeRefs[i])
			}
			continue
		}
		if trace != nil {
			traceReason := TraceReasonStartBackoff
			if reason == workStartDeferralParked {
				traceReason = TraceReasonParked
			}
			trace.RecordDecision(TraceSitePoolStartDeferred, traceReason, TraceOutcomeSkipped, routedToOrLegacyWorkflowTarget(wb), "", traceRecordPayload{
				"work_bead": wb.ID,
				"reason":    reason,
				"until":     until.UTC().Format(time.RFC3339),
			})
		}
	}
	return kept, keptRefs
}

// parkedWorkMailRecipient is the agent the park mail goes to: the city's
// mayor, resolved through the same recipient resolution gc mail send uses.
const parkedWorkMailRecipient = "mayor"

// parkMailDeps are the controller services the park mailer and its lookup
// use — captured when the policy is built, on the tick, where cr's config and
// provider are read everywhere — never read off cr inside the closures: the
// park's own send runs on the async commit goroutine, and a reload publishes
// cr.cfg / cr.sp under serviceStateMu in between, so a closure reading cr
// there would race the reload and could pair one config with another's
// provider.
type parkMailDeps struct {
	cfg      *config.City
	sp       runtime.Provider
	routes   *storageRoutes
	rec      events.Recorder
	cityPath string
	stderr   io.Writer
}

func (cr *CityRuntime) parkMailDeps() parkMailDeps {
	return parkMailDeps{cfg: cr.cfg, sp: cr.sp, routes: cr.storageRoutes, rec: cr.rec, cityPath: cr.cityPath, stderr: cr.stderr}
}

// workStartFailurePolicy builds the tick's policy: the record and gate side
// over the city's stores, and the park mailer (a mail to the mayor from the
// controller identity through the city mail provider, recorded as a MailSent
// event, nudged like gc mail send). Everything the closures need is captured
// here; nothing reads cr after this returns.
func (cr *CityRuntime) workStartFailurePolicy(workStore, sessStore beads.Store, rigStores map[string]beads.Store) *workStartFailurePolicy {
	if cr.parkMailRetry == nil {
		cr.parkMailRetry = &parkMailRetryState{}
	}
	deps := cr.parkMailDeps()
	cfg := deps.cfg
	return &workStartFailurePolicy{
		workStore:       workStore,
		rigStores:       rigStores,
		extraStores:     cr.storageRoutes.relocatedStores(),
		classStoreByRef: cr.storageRoutes.storeForClassRef,
		templateOf: func(b beads.Bead) string {
			return poolTemplateForWorkBead(cfg, b)
		},
		canonicalTemplate: func(template string) string {
			return normalizeAgentTemplateIdentity(cfg, template)
		},
		limitFor: func(template string) int {
			return findAgentByTemplate(cfg, template).EffectiveMaxStartFailures()
		},
		notify: deps.sendParkedWorkMail(workStore, sessStore),
		lookup: deps.findParkedWorkMail(workStore, sessStore),
		retry:  cr.parkMailRetry,
		stderr: cr.stderr,
	}
}

// findParkedWorkMail reports whether the mayor's mail already carries a
// message for this park, by its subject tag. The receipt is the message
// itself, so the scan covers everything the provider still holds — read or
// unread, open or ARCHIVED: a mail the mayor dismissed before a restart could
// stamp it is still a landed mail (the bead backend closes a message on
// archive, never deletes it; only the read-mail retention purge removes one,
// and that is the one window left). A provider with no record of archived
// mail (exec:) can only vouch for open mail; that limit is said on stderr.
func (d parkMailDeps) findParkedWorkMail(workStore, sessStore beads.Store) func(parkedWorkNotice) (bool, error) {
	return func(n parkedWorkNotice) (bool, error) {
		if workStore == nil || sessStore == nil {
			return false, errors.New("no store to read mail from")
		}
		mp := newCityMailProvider(d.routes, workStore, d.cfg, d.cityPath, d.rec)
		// The unique park tag survives a change of the mayor's mailbox.
		// An empty recipient scans every retained message, including archives.
		var all []mail.Message
		var err error
		if lister, ok := mp.(mail.ArchivedLister); ok {
			all, err = lister.AllIncludingArchived("")
		} else {
			fmt.Fprintf(d.stderr, "session reconciler: the mail provider keeps no record of archived mail; a park mail archived before its stamp persisted may be sent once more after a restart\n") //nolint:errcheck
			all, err = mp.All("")
		}
		if err != nil {
			return false, err
		}
		tag := n.Tag()
		for _, m := range all {
			if m.From == controllerMailIdentity && strings.Contains(m.Subject, tag) {
				return true, nil
			}
		}
		return false, nil
	}
}

func (d parkMailDeps) sendParkedWorkMail(workStore, sessStore beads.Store) func(parkedWorkNotice) error {
	return func(n parkedWorkNotice) error {
		if workStore == nil || sessStore == nil {
			return errors.New("no store to send mail through")
		}
		to, err := resolveMailRecipientIdentityCached(d.cityPath, d.cfg, sessStore, parkedWorkMailRecipient, nil)
		if err != nil {
			return fmt.Errorf("resolving recipient %q: %w", parkedWorkMailRecipient, err)
		}
		mp := newCityMailProvider(d.routes, workStore, d.cfg, d.cityPath, d.rec)
		m, err := mp.Send(controllerMailIdentity, to, n.Subject(), n.Body())
		if err != nil {
			return err
		}
		if d.rec != nil {
			d.rec.Record(events.Event{
				Type:    events.MailSent,
				Actor:   m.From,
				Subject: m.ID,
				Message: to,
				Payload: mailEventPayload(&m),
			})
		}
		fmt.Fprintf(d.stderr, "session reconciler: park mail %s sent to %s for work bead %s\n", m.ID, to, n.BeadID) //nolint:errcheck
		if info, resolveErr := sessionFrontDoor(sessStore).ResolveAddress(to, false); resolveErr == nil {
			target := resolveNudgeTargetFromSessionInfo(d.cityPath, d.cfg, info)
			if nudgeErr := sendMailNotifyWithWorker(target, workStore, d.sp, m.From); nudgeErr != nil {
				fmt.Fprintf(d.stderr, "session reconciler: park mail %s sent; nudging %s failed: %v\n", m.ID, to, nudgeErr) //nolint:errcheck
			}
		}
		return nil
	}
}

// poolTemplateForWorkBead is the canonical pool template a routed work bead
// belongs to: the route with any pool-slot suffix removed, then the agent's
// current qualified name (normalizeAgentTemplateIdentity), so a legacy bound
// identity left by a bound→unbound migration reads as the agent it names —
// the same normalization pool demand applies to the route.
func poolTemplateForWorkBead(cfg *config.City, b beads.Bead) string {
	return normalizeAgentTemplateIdentity(cfg, agentutil.NormalizePoolRouteTarget(cfg, routedToOrLegacyWorkflowTarget(b)))
}

// withMetadataPatch returns a fresh map: meta with patch applied. The store's
// Get result is never mutated in place.
func withMetadataPatch(meta, patch map[string]string) map[string]string {
	out := make(map[string]string, len(meta)+len(patch))
	for k, v := range meta {
		out[k] = v
	}
	for k, v := range patch {
		out[k] = v
	}
	return out
}

func defaultMaxStartFailures() int {
	return config.DefaultMaxStartFailures
}

// excludeStartDeferredWorkAlignedStores is excludeStartDeferredWorkAligned over
// the (beads, storeRefs, stores) triple the awake scan carries here: stores is
// cut by the same rows when it is index-aligned with the beads and passed
// through untouched otherwise (the convention filterReleasedAssignedWorkSnapshot
// follows).
func excludeStartDeferredWorkAlignedStores(workBeads []beads.Bead, storeRefs []string, stores []beads.Store, now time.Time, trace *sessionReconcilerTraceCycle) ([]beads.Bead, []string, []beads.Store) {
	kept, keptRefs := excludeStartDeferredWorkAligned(workBeads, storeRefs, now, trace)
	if len(stores) != len(workBeads) || len(kept) == len(workBeads) {
		return kept, keptRefs, stores
	}
	keptStores := make([]beads.Store, 0, len(kept))
	for i, wb := range workBeads {
		if deferred, _, _ := workStartDeferral(wb.Metadata, now); !deferred {
			keptStores = append(keptStores, stores[i])
		}
	}
	return kept, keptRefs, keptStores
}

// canonical maps a session's template through canonicalTemplate (identity
// when nil), the comparison partner of templateOf.
func (p *workStartFailurePolicy) canonical(template string) string {
	if p == nil || p.canonicalTemplate == nil {
		return template
	}
	return p.canonicalTemplate(template)
}

// earlierDeadline is the earlier of two deadlines where a zero time means
// "none".
func earlierDeadline(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case b.Before(a):
		return b
	default:
		return a
	}
}

// earliestStartDeferralDeadline is the earliest backoff deadline among the
// work beads deferred at now (zero when none is backed off; a park has no
// deadline): the moment the pool's demand changes with no write anywhere.
func earliestStartDeferralDeadline(workBeads []beads.Bead, now time.Time) time.Time {
	var earliest time.Time
	for _, wb := range workBeads {
		if deferred, reason, until := workStartDeferral(wb.Metadata, now); deferred && reason == workStartDeferralBackoff {
			earliest = earlierDeadline(earliest, until)
		}
	}
	return earliest
}

// liveWorkBead reads the row the store holds NOW. A caching store is read
// through its backing — a cache-served row is the same row twice, and an
// unfenced re-read that cannot see a concurrent reassign or clear fences
// nothing; wrappers are followed through their declared resolution target
// (the controller's beadPolicyStore, the typed class wrappers) to find it.
// Any other store answers as it does for Get.
func liveWorkBead(store beads.Store, id string) (beads.Bead, error) {
	if caching := cachingWorkStore(store); caching != nil {
		return caching.Backing().Get(id)
	}
	return store.Get(id)
}

// cachingWorkStore is the caching store behind a work store, found through
// the wrappers' declared resolution targets (the controller's
// beadPolicyStore, the typed class wrappers); nil when the store is not
// cached or the cache has no backing.
func cachingWorkStore(store beads.Store) *beads.CachingStore {
	inner := store
	for depth := 0; depth < 8; depth++ {
		target, ok := inner.(beads.ConditionalWritesResolveTargeter)
		if !ok {
			break
		}
		next := target.ConditionalWritesResolveTarget()
		if next == nil || next == inner {
			break
		}
		inner = next
	}
	if caching, ok := inner.(*beads.CachingStore); ok && caching.Backing() != nil {
		return caching
	}
	return nil
}

// writeLiveWorkRecord lands an unfenced patch on the row the live reads saw.
// A caching store's SetMetadataBatch skips the backing write when its CACHED
// row already carries every value in the patch (its idempotence guard) —
// true of a stale cached row, not of the backing row the patch was computed
// from, so a clear a confirmed start owes would never reach the backing
// while the controller logged it cleared. When the cached row would swallow
// the patch the backing is written directly (a no-op write when the two rows
// agree); otherwise the write goes through the cache, whose row refreshes
// with it.
func writeLiveWorkRecord(store beads.Store, id string, patch map[string]string) error {
	caching := cachingWorkStore(store)
	if caching == nil {
		return store.SetMetadataBatch(id, patch)
	}
	if cached, err := caching.Get(id); err == nil && metadataCarries(cached.Metadata, patch) {
		return caching.Backing().SetMetadataBatch(id, patch)
	}
	return caching.SetMetadataBatch(id, patch)
}

// metadataCarries reports whether meta already holds every value in patch —
// the comparison the caching store's idempotence guard makes.
func metadataCarries(meta, patch map[string]string) bool {
	for k, v := range patch {
		if meta[k] != v {
			return false
		}
	}
	return true
}
