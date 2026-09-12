package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// The worker subset of `gc bd` under a REMOTE city target (cr-gdeav.5.4 draft).
//
// WHY A SUBSET AND NOT A GENERAL REMOTE `gc bd`. Every bead verb today dies at
// resolveBdCity → resolveCity → errRemoteNotSupportedYet, and the verbs that
// could be routed without inventing new server semantics are exactly the four a
// worker's lifecycle needs: acquire, keep, hand back, close with a record. A
// general remote `gc bd` would need bd's whole verb surface (list, sql, dep
// tree, note append…) served over HTTP with the same row schemas, which is a
// different, larger contract than this one. So the whitelist is closed: a verb
// outside it refuses, and the refusal NAMES the subset, because "remote support
// is being enabled incrementally" tells an operator nothing actionable.
//
// PARITY RULE. The startup wrapper that runs these commands cannot tell which
// leg produced its output, so the remote leg keeps the local leg's exit codes
// (0 success, 1 lost claim / refused verb), its released-vs-skipped stdout
// vocabulary, and its claimant identity (BEADS_ACTOR). Where the two legs
// genuinely differ — the JSON row shape, see renderRemoteWorkerBead — the
// divergence is stated in the code and carried to the review lane rather than
// discovered by a parser in the field.

// bdWorkerVerb is one of the four routed lifecycle verbs.
type bdWorkerVerb string

const (
	workerVerbClaim     bdWorkerVerb = "claim"
	workerVerbHeartbeat bdWorkerVerb = "heartbeat"
	workerVerbRelease   bdWorkerVerb = "release"
	workerVerbClose     bdWorkerVerb = "close"
)

// bdWorkerRequest is a parsed worker-subset intent. It carries no transport
// detail, so argument parsing can be tested apart from the HTTP leg.
type bdWorkerRequest struct {
	verb      bdWorkerVerb
	id        string
	assignee  string
	sessionID string
	jsonOut   bool
	// close record (typed close, ADR-0009)
	outcome string
	commit  string
	branch  string
	reason  string
}

// workerSubsetRefusal is the message a non-worker verb gets under a remote
// target. Naming the subset is the point: an operator who is told only
// "unsupported" retries forever; an operator who is told which four verbs ARE
// supported can rewrite the call or stop.
func workerSubsetRefusal(verb string) string {
	return fmt.Sprintf("gc bd: %s is not supported against a remote city; the remote worker subset is "+
		"`update <id> --claim`, `heartbeat <id>`, `release-if-current <id> <assignee>` and `close <id>` "+
		"with a typed gc.work_outcome — every other bd verb needs the local store (or bd) and stays local\n", verb)
}

// parseBdWorkerSubset recognizes the routed verbs in an already-normalized bd
// arg list (extractBdScopeFlags and rewriteBdHeartbeatArgs have run). It
// reports handled=false when the verb is outside the subset, in which case the
// caller either continues locally or, under a remote target, refuses.
func parseBdWorkerSubset(bdArgs []string) (bdWorkerRequest, bool, error) {
	if len(bdArgs) == 0 {
		return bdWorkerRequest{}, false, nil
	}
	switch bdArgs[0] {
	case "heartbeat":
		if len(bdArgs) != 2 {
			return bdWorkerRequest{}, true, fmt.Errorf("usage: gc bd heartbeat <issue-id>")
		}
		return bdWorkerRequest{verb: workerVerbHeartbeat, id: bdArgs[1], assignee: bdWorkerClaimActor(), sessionID: bdWorkerSessionID()}, true, nil

	case "release-if-current":
		id, expected, ok, err := parseBdReleaseIfCurrentArgs(bdArgs)
		if err != nil || !ok {
			return bdWorkerRequest{}, ok, err
		}
		return bdWorkerRequest{verb: workerVerbRelease, id: id, assignee: expected, sessionID: bdWorkerSessionID()}, true, nil

	case "close":
		req, err := parseBdWorkerCloseArgs(bdArgs[1:])
		if err != nil {
			return bdWorkerRequest{}, true, err
		}
		if req.id == "" {
			return bdWorkerRequest{}, true, fmt.Errorf("usage: gc bd close <issue-id> [--reason TEXT] [--set-metadata gc.work_outcome=...]")
		}
		req.sessionID = bdWorkerSessionID()
		// The close carries the same holder identity the claim took: the server
		// fences the terminal write to it, so a remote close with no
		// BEADS_ACTOR is refused here rather than reaching a city that has to
		// guess who did the work.
		req.assignee = bdWorkerClaimActor()
		if req.id != "" && req.assignee == "" {
			return bdWorkerRequest{}, true, fmt.Errorf("gc bd close against a remote city requires BEADS_ACTOR to name the holder closing %s", req.id)
		}
		return req, true, nil

	case "update":
		if !bdArgsContain(bdArgs, "--claim") {
			return bdWorkerRequest{}, false, nil
		}
		id := bdWorkerPositionalID(bdArgs[1:])
		if id == "" {
			return bdWorkerRequest{}, true, fmt.Errorf("gc bd update --claim needs exactly one bead id")
		}
		return bdWorkerRequest{
			verb:      workerVerbClaim,
			id:        id,
			assignee:  bdWorkerClaimActor(),
			sessionID: bdWorkerSessionID(),
			jsonOut:   bdArgsContain(bdArgs, "--json"),
		}, true, nil
	}
	return bdWorkerRequest{}, false, nil
}

