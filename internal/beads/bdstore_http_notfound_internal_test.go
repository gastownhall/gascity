package beads

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// bdShowNotFoundEnvelope is bd show's --json stdout whenever none of the
// requested IDs resolved. bd prints this one sentence whatever the per-ID
// cause, so a missing bead and an existing bead whose lookup failed produce
// the same bytes.
const bdShowNotFoundEnvelope = `{"error": "no issues found matching the provided IDs", "schema_version": 1}`

// bdShowHTTPRefusal is bd show's error against a remote bd serve
// (BEADS_SERVER_URL). Its stderr never says "not found", for a missing bead
// and a failed lookup alike.
const bdShowHTTPRefusal = "exit status 1: bd show is not supported against HTTP workspace http://100.64.0.1:7491: part of this command is not served over HTTP; run it in a local workspace"

// bdShowFailedSearch is a direct-mode bd show error for an existing bead whose
// lookup hit a dead Dolt server. bd still prints the not-found envelope.
func bdShowFailedSearch(id string) string {
	return "exit status 1: Error fetching " + id + ": failed to search issues: dial tcp 127.0.0.1:3307: connect: connection refused"
}

func bdShowCmd(id string) string { return "show --json " + id }

func bdWispQueryCmd(id string) string {
	return "query --json ephemeral=true AND id=" + id + " --all --limit 1"
}

// bdExactIDQueryCmd is the absence confirmation, which sends id as a quoted
// literal so that bd's query lexer reads it verbatim.
func bdExactIDQueryCmd(id string) string { return `query --json id="` + id + `" --all --limit 1` }

// bdCanned is one canned bd reply: stdout, plus a non-empty err when the
// command fails.
type bdCanned struct {
	out string
	err string
}

// cannedBdRunner records every bd command and answers it from replies, keyed
// by the space-joined argv. A command without a canned reply fails the test,
// so an extra or reshaped query cannot pass unnoticed.
func cannedBdRunner(t *testing.T, replies map[string]bdCanned) *recordingRunner {
	t.Helper()
	return &recordingRunner{reply: func(args []string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		reply, ok := replies[cmd]
		if !ok {
			t.Errorf("unexpected command: bd %s", cmd)
			return nil, fmt.Errorf("unexpected command: bd %s", cmd)
		}
		if reply.err != "" {
			return []byte(reply.out), errors.New(reply.err)
		}
		return []byte(reply.out), nil
	}}
}

// bdCommands returns the commands r recorded, as the space-joined argv
// without the program name that cannedBdRunner keys its replies by.
func bdCommands(r *recordingRunner) []string {
	cmds := make([]string, 0, len(r.calls))
	for _, call := range r.calls {
		cmds = append(cmds, strings.Join(call[1:], " "))
	}
	return cmds
}

// TestBdStoreGetTreatsHTTPNotFoundEnvelopeAsNotFound: against a remote bd
// serve (BEADS_SERVER_URL), `bd show <missing-id> --json` exits 1 with the
// not-found envelope on stdout and only an unrelated "not served over HTTP"
// line on stderr. Classifying from stderr alone turned a plain absent bead
// into a hard error, which broke session address resolution (mail, handoff,
// prime) for remote workers. The envelope cannot prove absence by itself, so
// Get confirms it with an exact-ID bd query that bd serve answers with [] for
// a miss, after the wisp fallback for IDs eligible for one.
func TestBdStoreGetTreatsHTTPNotFoundEnvelopeAsNotFound(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		wisp bool // Get must run the wisp fallback query first
	}{
		{name: "bare name", id: "mayor", wisp: true},
		{name: "bead id", id: "ga-zz9p", wisp: true},
		{name: "child bead id", id: "ga-abc.9"},
		{name: "slash session name", id: "gascity/worker-1"},
		{name: "runtime session name", id: "gascity--gc__worker-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := cannedBdRunner(t, map[string]bdCanned{
				bdShowCmd(tc.id):         {out: bdShowNotFoundEnvelope, err: bdShowHTTPRefusal},
				bdWispQueryCmd(tc.id):    {out: "[]"},
				bdExactIDQueryCmd(tc.id): {out: "[]"},
			})
			_, err := NewBdStore("/city", runner.run).Get(tc.id)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get(%q) err = %v, want ErrNotFound", tc.id, err)
			}
			if !strings.Contains(err.Error(), "not served over HTTP") {
				t.Errorf("Get(%q) err = %v, want it to keep the bd show error that triggered the verdict", tc.id, err)
			}
			want := []string{bdShowCmd(tc.id)}
			if tc.wisp {
				want = append(want, bdWispQueryCmd(tc.id))
			}
			want = append(want, bdExactIDQueryCmd(tc.id))
			if got := bdCommands(runner); !slices.Equal(got, want) {
				t.Errorf("Get(%q) ran %q, want %q", tc.id, got, want)
			}
		})
	}
}

