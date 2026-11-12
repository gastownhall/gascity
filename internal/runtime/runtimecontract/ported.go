package runtimecontract

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file holds the RunProviderTests groups ported to the wire: the
// error-bearing attachment answer (two observe-group requirements), the
// metadata, discovery and process groups, and the concurrency group. Each
// probe is self-contained (it starts and stops its own sessions), an op that
// answers exit 2 SKIPs unless a declared token makes the op mandatory, and a
// present-but-wrong op FAILs.

// concurrentSessions is how many distinct names a concurrency probe drives
// at once, as RunProviderTests' *_ConcurrentDistinct* cases do.
const concurrentSessions = 3

// Metadata keys the meta group writes. They are not identity keys, so no
// observe.v1 promise reads them.
const (
	metaKeyRoundTrip = "gc-rpp-conf-roundtrip"
	metaKeyUnset     = "gc-rpp-conf-never-set"
	metaKeyRemove    = "gc-rpp-conf-remove-me"
	metaKeyEmpty     = "gc-rpp-conf-empty-me"
	metaKeyOverwrite = "gc-rpp-conf-overwrite"
	metaKeyFirst     = "gc-rpp-conf-key1"
	metaKeySecond    = "gc-rpp-conf-key2"
)

// --- Attachment (RPP-OBSERVE-017..018) ---

// attachmentDeclared gates the error-bearing attachment requirements.
func attachmentDeclared(hs runtime.ProtocolInfo) (Status, string, bool) {
	if !hs.Has(runtime.ProtocolCapabilityReportAttachment) {
		return StatusSkip, "report-attachment not declared", false
	}
	return StatusPass, "", true
}

// expectDetached runs is-attached for name and requires exactly "false",
// exit 0: the exec provider reads any failure, exit 2 included once
// report-attachment is declared, as could-not-tell.
func expectDetached(ctx context.Context, r *runner, name, what string) (Status, string) {
	res := r.op(ctx, "is-attached", name)
	switch {
	case res.unsupported:
		return StatusFail, fmt.Sprintf("is-attached on %s returned exit 2 although report-attachment is declared", what)
	case res.err != nil:
		return StatusFail, fmt.Sprintf("is-attached on %s failed (gc reads it as could-not-tell, never detached): %v", what, res.err)
	}
	if got := strings.TrimSpace(res.stdout); got != "false" {
		return StatusFail, fmt.Sprintf("is-attached on %s = %q, want \"false\"", what, got)
	}
	return StatusPass, fmt.Sprintf("is-attached on %s = false", what)
}

func probeObserveAttachedUnattached(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := attachmentDeclared(hs); !ok {
		return s, d
	}
	name := r.nextName()
	if s, d := requireStart(ctx, r, name); s != StatusPass {
		return s, d
	}
	defer r.stop(ctx, name)
	return expectDetached(ctx, r, name, "a running box with no client")
}

func probeObserveAttachedMissing(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := attachmentDeclared(hs); !ok {
		return s, d
	}
	return expectDetached(ctx, r, r.nextName(), "a name no box holds")
}

// --- Metadata (RPP-META-001..006) ---

// metaUnsupported is the error of a metadata op that answered exit 2.
type metaUnsupported struct{ op string }

func (e metaUnsupported) Error() string { return e.op + " returned exit 2" }

// metaSkipOrFail is the meta group's verdict on an op that answered exit
// 2: SKIP, unless observe.v1 is declared, whose rule 3 makes it a failure.
func metaSkipOrFail(hs runtime.ProtocolInfo, e metaUnsupported) (Status, string) {
	if hs.Has(runtime.ProtocolCapabilityObserveV1) {
		return StatusFail, e.Error() + " although observe.v1 is declared (a metadata op's exit 2 is a missing store)"
	}
	return StatusSkip, e.Error() + " — metadata ops not implemented"
}

// setMeta stores value under key for name.
func (r *runner) setMeta(ctx context.Context, name, key, value string) error {
	res := r.opTimeout(ctx, r.opts.OpTimeout, []byte(value), "set-meta", name, key)
	switch {
	case res.unsupported:
		return metaUnsupported{op: "set-meta"}
	case res.err != nil:
		return res.err
	}
	return nil
}

// getMeta reads key for name.
func (r *runner) getMeta(ctx context.Context, name, key string) (string, error) {
	res := r.op(ctx, "get-meta", name, key)
	switch {
	case res.unsupported:
		return "", metaUnsupported{op: "get-meta"}
	case res.err != nil:
		return "", res.err
	}
	return res.stdout, nil
}

