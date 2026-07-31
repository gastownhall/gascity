// Package slingprivate is Gas City's private command boundary for sling.
//
// It is the City half of the sling trust-boundary split. The public gateway
// lives in Beads Team Server: it terminates the caller's Bearer or browser
// session, resolves the tenant, and mints a single-use internal assertion (see
// [slingassert]). This package accepts that assertion — and nothing else — over
// a mutually authenticated connection, then dispatches exactly one normalized
// orchestration request.
//
// The two halves stay separate on purpose. Collapsing them would put public
// credential termination and City orchestration in one process, so a flaw in
// either would reach the other. Concretely, this boundary:
//
//   - refuses every public credential (Bearer, cookie session, CSRF header, a
//     city-write grant) whether presented alone or alongside an assertion, so
//     there is no path from a public credential to a private mutator and no
//     dual-credential ambiguity about which one authorized the dispatch;
//   - refuses any caller-supplied tenant or attribution field, so the verified
//     assertion is the only source of org, workspace, principal and source;
//   - refuses any request body that names a different target than the
//     assertion resolved, so attribution and formula variables cannot select
//     another resource;
//   - dispatches exactly one command, checked against a constant as well as a
//     runtime registry, so withdrawing sling closes sling and opens no broker
//     or service fallback in its place.
//
// Mount [Boundary.Handler] on a listener whose TLS config requires and verifies
// client certificates. It is not mounted on the public supervisor mux: a
// private boundary sharing the public listener would be one routing mistake
// away from being public.
package slingprivate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/slingassert"
)

// PathPrefix is the private command grammar: POST
// /internal/v0/city/{city}/sling/{command}. The city and command are path
// segments rather than body fields so both are bound by the assertion before
// the body is parsed, and so a request for a withdrawn command is refused
// without reading its body.
const PathPrefix = "/internal/v0/city/"

// AssertionHeader carries the BTS-minted internal assertion.
const AssertionHeader = "X-GC-Sling-Assertion"

// maxBodyBytes caps the body buffered to compute the assertion's body hash, so
// an unverified caller cannot exhaust memory by streaming before verification.
const maxBodyBytes = 1 << 20 // 1 MiB

// publicCredentialHeaders are the public-plane credentials that must never
// appear on this boundary. Presenting one is a rejection whether or not a valid
// assertion accompanies it: the private boundary authorizes on the assertion
// alone, so a second credential is either a caller aiming the wrong plane at
// this port or an attempt to make the authorizing credential ambiguous.
//
// X-GC-Request is the public CSRF header and X-GC-City-Write is the operator
// config grant; neither authorizes anything here, and both indicate a public
// request that has been re-pointed at the private port.
var publicCredentialHeaders = []string{
	"Authorization",
	"Cookie",
	"Proxy-Authorization",
	"X-GC-Request",
	"X-GC-City-Write",
}

// reservedVarKeys are variable names a caller may not set. Formula variables
// are interpolated into orchestration inputs, so a variable that shadows a
// tenant or target field would let approved attribution select another
// resource. They are refused rather than dropped: silently ignoring a variable
// the caller believes is in effect is its own failure mode.
var reservedVarKeys = []string{
	"org", "workspace", "principal", "source", "actor",
	"city", "project", "rig", "issue", "bead", "target", "owner",
}

// Command is the request the boundary hands to a dispatcher after verification.
// Its tenant and target fields come from the verified assertion only; Body is
// the caller's raw request body, already checked for tenant and target
// disagreement. A dispatcher must treat the named fields as authoritative and
// must not re-read tenancy out of Body.
type Command struct {
	Name        string
	OperationID string
	PolicyID    string

	Org       string
	Workspace string
	Principal string
	Source    string

	City    string
	Project string
	Issue   string

	Body []byte
}

// Result is a normalized dispatch outcome. Body is a complete response body the
// boundary relays verbatim, so the private layer never re-encodes an
// orchestration DTO it does not own.
type Result struct {
	Status int
	Body   []byte
}

// Dispatcher performs the single orchestration request a verified command
// authorizes. An error is a dispatch failure; the boundary turns it into
// normalized evidence and records it against the nonce, because the assertion
// was single-use and has been spent.
type Dispatcher interface {
	Dispatch(ctx context.Context, cmd Command) (Result, error)
}

// ResultStore records the outcome of a dispatched nonce so a duplicate internal
// delivery is answered with the prior result instead of dispatching twice.
// Implementations must be safe for concurrent use.
type ResultStore interface {
	Get(nonce string) (Result, bool)
	Put(nonce string, result Result, until time.Time)
}