// A genuine failure with no not-found envelope must stay an error. Only bd
// show fails here: the queries a miss would run answer [], so accepting this
// envelope as a miss would end in ErrNotFound.
func TestBdStoreGetKeepsRealFailures(t *testing.T) {
	runner := cannedBdRunner(t, map[string]bdCanned{
		bdShowCmd("ci-1"):         {out: `{"error": "listReadyWork: bd serve answered 401 unauthenticated"}`, err: "exit status 1"},
		bdWispQueryCmd("ci-1"):    {out: "[]"},
		bdExactIDQueryCmd("ci-1"): {out: "[]"},
	})
	_, err := NewBdStore("/city", runner.run).Get("ci-1")
	if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "getting bead") {
		t.Fatalf("Get err = %v, want a non-NotFound error", err)
	}
	if got, want := bdCommands(runner), []string{bdShowCmd("ci-1")}; !slices.Equal(got, want) {
		t.Errorf("Get ran %q, want only %q", got, want)
	}
}

// Only bd's not-found sentence makes an envelope a miss signal, and even that
// one is just a trigger for confirmation. An infrastructure "not found" in the
// envelope (a Dolt database mid-restart) says nothing about the bead. Callers
// treat ErrNotFound as confirmed absence (the orphan sweep acts on it), so Get
// must return the bd show error without querying further.
func TestBdStoreGetKeepsInfraNotFoundEnvelopesAsErrors(t *testing.T) {
	runner := cannedBdRunner(t, map[string]bdCanned{
		bdShowCmd("ci-1"):         {out: `{"error": "database not found: hq"}`, err: "exit status 1"},
		bdWispQueryCmd("ci-1"):    {out: "[]"},
		bdExactIDQueryCmd("ci-1"): {out: "[]"},
	})
	_, err := NewBdStore("/city", runner.run).Get("ci-1")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get err = %v, want an infrastructure error, not ErrNotFound", err)
	}
	if got, want := bdCommands(runner), []string{bdShowCmd("ci-1")}; !slices.Equal(got, want) {
		t.Errorf("Get ran %q, want only %q", got, want)
	}
}

// bd prints the same not-found envelope when the lookup of an existing bead
// fails: a dead bd serve or Dolt server, a rejected token. Unless the exact-ID
// query succeeds with no rows, absence is unproven and Get must return the bd
// show error, never ErrNotFound. Callers act on ErrNotFound as confirmed
// absence: the hook-claim fence drains the worker and the cache emits
// bead.closed.
func TestBdStoreGetUnconfirmedNotFoundEnvelopeIsNotErrNotFound(t *testing.T) {
	const (
		deadServer = "exit status 1: dial tcp 100.64.0.1:7491: connect: connection refused"
		deadDolt   = "exit status 1: dial tcp 127.0.0.1:3307: connect: connection refused"
	)
	for _, tc := range []struct {
		name    string
		id      string
		showErr string
		wisp    bool      // Get must run the wisp fallback query, which answers []
		confirm *bdCanned // the exact-ID query's reply; nil when Get must not run it
	}{
		{
			name: "remote dead server, child bead id", id: "ga-abc.1", showErr: bdShowHTTPRefusal,
			confirm: &bdCanned{err: deadServer},
		},
		{
			name: "remote rejected token, slash session name", id: "gascity/worker-1", showErr: bdShowHTTPRefusal,
			confirm: &bdCanned{out: `{"error": "unauthorized"}`, err: "exit status 1: unauthorized"},
		},
		{
			name: "remote dead server, bead id with an empty wisp query", id: "ga-abc", showErr: bdShowHTTPRefusal,
			wisp: true, confirm: &bdCanned{err: deadServer},
		},
		{
			name: "direct failed search, query errors", id: "ga-abc.1", showErr: bdShowFailedSearch("ga-abc.1"),
			confirm: &bdCanned{err: deadDolt},
		},
		{
			name: "direct failed search, bead id with an empty wisp query", id: "ga-abc", showErr: bdShowFailedSearch("ga-abc"),
			wisp: true, confirm: &bdCanned{err: deadDolt},
		},
		{
			name: "direct failed search, query finds the bead", id: "ga-abc.1", showErr: bdShowFailedSearch("ga-abc.1"),
			confirm: &bdCanned{out: `[{"id":"ga-abc.1","title":"live","status":"open","issue_type":"task"}]`},
		},
		{
			name: "query succeeds with no output", id: "ga-abc.1", showErr: bdShowHTTPRefusal,
			confirm: &bdCanned{},
		},
		{
			name: "id with a quote the literal cannot carry", id: `gascity/wor"ker-1`, showErr: bdShowHTTPRefusal,
		},
		{
			name: "non-ASCII id bd would re-encode", id: "gascity/w\u00f6rker-1", showErr: bdShowHTTPRefusal,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replies := map[string]bdCanned{
				bdShowCmd(tc.id): {out: bdShowNotFoundEnvelope, err: tc.showErr},
			}
			want := []string{bdShowCmd(tc.id)}
			if tc.wisp {
				replies[bdWispQueryCmd(tc.id)] = bdCanned{out: "[]"}
				want = append(want, bdWispQueryCmd(tc.id))
			}
			if tc.confirm != nil {
				replies[bdExactIDQueryCmd(tc.id)] = *tc.confirm
				want = append(want, bdExactIDQueryCmd(tc.id))
			}
			runner := cannedBdRunner(t, replies)
			_, err := NewBdStore("/city", runner.run).Get(tc.id)
			if err == nil {
				t.Fatalf("Get(%q) succeeded, want the bd show error", tc.id)
			}
			if errors.Is(err, ErrNotFound) {
				t.Fatalf("Get(%q) err = %v; an unconfirmed envelope must not read as ErrNotFound", tc.id, err)
			}
			if !strings.Contains(err.Error(), tc.showErr) {
				t.Errorf("Get(%q) err = %v, want the bd show error %q", tc.id, err, tc.showErr)
			}
			if tc.confirm != nil && tc.confirm.err != "" && !strings.Contains(err.Error(), tc.confirm.err) {
				t.Errorf("Get(%q) err = %v, want the failed query's error %q alongside", tc.id, err, tc.confirm.err)
			}
			if got := bdCommands(runner); !slices.Equal(got, want) {
				t.Errorf("Get(%q) ran %q, want %q", tc.id, got, want)
			}
		})
	}
}

