package beads

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// This file holds BdStore's ConditionalWriter machinery that has no exit-code to
// key on: bd surfaces every failure as exit 1 with a JSON error envelope, so both
// the capability probe and the result classifier are message/body based, mirroring
// the existing isBdTransientWriteError / isBdNotFound / isBdAmbiguousWriteError
// classifiers. The *IfMatch verbs and the metadata-CAS emulation consume that
// machinery through the runConditionalWrite retry wrapper below.

// BdStore satisfies the optional ConditionalWriter capability. The assertion is
// what activates promotion through DoltliteReadStore (which embeds *BdStore), so
// the F2 loud-degrade methods on *DoltliteReadStore land in the same change — see
// doltlite_read_store.go.
var (
	_ ConditionalWriter                = (*BdStore)(nil)
	_ conditionalWritesModeCarrier     = (*BdStore)(nil)
	_ conditionalWriteCapabilityProber = (*BdStore)(nil)
)

// probeConditionalWriteCapability adapts the four-verb probe and the runtime
// unsupported latch to the seam's capability answer. The reasons demand
// different operator responses, so incapable is split three ways with the
// latch preferred: a latch means bd rejected a real fenced write at runtime;
// a probe subprocess failure means bd itself is broken or missing (fix the
// runner environment, not the bd version); a probe miss means the live bd
// never advertised the flag (upgrade bd). The probe-failure reason is read
// from the memoized condWriteProbeErr so every later resolve reports the
// same cause, not just the one that ran the probe.
func (s *BdStore) probeConditionalWriteCapability() (bool, string) {
	capable, err := s.conditionalWritesCapable()
	if capable {
		return true, ""
	}
	s.condWriteMu.Lock()
	latched := s.condWriteLatched
	if err == nil {
		err = s.condWriteProbeErr
	}
	s.condWriteMu.Unlock()
	switch {
	case latched:
		return false, "conditional writes latched unsupported at runtime (bd rejected " + conditionalWriteFlag + ")"
	case err != nil:
		return false, "capability probe failed: " + err.Error()
	default:
		return false, "bd lacks " + conditionalWriteFlag + " (four-verb capability probe)"
	}
}

// conditionalWriteProbeVerbs are the bd subcommands whose --help output must all
// advertise --if-revision for the store to be treated as CAS-capable. All four
// are probed because consumers issue update/close/assign/delete conditional
// writes and a dev bd mid-merge of the revision-CAS feature can support one verb
// but not another; a single-verb probe would report capable and then eat runtime
// refusals with the probe still showing clean.
var conditionalWriteProbeVerbs = []string{"update", "close", "assign", "delete"}

const conditionalWriteFlag = "--if-revision"

// conditionalWritesCapable reports whether the bd behind this store parses
// --if-revision on every conditional-write verb. The verdict is memoized per
// store instance (mirroring bdReadyProjectionEnabled): the probe fires lazily on
// the first conditional write, never at construction, so short-lived read-only
// CLI paths (gc hook) pay no four-subprocess tax. A probe error or any missing
// flag degrades to incapable — a fail-closed veto, never an unconditional write.
//
// The runtime unsupported latch (markConditionalWritesUnsupported) is
// authoritative over the probe in both directions of skew: a bd downgraded in
// place, or a drifted PATH, stops issuing fenced writes for the process lifetime
// rather than silently degrading them. Nothing is persisted; a restart re-probes
// the live bd, matching the "no status files — query live state" rule.
func (s *BdStore) conditionalWritesCapable() (bool, error) {
	s.condWriteMu.Lock()
	defer s.condWriteMu.Unlock()
	if s.condWriteLatched {
		return false, nil
	}
	if s.condWriteProbed {
		return s.condWriteCapable, nil
	}
	for _, verb := range conditionalWriteProbeVerbs {
		out, err := s.runner(s.dir, "bd", verb, "--help")
		if err != nil || !bytes.Contains(out, []byte(conditionalWriteFlag)) {
			s.condWriteProbed, s.condWriteCapable = true, false
			// A runner failure is memoized alongside the incapable verdict so
			// diagnostics can distinguish "bd is broken/missing" from "bd is
			// too old" for the life of the store, and returned so first-call
			// sites can surface it. The verdict itself stays fail-closed
			// either way: no unconditional fallback.
			s.condWriteProbeErr = err
			return false, err
		}
	}
	s.condWriteProbed, s.condWriteCapable = true, true
	return true, nil
}