// parseBdWorkerCloseArgs reads the typed close off the spellings the local gate
// already reads — gc.work_outcome / gc.work_commit / gc.work_branch as
// --set-metadata pairs and bd's -r/--reason for the disposition text — rather
// than inventing a second flag vocabulary for the remote leg.
func parseBdWorkerCloseArgs(args []string) (bdWorkerRequest, error) {
	req := bdWorkerRequest{verb: workerVerbClose}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json":
			req.jsonOut = true
		case arg == "-r", arg == "--reason":
			if i+1 >= len(args) {
				return req, fmt.Errorf("%s needs a value", arg)
			}
			i++
			req.reason = args[i]
		case strings.HasPrefix(arg, "--reason="):
			req.reason = strings.TrimPrefix(arg, "--reason=")
		case arg == "--set-metadata", arg == "--metadata":
			if i+1 >= len(args) {
				return req, fmt.Errorf("%s needs a key=value pair", arg)
			}
			i++
			if err := req.absorbMetadataPair(args[i]); err != nil {
				return req, err
			}
		case strings.HasPrefix(arg, "--set-metadata="):
			if err := req.absorbMetadataPair(strings.TrimPrefix(arg, "--set-metadata=")); err != nil {
				return req, err
			}
		case strings.HasPrefix(arg, "-"):
			// An unrecognized flag cannot be proven not to name a bead id, so
			// the close is refused rather than guessed at — the same fail-closed
			// rule bdMutationWriteIDs applies to the passthrough path.
			return bdWorkerRequest{}, fmt.Errorf("gc bd close against a remote city does not understand %q", arg)
		default:
			if req.id != "" {
				return bdWorkerRequest{}, fmt.Errorf("gc bd close takes one bead id, got %q and %q", req.id, arg)
			}
			req.id = arg
		}
	}
	return req, nil
}

// absorbMetadataProject… maps a gc.work_* pair onto the close record. Any other
// key is refused: silently dropping a metadata write the caller asked for is
// worse than refusing the verb, and `gc bd close` refuses metadata writes on
// the local path too (gc bd close aborts outright on --set-metadata).
func (r *bdWorkerRequest) absorbMetadataPair(pair string) error {
	key, value, found := strings.Cut(pair, "=")
	if !found {
		return fmt.Errorf("gc bd close against a remote city needs metadata as key=value, got %q", pair)
	}
	switch strings.TrimSpace(key) {
	case beadmeta.WorkOutcomeMetadataKey:
		r.outcome = strings.TrimSpace(value)
	case beadmeta.WorkCommitMetadataKey:
		r.commit = strings.TrimSpace(value)
	case beadmeta.WorkBranchMetadataKey:
		r.branch = strings.TrimSpace(value)
	default:
		return fmt.Errorf("gc bd close against a remote city does not understand metadata key %q", strings.TrimSpace(key))
	}
	return nil
}