// Options configures a Boundary.
type Options struct {
	// Verifier authenticates assertions. Required.
	Verifier *slingassert.Verifier
	// Dispatcher performs the orchestration request. Required.
	Dispatcher Dispatcher
	// Workload maps a request's verified mTLS peer chain to the workload
	// identity the assertion must name. Required: without it the boundary
	// cannot bind an assertion to a transport and would accept a captured one
	// from any peer holding a trusted certificate.
	Workload func(*http.Request) (string, bool)
	// Results answers duplicate deliveries; defaults to an in-memory store.
	Results ResultStore
	// Retain bounds how long a result answers duplicates. Defaults to 10m.
	Retain time.Duration
	// Auditf receives the specific rejection reason. The response never carries
	// it — a distinguishable rejection is a tenant oracle — so this is the only
	// place the reason is visible. Defaults to log.Printf.
	Auditf func(format string, args ...any)
}

// Boundary is the private sling command endpoint.
type Boundary struct {
	verifier   *slingassert.Verifier
	dispatcher Dispatcher
	workload   func(*http.Request) (string, bool)
	results    ResultStore
	retain     time.Duration
	auditf     func(string, ...any)
	// closed is the rollback state: the boundary answers, and refuses
	// everything, without a verifier or a registry. See NewBoundary.
	closed bool
}

// New builds a Boundary, refusing a configuration that would weaken the
// boundary rather than defaulting around it.
func New(opts Options) (*Boundary, error) {
	if opts.Verifier == nil {
		return nil, errors.New("slingprivate: a verifier is required")
	}
	if opts.Dispatcher == nil {
		return nil, errors.New("slingprivate: a dispatcher is required")
	}
	if opts.Workload == nil {
		return nil, errors.New("slingprivate: a workload extractor is required")
	}
	results := opts.Results
	if results == nil {
		results = NewMemoryResultStore()
	}
	retain := opts.Retain
	if retain <= 0 {
		retain = 10 * time.Minute
	}
	auditf := opts.Auditf
	if auditf == nil {
		auditf = log.Printf
	}
	return &Boundary{
		verifier:   opts.Verifier,
		dispatcher: opts.Dispatcher,
		workload:   opts.Workload,
		results:    results,
		retain:     retain,
		auditf:     auditf,
	}, nil
}

// Handler returns the private mux. It routes only the private grammar; every
// other path is a flat 404, so the private listener exposes no other surface.
func (b *Boundary) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(PathPrefix, http.HandlerFunc(b.serve))
	return mux
}

// Uniform rejection. Every authorization, binding, registry and taint failure
// returns this exact response so no case is distinguishable from another —
// a caller cannot learn whether a city exists, whether a command is registered,
// or which claim disagreed. The reason goes to the audit sink instead.
var problemRejected = problem{
	status: http.StatusForbidden,
	body:   []byte(`{"status":403,"title":"Forbidden","detail":"sling command rejected"}`),
}

var (
	problemNotFound = problem{
		status: http.StatusNotFound,
		body:   []byte(`{"status":404,"title":"Not Found","detail":"not_found"}`),
	}
	problemBodyTooLarge = problem{
		status: http.StatusRequestEntityTooLarge,
		body:   []byte(`{"status":413,"title":"Request Entity Too Large","detail":"request body exceeds limit"}`),
	}
	problemBadBody = problem{
		status: http.StatusBadRequest,
		body:   []byte(`{"status":400,"title":"Bad Request","detail":"could not read request body"}`),
	}
	// problemInFlight answers a duplicate delivery whose first dispatch has not
	// finished. It is deliberately distinct from problemRejected: the caller
	// must retry rather than treat the command as refused, and it leaks nothing
	// beyond "this exact nonce is already in flight here", which the caller
	// minted and already knows.
	problemInFlight = problem{
		status: http.StatusConflict,
		body:   []byte(`{"status":409,"title":"Conflict","detail":"command already in flight"}`),
	}
	// problemDispatchFailed is the normalized evidence for an orchestrator
	// failure. It says the dispatch failed and nothing about why, because the
	// orchestrator's error text can name City-internal resources.
	problemDispatchFailed = problem{
		status: http.StatusBadGateway,
		body:   []byte(`{"status":502,"title":"Bad Gateway","detail":"orchestration dispatch failed"}`),
	}
)

type problem struct {
	status int
	body   []byte
}

func (p problem) writeTo(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(p.status)
	_, _ = w.Write(p.body)
}

func (b *Boundary) reject(w http.ResponseWriter, reason string) {
	b.auditf("slingprivate: rejected: %s", reason)
	problemRejected.writeTo(w)
}

