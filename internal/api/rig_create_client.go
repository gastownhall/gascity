package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/events"
)

// Rig-create wait tuning (gate G21). These are deliberately distinct from the
// 4-minute sessionMessageTimeout (client.go:sessionMessageTimeout): a WAN clone
// of a large repo routinely exceeds four minutes, so reusing that ceiling would
// strand a user mid-provision (the server finishes; the client reports failure).
// NEVER reuse sessionMessageTimeout for the rig-create wait.
const (
	// rigCreateWaitTimeout is the absolute watchdog on the whole rig-create wait,
	// including every reconnect. The provision keeps running server-side past it;
	// the CLI's resume recipe re-attaches.
	rigCreateWaitTimeout = 30 * time.Minute
	// rigCreateReconnectInitial is the first reconnect backoff delay.
	rigCreateReconnectInitial = 1 * time.Second
	// rigCreateReconnectMaxDelay caps the exponential reconnect backoff
	// (mirrors streamReconnectMax, cmd/gc/cmd_events.go).
	rigCreateReconnectMaxDelay = 30 * time.Second
	// rigCreateMaxSilentAttempts bounds consecutive connects that deliver no frame
	// at all — the "server is gone" cutoff. Any delivered frame (heartbeat or
	// typed event) resets the counter.
	rigCreateMaxSilentAttempts = 8
)

// rigWaitParams is the reconnect budget passed to waitForEventReconnecting. It
// is a parameter (not a package const) so tests compress the watchdog/backoff
// without sleeping 30 minutes; RigCreate passes the defaults.
type rigWaitParams struct {
	maxSilentAttempts int
	reconnectInitial  time.Duration
	reconnectMaxDelay time.Duration
}

// defaultRigWaitParams is the production reconnect budget.
func defaultRigWaitParams() rigWaitParams {
	return rigWaitParams{
		maxSilentAttempts: rigCreateMaxSilentAttempts,
		reconnectInitial:  rigCreateReconnectInitial,
		reconnectMaxDelay: rigCreateReconnectMaxDelay,
	}
}

// RigCreateRequest carries the parameters of a rig-create mutation. It mirrors
// the wire RigCreateBody. Path is deliberately absent: the remote path never
// forwards a client filesystem path; the server derives the clone destination.
type RigCreateRequest struct {
	Name          string
	Prefix        string
	DefaultBranch string
	GitURL        string // required: triggers async 202 provisioning
	RequestID     string // client-minted idempotency id; required with GitURL
}

// RigCreateResult is the terminal outcome of a rig create.
type RigCreateResult struct {
	Status        string // "created" (201 sync) | "provisioned" (202→terminal success) | "exists" (200 replay)
	Rig           string
	Prefix        string
	DefaultBranch string
	RequestID     string
}

// RigCreateWaitError wraps a failure to observe the terminal rig-create event —
// a lost stream (watchdog expiry, reconnect budget exhausted, or a permanent
// stream status), NOT a failed provision. The provision keeps running
// server-side; the CLI prints the request_id + a resume recipe.
type RigCreateWaitError struct {
	RequestID string
	Err       error
}

// Error implements the error interface.
func (e *RigCreateWaitError) Error() string {
	return fmt.Sprintf("lost the provisioning stream (request_id=%s): %v", e.RequestID, e.Err)
}

// Unwrap exposes the underlying stream error.
func (e *RigCreateWaitError) Unwrap() error { return e.Err }

// RigCreateDeadlineError is the absolute-watchdog expiry (rigCreateWaitTimeout):
// the client stopped waiting after the deadline but the provision is STILL
// RUNNING server-side. It is deliberately distinct from RigCreateWaitError (a
// genuinely lost stream) so the CLI prints an honest "still running, re-attach"
// message rather than implying the provision failed. Timeout is the elapsed
// budget for the message.
type RigCreateDeadlineError struct {
	RequestID string
	Timeout   time.Duration
}

// Error implements the error interface.
func (e *RigCreateDeadlineError) Error() string {
	return fmt.Sprintf("provisioning still running after %s (request_id=%s)", e.Timeout, e.RequestID)
}

// RigCreateFailedError is a terminal async failure (request.failed): the
// provision ran and rolled back. Code is the stable rigProvisionFailureCode
// (blocked_host, clone_failed, invalid_request, already_exists, provision_failed);
// a same-request_id retry re-clones cleanly (the rollback purge).
type RigCreateFailedError struct {
	RequestID string
	Code      string
	Message   string
}