// removeMeta removes key for name.
func (r *runner) removeMeta(ctx context.Context, name, key string) error {
	res := r.op(ctx, "remove-meta", name, key)
	switch {
	case res.unsupported:
		return metaUnsupported{op: "remove-meta"}
	case res.err != nil:
		return res.err
	}
	return nil
}

// metaStep is one metadata op in a meta probe's script: a set (value
// non-nil), a remove (remove set), or a get that must answer want.
type metaStep struct {
	key    string
	set    *string
	remove bool
	want   string
}

func setStep(key, value string) metaStep { return metaStep{key: key, set: &value} }
func getStep(key, want string) metaStep  { return metaStep{key: key, want: want} }
func removeStep(key string) metaStep     { return metaStep{key: key, remove: true} }

// metaProbe starts a session and runs steps against it in order. It asks
// get-meta first, on a name no box holds, so a script without metadata ops
// SKIPs (or FAILs under observe.v1) before its start is judged.
func metaProbe(steps ...metaStep) probe {
	return func(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
		if r.op(ctx, "get-meta", r.nextName(), metaKeyUnset).unsupported {
			return metaSkipOrFail(hs, metaUnsupported{op: "get-meta"})
		}
		name := r.nextName()
		if s, d := requireStart(ctx, r, name); s != StatusPass {
			return s, d
		}
		defer r.stop(ctx, name)
		for _, step := range steps {
			var err error
			switch {
			case step.set != nil:
				err = r.setMeta(ctx, name, step.key, *step.set)
			case step.remove:
				err = r.removeMeta(ctx, name, step.key)
			default:
				var got string
				got, err = r.getMeta(ctx, name, step.key)
				if err == nil && got != step.want {
					return StatusFail, fmt.Sprintf("get-meta %s = %q, want %q", step.key, got, step.want)
				}
			}
			var unsupported metaUnsupported
			if errors.As(err, &unsupported) {
				return metaSkipOrFail(hs, unsupported)
			}
			if err != nil {
				return StatusFail, err.Error()
			}
		}
		return StatusPass, "metadata answers as stored"
	}
}

var (
	probeMetaRoundTrip = metaProbe(
		setStep(metaKeyRoundTrip, "test-value"),
		getStep(metaKeyRoundTrip, "test-value"),
	)
	probeMetaUnsetKeyEmpty = metaProbe(
		getStep(metaKeyUnset, ""),
	)
	probeMetaRemoveThenEmpty = metaProbe(
		setStep(metaKeyRemove, "value"),
		removeStep(metaKeyRemove),
		getStep(metaKeyRemove, ""),
	)
	probeMetaEmptyValueOverwrites = metaProbe(
		setStep(metaKeyEmpty, "prior-value"),
		setStep(metaKeyEmpty, ""),
		getStep(metaKeyEmpty, ""),
	)
	probeMetaOverwritesPrevious = metaProbe(
		setStep(metaKeyOverwrite, "v1"),
		setStep(metaKeyOverwrite, "v2"),
		getStep(metaKeyOverwrite, "v2"),
	)
	probeMetaMultipleKeys = metaProbe(
		setStep(metaKeyFirst, "val1"),
		setStep(metaKeySecond, "val2"),
		getStep(metaKeyFirst, "val1"),
		getStep(metaKeySecond, "val2"),
	)
)

// --- Discovery (RPP-DISCOVERY-001..005) ---

// listRunning runs list-running prefix under env and returns its names.
func (r *runner) listRunning(ctx context.Context, env []string, prefix string) ([]string, outcome) {
	res := r.opEnv(ctx, r.opts.OpTimeout, env, nil, "list-running", prefix)
	if !res.ok() || res.stdout == "" {
		return nil, res
	}
	return strings.Split(res.stdout, "\n"), res
}

// listed reports whether names holds name.
func listed(names []string, name string) bool {
	for _, n := range names {
		if strings.TrimSpace(n) == name {
			return true
		}
	}
	return false
}

// listOrVerdict runs list-running prefix and returns the names, or the
// verdict when the op is absent (SKIP) or failed (FAIL).
func listOrVerdict(ctx context.Context, r *runner, prefix string) ([]string, Status, string, bool) {
	names, res := r.listRunning(ctx, nil, prefix)
	switch {
	case res.unsupported:
		return nil, StatusSkip, "list-running not implemented (exit 2)", false
	case res.err != nil:
		return nil, StatusFail, "list-running failed: " + res.err.Error(), false
	}
	return names, StatusPass, "", true
}