func (b *Boundary) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// Not a rejection oracle: the grammar has exactly one method and no
		// resource is consulted to know it.
		w.Header().Set("Allow", http.MethodPost)
		problemNotFound.writeTo(w)
		return
	}
	city, command, ok := parsePath(r.URL.Path)
	if !ok {
		problemNotFound.writeTo(w)
		return
	}
	if b.closed {
		b.reject(w, "the private sling boundary is rolled back")
		return
	}

	// The mTLS transport is checked first: without a verified peer nothing else
	// about this request is worth evaluating, and an unauthenticated caller
	// should not be able to make the boundary read a body.
	workload, ok := b.workload(r)
	if !ok || workload == "" {
		b.reject(w, "no verified mTLS workload identity on the connection")
		return
	}

	// No public credential, alone or alongside the assertion.
	for _, h := range publicCredentialHeaders {
		if r.Header.Get(h) != "" {
			b.reject(w, "public credential "+h+" presented on the private boundary")
			return
		}
	}

	// Exactly one assertion. Two would let a proxy or a caller choose which one
	// the boundary verifies.
	assertions := r.Header.Values(AssertionHeader)
	if len(assertions) != 1 || strings.TrimSpace(assertions[0]) == "" {
		b.reject(w, "expected exactly one assertion header")
		return
	}
	// This boundary dispatches exactly one command. Checking the constant as
	// well as the registry is what makes withdrawing sling close sling and
	// nothing else: no other registry entry — a broker or service command an
	// operator adds for another boundary — can ever be reached through here.
	if command != slingassert.CommandSlingCityWork {
		b.reject(w, "command "+command+" is not served by the sling boundary")
		return
	}
	// The registry gate runs before the body is read. This is the rollback
	// seam: a withdrawn command costs an unverified caller nothing to discover
	// and gives the boundary no work to do.
	if !b.verifier.Registered(command) {
		b.reject(w, "command "+command+" is not registered")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			problemBodyTooLarge.writeTo(w)
		} else {
			problemBadBody.writeTo(w)
		}
		return
	}

	assertion, err := b.verifier.Verify(assertions[0], slingassert.Expect{
		Workload: workload,
		Method:   r.Method,
		Command:  command,
		BodyHash: slingassert.BodyHash(body),
		City:     city,
	})
	switch {
	case errors.Is(err, slingassert.ErrReplay):
		// Duplicate internal delivery of an authentic assertion. Answer from
		// the result store; a miss means the first dispatch is still running,
		// so the caller must retry rather than get a second dispatch.
		if prior, ok := b.results.Get(assertion.Nonce); ok {
			b.auditf("slingprivate: duplicate delivery of nonce %s answered from the result store", assertion.Nonce)
			writeResult(w, prior)
			return
		}
		b.auditf("slingprivate: duplicate delivery of nonce %s while the first dispatch is in flight", assertion.Nonce)
		problemInFlight.writeTo(w)
		return
	case err != nil:
		b.reject(w, "assertion verification failed: "+err.Error())
		return
	}

	// The body may not restate the tenant, and may not name a target other than
	// the one the assertion resolved.
	if err := checkTaint(body, *assertion); err != nil {
		b.reject(w, err.Error())
		return
	}

	cmd := Command{
		Name:        assertion.Command,
		OperationID: assertion.OperationID,
		PolicyID:    assertion.PolicyID,
		Org:         assertion.Org,
		Workspace:   assertion.Workspace,
		Principal:   assertion.Principal,
		Source:      assertion.Source,
		City:        assertion.City,
		Project:     assertion.Project,
		Issue:       assertion.Issue,
		Body:        body,
	}
	result, dispatchErr := b.dispatcher.Dispatch(r.Context(), cmd)
	if dispatchErr != nil {
		// The assertion was single-use and is spent, so the failure is the
		// outcome of this nonce: record it and relay it to a duplicate too.
		// A genuine retry needs a fresh assertion, which is what keeps a failed
		// dispatch from being retried into a second real dispatch.
		b.auditf("slingprivate: dispatch failed for nonce %s: %v", assertion.Nonce, dispatchErr)
		result = Result{Status: problemDispatchFailed.status, Body: problemDispatchFailed.body}
	}
	b.results.Put(assertion.Nonce, result, time.Now().Add(b.retain))
	writeResult(w, result)
}

func writeResult(w http.ResponseWriter, result Result) {
	status := result.Status
	if status == 0 {
		status = http.StatusOK
	}
	if len(result.Body) > 0 && json.Valid(result.Body) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(status)
	_, _ = w.Write(result.Body)
}

