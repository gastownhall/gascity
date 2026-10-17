package main

// One opened leg set per work query: the ready session.
//
// A fresh seat's claim runs its generated work query once, and on a split city
// that query asks `gc ready` five to eight questions in sequence: in-progress
// work for each identity, assigned work for each identity, then the routed pool
// tier. Every `gc ready` is its own process, so every one of them opened the
// city store, each serving rig store and the relocated binding again — most of
// the cost of every question after the first.
//
// So `gc hook` runs that query under `gc internal ready-session`, which holds a
// session for exactly the lifetime of the one query: a listener the query's
// `gc ready` processes ask instead of opening stores themselves. The session
// opens the legs on the first question and answers every later one from the
// same opened stores, with a fresh read each time through the same reader
// (readyBeadsForOpts) rendered to the same bytes. It runs as a child of the
// hook with the query's own environment and working directory, so the stores it
// opens are the ones each `gc ready` would have opened.
//
// It is an optimization and never an outage, so every way it can fail ends in
// the in-process read: no session on a platform without abstract unix sockets,
// a question about another city, a session that cannot be reached. A read that
// FAILS inside the session is answered exactly like an in-process failure, and
// the session then drops its legs and closes the stores it opened them with, so
// the next question opens fresh stores — what the next `gc ready` process would
// have done.

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

const (
	// readySessionRequestEnv, set to "1" in a work query's environment, asks
	// shellWorkQueryWithEnv to run that query inside a ready session. `gc hook`
	// sets it (hookUsesReadySession); asking through the environment rather than
	// through a different runner means every runner that ends in
	// shellWorkQueryWithEnv honors it, whatever env it projects on the way.
	readySessionRequestEnv = "GC_WORK_QUERY_READY_SESSION"
	// readySessionAddrEnv carries the session's address to the `gc ready`
	// processes of the work query it serves.
	readySessionAddrEnv = "GC_READY_SESSION"
	// readySessionTokenEnv carries the secret every question must present. An
	// abstract socket has no file permissions, so the token — readable only in
	// the environment of this user's own processes — is what keeps another
	// local user from asking this city's stores questions.
	readySessionTokenEnv = "GC_READY_SESSION_TOKEN"
	// readySessionDialTimeout bounds connecting to a session on this host. It
	// is a hang guard, not a latency budget: a live session accepts at once.
	readySessionDialTimeout = 2 * time.Second
)

// readySessionQuestionTimeout bounds how long a connection may take to ask its
// question. An asker writes its question as it connects, so this is a hang
// guard rather than a latency budget: anyone on the host can reach an abstract
// socket, and a peer that connects and never asks would otherwise hold its
// handler — and the close that waits for it — forever. A variable so tests can
// shorten it.
var readySessionQuestionTimeout = 2 * time.Second

// hookUsesReadySession reports whether `gc hook` runs a's work query inside a
// ready session: the generated query of a split city, whose tiers ask `gc
// ready` several questions in sequence, on a platform that can run one. An
// operator's own work_query runs exactly as written, and a single-store city's
// generated query asks bd, not `gc ready`.
func hookUsesReadySession(a *config.Agent, topo config.QueryTopology) bool {
	return topo.FederatedReady && a != nil && strings.TrimSpace(a.WorkQuery) == "" && readySessionsSupported()
}

// withReadySessionRequest returns stores with readySessionRequestEnv set in
// each store's env, the env its work query runs with. The store set is built
// per leg — a rig-scoped agent's primary leg carries the rig's env, not the
// hook's — so the request is added where the query will read it, on copies.
func withReadySessionRequest(stores []hookStore) []hookStore {
	out := make([]hookStore, len(stores))
	for i, st := range stores {
		st.env = append(removeEnvKey(st.env, readySessionRequestEnv), readySessionRequestEnv+"=1")
		out[i] = st
	}
	return out
}

// errReadySessionUnsupported reports a platform with no abstract unix sockets.
var errReadySessionUnsupported = errors.New("ready sessions need abstract unix sockets")