// Error implements the error interface.
func (e *RigCreateFailedError) Error() string {
	return fmt.Sprintf("rig create failed: %s: %s", e.Code, e.Message)
}

// RigCreateConflictError is a structured 409: either a request_id reused for a
// different body, or a rig-name collision. When Code == "rig_name_conflict" and
// InFlightRequestID is set, another provision is in flight and the CLI can
// re-attach its event stream with --request-id <InFlightRequestID>.
type RigCreateConflictError struct {
	Code              string // request_id_conflict | rig_name_conflict
	Rig               string
	RequestID         string // request_id_conflict: the offending id
	InFlightRequestID string // rig_name_conflict: the in-flight provision's id (re-attach)
	EventCursor       string // rig_name_conflict: cursor to re-attach the in-flight stream
}

// Error implements the error interface.
func (e *RigCreateConflictError) Error() string {
	switch e.Code {
	case "rig_name_conflict":
		if e.InFlightRequestID != "" {
			return fmt.Sprintf("rig %q is being provisioned by an in-flight request (request_id=%s)", e.Rig, e.InFlightRequestID)
		}
		return fmt.Sprintf("rig name %q is already taken", e.Rig)
	case "request_id_conflict":
		return fmt.Sprintf("request_id %q was already used for a different rig-create request", e.RequestID)
	default:
		return "rig create conflict"
	}
}

// RigCreate creates a rig over the control plane (POST /v0/city/{city}/rigs).
// With GitURL set it drives the async protocol: POST → 202 {request_id,
// event_cursor} → reconnecting SSE wait (gate G21) for request.result.rig.create
// / request.failed, invoking onProgress for each rig.provision.progress frame.
// A remote client attaches the X-GC-City-Write grant automatically (gate G18);
// the SSE result stream never carries the grant. RigCreate validates the
// request_id but never mints it — the CLI owns minting so a failed wait can
// still print the resume recipe.
func (c *Client) RigCreate(req RigCreateRequest, onProgress func(RigProvisionProgressPayload)) (RigCreateResult, error) {
	if err := c.requireCityScope(); err != nil {
		return RigCreateResult{}, err
	}
	// A git_url add is idempotent only with a client-generated request_id.
	if strings.TrimSpace(req.GitURL) != "" && strings.TrimSpace(req.RequestID) == "" {
		return RigCreateResult{}, fmt.Errorf("rig create: a git_url add requires a request_id (client-generated idempotency key)")
	}

	body := genclient.CreateRigJSONRequestBody{Name: req.Name}
	setStrPtr(&body.Prefix, req.Prefix)
	setStrPtr(&body.DefaultBranch, req.DefaultBranch)
	setStrPtr(&body.GitUrl, req.GitURL)
	setStrPtr(&body.RequestId, req.RequestID)
	// Path stays empty on the git_url path — the server derives the destination.

	params := &genclient.CreateRigParams{XGCRequest: "true"}
	resp, err := c.cw.CreateRigWithResponse(context.Background(), c.cityName, params, body)
	if err != nil {
		return RigCreateResult{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return RigCreateResult{}, &connError{err: fmt.Errorf("nil response")}
	}
	// Decode the structured 409 (request_id or rig-name collision) before the
	// generic problem-detail mapping so the CLI can print the re-attach recipe.
	if resp.StatusCode() == http.StatusConflict {
		if cerr := rigConflictFromError(resp.ApplicationproblemJSONDefault); cerr != nil {
			return RigCreateResult{}, cerr
		}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), resp.ApplicationproblemJSONDefault); err != nil {
		return RigCreateResult{}, err
	}

	switch {
	case resp.JSON201 != nil:
		// Sync config-append (git_url absent). No wait.
		return RigCreateResult{
			Status:    "created",
			Rig:       derefStr(resp.JSON201.Rig),
			RequestID: derefStr(resp.JSON201.RequestId),
		}, nil
	case resp.JSON200 != nil:
		// Idempotent replay of a succeeded create. No wait.
		return RigCreateResult{
			Status:        "exists",
			Rig:           derefStr(resp.JSON200.Rig),
			Prefix:        derefStr(resp.JSON200.Prefix),
			DefaultBranch: derefStr(resp.JSON200.DefaultBranch),
			RequestID:     derefStr(resp.JSON200.RequestId),
		}, nil
	case resp.JSON202 != nil:
		return c.awaitRigProvision(resp.JSON202, req.RequestID, onProgress)
	default:
		return RigCreateResult{}, fmt.Errorf("rig create: API returned %d with no recognized body", resp.StatusCode())
	}
}