// listRunningPresent SKIPs a script whose list-running answers exit 2,
// asked about a prefix no box holds, before any session is started.
func listRunningPresent(ctx context.Context, r *runner) (Status, string, bool) {
	if _, res := r.listRunning(ctx, nil, r.nextName()); res.unsupported {
		return StatusSkip, "list-running not implemented (exit 2)", false
	}
	return StatusPass, "", true
}

// processAlivePresent SKIPs a script whose process-alive answers exit 2,
// asked about a name no box holds, before any session is started.
func processAlivePresent(ctx context.Context, r *runner) (Status, string, bool) {
	if r.processAlive(ctx, r.nextName(), r.agentProcess()).unsupported {
		return StatusSkip, "process-alive not implemented (exit 2)", false
	}
	return StatusPass, "", true
}

// startAll starts every name in order and returns a failing status if one
// cannot start. The caller stops them.
func startAll(ctx context.Context, r *runner, names []string) (Status, string) {
	for _, name := range names {
		if s, d := requireStart(ctx, r, name); s != StatusPass {
			return s, d
		}
	}
	return StatusPass, ""
}

// stopAll stops every name, best effort.
func stopAll(ctx context.Context, r *runner, names []string) {
	for _, name := range names {
		r.stop(ctx, name)
	}
}

// freshNames returns n fresh session names.
func (r *runner) freshNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = r.nextName()
	}
	return names
}

func probeDiscoveryFindsSessions(ctx context.Context, r *runner, _ runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := listRunningPresent(ctx, r); !ok {
		return s, d
	}
	names := r.freshNames(2)
	defer stopAll(ctx, r, names)
	if s, d := startAll(ctx, r, names); s != StatusPass {
		return s, d
	}
	got, s, d, ok := listOrVerdict(ctx, r, "")
	if !ok {
		return s, d
	}
	for _, name := range names {
		if !listed(got, name) {
			return StatusFail, fmt.Sprintf("list-running \"\" = %v, missing running session %q", got, name)
		}
	}
	return StatusPass, "list-running lists every running session"
}

func probeDiscoveryPrefixFiltering(ctx context.Context, r *runner, _ runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := listRunningPresent(ctx, r); !ok {
		return s, d
	}
	names := r.freshNames(2)
	defer stopAll(ctx, r, names)
	if s, d := startAll(ctx, r, names); s != StatusPass {
		return s, d
	}
	got, s, d, ok := listOrVerdict(ctx, r, names[0])
	if !ok {
		return s, d
	}
	switch {
	case !listed(got, names[0]):
		return StatusFail, fmt.Sprintf("list-running %q = %v, missing %q", names[0], got, names[0])
	case listed(got, names[1]):
		return StatusFail, fmt.Sprintf("list-running %q = %v, lists %q, which the prefix excludes", names[0], got, names[1])
	}
	return StatusPass, "list-running filters by prefix"
}

func probeDiscoveryExcludesStopped(ctx context.Context, r *runner, _ runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := listRunningPresent(ctx, r); !ok {
		return s, d
	}
	name := r.nextName()
	if s, d := requireStart(ctx, r, name); s != StatusPass {
		return s, d
	}
	if stop := r.stop(ctx, name); !stop.ok() {
		return StatusFail, fmt.Sprintf("stop failed: %v (exit 2: %v)", stop.err, stop.unsupported)
	}
	got, s, d, ok := listOrVerdict(ctx, r, name)
	if !ok {
		return s, d
	}
	if listed(got, name) {
		return StatusFail, fmt.Sprintf("list-running %q after stop = %v, still lists the stopped session", name, got)
	}
	return StatusPass, "list-running omits a stopped session"
}

func probeDiscoveryEmptyPrefix(ctx context.Context, r *runner, _ runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := listRunningPresent(ctx, r); !ok {
		return s, d
	}
	name := r.nextName()
	if s, d := requireStart(ctx, r, name); s != StatusPass {
		return s, d
	}
	defer r.stop(ctx, name)
	got, s, d, ok := listOrVerdict(ctx, r, "")
	if !ok {
		return s, d
	}
	if !listed(got, name) {
		return StatusFail, fmt.Sprintf("list-running \"\" = %v, missing %q", got, name)
	}
	return StatusPass, "list-running \"\" lists a running session"
}

