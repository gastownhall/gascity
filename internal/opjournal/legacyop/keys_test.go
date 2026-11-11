package legacyop

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// restartReq is a restart{fresh} request on gc-1 under key, kept in
// namespace: a session reset's with none, an in-session restart request's
// with its own.
func restartReq(key, namespace string) Request {
	return Request{
		Conversation: "gc-1", Principal: PrincipalOperator, Actor: opjournal.ActorOperator,
		Key: key, KeyNamespace: namespace, Kind: opjournal.KindRestart,
		Policy: opjournal.Policy{Restart: opjournal.RestartPolicy{Mode: opjournal.RestartFresh}},
	}
}

// ValidateKey refuses what Admit refuses of a client's key: an invalid key,
// one its namespace makes too long, named by its own length and the
// namespace's limit, a restart key kept as given that starts with a restart
// request's namespace, and a respond key in respond:, where a lane respond op
// is keyed by the raising it answers. A namespaced request's key, and another
// kind's, may start with anything.
func TestValidateKey(t *testing.T) {
	restart, k := opjournal.KindRestart, func(n int) string { return strings.Repeat("k", n) }
	respond := opjournal.KindRespond
	for _, tc := range []struct {
		name      string
		kind      opjournal.Kind
		namespace string
		key       string
		want      error
		message   string
	}{
		{name: "plain", kind: restart, key: "k1"},
		{name: "longest plain", kind: restart, key: k(opjournal.MaxClientKeyLen)},
		{name: "reset key in the handoff namespace", kind: restart, key: "handoff:k1", want: ErrReservedKey, message: `"handoff:k1" starts with "handoff:", which keeps gc handoff's keys`},
		{name: "reset key in the request-restart namespace", kind: restart, key: "request-restart:k1", want: ErrReservedKey, message: `which keeps gc runtime request-restart's keys`},
		{name: "a namespace past the start", kind: restart, key: "k1:handoff:k2"},
		{name: "handoff key spelling another namespace", kind: restart, namespace: KeyNamespaceHandoff, key: "request-restart:k1"},
		{name: "handoff key spelling its own", kind: restart, namespace: KeyNamespaceHandoff, key: "handoff:k1"},
		{name: "another kind's key", kind: opjournal.KindClose, key: "handoff:k1"},
		{name: "respond key in the lane's respond: namespace", kind: respond, key: "respond:gc-fact-1", want: ErrReservedKey, message: `"respond:gc-fact-1" starts with "respond:", which keys a lane respond op by the raising it answers`},
		{name: "respond key in a namespace that is not respond:", kind: respond, key: "Respond:k1"},
		{name: "respond key with respond: past the start", kind: respond, key: "k1:respond:k2"},
		{name: "respond key in a restart namespace", kind: respond, key: "handoff:k1"},
		{name: "restart key in respond:", kind: restart, key: "respond:k1"},
		{name: "another kind's key in respond:", kind: opjournal.KindClose, key: "respond:k1"},
		{name: "invalid in a namespace", kind: restart, namespace: KeyNamespaceHandoff, key: " k1", want: opjournal.ErrInvalidRequest, message: "leading or trailing space"},
		{name: "longest handoff key", kind: restart, namespace: KeyNamespaceHandoff, key: k(opjournal.MaxClientKeyLen - len(KeyNamespaceHandoff))},
		{name: "handoff key a byte too long", kind: restart, namespace: KeyNamespaceHandoff, key: k(505), want: opjournal.ErrInvalidRequest, message: `client key is 505 bytes, max 504 in the "handoff:" key namespace`},
		{name: "longest request-restart key", kind: restart, namespace: KeyNamespaceRequestRestart, key: k(opjournal.MaxClientKeyLen - len(KeyNamespaceRequestRestart))},
		{name: "request-restart key a byte too long", kind: restart, namespace: KeyNamespaceRequestRestart, key: k(497), want: opjournal.ErrInvalidRequest, message: `client key is 497 bytes, max 496 in the "request-restart:" key namespace`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateKey(tc.kind, tc.namespace, tc.key)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("ValidateKey = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) || !errors.Is(err, opjournal.ErrInvalidRequest) || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("ValidateKey = %v, want %v naming %q", err, tc.want, tc.message)
			}
			if reserved := errors.Is(err, ErrReservedKey); reserved != errors.Is(tc.want, ErrReservedKey) {
				t.Fatalf("ValidateKey = %v: reserved %t", err, reserved)
			}
		})
	}
}