// awaitRigProvision runs the gate-G21 reconnecting wait for the terminal event
// of an accepted (202) provision, forwarding each rig.provision.progress frame
// that carries this request_id to onProgress. clientRequestID is the
// client-minted idempotency key: it — not the server-echoed accepted.RequestId —
// keys the wait filter and the resume recipe, so a server that echoes the wrong
// (or an empty) id cannot silently steer the wait onto another provision's
// events. An empty client id is a hard error before dialing; a non-empty echo
// that disagrees is a server-contract violation surfaced as an error.
func (c *Client) awaitRigProvision(accepted *genclient.RigCreateResponseBody, clientRequestID string, onProgress func(RigProvisionProgressPayload)) (RigCreateResult, error) {
	requestID := strings.TrimSpace(clientRequestID)
	if requestID == "" {
		return RigCreateResult{}, fmt.Errorf("rig create: empty request_id before the provisioning wait (idempotency key required)")
	}
	if echoed := strings.TrimSpace(derefStr(accepted.RequestId)); echoed != "" && echoed != requestID {
		return RigCreateResult{}, fmt.Errorf("rig create: server echoed request_id %q for a request minted as %q", echoed, requestID)
	}
	cursor := derefStr(accepted.EventCursor)

	ctx, cancel := context.WithTimeout(context.Background(), rigCreateWaitTimeout)
	defer cancel()

	tap := func(env *sseEnvelope) {
		if env.Type != events.RigProvisionProgress {
			return
		}
		var p RigProvisionProgressPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return // best-effort progress; a malformed progress frame is not fatal
		}
		// Other concurrent provisions share the city stream — filter to ours.
		if p.RequestID != requestID {
			return
		}
		if onProgress != nil {
			onProgress(p)
		}
	}

	env, err := c.waitForEventReconnecting(ctx, requestID, events.RequestResultRigCreate, RequestOperationRigCreate, cursor, tap, defaultRigWaitParams())
	if err != nil {
		// The absolute watchdog (this ctx's only deadline) is NOT a lost stream:
		// the provision keeps running server-side. Surface it as an honest
		// "still running" so the CLI's re-attach recipe reads as a resume, not a
		// failure.
		if errors.Is(err, context.DeadlineExceeded) {
			return RigCreateResult{}, &RigCreateDeadlineError{RequestID: requestID, Timeout: rigCreateWaitTimeout}
		}
		return RigCreateResult{}, &RigCreateWaitError{RequestID: requestID, Err: err}
	}
	if env.Type == events.RequestFailed {
		var p RequestFailedPayload
		if derr := json.Unmarshal(env.Payload, &p); derr != nil {
			return RigCreateResult{}, fmt.Errorf("decode rig create failure: %w", derr)
		}
		return RigCreateResult{}, &RigCreateFailedError{RequestID: requestID, Code: p.ErrorCode, Message: p.ErrorMessage}
	}
	var p RigCreateSucceededPayload
	if derr := json.Unmarshal(env.Payload, &p); derr != nil {
		return RigCreateResult{}, fmt.Errorf("decode rig create result: %w", derr)
	}
	return RigCreateResult{
		Status:        "provisioned",
		Rig:           p.Rig,
		Prefix:        p.Prefix,
		DefaultBranch: p.DefaultBranch,
		RequestID:     p.RequestID,
	}, nil
}