// probeDiscoveryFailureIsNotAbsence gates ListRunning's error-vs-absence
// rule: a caller that frees identifiers on absence (pending-create rollback,
// orphan cleanup) acts destructively on a live session if a failed listing
// reads as an empty one. Under the conformance fault the backend cannot be
// read, so list-running must fail. Exit 2 is no failure: the exec provider
// reads it as an empty listing.
func probeDiscoveryFailureIsNotAbsence(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if !hs.Has(observeFaultsToken) {
		return StatusSkip, observeFaultsToken + " not declared"
	}
	if s, d, ok := listRunningPresent(ctx, r); !ok {
		return s, d
	}
	name := r.nextName()
	if s, d := requireStart(ctx, r, name); s != StatusPass {
		return s, d
	}
	defer r.stop(ctx, name)
	if _, s, d, ok := listOrVerdict(ctx, r, name); !ok {
		return s, d
	}
	got, res := r.listRunning(ctx, []string{"GC_RPP_FAULT=unavailable"}, name)
	switch {
	case res.unsupported:
		return StatusFail, "list-running under a backend fault returned exit 2, which gc reads as an empty listing"
	case res.ok() && !listed(got, name):
		return StatusFail, fmt.Sprintf("list-running %q under a backend fault answered %v, exit 0: a failed listing reads the running session absent", name, got)
	case res.ok():
		return StatusFail, fmt.Sprintf("list-running %q under a backend fault answered %v, exit 0: the listing did not read the backend", name, got)
	}
	return StatusPass, "a failed listing is a failure, never an absent session"
}

// --- Process (RPP-PROCESS-001..002) ---

// processAlive runs process-alive for name with processNames on stdin.
func (r *runner) processAlive(ctx context.Context, name string, processNames ...string) outcome {
	stdin := []byte{}
	if len(processNames) > 0 {
		stdin = []byte(strings.Join(processNames, "\n") + "\n")
	}
	return r.opTimeout(ctx, r.opts.OpTimeout, stdin, "process-alive", name)
}

func probeProcessEmptyNamesAlive(ctx context.Context, r *runner, _ runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := processAlivePresent(ctx, r); !ok {
		return s, d
	}
	name := r.nextName()
	if s, d := requireStart(ctx, r, name); s != StatusPass {
		return s, d
	}
	defer r.stop(ctx, name)
	res := r.processAlive(ctx, name)
	switch {
	case res.unsupported:
		return StatusSkip, "process-alive not implemented (exit 2)"
	case res.err != nil:
		return StatusFail, "process-alive with no process names failed: " + res.err.Error()
	}
	if got := strings.TrimSpace(res.stdout); got != "true" {
		return StatusFail, fmt.Sprintf("process-alive with no process names = %q, want \"true\"", got)
	}
	return StatusPass, "no process names reads alive"
}

func probeProcessFalseAfterStop(ctx context.Context, r *runner, _ runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := processAlivePresent(ctx, r); !ok {
		return s, d
	}
	name := r.nextName()
	if s, d := requireStart(ctx, r, name); s != StatusPass {
		return s, d
	}
	if stop := r.stop(ctx, name); !stop.ok() {
		return StatusFail, fmt.Sprintf("stop failed: %v (exit 2: %v)", stop.err, stop.unsupported)
	}
	res := r.processAlive(ctx, name, r.agentProcess())
	switch {
	case res.unsupported:
		return StatusSkip, "process-alive not implemented (exit 2)"
	case res.err != nil:
		return StatusPass, "process-alive on a stopped session fails, which gc reads as dead"
	}
	if got := strings.TrimSpace(res.stdout); got != "false" {
		return StatusFail, fmt.Sprintf("process-alive %s %q after stop = %q, want \"false\" (empty and true both read alive)", name, r.agentProcess(), got)
	}
	return StatusPass, "process-alive after stop = false"
}

// --- Concurrency (RPP-CONCURRENCY-001..005) ---

// concurrently runs op once per name, all at once, and returns the
// outcomes in name order.
func concurrently(names []string, op func(name string) outcome) []outcome {
	out := make([]outcome, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = op(name)
		}()
	}
	wg.Wait()
	return out
}