// parsePath matches /internal/v0/city/{city}/sling/{command} exactly. Control
// characters are refused: a decoded newline or NUL in a segment would let two
// distinct requests share one canonical form.
func parsePath(path string) (city, command string, ok bool) {
	if !strings.HasPrefix(path, PathPrefix) {
		return "", "", false
	}
	if strings.ContainsAny(path, "\n\r\x00") {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, PathPrefix)
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[1] != "sling" {
		return "", "", false
	}
	city, command = parts[0], parts[2]
	if city == "" || command == "" {
		return "", "", false
	}
	return city, command, true
}

// callerFields is the shape the taint check parses out of a request body. Every
// field here is either tenancy the caller may not assert or a target selector
// that must agree with the assertion.
type callerFields struct {
	Org       *string           `json:"org"`
	Workspace *string           `json:"workspace"`
	Principal *string           `json:"principal"`
	Source    *string           `json:"source"`
	Actor     *string           `json:"actor"`
	Owner     *string           `json:"owner"`
	City      *string           `json:"city"`
	Project   *string           `json:"project"`
	Rig       *string           `json:"rig"`
	Issue     *string           `json:"issue"`
	Bead      *string           `json:"bead"`
	Vars      map[string]string `json:"vars"`
}

// checkTaint enforces AC2's authority rules on the caller's body: the verified
// tenant is the only tenant, and every target selector the body carries must
// name what the assertion already resolved. Pointers distinguish "absent" from
// "present and empty" — a present-but-empty tenant field is still a caller
// asserting tenancy and is still refused.
func checkTaint(body []byte, a slingassert.Assertion) error {
	if len(body) == 0 {
		return nil
	}
	var f callerFields
	if err := json.Unmarshal(body, &f); err != nil {
		// A body the boundary cannot inspect cannot be cleared of taint.
		return errors.New("request body is not an inspectable object")
	}

	// Tenancy and attribution are the assertion's alone. The caller does not
	// get to restate them, not even correctly: accepting a matching value today
	// makes the field look load-bearing to the next reader.
	for name, present := range map[string]bool{
		"org":       f.Org != nil,
		"workspace": f.Workspace != nil,
		"principal": f.Principal != nil,
		"source":    f.Source != nil,
		"actor":     f.Actor != nil,
		"owner":     f.Owner != nil,
	} {
		if present {
			return errors.New("body carries caller-supplied " + name)
		}
	}

	// Target selectors may appear (the gateway forwards the caller's body
	// verbatim) but must agree exactly with what the assertion resolved.
	for _, sel := range []struct {
		name  string
		got   *string
		want  string
		empty bool
	}{
		{name: "city", got: f.City, want: a.City},
		{name: "project", got: f.Project, want: a.Project},
		{name: "rig", got: f.Rig, want: a.Project, empty: true},
		{name: "issue", got: f.Issue, want: a.Issue},
		{name: "bead", got: f.Bead, want: a.Issue, empty: true},
	} {
		if sel.got == nil {
			continue
		}
		if sel.empty && *sel.got == "" {
			// An explicitly empty optional selector selects nothing, so it
			// cannot re-target; the assertion's value still governs.
			continue
		}
		if *sel.got != sel.want {
			return errors.New("body " + sel.name + " disagrees with the verified target")
		}
	}

	for _, key := range reservedVarKeys {
		if _, ok := f.Vars[key]; ok {
			return errors.New("vars carries reserved key " + key)
		}
	}
	return nil
}

// MemoryResultStore is a process-local ResultStore. It is the right default for
// a single City process given short assertion lifetimes: a restart forgets
// prior results, after which a duplicate delivery of a spent nonce is answered
// as a replay rejection rather than dispatched again — the safe direction. Swap
// in a shared store for cross-process durability.
type MemoryResultStore struct {
	mu   sync.Mutex
	seen map[string]storedResult
}

type storedResult struct {
	result Result
	until  time.Time
}

// NewMemoryResultStore returns an empty in-memory store.
func NewMemoryResultStore() *MemoryResultStore {
	return &MemoryResultStore{seen: make(map[string]storedResult)}
}

// Get returns the recorded result for nonce when one is live.
func (m *MemoryResultStore) Get(nonce string) (Result, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.seen[nonce]
	if !ok || time.Now().After(entry.until) {
		return Result{}, false
	}
	return entry.result, true
}

// Put records result for nonce until until, sweeping expired entries first so
// the map cannot grow without bound.
func (m *MemoryResultStore) Put(nonce string, result Result, until time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for k, e := range m.seen {
		if now.After(e.until) {
			delete(m.seen, k)
		}
	}
	m.seen[nonce] = storedResult{result: result, until: until}
}