// waitForEventReconnecting is the gate-G21 rig-create wait: it loops
// waitForEventOnce, resuming after the max seq consumed, until the terminal
// event arrives, the absolute watchdog (ctx deadline) expires, the reconnect
// budget is exhausted, or a permanent stream status is hit.
//
// Resume cursor: the first attempt uses eventCursor (the 202 EventCursor, a
// pre-spawn capture under the server admission lock, so the terminal cannot be
// missed); subsequent attempts use after_seq=<lastSeq>. after_seq is
// strictly-greater on the server, so the terminal is neither missed nor
// double-processed. A cursor of "0" (the overloaded no-provider case) replays
// the whole log — harmless for correctness thanks to the request_id filter,
// potentially slow on a huge log.
//
// Heartbeat anchoring is two-layer: within a connection every frame resets the
// 45s idle watchdog (waitForEventOnce); across connections every delivered frame
// resets the silent-attempt counter and the backoff schedule. So a live-but-slow
// provision (heartbeating through a 20-minute clone with no progress frames)
// waits up to the watchdog, while a dead peer dies in ≤45s per attempt and
// exhausts the silent budget in a few minutes.
//
// generalization seam that future "generalize waitForEvent reconnect" work
// reuses for session waits; today only rig-create calls in, hence the constants.
//
//nolint:unparam // successType/failOp mirror waitForEventOnce and are the
func (c *Client) waitForEventReconnecting(ctx context.Context, requestID, successType, failOp, eventCursor string, onEnvelope func(*sseEnvelope), params rigWaitParams) (*sseEnvelope, error) {
	var lastSeq uint64
	attempt := 0        // exponential-backoff attempt counter (reset on any delivered frame)
	silent := 0         // consecutive connects that delivered zero frames
	consec401 := 0      // consecutive 401s (anti-spin against a revoked credential)
	var poisonSeen bool // a matching frame's payload failed to decode last attempt
	var poisonSeq uint64

	for {
		if err := ctx.Err(); err != nil {
			return nil, err // absolute watchdog or caller cancel
		}

		resume := eventCursor
		if lastSeq > 0 {
			resume = strconv.FormatUint(lastSeq, 10)
		}
		// TODO(remote-gc): a resume of "0" (the overloaded
		// no-provider EventCursor) replays the whole log, which can re-surface a
		// stale terminal for a since-recycled request_id, and a per-city seq
		// regression or event-log rotation could invalidate a numeric after_seq.
		// Both are server-property-dependent and unreachable with the real event
		// provider; revisit when the reconnect wait is generalized off rig-create.

		env, newSeq, sawFrame, err := c.waitForEventOnce(ctx, requestID, successType, failOp, resume, onEnvelope)
		if newSeq > lastSeq {
			lastSeq = newSeq
		}
		if err == nil {
			return env, nil
		}
		if sawFrame {
			// The peer is alive: reset the reconnect budget and 401 anti-spin.
			silent = 0
			attempt = 0
			consec401 = 0
		}

		// A matching terminal frame whose payload failed to decode: lastSeq was
		// NOT advanced past it, so the resume re-reads seq pde.Seq. Retry it once
		// (a transient truncation decodes cleanly on the re-read); if the identical
		// seq fails to decode a second time it is a genuinely malformed terminal —
		// surface a permanent, honest error instead of looping to the watchdog.
		var pde *ssePayloadDecodeError
		if errors.As(err, &pde) {
			if poisonSeen && poisonSeq == pde.Seq {
				return nil, fmt.Errorf("malformed terminal event at seq %d (request_id=%s): %w", pde.Seq, requestID, err)
			}
			poisonSeen = true
			poisonSeq = pde.Seq
			delay := rigReconnectBackoff(attempt, params)
			attempt++
			if !sleepOrDone(ctx, delay) {
				return nil, ctx.Err()
			}
			continue
		}

		var ce *sseConnectError
		if errors.As(err, &ce) {
			class := classifyRigStreamStatus(ce.Status, ce.RetryAfter, params.reconnectMaxDelay)
			switch {
			case class.permanent:
				return nil, err
			case class.reauth:
				consec401++
				if consec401 >= 2 {
					// Two consecutive 401s ⇒ the credential is revoked, not stale.
					return nil, fmt.Errorf("rig-create stream authorization rejected on two consecutive attempts: %w", err)
				}
				// A fresh bearer is minted live on the next connect. Wait a beat
				// first so a sub-second proxy/JWKS blip cannot trip the two-strike
				// cap on a still-valid credential (still capped at 2 dials total).
				if !sleepOrDone(ctx, reauthReconnectDelay(params)) {
					return nil, ctx.Err()
				}
				continue
			default:
				// Transient 429/503.
				consec401 = 0
				if !sawFrame {
					silent++
					if silent >= params.maxSilentAttempts {
						return nil, fmt.Errorf("rig-create stream delivered no frames across %d attempts: %w", silent, err)
					}
				}
				delay := class.delay
				if delay <= 0 {
					delay = rigReconnectBackoff(attempt, params)
					attempt++
				}
				if !sleepOrDone(ctx, delay) {
					return nil, ctx.Err()
				}
				continue
			}
		}

		// Transport-level failure: dial error, mid-stream EOF, scan error, or the
		// idle-watchdog cancel — all transient.
		consec401 = 0
		if !sawFrame {
			silent++
			if silent >= params.maxSilentAttempts {
				return nil, fmt.Errorf("rig-create stream delivered no frames across %d attempts: %w", silent, err)
			}
		}
		delay := rigReconnectBackoff(attempt, params)
		attempt++
		if !sleepOrDone(ctx, delay) {
			return nil, ctx.Err()
		}
	}
}