// bdWorkerClaimActor is the claimant identity, exactly as the local by-ID claim
// resolves it (cmd_bd_by_id.go:1281). The remote leg honors the SAME identity:
// a transport credential names a city connection, not a worker, and a claim
// stamped with the wrong identity is a claim no reaper can attribute.
func bdWorkerClaimActor() string { return strings.TrimSpace(os.Getenv("BEADS_ACTOR")) }

// bdWorkerSessionID names the gc session the verb is issued for, for
// attribution only — never as the ownership pointer.
func bdWorkerSessionID() string {
	if v := strings.TrimSpace(os.Getenv("GC_SESSION_ID")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("GC_SESSION"))
}

func bdArgsContain(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// bdWorkerPositionalID returns the first non-flag token, which is the bead id
// for `update <id> --claim`.
func bdWorkerPositionalID(args []string) string {
	var id string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if id != "" {
			return "" // more than one positional: ambiguous, refuse
		}
		id = a
	}
	return id
}

// routeBdWorkerRemote is doBd's remote branch: it decides whether this seat is
// remote, and if so either routes one of the four worker verbs or refuses a
// verb outside the subset. routed=false means "this seat is not remote" and the
// caller continues exactly as it did before — the failure path it was already
// on, with the failure message it already printed.
//
// A refused verb makes NO request. A call that fails after reaching the city is
// a different bug than one that never should have been sent, and a worker
// unwinding a startup failure must not be able to touch a bead it was refused
// permission to name.
func routeBdWorkerRemote(bdArgs []string, stdout, stderr io.Writer) (int, bool) {
	req, handled, parseErr := parseBdWorkerSubset(bdArgs)
	client, isRemote, _, err := resolveWriteTarget()
	if !isRemote {
		// Local seat (or the context cannot be read at all): not this branch's
		// question to answer. The caller's existing resolution owns it.
		return 0, false
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: resolving the remote city: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	if parseErr != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", parseErr) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	if !handled {
		verb := "this verb"
		if len(bdArgs) > 0 {
			verb = bdArgs[0]
		}
		fmt.Fprint(stderr, workerSubsetRefusal(verb)) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	return runBdWorkerRemote(client, req, stdout, stderr), true
}

// runBdWorkerRemote executes one routed verb over the remote control plane.
// client is the write-side remote client (buildRemoteWriteClient), so a
// hardened city's per-request city-write grant is attached (gate G18) and a
// remote error surfaces rather than falling back to a local store (gate G1).
func runBdWorkerRemote(client *api.Client, req bdWorkerRequest, stdout, stderr io.Writer) int {
	if strings.TrimSpace(req.id) == "" {
		fmt.Fprintf(stderr, "gc bd: a remote worker verb needs a bead id\n") //nolint:errcheck // best-effort stderr
		return 1
	}
	ctx := context.Background()
	switch req.verb {
	case workerVerbClaim:
		return runRemoteWorkerClaim(ctx, client, req, stdout, stderr)
	case workerVerbHeartbeat:
		res, err := client.WorkerHeartbeat(ctx, api.WorkerVerbRequest{SessionID: req.sessionID, Assignee: req.assignee, BeadID: req.id})
		if err != nil {
			return remoteWorkerFailure("heartbeat", req, err, stderr)
		}
		fmt.Fprintf(stdout, "%s lease refreshed (%s)\n", res.Bead.ID, res.Status) //nolint:errcheck // best-effort stdout
		return 0
	case workerVerbRelease:
		res, err := client.WorkerRelease(ctx, api.WorkerVerbRequest{SessionID: req.sessionID, Assignee: req.assignee, BeadID: req.id})
		if err != nil {
			return remoteWorkerFailure("release-if-current", req, err, stderr)
		}
		// The local leg's vocabulary is preserved verbatim: the orphan-recovery
		// scripts that read it key on these two words.
		if res.Skipped {
			fmt.Fprintln(stdout, "skipped") //nolint:errcheck // best-effort stdout
			return 0
		}
		fmt.Fprintln(stdout, "released") //nolint:errcheck // best-effort stdout
		return 0
	case workerVerbClose:
		res, err := client.WorkerClose(ctx, api.WorkerCloseRequest{
			SessionID: req.sessionID,
			Assignee:  req.assignee,
			BeadID:    req.id,
			Outcome:   req.outcome,
			Commit:    req.commit,
			Branch:    req.branch,
			Reason:    req.reason,
		})
		if err != nil {
			return remoteWorkerFailure("close", req, err, stderr)
		}
		return renderRemoteWorkerBead(res.Bead, req.jsonOut, stdout, stderr)
	}
	fmt.Fprintf(stderr, "gc bd: %s is not in the remote worker subset\n", string(req.verb)) //nolint:errcheck // best-effort stderr
	return 1
}

func runRemoteWorkerClaim(ctx context.Context, client *api.Client, req bdWorkerRequest, stdout, stderr io.Writer) int {
	if req.assignee == "" {
		fmt.Fprintf(stderr, "gc bd: claiming %s requires BEADS_ACTOR to name the claimant\n", req.id) //nolint:errcheck // best-effort stderr
		return 1
	}
	res, err := client.WorkerClaim(ctx, api.WorkerVerbRequest{SessionID: req.sessionID, Assignee: req.assignee, BeadID: req.id})
	if err != nil {
		if code, ok := api.WorkerVerbStatusCode(err); ok && code == http.StatusConflict {
			// A lost claim is the local leg's exit 1 with the holder named —
			// the caller must not proceed, and must not think it broke the
			// connection either.
			fmt.Fprintf(stderr, "gc bd: %s was not claimed for %q: the city reports it is held elsewhere\n", req.id, req.assignee) //nolint:errcheck // best-effort stderr
			return 1
		}
		return remoteWorkerFailure("update --claim", req, err, stderr)
	}
	return renderRemoteWorkerBead(res.Bead, req.jsonOut, stdout, stderr)
}

// renderRemoteWorkerBead prints the bead a worker verb returned.
//
// DIVERGENCE, STATED ON PURPOSE: the local by-ID leg renders --json as a
// one-element ARRAY (printBdByIDBead marshals []beads.Bead{b},
// cmd_bd_by_id.go:1541-1549) while the remote leg emits one JSON OBJECT here.
// The drafts pin the object (cmd/gc/remote_worker_test.go), on the grounds that
// a lifecycle call answers one bead and a wrapper should not index [0]; making
// the two legs agree is a decision about every existing consumer of the local
// row, so it belongs to the review lane, not to a worker atom. Do not "fix" one
// side silently.
func renderRemoteWorkerBead(b beads.Bead, jsonOut bool, stdout, stderr io.Writer) int {
	if jsonOut {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		if err := enc.Encode(b); err != nil {
			fmt.Fprintf(stderr, "gc bd: rendering %q: %v\n", b.ID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if _, werr := stdout.Write(buf.Bytes()); werr != nil {
			fmt.Fprintf(stderr, "gc bd: writing %q: %v\n", b.ID, werr) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "%-12s %s\n", "id:", b.ID)         //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "%-12s %s\n", "status:", b.Status) //nolint:errcheck // best-effort stdout
	if strings.TrimSpace(b.Assignee) != "" {
		fmt.Fprintf(stdout, "%-12s %s\n", "assignee:", b.Assignee) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// remoteWorkerFailure maps a transport/HTTP failure to an exit code and a
// message that names which kind it was: a 501 is a city whose store cannot
// serve the verb (operator action: check the store capability), a 401/403 is a
// grant problem, anything else is reachability. Telling an operator "remote
// failed" for a 501 sends them to the wrong subsystem.
func remoteWorkerFailure(verb string, req bdWorkerRequest, err error, stderr io.Writer) int {
	if code, ok := api.WorkerVerbStatusCode(err); ok {
		switch code {
		case http.StatusNotImplemented:
			fmt.Fprintf(stderr, "gc bd %s: the city's store cannot serve this verb: %v\n", verb, err) //nolint:errcheck // best-effort stderr
			return 1
		case http.StatusForbidden, http.StatusUnauthorized:
			fmt.Fprintf(stderr, "gc bd %s: the city refused the write (grant/credential): %v\n", verb, err) //nolint:errcheck // best-effort stderr
			return 1
		case http.StatusNotFound:
			fmt.Fprintf(stderr, "gc bd %s: %s not found on the remote city\n", verb, req.id) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	fmt.Fprintf(stderr, "gc bd %s: remote city: %v\n", verb, err) //nolint:errcheck // best-effort stderr
	return 1
}