// A namespaced request's op is stored under its key in its namespace, and
// the admission names the key as given; a minted key stays as minted. A
// restart request that keeps its key as given and starts it with a
// namespace never names that namespace's op: auto runs it unrecorded and
// require refuses it, counted invalid_key, the namespaced op untouched. A
// key its namespace makes too long is refused the same way.
func TestAdmitKeepsKeysInTheirNamespace(t *testing.T) {
	for _, surface := range []string{"api", "cli"} {
		recorder := func(e *env, mode gate.Mode) *Recorder {
			if surface == "api" {
				return apiRecorder(e, mode)
			}
			return cliRecorder(e, mode)
		}
		t.Run(surface+"/namespaced", func(t *testing.T) {
			e := newEnv(t)
			adm, err := recorder(e, gate.Auto).Admit(restartReq("k1", KeyNamespaceHandoff))
			if err != nil || !adm.Recorded || adm.Key != "k1" || adm.Minted || adm.Op.ClientKey != "handoff:k1" {
				t.Fatalf("Admit = %+v, %v; want handoff:k1 recorded, named k1", adm, err)
			}
			if want := mustPreviewID(t, restartReq("handoff:k1", "")); adm.Op.ID != want {
				t.Fatalf("op id %s, want %s", adm.Op.ID, want)
			}
			adm.Finish(Result{Outcome: succeeded(ViaLegacy)})
		})
		t.Run(surface+"/minted", func(t *testing.T) {
			e := newEnv(t)
			adm, err := recorder(e, gate.Auto).Admit(restartReq("", KeyNamespaceHandoff))
			if err != nil || !adm.Recorded || !adm.Minted || !strings.HasPrefix(adm.Key, mintedKeyPrefix) || adm.Op.ClientKey != adm.Key {
				t.Fatalf("Admit = %+v, %v; want the minted key stored as minted", adm, err)
			}
			adm.Finish(Result{Outcome: succeeded(ViaLegacy)})
		})
		for _, tc := range []struct {
			name string
			req  Request
			want error
		}{
			{"reserved", restartReq("handoff:k1", ""), ErrReservedKey},
			{"too long in its namespace", restartReq(strings.Repeat("k", 505), KeyNamespaceHandoff), opjournal.ErrInvalidRequest},
		} {
			for _, mode := range []gate.Mode{gate.Auto, gate.Require} {
				t.Run(surface+"/"+tc.name+"/"+string(mode), func(t *testing.T) {
					e := newEnv(t)
					handoff := e.seed(t, restartReq("handoff:k1", ""), ExecutorCLI)
					if _, err := e.st.Close(handoff.ID, succeeded(ViaLegacy), string(ExecutorCLI)); err != nil {
						t.Fatal(err)
					}
					before := len(e.rec.Ops())
					counts := captureCounts(t)
					adm, err := recorder(e, mode).Admit(tc.req)
					if mode == gate.Require {
						if adm != nil || !errors.Is(err, tc.want) {
							t.Fatalf("Admit = %+v, %v; want %v", adm, err, tc.want)
						}
					} else if err != nil || adm.Decision != Execute || adm.Recorded || !errors.Is(adm.Unrecordable, tc.want) || adm.Key != tc.req.Key {
						t.Fatalf("Admit = %+v, %v; want it run unrecorded for %v", adm, err, tc.want)
					}
					result := ResultUnrecorded
					if mode == gate.Require {
						result = ResultRefused
					}
					if got := counts(); len(got) != 1 || got[0] != (countedStep{"restart", surface, StageAdmit, result, ReasonInvalidKey}) {
						t.Fatalf("counts = %+v", got)
					}
					if n := len(e.rec.Ops()); n != before {
						t.Fatalf("Admit made %d store calls, want none", n-before)
					}
				})
			}
		}
	}
}