// rigStreamRetry is the decision for a non-200 SSE connect during the rig-create
// wait: reconnect transiently (with an optional Retry-After floor), re-auth
// (401), or give up (permanent).
type rigStreamRetry struct {
	permanent bool
	reauth    bool
	delay     time.Duration // Retry-After floor; 0 => use the caller's exponential backoff
}

// classifyRigStreamStatus is the internal/api twin of cmd/gc's
// classifyStreamStatus (unifying the two is future work): 429/503 transient
// honoring Retry-After, 401 re-auth, everything else permanent.
func classifyRigStreamStatus(status int, retryAfter string, maxDelay time.Duration) rigStreamRetry {
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return rigStreamRetry{delay: parseRigRetryAfter(retryAfter, maxDelay)}
	case http.StatusUnauthorized:
		return rigStreamRetry{reauth: true}
	default:
		return rigStreamRetry{permanent: true}
	}
}

// parseRigRetryAfter parses a Retry-After delta-seconds value, bounded so a
// hostile server cannot pin the client offline. An HTTP-date form is ignored
// (over-precise for a client backoff). Mirrors cmd/gc's parseRetryAfter.
func parseRigRetryAfter(v string, maxDelay time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return 0
	}
	d := time.Duration(secs) * time.Second
	if bound := maxDelay * 4; bound > 0 && d > bound {
		d = bound
	}
	return d
}

// rigReconnectBackoff returns the exponential backoff for the given attempt
// (0 = first retry), doubling from reconnectInitial up to reconnectMaxDelay.
func rigReconnectBackoff(attempt int, params rigWaitParams) time.Duration {
	d := params.reconnectInitial
	if d <= 0 {
		d = rigCreateReconnectInitial
	}
	maxDelay := params.reconnectMaxDelay
	if maxDelay <= 0 {
		maxDelay = rigCreateReconnectMaxDelay
	}
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= maxDelay {
			return maxDelay
		}
	}
	return d
}

// reauthReconnectDelay is the pause before the SECOND 401 reconnect attempt so a
// sub-second credential blip (a proxy hiccup, a JWKS rotation window) does not
// trip the two-strike anti-spin cap. It reuses the reconnect-initial backoff
// (1s in production, compressed in tests via the injectable params).
func reauthReconnectDelay(params rigWaitParams) time.Duration {
	d := params.reconnectInitial
	if d <= 0 {
		d = rigCreateReconnectInitial
	}
	return d
}

// sleepOrDone sleeps for d, honoring ctx cancellation. It returns false when ctx
// was canceled during the wait (the caller should stop). A non-positive delay
// returns true immediately.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// rigConflictFromError decodes a structured 409 rig-create conflict from the
// ErrorModel.Errors list the server emits (huma_handlers_rigs.go: mapRigAdmitError).
// It returns nil when the body is not a recognized rig-create conflict, so the
// caller falls through to the generic problem-detail mapping.
func rigConflictFromError(pd *genclient.ErrorModel) *RigCreateConflictError {
	if pd == nil || pd.Errors == nil {
		return nil
	}
	fields := map[string]string{}
	for _, d := range *pd.Errors {
		if d.Location == nil {
			continue
		}
		key := strings.TrimPrefix(*d.Location, "body.")
		if s, ok := d.Value.(string); ok {
			fields[key] = s
		}
	}
	code := fields["code"]
	if code != "request_id_conflict" && code != "rig_name_conflict" {
		return nil
	}
	return &RigCreateConflictError{
		Code:              code,
		Rig:               fields["name"],
		RequestID:         fields["request_id"],
		InFlightRequestID: fields["in_flight_request_id"],
		EventCursor:       fields["event_cursor"],
	}
}