// readySessionsSupported reports whether this platform can run a ready
// session. Sessions listen on an abstract unix socket, which only Linux has: it
// is not a file, so a session killed together with its work query — the hook
// SIGKILLs the whole process group when a query times out — leaves nothing
// behind.
func readySessionsSupported() bool { return runtime.GOOS == "linux" }

// readySessionListen opens a session's listener and returns its address. A
// variable so tests can make it fail.
var readySessionListen = listenReadySession

func listenReadySession() (net.Listener, string, error) {
	if !readySessionsSupported() {
		return nil, "", errReadySessionUnsupported
	}
	nonce, err := randomHex(12)
	if err != nil {
		return nil, "", err
	}
	addr := "@gc-ready-session-" + nonce
	ln, err := net.Listen("unix", addr)
	if err != nil {
		return nil, "", fmt.Errorf("listening on %s: %w", addr, err)
	}
	return ln, addr, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// readySessionRequest is one `gc ready` question: the asker's resolved city and
// its parsed flags.
type readySessionRequest struct {
	Token          string   `json:"token"`
	CityPath       string   `json:"city_path"`
	Assignee       string   `json:"assignee,omitempty"`
	Unassigned     bool     `json:"unassigned,omitempty"`
	MetadataFields []string `json:"metadata_fields,omitempty"`
	ExcludeTypes   []string `json:"exclude_types,omitempty"`
	ExcludeLabels  []string `json:"exclude_labels,omitempty"`
	SortOrder      string   `json:"sort_order,omitempty"`
	Limit          int      `json:"limit,omitempty"`
	Status         string   `json:"status,omitempty"`
}

func readySessionRequestFor(token, cityPath string, opts readyOpts) readySessionRequest {
	return readySessionRequest{
		Token:          token,
		CityPath:       cityPath,
		Assignee:       opts.assignee,
		Unassigned:     opts.unassigned,
		MetadataFields: opts.metadataFields,
		ExcludeTypes:   opts.excludeTypes,
		ExcludeLabels:  opts.excludeLabels,
		SortOrder:      opts.sortOrder,
		Limit:          opts.limit,
		Status:         opts.status,
	}
}

func (r readySessionRequest) readyOpts() readyOpts {
	return readyOpts{
		assignee:       r.Assignee,
		unassigned:     r.Unassigned,
		metadataFields: r.MetadataFields,
		excludeTypes:   r.ExcludeTypes,
		excludeLabels:  r.ExcludeLabels,
		sortOrder:      r.SortOrder,
		limit:          r.Limit,
		status:         r.Status,
	}
}

// readySessionResponse is the session's answer to one question.
type readySessionResponse struct {
	// Served is false when the session declined the question — another city,
	// or a token that is not this session's — and the asker reads in-process.
	Served bool `json:"served"`
	// Output is exactly what the in-process read writes to stdout.
	Output []byte `json:"output,omitempty"`
	// Error is the failure the in-process read prints as "gc ready: <Error>".
	Error string `json:"error,omitempty"`
}

// readySession answers one work query's `gc ready` questions from one opened
// leg set.
type readySession struct {
	addr     string
	token    string
	cityPath string
	listener net.Listener
	stderr   io.Writer
	done     chan struct{}

	// connMu guards conns: every connection a handler still holds, which serve
	// drops once it stops accepting.
	connMu sync.Mutex
	conns  map[net.Conn]struct{}

	mu sync.Mutex
	// legs is the opened leg set: nil before the first question, and again
	// after any failed read, so the next question opens fresh stores.
	legs []readyLeg
	// owned is what opening legs opened, closed when they are dropped; see
	// openReadyLegs for why that is not every leg's store.
	owned []beads.Store
}

// startReadySession resolves this process's city — the city the work query's
// `gc ready` processes resolve, since they run with its environment and working
// directory — and starts serving questions about it.
func startReadySession(stderr io.Writer) (*readySession, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return nil, fmt.Errorf("resolving the city: %w", err)
	}
	token, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	ln, addr, err := readySessionListen()
	if err != nil {
		return nil, err
	}
	return serveReadySession(ln, addr, token, cityPath, stderr), nil
}