// The exact-ID query matches wisps too, so it must run after the wisp
// fallback. A wisp-tier bead that bd show cannot see must come back from the
// fallback, not turn into a hard error because the confirmation found a row.
func TestBdStoreGetNotFoundEnvelopeFindsWispBeforeConfirming(t *testing.T) {
	const id = "gc-wisp-abc"
	row := `[{"id":"gc-wisp-abc","title":"context cycle","status":"open","issue_type":"message","ephemeral":true}]`
	runner := cannedBdRunner(t, map[string]bdCanned{
		bdShowCmd(id):         {out: bdShowNotFoundEnvelope, err: bdShowHTTPRefusal},
		bdWispQueryCmd(id):    {out: row},
		bdExactIDQueryCmd(id): {out: row},
	})
	b, err := NewBdStore("/city", runner.run).Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	if b.ID != id || !b.Ephemeral {
		t.Errorf("Get(%q) = %+v, want the ephemeral wisp bead", id, b)
	}
	if got, want := bdCommands(runner), []string{bdShowCmd(id), bdWispQueryCmd(id)}; !slices.Equal(got, want) {
		t.Errorf("Get(%q) ran %q, want %q", id, got, want)
	}
}

// A direct-mode miss names the bead on stderr, which is a verdict by itself.
// Get keeps returning a plain ErrNotFound for it and runs no exact-ID query,
// even though bd also printed the not-found envelope.
func TestBdStoreGetStderrMissSkipsConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		wisp bool // Get must run the wisp fallback query, which answers []
	}{
		{name: "child bead id", id: "ga-abc.9"},
		{name: "bead id with an empty wisp query", id: "ga-abc", wisp: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replies := map[string]bdCanned{
				bdShowCmd(tc.id): {
					out: bdShowNotFoundEnvelope,
					err: fmt.Sprintf("exit status 1: Error fetching %s: no issue found matching %q", tc.id, tc.id),
				},
			}
			want := []string{bdShowCmd(tc.id)}
			if tc.wisp {
				replies[bdWispQueryCmd(tc.id)] = bdCanned{out: "[]"}
				want = append(want, bdWispQueryCmd(tc.id))
			}
			runner := cannedBdRunner(t, replies)
			_, err := NewBdStore("/city", runner.run).Get(tc.id)
			wantErr := fmt.Sprintf("getting bead %q: %v", tc.id, ErrNotFound)
			if !errors.Is(err, ErrNotFound) || err.Error() != wantErr {
				t.Fatalf("Get(%q) err = %v, want %q", tc.id, err, wantErr)
			}
			if got := bdCommands(runner); !slices.Equal(got, want) {
				t.Errorf("Get(%q) ran %q, want %q", tc.id, got, want)
			}
		})
	}
}

// isExactIDQueryable admits every printable-ASCII ID, which a quoted bd query
// literal carries verbatim. It refuses the quote that ends the literal, the
// backslash that starts an escape, non-ASCII bytes, which bd's byte-at-a-time
// lexer re-encodes, and control characters.
func TestIsExactIDQueryable(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{id: "ga-abc", want: true},
		{id: "ga-abc.1", want: true},
		{id: "gascity/worker-1", want: true},
		{id: "gascity--gc__worker-1", want: true},
		{id: "gascity/worker 1", want: true},
		{id: "AND", want: true},
		{id: "it's", want: true},
		{id: "ga-abc*", want: true},
		{id: " ~", want: true},
		{id: "", want: false},
		{id: `gascity/wor"ker-1`, want: false},
		{id: `gascity\worker-1`, want: false},
		{id: "ga-abc\t1", want: false},
		{id: "ga-abc\x1f", want: false},
		{id: "ga-abc\x7f", want: false},
		{id: "gascity/w\u00f6rker-1", want: false},
		{id: "ga-abc\xff", want: false},
	} {
		if got := isExactIDQueryable(tc.id); got != tc.want {
			t.Errorf("isExactIDQueryable(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