// markConditionalWritesUnsupported latches this store instance incapable after a
// real conditional write returned ErrConditionalWriteUnsupported. Because the
// latch is authoritative over the probe, one machine-confirmed unsupported
// response halts every subsequent fenced write on this store — the capability
// veto — instead of letting a stale "capable" probe verdict keep issuing writes
// bd can no longer honor.
func (s *BdStore) markConditionalWritesUnsupported() {
	s.condWriteMu.Lock()
	defer s.condWriteMu.Unlock()
	s.condWriteLatched = true
}

// Machine body codes bd emits (or, per beads #4682, will emit) for conditional
// writes. The codes are provisional until #4682 lands; the //go:build integration
// conformance row against a #4682-capable bd is the authoritative guard.
const (
	bdConditionalCodePreconditionFailed = "precondition-failed"
	bdConditionalCodeUnsupported        = "conditional-write-unsupported"
)

// bdConditionalErrorBody is the machine JSON bd attaches to a failed conditional
// write. bd's error envelope is either flat ({"error","hint","schema_version",
// ...}) or wrapped ({"schema_version","data":{...}}); decodeBdConditionalBody
// handles both. The revision fields are pointers so an absent field (nil) is
// distinguishable from a legitimate zero revision.
type bdConditionalErrorBody struct {
	Error            string `json:"error"`
	Code             string `json:"code"`
	ExpectedRevision *int64 `json:"expected_revision"`
	CurrentRevision  *int64 `json:"current_revision"`
}

// hasDiscriminator reports whether the body carries a signal the classifier keys
// on — a machine code or a revision field. Bodies without one are unhelpful
// human-message-only envelopes.
func (b bdConditionalErrorBody) hasDiscriminator() bool {
	return b.Code != "" || b.ExpectedRevision != nil || b.CurrentRevision != nil
}

// parseBdConditionalErrorBody recovers bd's structured error body from the
// command stdout or, when bd wrote the JSON envelope to stderr (which
// classifyBDExecResult folds into err.Error()), from the error string. bd splits
// streams inconsistently (bdStdoutErrorDetail embeds only the human "error" text
// into err.Error(), while the machine fields ride whichever stream carried the
// JSON), so both sources are scanned and the first object carrying a real
// discriminator (code / revision fields) wins over incidental message-only or
// log envelopes. ok is false when no JSON object is recoverable from either
// source.
func parseBdConditionalErrorBody(out []byte, err error) (bdConditionalErrorBody, bool) {
	sources := [][]byte{out}
	if err != nil {
		sources = append(sources, []byte(err.Error()))
	}
	var (
		fallback bdConditionalErrorBody
		haveAny  bool
	)
	for _, src := range sources {
		for _, body := range decodeBdConditionalBodies(src) {
			if body.hasDiscriminator() {
				return body, true
			}
			if !haveAny {
				fallback, haveAny = body, true
			}
		}
	}
	return fallback, haveAny
}

// decodeBdConditionalBodies scans src for every JSON object it contains, in
// order, unwrapping the {"data":{...}} envelope form. It is deliberately more
// tolerant than extractJSON, which stops at the first '{' OR '[': bd prefixes and
// interleaves its error envelope with log lines that are either bracketed
// ("[WARN] dolt reconnect") or JSON ({"level":"info",...}), and either would hide
// a coded precondition body from a single first-brace parse. Each candidate is
// decoded with json.Decoder so trailing bytes after one object don't reject it;
// callers pick the object carrying a discriminator.
func decodeBdConditionalBodies(src []byte) []bdConditionalErrorBody {
	var bodies []bdConditionalErrorBody
	for i := 0; i < len(src); {
		brace := bytes.IndexByte(src[i:], '{')
		if brace < 0 {
			break
		}
		i += brace
		dec := json.NewDecoder(bytes.NewReader(src[i:]))
		var env struct {
			Data *bdConditionalErrorBody `json:"data"`
			bdConditionalErrorBody
		}
		if dec.Decode(&env) != nil {
			i++ // not a valid object at this '{'; step past it and keep scanning
			continue
		}
		if env.Data != nil {
			bodies = append(bodies, *env.Data)
		} else {
			bodies = append(bodies, env.bdConditionalErrorBody)
		}
		i += int(dec.InputOffset())
	}
	return bodies
}