// serveReadySession starts answering questions about cityPath on ln.
func serveReadySession(ln net.Listener, addr, token, cityPath string, stderr io.Writer) *readySession {
	s := &readySession{addr: addr, token: token, cityPath: cityPath, listener: ln, stderr: stderr, done: make(chan struct{}), conns: map[net.Conn]struct{}{}}
	go s.serve()
	return s
}

// env is what a process needs in its environment to ask this session.
func (s *readySession) env() []string {
	return []string{readySessionAddrEnv + "=" + s.addr, readySessionTokenEnv + "=" + s.token}
}

// serve answers each connection on its own handler until the listener closes,
// then drops every connection still open and waits for the handlers to return.
// The query has exited by then, so nothing still connected has a question worth
// waiting for: a peer that never asked, or an asker the query left behind,
// which reads in-process.
func (s *readySession) serve() {
	defer close(s.done)
	var handlers sync.WaitGroup
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			break
		}
		s.trackConn(conn)
		handlers.Go(func() {
			defer s.untrackConn(conn)
			s.handle(conn)
		})
	}
	s.dropConns()
	handlers.Wait()
}

// trackConn records conn as held by a handler, for dropConns.
func (s *readySession) trackConn(conn net.Conn) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.conns[conn] = struct{}{}
}

// untrackConn forgets conn once its handler is done with it.
func (s *readySession) untrackConn(conn net.Conn) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	delete(s.conns, conn)
}

// dropConns closes every connection a handler still holds, which ends a
// pending read or write on it at once.
func (s *readySession) dropConns() {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	for conn := range s.conns {
		conn.Close() //nolint:errcheck // dropping it is the point
	}
}

// handle answers the one question a connection carries. A connection that
// breaks mid-exchange is the asker's to notice: it reads in-process.
func (s *readySession) handle(conn net.Conn) {
	defer conn.Close() //nolint:errcheck // the exchange is over either way
	// Nothing reads conn after the question, so the deadline bounds only the
	// asking, never the answer's store opens and reads.
	if err := conn.SetReadDeadline(time.Now().Add(readySessionQuestionTimeout)); err != nil {
		s.debugf("bounding a question: %v", err)
		return
	}
	var req readySessionRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		s.debugf("reading a question: %v", err)
		return
	}
	if err := json.NewEncoder(conn).Encode(s.answer(req)); err != nil {
		s.debugf("sending an answer: %v", err)
	}
}