// concurrencyDeclared is the concurrency group's gate.
func concurrencyDeclared(hs runtime.ProtocolInfo) (Status, string, bool) {
	if !hs.Has(runtime.ProtocolCapabilityObserveV1) {
		return StatusSkip, "observe.v1 not declared (its rule 9 is the concurrency promise)", false
	}
	return StatusPass, "", true
}

// allOK fails on the first outcome that is not a success.
func allOK(op string, names []string, outs []outcome) (Status, string, bool) {
	for i, res := range outs {
		switch {
		case res.unsupported:
			return StatusFail, fmt.Sprintf("concurrent %s %s returned exit 2", op, names[i]), false
		case res.err != nil:
			return StatusFail, fmt.Sprintf("concurrent %s %s failed: %v", op, names[i], res.err), false
		}
	}
	return StatusPass, "", true
}

func probeConcurrencyStart(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := concurrencyDeclared(hs); !ok {
		return s, d
	}
	names := r.freshNames(concurrentSessions)
	defer stopAll(ctx, r, names)
	outs := concurrently(names, func(name string) outcome { return r.start(ctx, name) })
	if s, d, ok := allOK("start", names, outs); !ok {
		return s, d
	}
	for _, name := range names {
		if s, d := expectRunning(ctx, r, name, true, "after a concurrent start"); s != StatusPass {
			return s, d
		}
	}
	return StatusPass, fmt.Sprintf("%d concurrent starts all running", len(names))
}

func probeConcurrencyStop(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := concurrencyDeclared(hs); !ok {
		return s, d
	}
	names := r.freshNames(concurrentSessions)
	defer stopAll(ctx, r, names)
	if s, d := startAll(ctx, r, names); s != StatusPass {
		return s, d
	}
	outs := concurrently(names, func(name string) outcome { return r.stop(ctx, name) })
	if s, d, ok := allOK("stop", names, outs); !ok {
		return s, d
	}
	for _, name := range names {
		if s, d := expectRunning(ctx, r, name, false, "after a concurrent stop"); s != StatusPass {
			return s, d
		}
	}
	return StatusPass, fmt.Sprintf("%d concurrent stops all stopped", len(names))
}

// concurrentQuery starts names, runs op on them all at once, and requires
// each answer to satisfy check. present, when set, gates an optional op
// before any session starts (absent = SKIP); an exit 2 under concurrency
// from an op that answered before FAILs.
func concurrentQuery(ctx context.Context, r *runner, hs runtime.ProtocolInfo, opName string,
	present func(context.Context, *runner) (Status, string, bool),
	op func(name string) outcome, check func(name, stdout string) string,
) (Status, string) {
	if s, d, ok := concurrencyDeclared(hs); !ok {
		return s, d
	}
	if present != nil {
		if s, d, ok := present(ctx, r); !ok {
			return s, d
		}
	}
	names := r.freshNames(concurrentSessions)
	defer stopAll(ctx, r, names)
	if s, d := startAll(ctx, r, names); s != StatusPass {
		return s, d
	}
	outs := concurrently(names, op)
	if s, d, ok := allOK(opName, names, outs); !ok {
		return s, d
	}
	for i, res := range outs {
		if problem := check(names[i], res.stdout); problem != "" {
			return StatusFail, fmt.Sprintf("concurrent %s %s: %s", opName, names[i], problem)
		}
	}
	return StatusPass, fmt.Sprintf("%d concurrent %s answers correct", len(names), opName)
}

// wantTrue is a concurrentQuery check for a "true" answer.
func wantTrue(_ string, stdout string) string {
	if got := strings.TrimSpace(stdout); got != "true" {
		return fmt.Sprintf("answered %q, want \"true\"", got)
	}
	return ""
}

func probeConcurrencyIsRunning(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	return concurrentQuery(ctx, r, hs, "is-running", nil,
		func(name string) outcome { return r.isRunning(ctx, name) }, wantTrue)
}

func probeConcurrencyProcessAlive(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	return concurrentQuery(ctx, r, hs, "process-alive", processAlivePresent,
		func(name string) outcome { return r.processAlive(ctx, name, r.agentProcess()) }, wantTrue)
}

func probeConcurrencyListRunning(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	return concurrentQuery(ctx, r, hs, "list-running", listRunningPresent,
		func(name string) outcome {
			_, res := r.listRunning(ctx, nil, name)
			return res
		},
		func(name, stdout string) string {
			if !listed(strings.Split(stdout, "\n"), name) {
				return fmt.Sprintf("listed %q, missing its own session", stdout)
			}
			return ""
		})
}