// classifyConditionalWriteResult maps a bd conditional-write invocation's result
// to the typed ConditionalWriter error surface. It is pure over exactly what the
// runner returns — (out, err) — and message/body based, not exit-code based:
// BdStore has no exit-code path and bd exits 1 for every error while writing a
// JSON envelope, so the "exit 9 / exit 13" split in the design doc is a misnomer
// for this codebase. The signals here are the machine body code and message
// substrings.
//
// The mapping, in priority order:
//   - nil on success.
//   - A machine body code is the AUTHORITATIVE discriminator when present: the
//     precondition code yields *PreconditionFailedError, the unsupported code
//     yields the latching ErrConditionalWriteUnsupported. A code never coexists
//     with a different class, so informational revision fields on, say, a
//     close-authority refusal cannot be misread as a precondition.
//   - The unknown-flag usage error a pre-#4682 bd emits for --if-revision yields
//     ErrConditionalWriteUnsupported (the interim probe-miss signal). It is
//     ANCHORED to "unknown flag: --if-revision" so a capable bd's usage echo —
//     which merely lists the flag — can never latch the store incapable.
//   - The ambiguous connection class outranks any remaining gate-refusal or
//     field-based precondition guess: the write MAY have committed, so it is
//     surfaced as-is for the caller's self-win contract rather than reported as a
//     definitive did-not-commit.
//   - bd's not-found phrasings map to ErrNotFound so delete/close stay idempotent.
//   - Any other machine code is a per-write *GateRefusalError (never latches).
//   - A code-less body with revision fields or a precondition message is a
//     defensive precondition (bd omitted the code); everything else surfaces as-is.
//
// The precondition ID and Expected are finalized by the calling verb wrapper:
// Expected is always the caller's own snapshot argument, and Raw preserves the
// backend body when the two disagree. bd #4682 is unlanded, so the
// precondition/unsupported substrings are provisional; the //go:build integration
// conformance row (S2-T12) is the authoritative guard.
func classifyConditionalWriteResult(out []byte, err error) error {
	if err == nil {
		return nil
	}
	body, bodyOK := parseBdConditionalErrorBody(out, err)
	msg := err.Error()

	// A recognized machine body code is the authoritative discriminator: it
	// dominates the revision fields AND the message heuristics below. A present
	// body also means bd actually answered (it did not drop mid-write), so a code
	// legitimately outranks the ambiguous-connection class too.
	if bodyOK {
		switch body.Code {
		case bdConditionalCodePreconditionFailed:
			return newPreconditionFailed(body, out, err)
		case bdConditionalCodeUnsupported:
			return ErrConditionalWriteUnsupported
		}
	}

	// Interim unsupported signal: a pre-#4682 bd rejects --if-revision as an
	// unknown flag. Anchored so a capable bd's usage echo cannot latch it.
	if isBdUnknownIfRevisionFlag(msg) {
		return ErrConditionalWriteUnsupported
	}

	// Ambiguous connection class: the write MAY have committed. This outranks the
	// message-based gate/not-found/precondition heuristics below (all of which
	// would wrongly tell the caller definitively what happened), but not a
	// recognized machine code above, which proves bd answered.
	if isBdAmbiguousWriteError(err) {
		return err
	}

	// Any other machine code is a per-write policy gate refusal (never latches).
	// It precedes the message not-found heuristic so a refusal whose human text
	// merely contains "not found" (e.g. "lease not found for holder") is not
	// silently swallowed into idempotent success.
	if bodyOK && body.Code != "" {
		return &GateRefusalError{Code: body.Code, Raw: conditionalRawDetail(out, err)}
	}

	// Code-less not-found stays idempotent for delete/close callers.
	if isBdNotFound(err) {
		return ErrNotFound
	}

	// Code-less defensive precondition: revision fields present, or bd emitted
	// only a human precondition message.
	if isBdConditionalPrecondition(body, msg) {
		return newPreconditionFailed(body, out, err)
	}

	return err
}