// answer answers one question through the session's legs, opening them first
// if they are not open. Questions are answered one at a time: the work query
// asks them in sequence, and the legs read concurrently inside each one.
func (s *readySession) answer(req readySessionRequest) readySessionResponse {
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.token)) != 1 || req.CityPath != s.cityPath {
		return readySessionResponse{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.legs == nil {
		legs, owned, err := openReadyLegs(s.cityPath, s.stderr)
		if err != nil {
			return readySessionResponse{Served: true, Error: err.Error()}
		}
		s.debugf("opened %d legs for %s", len(legs), s.cityPath)
		s.legs, s.owned = legs, owned
	}
	var out bytes.Buffer
	if err := writeReadyAnswer(&out, s.legs, req.readyOpts()); err != nil {
		if closeErr := s.dropLegs(); closeErr != nil {
			s.debugf("closing the legs of a failed read: %v", closeErr)
		}
		return readySessionResponse{Served: true, Error: err.Error()}
	}
	return readySessionResponse{Served: true, Output: out.Bytes()}
}

// dropLegs closes the stores the legs were opened with and forgets the legs,
// so the next question opens fresh ones. The caller holds s.mu.
func (s *readySession) dropLegs() error {
	err := closeReadyStores(s.owned)
	s.legs, s.owned = nil, nil
	return err
}

// close stops accepting questions, drops every connection still open and waits
// for the handlers to return — one still answering finishes its read first —
// then closes the stores the session opened.
func (s *readySession) close() error {
	err := s.listener.Close()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(err, s.dropLegs())
}

// debugf prints a session diagnostic under GC_DEBUG. Session diagnostics go
// to the work query's stderr, which the hook classifies on failure, so they
// stay off it unless asked for.
func (s *readySession) debugf(format string, args ...any) {
	if gcDebugEnabled() {
		fmt.Fprintf(s.stderr, "gc ready session: "+format+"\n", args...) //nolint:errcheck // best-effort stderr
	}
}

// readyThroughSession asks this process's ready session the question, when it
// runs under one. It reports served=false — and the caller reads in-process —
// when there is no session, when the session declines (another city), or when
// the session cannot be reached, which it says on stderr without the dial
// error's text: the hook classifies a failed query by markers such as
// "connection refused", and a session it could not reach is not a store it
// could not read. GC_DEBUG adds the error.
func readyThroughSession(cityPath string, opts readyOpts, stdout, stderr io.Writer) (code int, served bool) {
	addr := strings.TrimSpace(os.Getenv(readySessionAddrEnv))
	if addr == "" {
		return 0, false
	}
	resp, err := askReadySession(addr, readySessionRequestFor(os.Getenv(readySessionTokenEnv), cityPath, opts))
	if err != nil {
		fmt.Fprintln(stderr, "gc ready: the work query's ready session did not answer; reading the stores directly") //nolint:errcheck // best-effort stderr
		if gcDebugEnabled() {
			fmt.Fprintf(stderr, "gc ready: ready session %s: %v\n", addr, err) //nolint:errcheck // best-effort stderr
		}
		return 0, false
	}
	if !resp.Served {
		return 0, false
	}
	if gcDebugEnabled() {
		fmt.Fprintln(stderr, "gc ready: answered by the work query's ready session") //nolint:errcheck // best-effort stderr
	}
	if resp.Error != "" {
		fmt.Fprintf(stderr, "gc ready: %s\n", resp.Error) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	if _, err := stdout.Write(resp.Output); err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	return 0, true
}

func askReadySession(addr string, req readySessionRequest) (readySessionResponse, error) {
	conn, err := net.DialTimeout("unix", addr, readySessionDialTimeout)
	if err != nil {
		return readySessionResponse{}, err
	}
	defer conn.Close() //nolint:errcheck // the exchange is over either way
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return readySessionResponse{}, fmt.Errorf("sending the question: %w", err)
	}
	var resp readySessionResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return readySessionResponse{}, fmt.Errorf("reading the answer: %w", err)
	}
	return resp, nil
}

// runReadySession runs argv — the work query — inside one ready session, and
// returns argv's exit status. Its stdin, stdout and stderr are the query's.
// When no session can start the query runs without one, as it would have
// without the wrapper; only a failure on a platform that supports sessions is
// worth saying.
func runReadySession(argv []string, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "gc internal ready-session: no work query to run") //nolint:errcheck // best-effort stderr
		return 2
	}
	var sessionEnv []string
	session, err := startReadySession(stderr)
	switch {
	case err == nil:
		sessionEnv = session.env()
		defer func() {
			if err := session.close(); err != nil {
				session.debugf("closing: %v", err)
			}
		}()
	case errors.Is(err, errReadySessionUnsupported):
	default:
		fmt.Fprintf(stderr, "gc internal ready-session: no ready session (%v); the work query's gc ready calls read the stores directly\n", err) //nolint:errcheck // best-effort stderr
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = append(removeEnvKey(os.Environ(), readySessionRequestEnv), sessionEnv...)
	return readySessionExitStatus(cmd.Run(), argv[0], stderr)
}

// readySessionExitStatus is the shell's rendering of how the query ended: its
// own exit code, 128+N for a signal, 127 for a query that could not start.
func readySessionExitStatus(err error, name string, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exitErr.ExitCode()
	}
	fmt.Fprintf(stderr, "gc internal ready-session: running %s: %v\n", name, err) //nolint:errcheck // best-effort stderr
	return 127
}

// newInternalReadySessionCmd is `gc internal ready-session -- <query...>`: the
// wrapper `gc hook` runs a split city's generated work query under. See the
// top of this file.
func newInternalReadySessionCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:    "ready-session -- <command> [args...]",
		Short:  "Run a work query inside one ready session for its gc ready calls",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if code := runReadySession(args, stdout, stderr); code != 0 {
				return &commandExitError{code: code}
			}
			return nil
		},
	}
}