// newPreconditionFailed builds a *PreconditionFailedError from a classified body,
// filling Expected/Current when the backend supplied them (zero otherwise) and
// always preserving the raw body for forensics.
func newPreconditionFailed(body bdConditionalErrorBody, out []byte, err error) *PreconditionFailedError {
	pfe := &PreconditionFailedError{Raw: conditionalRawDetail(out, err)}
	if body.ExpectedRevision != nil {
		pfe.Expected = *body.ExpectedRevision
	}
	if body.CurrentRevision != nil {
		pfe.Current = *body.CurrentRevision
	}
	return pfe
}

// isBdConditionalPrecondition reports whether a code-less failure is nonetheless a
// revision-precondition mismatch, inferred from revision fields or a precondition
// message (both hyphenated and spaced forms bd might use).
func isBdConditionalPrecondition(body bdConditionalErrorBody, msg string) bool {
	if body.ExpectedRevision != nil || body.CurrentRevision != nil {
		return true
	}
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "precondition failed") ||
		strings.Contains(lower, "precondition-failed") ||
		strings.Contains(lower, "revision mismatch")
}

// isBdUnknownIfRevisionFlag matches the usage error a bd without revision-CAS
// support emits for --if-revision. It is ANCHORED to the flag name immediately
// following the parser's "unknown flag" / "not defined" marker: a cobra usage
// echo lists --if-revision in its flags block on ANY flag error, so a floating
// "contains if-revision" check would latch a CAPABLE bd the moment gascity passed
// some unrelated unknown flag — the exact silent-degrade the latch must avoid.
func isBdUnknownIfRevisionFlag(msg string) bool {
	lower := strings.ToLower(msg)
	for _, anchor := range []string{
		"unknown flag: --if-revision",
		"unknown flag: -if-revision",
		"unknown flag '--if-revision'",
		"flag provided but not defined: -if-revision",
		"flag provided but not defined: --if-revision",
	} {
		if strings.Contains(lower, anchor) {
			return true
		}
	}
	return false
}

// conditionalRawDetail returns a bounded forensic snapshot of a failed
// conditional write, preferring the command output and falling back to the error
// string.
func conditionalRawDetail(out []byte, err error) string {
	if len(bytes.TrimSpace(out)) > 0 {
		return truncateRawOutput(out, 512)
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// Retry/emulation tuning for the fenced-write path.
const (
	// conditionalWriteMaxAttempts bounds the serialization-class retry inside
	// runConditionalWrite (mirrors bdTransientWriteAttempts). Ambiguous and
	// precondition results are never retried here, so this only caps
	// definitely-rolled-back serialization conflicts.
	conditionalWriteMaxAttempts = 3
	// casEmulationMaxAttempts bounds the metadata-CAS emulation loop's
	// precondition retries under cross-key revision interference (DESIGN §8.4).
	casEmulationMaxAttempts = 4
	// casEmulationBaseBackoff is the first backoff step; it doubles per attempt
	// and is jittered (DESIGN §8.4). Shared by the serialization retry.
	casEmulationBaseBackoff = 25 * time.Millisecond
)

// conditionalWriteSleep is the backoff seam for both the serialization retry in
// runConditionalWrite and the metadata-CAS emulation loop. Tests override it to a
// no-op so contention/exhaustion cases don't actually sleep 25→200ms per racer.
// It is a package-level var, so a test that reassigns it must not run in parallel
// with the fenced-write path and must restore it via t.Cleanup.
var conditionalWriteSleep = func(d time.Duration) { time.Sleep(d) }

// conditionalWriteBackoff returns the jittered backoff for the attempt-th retry:
// casEmulationBaseBackoff doubled per attempt, then equal-jittered to spread
// concurrent racers. math/rand is fine in production (banned only in Workflow
// scripts); its global source is safe for concurrent use.
func conditionalWriteBackoff(attempt int) time.Duration {
	base := casEmulationBaseBackoff << (attempt - 1)
	return base/2 + time.Duration(rand.Int63n(int64(base)/2+1))
}

// UpdateIfMatch applies opts to id only if the bead's revision still equals
// expectedRevision, via bd update --if-revision. An empty opts is invalid
// input (ErrEmptyConditionalUpdate) on every ConditionalWriter — bd cannot
// even express an empty fenced update, so a silent nil here would diverge
// from the stores that can. If this store cannot fence, it returns
// ErrConditionalWriteUnsupported rather than falling through to an
// unconditional write.
func (s *BdStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	if isEmptyUpdateOpts(opts) {
		return fmt.Errorf("conditional update %s: %w", id, ErrEmptyConditionalUpdate)
	}
	if capable, _ := s.conditionalWritesCapable(); !capable {
		return ErrConditionalWriteUnsupported
	}
	args := bdUpdateArgs(id, opts)
	if len(args) == 3 {
		return nil
	}
	args = append(args, conditionalWriteFlag, strconv.FormatInt(expectedRevision, 10))
	return s.runConditionalWrite(id, expectedRevision, args...)
}

// CloseIfMatch closes id only if its revision still equals expectedRevision. It
// deliberately does NOT port the unconditional close's import-revert re-read
// honesty guard: for a fenced write a precondition or gate result must surface,
// not be masked by a status re-read.
func (s *BdStore) CloseIfMatch(id string, expectedRevision int64) error {
	if capable, _ := s.conditionalWritesCapable(); !capable {
		return ErrConditionalWriteUnsupported
	}
	args := append(bdCloseArgs("", id), conditionalWriteFlag, strconv.FormatInt(expectedRevision, 10))
	return s.runConditionalWrite(id, expectedRevision, args...)
}

// DeleteIfMatch deletes id only if its revision still equals expectedRevision.
func (s *BdStore) DeleteIfMatch(id string, expectedRevision int64) error {
	if capable, _ := s.conditionalWritesCapable(); !capable {
		return ErrConditionalWriteUnsupported
	}
	args := []string{"delete", "--force", "--json", id, conditionalWriteFlag, strconv.FormatInt(expectedRevision, 10)}
	return s.runConditionalWrite(id, expectedRevision, args...)
}

// runConditionalWrite runs a single fenced bd write (…--if-revision N) and
// returns the classified, caller-finalized error. It is the dedicated retry
// wrapper for conditional writes and MUST NOT route through
// runBDTransientWrite: replaying a stale fence after a maybe-committed write is
// wrong, and blind-retrying a precondition converts a signal into a spin.
//
// Retry policy (DESIGN §8.2, validated by the Phase-5 design pass):
//   - success → nil.
//   - AMBIGUOUS connection error (isBdAmbiguousWriteError: i/o timeout, reset,
//     broken pipe, …) → surfaced as-is, NEVER retried: the write may have
//     committed, so the caller's self-win re-read decides. This branch MUST
//     precede the serialization branch because isBdTransientWriteError is a
//     superset of isBdAmbiguousWriteError — retrying a maybe-committed write
//     would misreport a landed write as a precondition.
//   - SERIALIZATION-class transient (transient AND not ambiguous: the txn rolled
//     back) → re-read the revision; if it moved, the fence is permanently stale
//     (revisions are monotonic and never reused) so return a precondition
//     immediately rather than replaying a doomed fence; otherwise back off and
//     retry the SAME argv with the SAME expectedRevision. Re-fencing with a
//     freshly-read revision would silently downgrade CAS to last-writer-wins.
//   - everything else → classified as-is.
//
// The doltlite --dolt-auto-commit prefix is applied once via bdTransientWriteArgs
// so a doltlite backend still gets it; the argv is not re-prefixed per attempt.
func (s *BdStore) runConditionalWrite(id string, expectedRevision int64, args ...string) error {
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}
	prefixed := s.bdTransientWriteArgs(args)
	var (
		lastOut []byte
		lastErr error
	)
	for attempt := 1; ; attempt++ {
		out, err := s.runner(s.dir, "bd", prefixed...)
		if err == nil {
			return nil
		}
		lastOut, lastErr = out, err
		if isBdAmbiguousWriteError(err) {
			break
		}
		if isBdTransientWriteError(err) && attempt < conditionalWriteMaxAttempts {
			if cur, getErr := s.Get(id); getErr == nil && cur.Revision != expectedRevision {
				return s.finalizeConditionalWrite(id, verb, expectedRevision,
					&PreconditionFailedError{Current: cur.Revision})
			}
			conditionalWriteSleep(conditionalWriteBackoff(attempt))
			continue
		}
		break
	}
	return s.finalizeConditionalWrite(id, verb, expectedRevision, classifyConditionalWriteResult(lastOut, lastErr))
}

// finalizeConditionalWrite stamps the caller-owned fields onto a classified
// conditional-write error and latches the store on a machine-confirmed
// unsupported response. Centralizing this here (the only frame that reliably
// holds both id and expectedRevision) keeps the per-verb wrappers thin and stops
// the ID/Expected pairing from drifting across them.
//   - unsupported → latch (authoritative over the probe) and surface.
//   - precondition → override Expected with the caller's own argument (the
//     conformance harness asserts this unconditionally) and fill ID when the
//     backend left it empty; Current is left to the classifier / re-read.
//   - gate refusal → fill ID/Verb for forensics; never latches.
func (s *BdStore) finalizeConditionalWrite(id, verb string, expectedRevision int64, err error) error {
	if err == nil {
		return nil
	}
	if IsConditionalWriteUnsupported(err) {
		s.markConditionalWritesUnsupported()
		return err
	}
	var pfe *PreconditionFailedError
	if errors.As(err, &pfe) {
		if pfe.ID == "" {
			pfe.ID = id
		}
		pfe.Expected = expectedRevision
		return err
	}
	var gre *GateRefusalError
	if errors.As(err, &gre) {
		if gre.ID == "" {
			gre.ID = id
		}
		if gre.Verb == "" {
			gre.Verb = verb
		}
		return err
	}
	return err
}

// CompareAndSetMetadataKey atomically sets metadata[key]=next iff the current
// value equals expected, emulated over bd's revision fence (bd has no value-CAS
// primitive). The loop is bounded because control/member beads are metadata-hot:
// an unrelated-key write between the read and the fenced write bumps the revision
// and produces a spurious precondition even though nobody touched our key.
// Exhaustion returns *CASRetriesExhaustedError — a transient, distinct from a
// genuine value loss ((false,nil)) or a precondition — so consumers re-enter
// level-triggered instead of stranding a reservation (DESIGN §8.4).
func (s *BdStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	if capable, _ := s.conditionalWritesCapable(); !capable {
		return false, ErrConditionalWriteUnsupported
	}
	for attempt := 1; ; attempt++ {
		b, err := s.Get(id)
		if err != nil {
			return false, err
		}
		// "" ≡ absent: an absent key reads back as "" from the map, so an empty
		// expected legitimately claims an absent or empty-valued key and only
		// those.
		if b.Metadata[key] != expected {
			return false, nil
		}
		// Build the fenced set through bdUpdateArgs so the metadata write carries
		// the same --json envelope (and future flag handling) as the *IfMatch
		// verbs; the classifier is body-based and a plain-text error body would
		// misclassify a precondition into the surface-as-is default.
		args := append(bdUpdateArgs(id, UpdateOpts{Metadata: map[string]string{key: next}}),
			conditionalWriteFlag, strconv.FormatInt(b.Revision, 10))
		err = s.runConditionalWrite(id, b.Revision, args...)
		switch {
		case err == nil:
			return true, nil
		case IsPreconditionFailed(err):
			// The revision moved under us; re-read and re-check the value next
			// lap. On the final lap, one more read distinguishes the outcomes
			// before the exhaustion verdict: the interfering write may have
			// been a competitor landing on OUR key, which is a genuine value
			// loss ((false, nil)) — misreporting it as exhaustion would make
			// the caller re-enter a race it already definitively lost.
			if attempt >= casEmulationMaxAttempts {
				if final, readErr := s.Get(id); readErr == nil && final.Metadata[key] != expected {
					return false, nil
				}
				return false, &CASRetriesExhaustedError{
					ID: id, Key: key, Attempts: attempt, LastRevision: b.Revision,
				}
			}
			conditionalWriteSleep(conditionalWriteBackoff(attempt))
		default:
			// Unsupported (already latched in finalizeConditionalWrite), gate
			// refusal, or an ambiguous maybe-committed write: surface as-is. An
			// ambiguous write is deliberately NOT re-checked as a value loss —
			// doing so would report (false,nil) after the write may have landed.
			return false, err
		}
	}
}
