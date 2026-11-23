package main

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The fixtures model two cities on one shared store: city B guards rig
// "beta", city A guards rig "alpha". Both cities' session stores use the same
// id prefix, so their store-minted short ids collide and only the 128-bit
// pool tokens tell their pool sessions apart.
const (
	tokenB      = "0123456789abcdef0123456789abcdef"
	tokenA      = "fedcba9876543210fedcba9876543210"
	tokenB2     = "00112233445566778899aabbccddeeff"
	poolIDB     = "gcx-session-" + tokenB
	poolIDA     = "gcx-session-" + tokenA
	poolIDB2    = "gcx-session-" + tokenB2
	poolNameB   = "crew.worker-" + poolIDB
	poolNameA   = "crew.worker-" + poolIDA
	shortIDB    = "gcx-17"
	templateB   = "beta/crew.worker"
	templateA   = "alpha/crew.worker"
	cityPathB   = "/cities/b"
	cityPathA   = "/cities/a"
	ownerStampB = "beta/"
)

func rnsGuardedRig(name string) config.Rig {
	return config.Rig{Name: name, Federation: config.RigFederationRig, Namespace: config.RigNamespaceExclusive}
}

// rnsSessionBead is a session record as the city's session store holds it.
func rnsSessionBead(id, template, sessionName string) beads.Bead {
	md := map[string]string{"template": template}
	if sessionName != "" {
		md["session_name"] = sessionName
	}
	return beads.Bead{ID: id, Type: sessionpkg.BeadType, Status: "open", Labels: []string{sessionpkg.LabelSession}, Metadata: md}
}

// rnsCountingStore is a session store that counts reads and can fail them.
type rnsCountingStore struct {
	beads.Store
	gets   atomic.Int64
	lists  atomic.Int64
	prefix string
	mu     sync.Mutex
	err    error
}

// IDPrefix reports the store's session id prefix, as a bd-backed sessions
// store does; "" means it has none.
func (s *rnsCountingStore) IDPrefix() string { return s.prefix }

func (s *rnsCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists.Add(1)
	s.mu.Lock()
	err := s.err
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.Store.List(q)
}

func (s *rnsCountingStore) Get(id string) (beads.Bead, error) {
	s.gets.Add(1)
	s.mu.Lock()
	err := s.err
	s.mu.Unlock()
	if err != nil {
		return beads.Bead{}, err
	}
	return s.Store.Get(id)
}

func (s *rnsCountingStore) setErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

func rnsNewCountingStore(records ...beads.Bead) *rnsCountingStore {
	return &rnsCountingStore{Store: beads.NewMemStoreFrom(100, records, nil), prefix: "gcx"}
}

func rnsStoreOpener(s beads.Store) func() (beads.SessionStore, error) {
	return func() (beads.SessionStore, error) { return beadstest.Sessions(s), nil }
}

// rnsCityBResolver is city B's resolver over B's own session records: its pool
// session (open, in the snapshot), a second pool session that is closed (in
// the store only), and a short-id session of a rig agent.
func rnsCityBResolver(t *testing.T) (*citySessionResolver, *rnsCountingStore) {
	t.Helper()
	store := rnsNewCountingStore(
		rnsSessionBead(poolIDB, templateB, poolNameB),
		rnsSessionBead(poolIDB2, templateB, "crew.worker-"+poolIDB2),
		rnsSessionBead(shortIDB, templateB, "beta--crew__worker"),
	)
	return newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store)), store
}

func rnsCityBNamespace(t *testing.T) (*rigNamespace, *rnsCountingStore) {
	t.Helper()
	res, store := rnsCityBResolver(t)
	ns, ok := newRigNamespace(rnsGuardedRig("beta"), res)
	if !ok {
		t.Fatal("guarded rig beta returned no namespace")
	}
	return ns, store
}

func rnsRow(assignee string, md map[string]string) beads.Bead {
	return beads.Bead{ID: "bt-1", Type: "task", Status: "open", Assignee: assignee, Metadata: md}
}

func rnsMD(kv ...string) map[string]string {
	m := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// TestRigNamespaceWorkedExamples pins the federation guard's worked examples:
// city B, guarded rig beta, and city A with rig alpha on the same store.
func TestRigNamespaceWorkedExamples(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	cases := []struct {
		name string
		bead beads.Bead
		want rowClass
	}{
		{"routed qualified inside", rnsRow("", rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.intake")), rowOwned},
		{"routed bare", rnsRow("", rnsMD(beadmeta.RoutedToMetadataKey, "crew.intake")), rowForeign},
		{"routed other rig", rnsRow("", rnsMD(beadmeta.RoutedToMetadataKey, "alpha/crew.intake")), rowForeign},
		{"assignee own pool session", rnsRow(poolIDB, nil), rowOwned},
		{"assignee other city's pool session", rnsRow(poolIDA, nil), rowForeign},
		{"assignee short session id", rnsRow("gcs-17", nil), rowForeign},
		{"assignee bare agent", rnsRow("scout", nil), rowForeign},
		{"assignee person", rnsRow("alice", nil), rowForeign},
		{"graph-first mint", rnsRow("", rnsMD(beadmeta.DeferredRoutedToMetadataKey, "beta/crew.worker", beadmeta.RunTargetMetadataKey, "crew.worker")), rowOwned},
		{"detached orphan own stamp", rnsRow("", rnsMD(beadmeta.SessionIDMetadataKey, poolIDB)), rowOwned},
		{"detached orphan other city's stamp", rnsRow("", rnsMD(beadmeta.SessionIDMetadataKey, poolIDA)), rowForeign},
		{"order tracking own creation stamp", rnsRow("", rnsMD(beadmeta.OwnerNamespaceMetadataKey, ownerStampB)), rowOwned},
		{"no keys", rnsRow("", rnsMD("title_hint", "x")), rowUnattributed},
		{"routed case variant", rnsRow("", rnsMD(beadmeta.RoutedToMetadataKey, "Beta/crew.intake")), rowForeign},
		{"routed session spelling", rnsRow("", rnsMD(beadmeta.RoutedToMetadataKey, "beta--crew__intake")), rowOwned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ns.classify(tc.bead); got != tc.want {
				t.Fatalf("classify = %s, want %s (%+v)", got, tc.want, ns.classifyRow(tc.bead))
			}
		})
	}
}

func TestRigNamespaceClassifyRules(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	cases := []struct {
		name     string
		bead     beads.Bead
		want     rowClass
		wantRule rowRule
		wantKey  string
	}{
		// Owner keys: every non-empty value must be admitted.
		{"person claim on own route", rnsRow("alice", rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker")), rowForeign, rowRuleOwnerKeys, rowAssigneeKey},
		{"own claim on own route", rnsRow("beta/crew.worker", rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker")), rowOwned, rowRuleOwnerKeys, ""},
		{"pool claim on own route", rnsRow(poolIDB, rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker")), rowOwned, rowRuleOwnerKeys, ""},
		{"execution route outside", rnsRow("", rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker", beadmeta.ExecutionRoutedToMetadataKey, "alpha/crew.worker")), rowForeign, rowRuleOwnerKeys, beadmeta.ExecutionRoutedToMetadataKey},
		{"deferred assignee outside", rnsRow("", rnsMD(beadmeta.DeferredAssigneeMetadataKey, "scout")), rowForeign, rowRuleOwnerKeys, beadmeta.DeferredAssigneeMetadataKey},
		{"deferred execution route inside", rnsRow("", rnsMD(beadmeta.DeferredExecutionRoutedToMetadataKey, "beta/crew.worker")), rowOwned, rowRuleOwnerKeys, ""},
		{"stranded assignee other city", rnsRow("", rnsMD(beadmeta.StrandedAssigneeMetadataKey, poolIDA)), rowForeign, rowRuleOwnerKeys, beadmeta.StrandedAssigneeMetadataKey},
		{"stranded assignee own closed pool", rnsRow("", rnsMD(beadmeta.StrandedAssigneeMetadataKey, poolIDB2)), rowOwned, rowRuleOwnerKeys, ""},
		{"blank owner values are absent", rnsRow("   ", rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker", beadmeta.ExecutionRoutedToMetadataKey, " ")), rowOwned, rowRuleOwnerKeys, ""},
		{"stale foreign stamp on own routed row", rnsRow("", rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker", beadmeta.SessionIDMetadataKey, poolIDA)), rowOwned, rowRuleOwnerKeys, ""},
		{"own stamp cannot rescue a foreign owner", rnsRow("scout", rnsMD(beadmeta.SessionIDMetadataKey, poolIDB)), rowForeign, rowRuleOwnerKeys, rowAssigneeKey},

		// Claim stamps, only when no owner key is set.
		{"released row one own stamp among foreign", rnsRow("", rnsMD(beadmeta.SessionIDMetadataKey, poolIDA, beadmeta.SessionNameMetadataKey, poolNameB)), rowOwned, rowRuleClaimStamps, beadmeta.SessionNameMetadataKey},
		{"camel session id own", rnsRow("", rnsMD(beadmeta.SessionIDCamelMetadataKey, poolIDB)), rowOwned, rowRuleClaimStamps, beadmeta.SessionIDCamelMetadataKey},
		{"camel session name other city", rnsRow("", rnsMD(beadmeta.SessionNameCamelMetadataKey, poolNameA)), rowForeign, rowRuleClaimStamps, beadmeta.SessionNameCamelMetadataKey},
		{"stranded session own", rnsRow("", rnsMD(beadmeta.StrandedSessionMetadataKey, poolIDB2)), rowOwned, rowRuleClaimStamps, beadmeta.StrandedSessionMetadataKey},
		{"stamp lexical session spelling", rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, "beta--crew__worker")), rowOwned, rowRuleClaimStamps, beadmeta.SessionNameMetadataKey},
		{"stamp short id of own record", rnsRow("", rnsMD(beadmeta.SessionIDMetadataKey, shortIDB)), rowForeign, rowRuleClaimStamps, beadmeta.SessionIDMetadataKey},
		{"foreign stamps beat own creation stamp", rnsRow("", rnsMD(beadmeta.SessionIDMetadataKey, poolIDA, beadmeta.OwnerNamespaceMetadataKey, ownerStampB)), rowForeign, rowRuleClaimStamps, beadmeta.SessionIDMetadataKey},

		// Creation stamp, only when no owner key or stamp is set.
		{"creation stamp other rig", rnsRow("", rnsMD(beadmeta.OwnerNamespaceMetadataKey, "alpha/")), rowForeign, rowRuleCreationStamp, beadmeta.OwnerNamespaceMetadataKey},
		{"creation stamp case variant", rnsRow("", rnsMD(beadmeta.OwnerNamespaceMetadataKey, "Beta/")), rowForeign, rowRuleCreationStamp, beadmeta.OwnerNamespaceMetadataKey},
		{"creation stamp without slash", rnsRow("", rnsMD(beadmeta.OwnerNamespaceMetadataKey, "beta")), rowForeign, rowRuleCreationStamp, beadmeta.OwnerNamespaceMetadataKey},
		{"creation stamp session spelling folds to the namespace", rnsRow("", rnsMD(beadmeta.OwnerNamespaceMetadataKey, "beta--")), rowOwned, rowRuleCreationStamp, beadmeta.OwnerNamespaceMetadataKey},
		{"creation stamp padded", rnsRow("", rnsMD(beadmeta.OwnerNamespaceMetadataKey, " beta/ ")), rowOwned, rowRuleCreationStamp, beadmeta.OwnerNamespaceMetadataKey},
		{"creation stamp blank", rnsRow("", rnsMD(beadmeta.OwnerNamespaceMetadataKey, "  ")), rowUnattributed, rowRuleNone, ""},
		{"own creation stamp does not rescue a foreign owner", rnsRow("", rnsMD(beadmeta.RoutedToMetadataKey, "alpha/crew.worker", beadmeta.OwnerNamespaceMetadataKey, ownerStampB)), rowForeign, rowRuleOwnerKeys, beadmeta.RoutedToMetadataKey},

		// Not keys.
		{"run target alone inside", rnsRow("", rnsMD(beadmeta.RunTargetMetadataKey, "beta/crew.worker")), rowUnattributed, rowRuleNone, ""},
		{"run target alone outside", rnsRow("", rnsMD(beadmeta.RunTargetMetadataKey, "alpha/crew.worker")), rowUnattributed, rowRuleNone, ""},
		{"session affinity marker", rnsRow("", rnsMD(beadmeta.SessionAffinityMetadataKey, "require")), rowUnattributed, rowRuleNone, ""},
		{"template on a work row is not a key", rnsRow("", rnsMD("template", "alpha/crew.worker", "alias", "scout")), rowUnattributed, rowRuleNone, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ns.classifyRow(tc.bead)
			if got.Class != tc.want || got.Rule != tc.wantRule || got.Key != tc.wantKey {
				t.Fatalf("classifyRow = {%s %s %q %q}, want {%s %s %q}", got.Class, got.Rule, got.Key, got.Value, tc.want, tc.wantRule, tc.wantKey)
			}
			if got.Err != nil {
				t.Fatalf("unexpected lookup error: %v", got.Err)
			}
		})
	}
}

func TestRigNamespaceClassifiesSessionRows(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	sessionRow := func(template, alias string) beads.Bead {
		b := rnsSessionBead(poolIDB, template, "")
		if alias != "" {
			b.Metadata["alias"] = alias
		}
		return b
	}
	repairable := sessionRow("alpha/crew.worker", "")
	repairable.Type = ""
	cases := []struct {
		name string
		bead beads.Bead
		want rowClass
	}{
		{"own rig template", sessionRow(templateB, ""), rowOwned},
		{"own rig template and alias", sessionRow(templateB, "beta/crew.lead"), rowOwned},
		{"other rig template", sessionRow(templateA, ""), rowForeign},
		{"city-scoped template", sessionRow("crew.worker", ""), rowForeign},
		{"alias outside", sessionRow(templateB, "scout"), rowForeign},
		{"repairable session row other rig", repairable, rowForeign},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ns.classify(tc.bead); got != tc.want {
				t.Fatalf("classify = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestRigNamespaceAdmission covers the lexical arm's edge cases (fold, case,
// empty, bare namespace) and the provenance arm's refusals.
func TestRigNamespaceAdmission(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	cases := []struct {
		v    string
		want admitVia
	}{
		{"beta/crew.worker", admitLexical},
		{"beta/crew.worker-2", admitLexical},
		{"beta--crew__worker", admitLexical},
		{"beta/core.control-dispatcher", admitLexical},
		{"  beta/crew.worker  ", admitLexical},
		{"beta/alpha/crew.worker", admitLexical},
		{"beta/", admitNone},
		{"beta--", admitNone},
		{"beta", admitNone},
		{"beta-crew.worker", admitNone},
		{"beta---crew.worker", admitNone},
		{"beta__pk.worker", admitNone},
		{"beta.crew.worker", admitNone},
		{"Beta/crew.worker", admitNone},
		{"BETA/crew.worker", admitNone},
		{"xbeta/crew.worker", admitNone},
		{"alpha/beta/crew.worker", admitNone},
		{"alpha--beta--pk", admitNone},
		{"crew.worker", admitNone},
		{"scout", admitNone},
		{"", admitNone},
		{"   ", admitNone},
		{"gcs-17", admitNone},
		{"zz-wisp-a1b2c3", admitNone},
		{shortIDB, admitNone},
		{poolIDB, admitSessionRecord},
		{"  " + poolIDB + " ", admitSessionRecord},
		{poolNameB, admitSessionRecord},
		{poolIDB2, admitSessionRecord},
		{poolIDA, admitNone},
		{poolNameA, admitNone},
		{"evil-" + poolIDB, admitNone},
		{"gcx-session-" + "0123456789ABCDEF0123456789ABCDEF", admitNone},
		{"gcx-session-" + tokenB[:31], admitNone},
		{"gcx-session-" + tokenB + "0", admitNone},
		{"gcx_session_" + tokenB, admitNone},
		{"session-" + tokenB, admitNone},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.v), func(t *testing.T) {
			a := ns.admission(tc.v)
			if a.Via != tc.want {
				t.Fatalf("admission(%q) = %s, want %s", tc.v, a.Via, tc.want)
			}
			if a.Err != nil {
				t.Fatalf("admission(%q) err = %v", tc.v, a.Err)
			}
			if ns.admits(tc.v) != (tc.want != admitNone) {
				t.Fatalf("admits(%q) disagrees with admission", tc.v)
			}
		})
	}
}

// TestRigNamespaceProvenanceRequiresRigTemplate: one of the city's own pool
// sessions is admitted only on the rig its template belongs to.
func TestRigNamespaceProvenanceRequiresRigTemplate(t *testing.T) {
	cases := []struct {
		template string
		want     bool
	}{
		{templateB, true},
		// Folds inside, but no rig agent's qualified name is spelled so: a
		// city-scoped agent named "beta--crew.worker".
		{"beta--crew.worker", false},
		{"beta-1/crew.worker", false},
		{"betax/crew.worker", false},
		{" beta/crew.worker ", true},
		{templateA, false},
		{"Beta/crew.worker", false},
		{"crew.worker", false},
		{"beta/", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.template), func(t *testing.T) {
			store := rnsNewCountingStore(rnsSessionBead(poolIDB, tc.template, poolNameB))
			ns, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store)))
			if got := ns.admits(poolIDB); got != tc.want {
				t.Fatalf("admits(pool id) with template %q = %v, want %v", tc.template, got, tc.want)
			}
		})
	}
}

// TestRigNamespaceRejectsNonSessionRecords: a pool-shaped id that is not a
// session record in the city's store is not one of its sessions.
func TestRigNamespaceRejectsNonSessionRecords(t *testing.T) {
	work := beads.Bead{ID: poolIDB, Type: "task", Status: "open", Metadata: rnsMD("template", templateB, "session_name", poolNameB)}
	store := rnsNewCountingStore(work)
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store)))
	if ns.admits(poolIDB) || ns.admits(poolNameB) {
		t.Fatal("a non-session row with a pool-shaped id was admitted")
	}
}

// rnsFakeResolver returns a fixed record for any lookup, to prove rigNamespace
// re-checks what a resolver says.
type rnsFakeResolver struct {
	rec ownSessionRecord
	ok  bool
	err error
}

func (f rnsFakeResolver) lookupOwnSession(string) (ownSessionRecord, bool, error) {
	return f.rec, f.ok, f.err
}

func (f rnsFakeResolver) lookupOwnSessionsByName(string, func(ownSessionRecord) bool) ([]ownSessionRecord, error) {
	if !f.ok {
		return nil, f.err
	}
	return []ownSessionRecord{f.rec}, f.err
}

func TestRigNamespaceRechecksResolverAnswers(t *testing.T) {
	cases := []struct {
		name string
		rec  ownSessionRecord
		v    string
		want bool
	}{
		{"exact id", ownSessionRecord{ID: poolIDB, Template: templateB}, poolIDB, true},
		{"exact name", ownSessionRecord{ID: poolIDB, SessionName: poolNameB, Template: templateB}, poolNameB, true},
		{"record for another id", ownSessionRecord{ID: poolIDB2, Template: templateB}, poolIDB, false},
		{"record with another name", ownSessionRecord{ID: poolIDB, SessionName: "other-" + poolIDB, Template: templateB}, poolNameB, false},
		{"short-id record", ownSessionRecord{ID: shortIDB, SessionName: "x-" + poolIDB, Template: templateB}, "x-" + poolIDB, false},
		{"template outside", ownSessionRecord{ID: poolIDB, Template: templateA}, poolIDB, false},
		{"template folds inside only", ownSessionRecord{ID: poolIDB, Template: "beta--crew"}, poolIDB, false},
		// Records with malformed ids that answer to a well-formed name: the
		// id's shape is checked on the record, not only on the lookup key.
		{"record id token too short", ownSessionRecord{ID: "gcx-session-" + tokenB[:16], SessionName: poolNameB, Template: templateB}, poolNameB, false},
		{"record id token upper case", ownSessionRecord{ID: "gcx-session-" + strings.ToUpper(tokenB), SessionName: poolNameB, Template: templateB}, poolNameB, false},
		{"record id with a tail", ownSessionRecord{ID: "gcx-session-" + tokenB + "-x", SessionName: poolNameB, Template: templateB}, poolNameB, false},
		{"record id with folded separators", ownSessionRecord{ID: "gcx_session_" + tokenB, SessionName: poolNameB, Template: templateB}, poolNameB, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, _ := newRigNamespace(rnsGuardedRig("beta"), rnsFakeResolver{rec: tc.rec, ok: true})
			if got := ns.admits(tc.v); got != tc.want {
				t.Fatalf("admits(%q) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

func TestRigNamespaceFailsClosed(t *testing.T) {
	t.Run("nil namespace", func(t *testing.T) {
		var ns *rigNamespace
		if ns.admits("beta/crew.worker") {
			t.Fatal("nil namespace admitted an identity")
		}
		if got := ns.classify(rnsRow("", nil)); got != rowForeign {
			t.Fatalf("nil namespace classify = %s, want foreign", got)
		}
		if _, ok := ns.ownerStamp(); ok {
			t.Fatal("nil namespace offered an owner stamp")
		}
	})
	t.Run("zero row class is foreign", func(t *testing.T) {
		var c rowClass
		if c != rowForeign {
			t.Fatalf("zero rowClass = %s", c)
		}
	})
	t.Run("guarded rig without a name", func(t *testing.T) {
		ns, ok := newRigNamespace(config.Rig{Federation: config.RigFederationRig}, rnsFakeResolver{rec: ownSessionRecord{ID: poolIDB, Template: "/x"}, ok: true})
		if !ok {
			t.Fatal("guarded rig without a name returned no namespace")
		}
		for _, v := range []string{"/x", "/", poolIDB, "beta/crew.worker"} {
			if ns.admits(v) {
				t.Fatalf("nameless namespace admitted %q", v)
			}
		}
		if got := ns.classify(rnsRow("", rnsMD(beadmeta.OwnerNamespaceMetadataKey, "/"))); got != rowForeign {
			t.Fatalf("nameless namespace classify(stamp) = %s, want foreign", got)
		}
		if _, ok := ns.ownerStamp(); ok {
			t.Fatal("nameless namespace offered an owner stamp")
		}
	})
	t.Run("nil resolver admits no session id", func(t *testing.T) {
		ns, _ := newRigNamespace(rnsGuardedRig("beta"), nil)
		if ns.admits(poolIDB) {
			t.Fatal("nil resolver admitted a pool id")
		}
		if !ns.admits("beta/crew.worker") {
			t.Fatal("nil resolver blocked the lexical arm")
		}
	})
	t.Run("out-of-enum federation is guarded", func(t *testing.T) {
		if _, ok := newRigNamespace(config.Rig{Name: "beta", Federation: "RIG "}, nil); !ok {
			t.Fatal("federation \"RIG \" was not guarded")
		}
		if _, ok := newRigNamespace(config.Rig{Name: "beta", Federation: "bogus"}, nil); !ok {
			t.Fatal("federation \"bogus\" was not guarded")
		}
	})
	t.Run("unguarded rig has no namespace", func(t *testing.T) {
		for _, f := range []string{"", "city", " City "} {
			if ns, ok := newRigNamespace(config.Rig{Name: "beta", Federation: f}, nil); ok || ns != nil {
				t.Fatalf("federation %q returned a namespace", f)
			}
		}
	})
}

// TestRigNamespaceUnreadableSessionStore: a failed session lookup admits
// nothing, makes the row Foreign for that pass with the error reported, and
// is not memoized, so the next pass recovers.
func TestRigNamespaceUnreadableSessionStore(t *testing.T) {
	ns, store := rnsCityBNamespace(t)
	boom := errors.New("session store offline")
	store.setErr(boom)

	a := ns.admission(poolIDB)
	if a.Admitted() || !errors.Is(a.Err, boom) {
		t.Fatalf("admission during outage = %+v, want not admitted with error", a)
	}
	got := ns.classifyRow(rnsRow(poolIDB, nil))
	if got.Class != rowForeign || !errors.Is(got.Err, boom) || got.Key != rowAssigneeKey {
		t.Fatalf("owner-key row during outage = %+v, want foreign with error", got)
	}
	got = ns.classifyRow(rnsRow("", rnsMD(beadmeta.SessionIDMetadataKey, poolIDB)))
	if got.Class != rowForeign || !errors.Is(got.Err, boom) {
		t.Fatalf("stamp row during outage = %+v, want foreign with error", got)
	}
	// The lexical arm needs no lookup and keeps working.
	if got := ns.classify(rnsRow("beta/crew.worker", nil)); got != rowOwned {
		t.Fatalf("lexical row during outage = %s, want owned", got)
	}

	store.setErr(nil)
	// Within the pass, lookups back off instead of re-reading a failing
	// store.
	if a := ns.admission(poolIDB); a.Admitted() || !errors.Is(a.Err, boom) {
		t.Fatalf("admission during backoff = %+v, want not admitted with error", a)
	}
	ns.sessions.(*citySessionResolver).beginPass()
	if !ns.admits(poolIDB) {
		t.Fatal("error was memoized: pool id still refused after the store recovered")
	}
}

func TestRigNamespaceUnopenableSessionStore(t *testing.T) {
	var opens atomic.Int64
	var fail atomic.Bool
	fail.Store(true)
	store := rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, poolNameB))
	res := newControllerSessionResolver(cityPathB, nil, func() (beads.SessionStore, error) {
		opens.Add(1)
		if fail.Load() {
			return beads.SessionStore{}, errors.New("cannot open")
		}
		return beadstest.Sessions(store), nil
	})
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	if a := ns.admission(poolIDB); a.Admitted() || a.Err == nil {
		t.Fatalf("admission with unopenable store = %+v", a)
	}
	fail.Store(false)
	res.beginPass()
	for i := 0; i < 2; i++ {
		if !ns.admits(poolIDB) {
			t.Fatalf("pool id refused after the store became openable (call %d)", i+1)
		}
	}
	if got := opens.Load(); got != 2 {
		t.Fatalf("store opened %d times, want 2 (one failure, then kept)", got)
	}
}

// TestCLISessionResolverOpensStoreLazily: the one-shot resolver opens the
// session store only for a pool-shaped identity it cannot answer from the
// caller's own session, and keeps it for the process.
func TestCLISessionResolverOpensStoreLazily(t *testing.T) {
	var opens atomic.Int64
	store := rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, poolNameB))
	// A fresh directory is no session's GC_CITY_PATH, so the env fast path
	// cannot answer whatever the test process inherited.
	res := newCLISessionResolver(t.TempDir(), func() (beads.SessionStore, error) {
		opens.Add(1)
		return beadstest.Sessions(store), nil
	})
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	if !ns.admits("beta/crew.worker") || ns.admits("scout") {
		t.Fatal("lexical answers wrong")
	}
	if got := opens.Load(); got != 0 {
		t.Fatalf("store opened %d times for identities that need no lookup", got)
	}
	if !ns.admits(poolIDB) || ns.admits(poolIDA) {
		t.Fatal("store answers wrong")
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("store opened %d times, want 1", got)
	}
}

func TestCitySessionResolverSnapshotHit(t *testing.T) {
	snap := newSessionBeadSnapshotFromInfos([]sessionpkg.Info{{
		ID: poolIDB, Type: sessionpkg.BeadType, Template: templateB,
		SessionName: poolNameB, SessionNameMetadata: poolNameB,
	}})
	res := newControllerSessionResolver(cityPathB, func() sessionRecordIndex { return snap }, nil)
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	if !ns.admits(poolIDB) || !ns.admits(poolNameB) {
		t.Fatal("snapshot record not admitted by id and name")
	}
	if ns.admits(poolIDA) {
		t.Fatal("unknown id admitted with no store")
	}
}

func TestCitySessionResolverSnapshotNameNeedsRecordedName(t *testing.T) {
	// Info.SessionName falls back to a derived name when none is recorded;
	// provenance answers only to the recorded one.
	snap := newSessionBeadSnapshotFromInfos([]sessionpkg.Info{{
		ID: poolIDB, Type: sessionpkg.BeadType, Template: templateB, SessionName: poolNameB,
	}})
	res := newControllerSessionResolver(cityPathB, func() sessionRecordIndex { return snap }, nil)
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	if ns.admits(poolNameB) {
		t.Fatal("derived session name admitted")
	}
	if !ns.admits(poolIDB) {
		t.Fatal("pool id not admitted")
	}
}

func TestCitySessionResolverLiveLookupMemo(t *testing.T) {
	ns, store := rnsCityBNamespace(t)
	for i := 0; i < 3; i++ {
		if !ns.admits(poolIDB) {
			t.Fatal("own pool id not admitted")
		}
	}
	if got := store.gets.Load(); got != 1 {
		t.Fatalf("store read %d times for one id, want 1", got)
	}
	// The remembered record also answers to its session name.
	for i := 0; i < 3; i++ {
		if !ns.admits(poolNameB) {
			t.Fatal("own pool session name not admitted")
		}
	}
	if got := store.gets.Load(); got != 1 {
		t.Fatalf("store read %d times after name lookups, want 1", got)
	}
	// Another record's name costs a by-id read of its embedded id, once.
	for i := 0; i < 3; i++ {
		if !ns.admits("crew.worker-" + poolIDB2) {
			t.Fatal("own closed pool session name not admitted")
		}
	}
	if got := store.gets.Load(); got != 2 {
		t.Fatalf("store read %d times after another name, want 2", got)
	}
	// Identities that cannot be pool sessions never reach the store.
	for _, v := range []string{"scout", "gcs-17", shortIDB, "alice", "alpha/crew.worker", "crew.intake", ""} {
		ns.admits(v)
		ns.classify(rnsRow(v, nil))
	}
	if got := store.gets.Load(); got != 2 {
		t.Fatalf("store read %d times after non-pool identities, want 2", got)
	}
}

func TestCitySessionResolverNegativeMemoExpires(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := rnsNewCountingStore()
	res := newCitySessionResolver(citySessionResolverOptions{
		CityPath:  cityPathB,
		OpenStore: rnsStoreOpener(store),
		Now:       func() time.Time { return now },
	})
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	for i := 0; i < 3; i++ {
		if ns.admits(poolIDA) {
			t.Fatal("unknown pool id admitted")
		}
	}
	if got := store.gets.Load(); got != 1 {
		t.Fatalf("store read %d times for a repeated miss, want 1", got)
	}
	now = now.Add(ownSessionNegativeTTL)
	ns.admits(poolIDA)
	if got := store.gets.Load(); got != 2 {
		t.Fatalf("store read %d times after the miss expired, want 2", got)
	}
}

func TestCitySessionResolverMissThenCreated(t *testing.T) {
	// A pool id is pinned before its record exists. A miss cached then must
	// not outlive the record's appearance in the controller's snapshot, nor
	// come back once the session closes and leaves the snapshot.
	var snap *sessionBeadSnapshot
	mem := beads.NewMemStoreFrom(100, nil, nil)
	mem.HonorExplicitIDs = true
	store := &rnsCountingStore{Store: mem, prefix: "gcx"}
	res := newControllerSessionResolver(cityPathB, func() sessionRecordIndex { return snap }, rnsStoreOpener(store))
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	if ns.admits(poolIDB) {
		t.Fatal("admitted before the record exists")
	}
	if _, err := store.Create(rnsSessionBead(poolIDB, templateB, "")); err != nil {
		t.Fatal(err)
	}
	snap = newSessionBeadSnapshotFromInfos([]sessionpkg.Info{{ID: poolIDB, Type: sessionpkg.BeadType, Template: templateB}})
	if !ns.admits(poolIDB) {
		t.Fatal("snapshot record refused behind a cached miss")
	}
	if err := store.Close(poolIDB); err != nil {
		t.Fatal(err)
	}
	snap = newSessionBeadSnapshotFromInfos(nil)
	if !ns.admits(poolIDB) {
		t.Fatal("own closed pool session refused behind the miss cached before it existed")
	}
}

func TestCitySessionResolverSessionNameLookup(t *testing.T) {
	// Template bases may contain '-', so a name has several candidate ids.
	name := "crew-worker-" + poolIDB
	store := rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, name))
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store)))
	if !ns.admits(name) {
		t.Fatal("pool session name with '-' in its base not admitted")
	}
	if ns.admits("crew-other-" + poolIDB) {
		t.Fatal("a name the record does not carry was admitted")
	}
}

// TestCitySessionResolverPoolNameBaseStartsWithIDPrefix: a rig agent whose
// template base starts with the city's own "<prefix>-session-" runs pool
// sessions whose names start with it too. Such a name is admitted through its
// trailing id, and the whole name is never read as an id.
func TestCitySessionResolverPoolNameBaseStartsWithIDPrefix(t *testing.T) {
	template := "beta/gcx-session-worker"
	name := PoolSessionName(template, poolIDB)
	if name != "gcx-session-worker-"+poolIDB {
		t.Fatalf("PoolSessionName = %q", name)
	}
	store := rnsNewCountingStore(rnsSessionBead(poolIDB, template, name))
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store)))
	if a := ns.admission(name); a.Via != admitSessionRecord || a.Err != nil {
		t.Fatalf("admission(%q) = %+v, want session_record", name, a)
	}
	for _, b := range []beads.Bead{rnsRow(name, nil), rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, name))} {
		if got := ns.classifyRow(b); got.Class != rowOwned {
			t.Errorf("own row %+v classifies %+v, want owned", b, got)
		}
	}
	if got := store.gets.Load(); got != 1 {
		t.Fatalf("store read %d times, want 1 (the trailing id only)", got)
	}
}

func TestCitySessionResolverEnvFastPath(t *testing.T) {
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	cityB := t.TempDir()
	cityA := t.TempDir()
	self := map[string]string{
		"GC_CITY_PATH":    cityB,
		"GC_SESSION_ID":   poolIDB,
		"GC_SESSION_NAME": poolNameB,
		"GC_TEMPLATE":     templateB,
	}
	cases := []struct {
		name     string
		env      map[string]string
		v        string
		want     bool
		wantOpen bool
	}{
		{"own id", self, poolIDB, true, false},
		{"own name", self, poolNameB, true, false},
		{"GC_CITY fallback", map[string]string{"GC_CITY": cityB, "GC_SESSION_ID": poolIDB, "GC_TEMPLATE": templateB}, poolIDB, true, false},
		{"another id still needs the store", self, poolIDA, false, true},
		{"env of another city", map[string]string{"GC_CITY_PATH": cityA, "GC_SESSION_ID": poolIDB, "GC_TEMPLATE": templateB}, poolIDB, false, true},
		{"env without city", map[string]string{"GC_SESSION_ID": poolIDB, "GC_TEMPLATE": templateB}, poolIDB, false, true},
		{"env without template", map[string]string{"GC_CITY_PATH": cityB, "GC_SESSION_ID": poolIDB}, poolIDB, false, true},
		{"env self with template outside", map[string]string{"GC_CITY_PATH": cityB, "GC_SESSION_ID": poolIDB, "GC_TEMPLATE": templateA}, poolIDB, false, false},
		{"env self with short id", map[string]string{"GC_CITY_PATH": cityB, "GC_SESSION_ID": shortIDB, "GC_TEMPLATE": templateB}, shortIDB, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opened atomic.Bool
			res := newCitySessionResolver(citySessionResolverOptions{
				CityPath: cityB,
				Getenv:   env(tc.env),
				OpenStore: func() (beads.SessionStore, error) {
					opened.Store(true)
					return beadstest.Sessions(rnsNewCountingStore()), nil
				},
			})
			ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
			if got := ns.admits(tc.v); got != tc.want {
				t.Fatalf("admits(%q) = %v, want %v", tc.v, got, tc.want)
			}
			if opened.Load() != tc.wantOpen {
				t.Fatalf("session store opened = %v, want %v", opened.Load(), tc.wantOpen)
			}
		})
	}
}

// TestRigNamespaceTwoCitiesShareNoResolverState: two cities in one process,
// with colliding store prefixes, never admit each other's pool sessions,
// whatever either has looked up or memoized before.
func TestRigNamespaceTwoCitiesShareNoResolverState(t *testing.T) {
	storeB := rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, poolNameB))
	storeA := rnsNewCountingStore(rnsSessionBead(poolIDA, templateA, poolNameA))
	resB := newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(storeB))
	resA := newControllerSessionResolver(cityPathA, nil, rnsStoreOpener(storeA))
	nsB, _ := newRigNamespace(rnsGuardedRig("beta"), resB)
	nsA, _ := newRigNamespace(rnsGuardedRig("alpha"), resA)

	// Warm both memos with positives and negatives.
	if !nsA.admits(poolIDA) || !nsA.admits(poolNameA) {
		t.Fatal("city A refused its own pool session")
	}
	if !nsB.admits(poolIDB) || !nsB.admits(poolNameB) {
		t.Fatal("city B refused its own pool session")
	}
	for i := 0; i < 2; i++ {
		if nsB.admits(poolIDA) || nsB.admits(poolNameA) {
			t.Fatal("city B admitted city A's pool session")
		}
		if nsA.admits(poolIDB) || nsA.admits(poolNameB) {
			t.Fatal("city A admitted city B's pool session")
		}
	}
	// A's row as B sees it, and the reverse.
	if got := nsB.classify(rnsRow(poolIDA, nil)); got != rowForeign {
		t.Fatalf("B classifies A's pool claim %s", got)
	}
	if got := nsA.classify(rnsRow(poolIDB, nil)); got != rowForeign {
		t.Fatalf("A classifies B's pool claim %s", got)
	}
	if got := nsA.classify(rnsRow(poolIDA, nil)); got != rowOwned {
		t.Fatalf("A classifies its own pool claim %s", got)
	}
	// The same resolver serves every rig of its city, never the other city.
	if resA == resB {
		t.Fatal("cities share a resolver")
	}
	// A creation stamp of one city is the other's Foreign.
	if got := nsB.classify(rnsRow("", rnsMD(beadmeta.OwnerNamespaceMetadataKey, "alpha/"))); got != rowForeign {
		t.Fatalf("B classifies A's creation stamp %s", got)
	}
	if got := nsA.classify(rnsRow("", rnsMD(beadmeta.OwnerNamespaceMetadataKey, ownerStampB))); got != rowForeign {
		t.Fatalf("A classifies B's creation stamp %s", got)
	}
}

// TestRigNamespaceForgedCreationStamp pins what the creation stamp can and
// cannot do. A row whose only attribution is gc.owner_namespace = "beta/"
// is Owned in B whoever wrote it: the classifier cannot tell a stamp the
// guarded store wrote from one another writer copied or set. The store
// guard therefore must be the only writer of the key (it overwrites it on
// create and refuses it in updates). Any identity on the row outranks the
// stamp, so a stamp cannot capture a row another city owns or has claimed.
func TestRigNamespaceForgedCreationStamp(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	forged := map[string]string{beadmeta.OwnerNamespaceMetadataKey: ownerStampB}
	if got := ns.classify(rnsRow("", forged)); got != rowOwned {
		t.Fatalf("identity-less stamped row = %s, want owned (documented residual)", got)
	}
	withOwner := rnsMD(beadmeta.OwnerNamespaceMetadataKey, ownerStampB, beadmeta.RoutedToMetadataKey, "alpha/crew.worker")
	if got := ns.classify(rnsRow("", withOwner)); got != rowForeign {
		t.Fatalf("stamped row routed to another rig = %s, want foreign", got)
	}
	if got := ns.classify(rnsRow(poolIDA, forged)); got != rowForeign {
		t.Fatalf("stamped row claimed by another city = %s, want foreign", got)
	}
	withStamp := rnsMD(beadmeta.OwnerNamespaceMetadataKey, ownerStampB, beadmeta.SessionNameMetadataKey, poolNameA)
	if got := ns.classify(rnsRow("", withStamp)); got != rowForeign {
		t.Fatalf("stamped row with another city's claim stamp = %s, want foreign", got)
	}
}

func TestRigNamespaceOwnerStamp(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	v, ok := ns.ownerStamp()
	if !ok || v != ownerStampB || rigNamespaceOwnerStampKey != beadmeta.OwnerNamespaceMetadataKey {
		t.Fatalf("ownerStamp = %q %v", v, ok)
	}
	k := rigNamespaceOwnerStampKey
	if got := ns.classify(rnsRow("", rnsMD(k, v))); got != rowOwned {
		t.Fatalf("own stamp classifies %s", got)
	}
	if ns.rigName() != "beta" || ns.namespace() != ownerStampB {
		t.Fatalf("rigName/namespace = %q %q", ns.rigName(), ns.namespace())
	}
}

func TestOwnPoolSessionIDCandidates(t *testing.T) {
	long := strings.Repeat("a", maxOwnSessionIdentityLen)
	own := []string{"gcx", "hq"}
	cases := []struct {
		v    string
		want []string
	}{
		{poolIDB, []string{poolIDB}},
		{poolNameB, []string{poolIDB}},
		{"a-b-" + poolIDB, []string{poolIDB}},
		// Long, dashed template bases still reach the id.
		{"nightly-report-builder-gate-" + poolIDB, []string{poolIDB}},
		{"a-b-c-d-e-f-" + poolIDB, []string{poolIDB}},
		{"hq-session-" + tokenB, []string{"hq-session-" + tokenB}},
		{"crew-hq-session-" + tokenB, []string{"hq-session-" + tokenB}},
		// A template base may start with an own "<prefix>-session-": only the
		// trailing id is a candidate, though the whole name has the pool shape.
		{"gcx-session-worker-" + poolIDB, []string{poolIDB}},
		{"hq-session-worker-" + poolIDB, []string{poolIDB}},
		{"gcx-session-" + tokenA + "-" + poolIDB, []string{poolIDB}},
		// Another prefix is never a candidate, as an id or inside a name.
		{"gcs-session-" + tokenB, nil},
		{"crew-gcs-session-" + tokenB, nil},
		{"xgcx-session-" + tokenB, nil},
		{"gcx-session-" + strings.ToUpper(tokenB), nil},
		{"gcx-session-" + tokenB[:31], nil},
		{"gcx-session-" + tokenB + "0", nil},
		{"gcx_session_" + tokenB, nil},
		{"scout", nil},
		{"gcx-17", nil},
		{"", nil},
		{"session-" + tokenB, nil},
		{long + "-" + poolIDB, nil},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%.40q", tc.v), func(t *testing.T) {
			got := ownPoolSessionIDCandidates(tc.v, own)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("candidates = %q, want %q", got, tc.want)
			}
		})
	}
	if got := ownPoolSessionIDCandidates(poolIDB, nil); got != nil {
		t.Fatalf("candidates without prefixes = %q", got)
	}
}

func TestEmbedsPoolSessionID(t *testing.T) {
	cases := map[string]bool{
		poolIDB:                                  true,
		poolNameB:                                true,
		"beta--" + poolIDA:                       true,
		"beta/x-" + poolIDA:                      true,
		" " + poolIDA + " ":                      true,
		"gcs-session-" + tokenA:                  true,
		"beta/crew.worker":                       false,
		"gcx-17":                                 false,
		"session-" + tokenA:                      false,
		"gcx-session-" + tokenA[:31]:             false,
		"gcx-session-" + strings.ToUpper(tokenA): false,
		"gcx-session-" + tokenA + "-x":           false,
		"gcx_session_" + tokenA:                  false,
		"gc_-session-" + tokenA:                  true,
		"-session-" + tokenA:                     true,
	}
	for v, want := range cases {
		if got := embedsPoolSessionID(v); got != want {
			t.Errorf("embedsPoolSessionID(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestRigNamespaceKeyHelpers(t *testing.T) {
	for _, k := range []string{
		beadmeta.RoutedToMetadataKey, beadmeta.ExecutionRoutedToMetadataKey,
		beadmeta.DeferredAssigneeMetadataKey, beadmeta.DeferredRoutedToMetadataKey,
		beadmeta.DeferredExecutionRoutedToMetadataKey, beadmeta.StrandedAssigneeMetadataKey,
		beadmeta.RunTargetMetadataKey,
	} {
		if !isRigNamespaceIdentityPredicateKey(k) {
			t.Errorf("%s is not an identity predicate key", k)
		}
	}
	for _, k := range []string{beadmeta.SessionAffinityMetadataKey, beadmeta.SessionIDMetadataKey, beadmeta.OwnerNamespaceMetadataKey, "template", ""} {
		if isRigNamespaceIdentityPredicateKey(k) {
			t.Errorf("%s is an identity predicate key", k)
		}
	}
	for _, k := range rigNamespaceRouteKeys {
		if !isRigNamespaceRouteKey(k) || !isRigNamespaceIdentityPredicateKey(k) {
			t.Errorf("route key %s misclassified", k)
		}
	}
	for _, k := range rigNamespaceAssigneeMetadataKeys {
		if isRigNamespaceRouteKey(k) {
			t.Errorf("assignee key %s is a route key", k)
		}
	}
	quals := map[string]bool{
		"beta/crew.worker": true, "beta--crew.worker": true, "alpha/x": true,
		"crew.worker": false, "beta-pk": false, "beta---pk": false, "": false, " ": false,
		"/x": false, "--x": false, "x/": false, "x--": false, "a//b": false, "beta/crew/x": true,
	}
	for v, want := range quals {
		if got := rigRouteQualified(v); got != want {
			t.Errorf("rigRouteQualified(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestRigNamespaceForRigNameAndBeadID(t *testing.T) {
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "alpha", Prefix: "al"},
		{Name: "beta", Prefix: "bt", Federation: config.RigFederationRig},
		{Name: "gamma", Prefix: "gm-x", Federation: config.RigFederationRig},
	}}
	res := newControllerSessionResolver(cityPathB, nil, nil)
	names := []struct {
		name        string
		wantNS      string
		wantGuarded bool
		wantKnown   bool
	}{
		{"beta", ownerStampB, true, true},
		{"alpha", "", false, true},
		{"Beta", "", false, false},
		{"missing", "", false, false},
		{"", "", false, false},
	}
	for _, tc := range names {
		ns, guarded, known := rigNamespaceForRigName(cfg, tc.name, res)
		if guarded != tc.wantGuarded || known != tc.wantKnown || ns.namespace() != tc.wantNS {
			t.Errorf("rigNamespaceForRigName(%q) = %q guarded=%v known=%v, want %q %v %v",
				tc.name, ns.namespace(), guarded, known, tc.wantNS, tc.wantGuarded, tc.wantKnown)
		}
	}
	ids := []struct {
		id     string
		wantNS string
		wantOK bool
	}{
		{"bt-12", ownerStampB, true},
		{"BT-a1b2", ownerStampB, true},
		{"bt-12.3", ownerStampB, true},
		{"gm-x-7", "gamma/", true},
		{"al-12", "", false},
		{"zz-12", "", false},
		{"", "", false},
	}
	for _, tc := range ids {
		ns, ok := rigNamespaceForBeadID(cfg, tc.id, res)
		if ok != tc.wantOK || ns.namespace() != tc.wantNS {
			t.Errorf("rigNamespaceForBeadID(%q) = %q %v, want %q %v", tc.id, ns.namespace(), ok, tc.wantNS, tc.wantOK)
		}
	}
	shared := &config.City{Rigs: []config.Rig{
		{Name: "alpha", Prefix: "pk", Federation: config.RigFederationRig},
		{Name: "beta", Prefix: "PK", Federation: config.RigFederationRig},
	}}
	ns, ok := rigNamespaceForBeadID(shared, "pk-1", res)
	if !ok || ns.admits("alpha/x") || ns.admits("beta/x") {
		t.Fatal("a prefix shared by two guarded rigs did not fail closed")
	}
}

// TestRigNamespaceConcurrentUse exercises one city's namespace from many
// goroutines, as the controller does.
func TestRigNamespaceConcurrentUse(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	var wg sync.WaitGroup
	var bad atomic.Int64
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if ns.classify(rnsRow(poolIDB, nil)) != rowOwned || ns.classify(rnsRow(poolIDA, nil)) != rowForeign {
					bad.Add(1)
				}
				ns.admits(fmt.Sprintf("gcx-session-%032x", g*1000+i))
			}
		}(g)
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d inconsistent answers under concurrency", bad.Load())
	}
}

// TestCitySessionResolverMemoKeepsOwnRecords: misses are bounded, and a
// stream of other cities' pool ids never evicts this city's own records.
func TestCitySessionResolverMemoKeepsOwnRecords(t *testing.T) {
	ns, store := rnsCityBNamespace(t)
	res := ns.sessions.(*citySessionResolver)
	if !ns.admits(poolIDB) || !ns.admits(poolIDB2) {
		t.Fatal("own ids not admitted")
	}
	for i := 0; i < ownSessionNegativeLimit+10; i++ {
		if ns.admits(fmt.Sprintf("gcx-session-%032x", i+1)) {
			t.Fatalf("unknown id %d admitted", i)
		}
	}
	res.mu.Lock()
	misses, hits := len(res.misses), len(res.hits)
	res.mu.Unlock()
	if misses > ownSessionNegativeLimit {
		t.Fatalf("misses hold %d entries, limit %d", misses, ownSessionNegativeLimit)
	}
	if hits != 2 {
		t.Fatalf("hits hold %d entries, want 2", hits)
	}
	before := store.gets.Load()
	if !ns.admits(poolIDB) || !ns.admits(poolIDB2) {
		t.Fatal("own ids refused after the misses")
	}
	if got := store.gets.Load() - before; got != 0 {
		t.Fatalf("own records re-read %d times after foreign misses", got)
	}
}

// TestCitySessionResolverHitsDoNotExpire: this city's own records stay
// remembered past the miss TTL.
func TestCitySessionResolverHitsDoNotExpire(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := rnsNewCountingStore(rnsSessionBead(poolIDB2, templateB, "crew.worker-"+poolIDB2))
	res := newCitySessionResolver(citySessionResolverOptions{CityPath: cityPathB, OpenStore: rnsStoreOpener(store), Now: func() time.Time { return now }})
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	if !ns.admits(poolIDB2) {
		t.Fatal("own id not admitted")
	}
	now = now.Add(10 * ownSessionNegativeTTL)
	if !ns.admits(poolIDB2) {
		t.Fatal("own id refused later")
	}
	if got := store.gets.Load(); got != 1 {
		t.Fatalf("store read %d times, want 1", got)
	}
}

// TestRigNamespacePoolSlotAndNamedSessionSpellings: pool slot spellings
// ("<agent>-N") and named-session names fold under the bead store's rules.
// The lexical arm admits any spelling that folds inside the namespace and
// embeds no pool session id. Config validation refuses this city's own
// spellings that would land there from outside the rig: another agent's
// qualified name, its first pool slot and the session name its base name
// yields, its namepool instance names, and named sessions' qualified names.
// These cases pin the classifier's half: no slot or session spelling outside
// the namespace is admitted.
func TestRigNamespacePoolSlotAndNamedSessionSpellings(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	lexical := []struct {
		v    string
		want bool
	}{
		{"beta/crew.worker-1", true},
		{"beta/crew.worker-12", true},
		{"beta--crew.worker-1", true},
		{"beta--crew__worker_1", true},
		{"beta/lead", true},
		{"beta--lead", true},
		// Inside the namespace to the store. Validation refuses a city agent
		// ("beta-", whose first slot is "beta--1"; "beta--crew") or named
		// session of this city that would spell them.
		{"beta--1", true},
		{"beta--crew-1", true},
		// City-scoped slots and other rigs' slots.
		{"crew.worker-1", false},
		{"beta-1", false},
		{"beta_1", false},
		{"beta.1", false},
		{"beta-crew-1", false},
		{"beta---crew-1", false},
		{"beta1/crew-1", false},
		{"beta-1/crew.worker", false},
		{"beta_1--crew.worker-1", false},
		{"alpha/crew.worker-1", false},
		{"alpha/beta-1", false},
		{"alpha--beta--crew-1", false},
		{"Beta--crew.worker-1", false},
		{"lead", false},
		{"-beta/crew", false},
		{"_beta/crew", false},
	}
	for _, tc := range lexical {
		if got := ns.admits(tc.v); got != tc.want {
			t.Errorf("admits(%q) = %v, want %v", tc.v, got, tc.want)
		}
	}

	// A rig whose name differs only after a separator is another namespace.
	other, _ := newRigNamespace(rnsGuardedRig("beta-1"), nil)
	for v, want := range map[string]bool{
		"beta-1/crew": true, "beta_1/crew": true, "beta.1--crew": true,
		"beta/crew": false, "beta-1-crew": false, "beta--1/crew": false,
	} {
		if got := other.admits(v); got != want {
			t.Errorf("rig beta-1 admits(%q) = %v, want %v", v, got, want)
		}
	}
}

// TestRigNamespaceProvenanceRefusesOtherSessionRecords: records in the
// city's own session store vouch only for its pool sessions of this rig's
// agents, never for a pool of a city-scoped agent whose name folds into the
// namespace, another rig's pool, or a named session's short id.
func TestRigNamespaceProvenanceRefusesOtherSessionRecords(t *testing.T) {
	foldedPoolID := "gcx-session-" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	otherRigPoolID := "gcx-session-" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cityPoolID := "gcx-session-" + "cccccccccccccccccccccccccccccccc"
	store := rnsNewCountingStore(
		rnsSessionBead(poolIDB, templateB, poolNameB),
		rnsSessionBead(foldedPoolID, "beta--crew", "beta--crew-"+foldedPoolID),
		rnsSessionBead(otherRigPoolID, "beta-1/crew", "crew-"+otherRigPoolID),
		rnsSessionBead(cityPoolID, "crew.worker", "crew.worker-"+cityPoolID),
		rnsSessionBead("gcx-21", "beta/lead", "beta--lead"),
		rnsSessionBead("gcx-22", "lead", "lead"),
	)
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store)))
	cases := []struct {
		v    string
		want admitVia
	}{
		{poolIDB, admitSessionRecord},
		{poolNameB, admitSessionRecord},
		// The same name under the store's fold is the same actor.
		{"crew_worker-" + poolIDB, admitSessionRecord},
		// A fold variant of the id itself is never looked up.
		{"gcx_session_" + tokenB, admitNone},
		{foldedPoolID, admitNone},
		{otherRigPoolID, admitNone},
		{"crew-" + otherRigPoolID, admitNone},
		{cityPoolID, admitNone},
		{"crew.worker-" + cityPoolID, admitNone},
		{"gcx-21", admitNone},
		{"gcx-22", admitNone},
		{"lead", admitNone},
		// The named session's alias is admitted lexically, as its claims are.
		{"beta--lead", admitLexical},
		// A pool session name of the folded city agent spells itself inside
		// the namespace, but it embeds a pool id, so only a record of a rig
		// agent admits it, and this record's template is not one.
		{"beta--crew-" + foldedPoolID, admitNone},
	}
	for _, tc := range cases {
		a := ns.admission(tc.v)
		if a.Via != tc.want || a.Err != nil {
			t.Errorf("admission(%q) = %s (err %v), want %s", tc.v, a.Via, a.Err, tc.want)
		}
	}
}

// TestRigNamespaceForeignPoolSessionNameNotAdmittedLexically: a pool
// session's runtime name is its template's last segment plus its id, so city
// A's agent "alpha/beta-" runs sessions named "beta--<A's id>", which fold
// into "beta/". B must not own A's released row that carries only those
// stamps, while A owns it and B keeps its own "beta/beta-" pool rows through
// its records.
func TestRigNamespaceForeignPoolSessionNameNotAdmittedLexically(t *testing.T) {
	nameA := PoolSessionName("alpha/beta-", poolIDA)
	if nameA != "beta--"+poolIDA {
		t.Fatalf("PoolSessionName = %q", nameA)
	}
	nsB, _ := rnsCityBNamespace(t)
	released := rnsRow("", rnsMD(beadmeta.SessionIDMetadataKey, poolIDA, beadmeta.SessionNameMetadataKey, nameA))
	if got := nsB.classifyRow(released); got.Class != rowForeign {
		t.Errorf("B classifies A's released row %+v, want foreign", got)
	}
	if a := nsB.admission(nameA); a.Admitted() {
		t.Errorf("B admits A's pool session name %q via %s", nameA, a.Via)
	}
	for _, key := range []string{beadmeta.SessionNameCamelMetadataKey, beadmeta.StrandedSessionMetadataKey} {
		if got := nsB.classify(rnsRow("", rnsMD(key, nameA))); got != rowForeign {
			t.Errorf("B classifies A's %s stamp %s, want foreign", key, got)
		}
	}
	for _, key := range []string{rowAssigneeKey, beadmeta.RoutedToMetadataKey, beadmeta.StrandedAssigneeMetadataKey} {
		b := rnsRow("", rnsMD(key, nameA))
		if key == rowAssigneeKey {
			b = rnsRow(nameA, nil)
		}
		if got := nsB.classify(b); got != rowForeign {
			t.Errorf("B classifies A's name in owner key %s as %s, want foreign", key, got)
		}
	}

	storeA := rnsNewCountingStore(rnsSessionBead(poolIDA, "alpha/beta-", nameA))
	nsA, _ := newRigNamespace(rnsGuardedRig("alpha"), newControllerSessionResolver(cityPathA, nil, rnsStoreOpener(storeA)))
	if got := nsA.classify(released); got != rowOwned {
		t.Errorf("A classifies its own released row %s, want owned", got)
	}

	nameB := PoolSessionName("beta/beta-", poolIDB)
	storeB := rnsNewCountingStore(rnsSessionBead(poolIDB, "beta/beta-", nameB))
	nsB2, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(storeB)))
	for _, b := range []beads.Bead{
		rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, nameB)),
		rnsRow(nameB, nil),
		rnsRow(poolIDB, rnsMD(beadmeta.RoutedToMetadataKey, "beta/beta-")),
	} {
		if got := nsB2.classifyRow(b); got.Class != rowOwned {
			t.Errorf("B classifies its own row %+v as %+v, want owned", b, got)
		}
	}
	if a := nsB2.admission(nameB); a.Via != admitSessionRecord {
		t.Errorf("B's own pool name admitted via %s, want session_record", a.Via)
	}
}

// TestRigNamespaceNameStampsNeedOwnRecord: a name-holding claim stamp is
// admitted only by a record in this city's sessions that carries the name and
// belongs to one of the rig's agents, never by its spelling alone.
func TestRigNamespaceNameStampsNeedOwnRecord(t *testing.T) {
	store := rnsNewCountingStore(
		rnsSessionBead("gcx-21", "beta/lead", "beta--lead"),
		rnsSessionBead("gcx-22", "lead", "beta--city-lead"),
		rnsSessionBead("gcx-23", "beta--fold", "beta--fold"),
		rnsSessionBead("gcx-24", "beta/old", "beta--old"),
	)
	if err := store.Close("gcx-24"); err != nil {
		t.Fatal(err)
	}
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store)))
	cases := []struct {
		name string
		key  string
		v    string
		want rowClass
	}{
		{"own rig named session", beadmeta.SessionNameMetadataKey, "beta--lead", rowOwned},
		{"own rig named session camel", beadmeta.SessionNameCamelMetadataKey, "beta--lead", rowOwned},
		{"own rig named session stranded", beadmeta.StrandedSessionMetadataKey, "beta--lead", rowOwned},
		{"own closed rig session", beadmeta.SessionNameMetadataKey, "beta--old", rowOwned},
		{"no record", beadmeta.SessionNameMetadataKey, "beta--ghost", rowForeign},
		{"city-scoped template", beadmeta.SessionNameMetadataKey, "beta--city-lead", rowForeign},
		{"template folds inside only", beadmeta.SessionNameMetadataKey, "beta--fold", rowForeign},
		{"qualified spelling no record", beadmeta.SessionNameMetadataKey, "beta/lead", rowForeign},
		// An id stamp is still admitted lexically (L or P).
		{"id stamp lexical", beadmeta.SessionIDMetadataKey, "beta/lead", rowOwned},
		{"id stamp short id", beadmeta.SessionIDMetadataKey, "gcx-21", rowForeign},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ns.classifyRow(rnsRow("", rnsMD(tc.key, tc.v)))
			if got.Class != tc.want || got.Err != nil {
				t.Fatalf("classifyRow(%s=%q) = %+v, want %s", tc.key, tc.v, got, tc.want)
			}
		})
	}
	before := store.lists.Load()
	for i := 0; i < 3; i++ {
		ns.classify(rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, "beta--ghost")))
		ns.classify(rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, "beta--lead")))
	}
	if got := store.lists.Load() - before; got != 0 {
		t.Fatalf("remembered name lookups re-queried the store %d times", got)
	}
}

func TestRigNamespaceNameStampFromSnapshot(t *testing.T) {
	snap := newSessionBeadSnapshotFromInfos([]sessionpkg.Info{{
		ID: "gcx-31", Type: sessionpkg.BeadType, Template: "beta/lead",
		SessionName: "beta--lead", SessionNameMetadata: "beta--lead",
	}})
	store := rnsNewCountingStore()
	res := newControllerSessionResolver(cityPathB, func() sessionRecordIndex { return snap }, rnsStoreOpener(store))
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	if got := ns.classify(rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, "beta--lead"))); got != rowOwned {
		t.Fatalf("snapshot named session stamp = %s, want owned", got)
	}
	if store.lists.Load() != 0 {
		t.Fatal("store queried for a name the snapshot answers")
	}
}

// TestRigNamespaceSnapshotRecordShapeChecked: a record in the snapshot that
// answers to a pool session name is still refused when its own id is not an
// unguessable pool id.
func TestRigNamespaceSnapshotRecordShapeChecked(t *testing.T) {
	for _, id := range []string{
		"gcx-session-" + tokenB[:16],
		"gcx-session-" + strings.ToUpper(tokenB),
		"gcx-session-" + tokenB + "-x",
		"gcx_session_" + tokenB,
		"gcx-41",
	} {
		snap := newSessionBeadSnapshotFromInfos([]sessionpkg.Info{{
			ID: id, Type: sessionpkg.BeadType, Template: templateB,
			SessionName: poolNameB, SessionNameMetadata: poolNameB,
		}})
		res := newControllerSessionResolver(cityPathB, func() sessionRecordIndex { return snap }, nil)
		ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
		if ns.admits(poolNameB) {
			t.Errorf("record id %q admitted pool session name %q", id, poolNameB)
		}
	}
}

// rnsPartialIDStore resolves a requested id to another record, as a store
// that accepts partial ids may.
type rnsPartialIDStore struct {
	*rnsCountingStore
	resolve map[string]string
}

func (s *rnsPartialIDStore) Get(id string) (beads.Bead, error) {
	if to, ok := s.resolve[id]; ok {
		id = to
	}
	return s.rnsCountingStore.Get(id)
}

func TestCitySessionResolverRefusesAnotherIDFromGet(t *testing.T) {
	name := "crew.worker-" + poolIDB
	store := &rnsPartialIDStore{
		rnsCountingStore: rnsNewCountingStore(rnsSessionBead(poolIDB2, templateB, name)),
		resolve:          map[string]string{poolIDB: poolIDB2},
	}
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store)))
	if ns.admits(name) {
		t.Fatal("a record read back under another id was admitted")
	}
}

// TestCitySessionResolverOwnPrefixesOnly: only ids under this city's own
// session id prefixes are looked up.
func TestCitySessionResolverOwnPrefixesOnly(t *testing.T) {
	hqID := "hq-session-" + tokenB
	rigID := "bt-session-" + tokenB
	records := []beads.Bead{
		rnsSessionBead(poolIDB, templateB, poolNameB),
		rnsSessionBead(hqID, templateB, "crew.worker-"+hqID),
		rnsSessionBead(rigID, templateB, "crew.worker-"+rigID),
	}
	t.Run("store prefix and HQ prefix", func(t *testing.T) {
		store := rnsNewCountingStore(records...)
		ns, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store), "hq"))
		for _, v := range []string{poolIDB, poolNameB, hqID, "crew.worker-" + hqID} {
			if !ns.admits(v) {
				t.Errorf("own id or name %q not admitted", v)
			}
		}
		before := store.gets.Load()
		for _, v := range []string{rigID, "crew.worker-" + rigID, "gcs-session-" + tokenA} {
			if ns.admits(v) {
				t.Errorf("id under a foreign prefix %q admitted", v)
			}
		}
		if got := store.gets.Load() - before; got != 0 {
			t.Errorf("ids under foreign prefixes were read %d times", got)
		}
	})
	t.Run("no prefix known", func(t *testing.T) {
		store := rnsNewCountingStore(records...)
		store.prefix = ""
		ns, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store)))
		if ns.admits(poolIDB) {
			t.Error("pool id admitted with no session id prefix known")
		}
		if got := store.gets.Load(); got != 0 {
			t.Errorf("store read %d times with no prefix known", got)
		}
	})
}

// TestCitySessionResolverSingleFlight: concurrent lookups of one id share one
// store read.
func TestCitySessionResolverSingleFlight(t *testing.T) {
	store := &rnsGateStore{rnsCountingStore: rnsNewCountingStore(), release: make(chan struct{})}
	res := newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store))
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	const n = 32
	var started atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < n; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started.Add(1)
			ns.classify(rnsRow(poolIDA, nil))
		}()
	}
	deadline := time.Now().Add(30 * time.Second)
	for started.Load() < n || store.gets.Load() == 0 {
		if time.Now().After(deadline) {
			close(store.release)
			wg.Wait()
			t.Fatalf("the store read never started (started=%d, reads=%d)", started.Load(), store.gets.Load())
		}
		runtime.Gosched()
	}
	for i := 0; i < 100; i++ {
		runtime.Gosched()
	}
	close(store.release)
	wg.Wait()
	if got := store.gets.Load(); got != 1 {
		t.Fatalf("%d concurrent classifications of one id issued %d store reads, want 1", n, got)
	}
}

// rnsGateStore blocks reads until release is closed.
type rnsGateStore struct {
	*rnsCountingStore
	release chan struct{}
}

func (s *rnsGateStore) Get(id string) (beads.Bead, error) {
	s.gets.Add(1)
	<-s.release
	return s.Store.Get(id)
}

// TestCitySessionResolverOutageBacksOff: after the first store error, lookups
// that need the store fail fast until the next pass; the snapshot still
// answers, and the store handle is reopened afterwards.
func TestCitySessionResolverOutageBacksOff(t *testing.T) {
	var opens atomic.Int64
	store := rnsNewCountingStore(rnsSessionBead(poolIDB2, templateB, "crew.worker-"+poolIDB2))
	snap := newSessionBeadSnapshotFromInfos([]sessionpkg.Info{{ID: poolIDB, Type: sessionpkg.BeadType, Template: templateB}})
	res := newControllerSessionResolver(cityPathB, func() sessionRecordIndex { return snap }, func() (beads.SessionStore, error) {
		opens.Add(1)
		return beadstest.Sessions(store), nil
	})
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	store.setErr(errors.New("offline"))
	for i := 0; i < 100; i++ {
		got := ns.classifyRow(rnsRow(fmt.Sprintf("gcx-session-%032x", i+1), nil))
		if got.Class != rowForeign || got.Err == nil {
			t.Fatalf("row %d during outage = %+v, want foreign with error", i, got)
		}
	}
	if got := store.gets.Load(); got != 1 {
		t.Fatalf("100 rows during an outage issued %d store reads, want 1", got)
	}
	if !ns.admits(poolIDB) {
		t.Fatal("snapshot record refused during the outage")
	}
	store.setErr(nil)
	res.beginPass()
	if !ns.admits(poolIDB2) {
		t.Fatal("store record refused after the outage ended")
	}
	if got := opens.Load(); got != 2 {
		t.Fatalf("store opened %d times, want 2 (reopened after the error)", got)
	}
}

// TestCitySessionResolverReopensAfterInvalidate: a reload that replaces the
// sessions store is picked up after invalidateStore.
func TestCitySessionResolverReopensAfterInvalidate(t *testing.T) {
	old := rnsNewCountingStore()
	fresh := rnsNewCountingStore(rnsSessionBead(poolIDB2, templateB, "crew.worker-"+poolIDB2))
	var current atomic.Pointer[rnsCountingStore]
	current.Store(old)
	res := newControllerSessionResolver(cityPathB, nil, func() (beads.SessionStore, error) { return beadstest.Sessions(current.Load()), nil })
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	if ns.admits(poolIDA) {
		t.Fatal("unknown id admitted")
	}
	current.Store(fresh)
	res.invalidateStore()
	if !ns.admits(poolIDB2) {
		t.Fatal("record in the replacement store refused")
	}
	if old.gets.Load() != 1 || fresh.gets.Load() != 1 {
		t.Fatalf("reads old=%d fresh=%d, want 1 and 1", old.gets.Load(), fresh.gets.Load())
	}
}

func TestCitySessionResolverEnvOnlyForCLI(t *testing.T) {
	if newControllerSessionResolver(cityPathB, nil, nil).envFastPath {
		t.Fatal("controller resolver reads the process environment")
	}
	if !newCLISessionResolver(cityPathB, nil).envFastPath {
		t.Fatal("CLI resolver does not read the process environment")
	}
}

func TestRigNamespaceStringNames(t *testing.T) {
	pins := map[string]string{
		admitNone.String():            "none",
		admitLexical.String():         "lexical",
		admitSessionRecord.String():   "session_record",
		rowForeign.String():           "foreign",
		rowOwned.String():             "owned",
		rowUnattributed.String():      "unattributed",
		rowRuleNone.String():          "none",
		rowRuleOwnerKeys.String():     "owner_keys",
		rowRuleClaimStamps.String():   "claim_stamps",
		rowRuleCreationStamp.String(): "creation_stamp",
	}
	for got, want := range pins {
		if got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}

func TestRigNamespaceBlankKeysDoNotCount(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	for _, k := range append(append([]string{}, rigNamespaceClaimStampKeys...), rigNamespaceOwnerMetadataKeys...) {
		if got := ns.classify(rnsRow("", rnsMD(k, " \t"))); got != rowUnattributed {
			t.Errorf("blank %s classifies %s, want unattributed", k, got)
		}
	}
	if got := ns.classify(rnsRow(" ", nil)); got != rowUnattributed {
		t.Errorf("blank assignee classifies %s, want unattributed", got)
	}
}

// TestRigNamespaceDenyAllIsForeign: a nameless or deny-all namespace
// classifies every row Foreign, identity-less rows included.
func TestRigNamespaceDenyAllIsForeign(t *testing.T) {
	nameless, _ := newRigNamespace(config.Rig{Federation: config.RigFederationRig}, nil)
	for _, ns := range []*rigNamespace{denyAllRigNamespace(nil), nameless, nil} {
		for _, b := range []beads.Bead{rnsRow("", nil), rnsRow("", rnsMD("title_hint", "x")), rnsRow("beta/x", nil)} {
			if got := ns.classify(b); got != rowForeign {
				t.Errorf("deny-all classify(%+v) = %s, want foreign", b, got)
			}
		}
	}
}

func TestRigNamespaceSessionRowTemplateNeedsLiteralRig(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	for template, want := range map[string]rowClass{
		"beta/crew.worker":  rowOwned,
		"beta--crew.worker": rowForeign,
		"beta_crew":         rowForeign,
	} {
		b := rnsSessionBead(poolIDB, template, "")
		if got := ns.classify(b); got != want {
			t.Errorf("session row template %q = %s, want %s", template, got, want)
		}
	}
}

// TestRigNamespaceManyCitiesConcurrently: several cities in one process, each
// with its own resolver, used concurrently with memo expiry and outages,
// never admit each other's pool sessions.
func TestRigNamespaceManyCitiesConcurrently(t *testing.T) {
	const cities = 6
	type city struct {
		ns    *rigNamespace
		res   *citySessionResolver
		store *rnsCountingStore
		ids   []string
	}
	var clock atomic.Int64
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	cs := make([]city, cities)
	for c := 0; c < cities; c++ {
		rig := fmt.Sprintf("rig%d", c)
		var recs []beads.Bead
		var ids []string
		for i := 0; i < 20; i++ {
			id := fmt.Sprintf("gcs-session-%016x%016x", c+1, i+1) // every city shares the prefix
			ids = append(ids, id)
			recs = append(recs, rnsSessionBead(id, rig+"/crew.worker", "crew.worker-"+id))
		}
		store := rnsNewCountingStore(recs...)
		store.prefix = "gcs"
		res := newCitySessionResolver(citySessionResolverOptions{CityPath: fmt.Sprintf("/cities/%d", c), OpenStore: rnsStoreOpener(store), Now: now})
		ns, _ := newRigNamespace(rnsGuardedRig(rig), res)
		cs[c] = city{ns: ns, res: res, store: store, ids: ids}
	}
	var wrong atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 24; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				me := cs[(g+i)%cities]
				other := cs[(g+i+1)%cities]
				id := other.ids[i%len(other.ids)]
				if me.ns.admits(id) || me.ns.admits("crew.worker-"+id) {
					wrong.Add(1)
				}
				if me.ns.classify(rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, "crew.worker-"+id))) != rowForeign {
					wrong.Add(1)
				}
				own := me.ns.admission(me.ids[i%len(me.ids)])
				if !own.Admitted() && own.Err == nil {
					wrong.Add(1)
				}
				if i%50 == 0 {
					clock.Add(int64(ownSessionNegativeTTL))
				}
				if i%97 == 0 {
					me.store.setErr(errors.New("blip"))
					me.ns.admits(fmt.Sprintf("gcs-session-%032x", g*1000+i))
					me.store.setErr(nil)
					me.res.beginPass()
				}
			}
		}(g)
	}
	wg.Wait()
	if wrong.Load() != 0 {
		t.Fatalf("%d wrong answers across cities", wrong.Load())
	}
}

// TestRigNamespaceNameStampOutsideNamespaceNotAdmitted: a rig agent's session
// can run under a name outside the rig's namespace (a session template of
// "{{.Name}}" renders "beta/lead" as "lead", as does a tmux alias), and
// another city importing the same pack runs its sessions under that name.
// B's record of the name must not make A's released row B's.
func TestRigNamespaceNameStampOutsideNamespaceNotAdmitted(t *testing.T) {
	name := "lead"
	if got := config.NamedSessionRuntimeName("city-b", config.Workspace{SessionTemplate: "{{.Name}}"}, "beta/lead"); got != name {
		t.Fatalf("runtime name = %q", got)
	}
	store := rnsNewCountingStore(rnsSessionBead("gcx-21", "beta/lead", name))
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store)))
	for _, key := range []string{beadmeta.SessionNameMetadataKey, beadmeta.SessionNameCamelMetadataKey, beadmeta.StrandedSessionMetadataKey} {
		row := rnsRow("", rnsMD(beadmeta.SessionIDMetadataKey, "gcx-77", key, name))
		if got := ns.classifyRow(row); got.Class != rowForeign {
			t.Errorf("B classifies A's row stamped %s=%q as %+v, want foreign", key, name, got)
		}
		if got := ns.classify(rnsRow("", rnsMD(key, name))); got != rowForeign {
			t.Errorf("stamp %s=%q alone classifies %s, want foreign", key, name, got)
		}
	}
	if got := store.lists.Load(); got != 0 {
		t.Errorf("a name outside the namespace was queried %d times", got)
	}
}

// TestRigNamespaceEveryNameStampFollowsTheNameRule: gc.sessionName and
// gc.stranded_session, like gc.session_name, need a name inside the namespace
// and a record of it.
func TestRigNamespaceEveryNameStampFollowsTheNameRule(t *testing.T) {
	store := rnsNewCountingStore(
		rnsSessionBead("gcx-21", "beta/lead", "beta--lead"),
		rnsSessionBead("gcx-25", "beta/out", "out-lead"),
	)
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store)))
	for _, key := range []string{beadmeta.SessionNameMetadataKey, beadmeta.SessionNameCamelMetadataKey, beadmeta.StrandedSessionMetadataKey} {
		for v, want := range map[string]rowClass{
			"beta--lead":  rowOwned,
			"beta--ghost": rowForeign, // inside, no record
			"beta/ghost":  rowForeign, // inside, no record
			"out-lead":    rowForeign, // recorded, outside
		} {
			if got := ns.classify(rnsRow("", rnsMD(key, v))); got != want {
				t.Errorf("%s=%q classifies %s, want %s", key, v, got, want)
			}
		}
	}
}

// TestRigNamespaceNameStampRechecksResolverName: the name path re-checks the
// name of the record a resolver returns.
func TestRigNamespaceNameStampRechecksResolverName(t *testing.T) {
	lying := rnsFakeResolver{rec: ownSessionRecord{ID: "gcx-61", SessionName: "beta--other", Template: "beta/lead"}, ok: true}
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), lying)
	if got := ns.classify(rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, "beta--lead"))); got != rowForeign {
		t.Fatalf("record named otherwise admitted the stamp: %s", got)
	}
	honest := rnsFakeResolver{rec: ownSessionRecord{ID: "gcx-61", SessionName: "beta--lead", Template: "beta/lead"}, ok: true}
	ns, _ = newRigNamespace(rnsGuardedRig("beta"), honest)
	if got := ns.classify(rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, "beta--lead"))); got != rowOwned {
		t.Fatalf("matching record refused: %s", got)
	}
}

func TestRigNamespaceNameStampNilResolver(t *testing.T) {
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), nil)
	for _, key := range []string{beadmeta.SessionNameMetadataKey, beadmeta.SessionNameCamelMetadataKey, beadmeta.StrandedSessionMetadataKey} {
		if got := ns.classify(rnsRow("", rnsMD(key, "beta--lead"))); got != rowForeign {
			t.Errorf("%s with a nil resolver classifies %s, want foreign", key, got)
		}
	}
}

func TestCitySessionResolverNameLookupSkipsNonSessionRows(t *testing.T) {
	work := beads.Bead{ID: "gcx-71", Type: "task", Status: "open", Metadata: rnsMD("session_name", "beta--lead", "template", "beta/lead")}
	store := rnsNewCountingStore(work)
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store)))
	if got := ns.classify(rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, "beta--lead"))); got != rowForeign {
		t.Fatalf("a work row carrying session metadata admitted a name stamp: %s", got)
	}
}

// rnsSupersetStore ignores metadata filters on List, as a backend that
// cannot push them down may.
type rnsSupersetStore struct{ *rnsCountingStore }

func (s *rnsSupersetStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	q.Metadata = nil
	q.AllowScan = true
	return s.rnsCountingStore.List(q)
}

func TestCitySessionResolverNameLookupExact(t *testing.T) {
	store := &rnsSupersetStore{rnsNewCountingStore(
		rnsSessionBead("gcx-21", "beta/lead", "beta--lead"),
		rnsSessionBead("gcx-22", "beta/other", "beta--other"),
	)}
	res := newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store))
	recs, err := res.lookupOwnSessionsByName("beta--lead", nil)
	if err != nil || len(recs) != 1 || recs[0].ID != "gcx-21" {
		t.Fatalf("lookupOwnSessionsByName = %+v, %v; want only gcx-21", recs, err)
	}
}

// TestCitySessionResolverNameLookupReadsPastFirstRecord: a name with several
// records is admitted by its rig record even when another comes first.
func TestCitySessionResolverNameLookupReadsPastFirstRecord(t *testing.T) {
	store := rnsNewCountingStore(
		rnsSessionBead("gcx-21", "lead", "beta--lead"),
		rnsSessionBead("gcx-22", "beta/lead", "beta--lead"),
	)
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store)))
	if got := ns.classify(rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, "beta--lead"))); got != rowOwned {
		t.Fatalf("rig record behind a city record refused: %s", got)
	}
}

// TestCitySessionResolverNameFastPathsDoNotHideStoreRecords: a record the
// controller's snapshot or the caller's own session holds under a name, but
// that is not one of the rig's agents, does not hide the rig's record of that
// name in the session store, here a closed one.
func TestCitySessionResolverNameFastPathsDoNotHideStoreRecords(t *testing.T) {
	newStore := func(t *testing.T) *rnsCountingStore {
		t.Helper()
		store := rnsNewCountingStore(
			rnsSessionBead("gcx-21", "lead", "beta--lead"),
			rnsSessionBead("gcx-22", "beta/lead", "beta--lead"),
		)
		if err := store.Close("gcx-22"); err != nil {
			t.Fatal(err)
		}
		return store
	}
	row := rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, "beta--lead"))
	t.Run("snapshot", func(t *testing.T) {
		snap := newSessionBeadSnapshotFromInfos([]sessionpkg.Info{{
			ID: "gcx-21", Type: sessionpkg.BeadType, Template: "lead",
			SessionName: "beta--lead", SessionNameMetadata: "beta--lead",
		}})
		res := newControllerSessionResolver(cityPathB, func() sessionRecordIndex { return snap }, rnsStoreOpener(newStore(t)))
		ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
		if got := ns.classifyRow(row); got.Class != rowOwned {
			t.Fatalf("rig record behind a city-scoped snapshot record = %+v, want owned", got)
		}
	})
	t.Run("own session", func(t *testing.T) {
		city := t.TempDir()
		env := map[string]string{"GC_CITY_PATH": city, "GC_SESSION_ID": "gcx-21", "GC_SESSION_NAME": "beta--lead", "GC_TEMPLATE": "lead"}
		res := newCitySessionResolver(citySessionResolverOptions{
			CityPath:  city,
			Getenv:    func(k string) string { return env[k] },
			OpenStore: rnsStoreOpener(newStore(t)),
		})
		ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
		if got := ns.classifyRow(row); got.Class != rowOwned {
			t.Fatalf("rig record behind the caller's city-scoped session = %+v, want owned", got)
		}
	})
}

// TestCitySessionResolverNameMemoYieldsToSnapshot is the name twin of
// TestCitySessionResolverMissThenCreated: a store answer remembered for a name
// before the rig's record of it existed (a miss, or another record of the
// name) does not hide that record once its session closes and leaves the
// snapshot.
func TestCitySessionResolverNameMemoYieldsToSnapshot(t *testing.T) {
	info := func(b beads.Bead) sessionpkg.Info {
		name := b.Metadata["session_name"]
		return sessionpkg.Info{ID: b.ID, Type: sessionpkg.BeadType, Template: b.Metadata["template"], SessionName: name, SessionNameMetadata: name}
	}
	row := rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, "beta--lead"))
	for _, tc := range []struct {
		name string
		// before are open under the name before the rig's record exists.
		before []beads.Bead
	}{
		{"miss", nil},
		{"another record", []beads.Bead{rnsSessionBead("gcx-21", "lead", "beta--lead")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStoreFrom(100, nil, nil)
			mem.HonorExplicitIDs = true
			store := &rnsCountingStore{Store: mem, prefix: "gcx"}
			var open []sessionpkg.Info
			for _, b := range tc.before {
				if _, err := store.Create(b); err != nil {
					t.Fatal(err)
				}
				open = append(open, info(b))
			}
			snap := newSessionBeadSnapshotFromInfos(open)
			res := newControllerSessionResolver(cityPathB, func() sessionRecordIndex { return snap }, rnsStoreOpener(store))
			ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
			if got := ns.classifyRow(row); got.Class != rowForeign {
				t.Fatalf("before the rig's record exists = %+v, want foreign", got)
			}
			for _, b := range tc.before {
				if err := store.Close(b.ID); err != nil {
					t.Fatal(err)
				}
			}
			rigLead := rnsSessionBead("gcx-22", "beta/lead", "beta--lead")
			if _, err := store.Create(rigLead); err != nil {
				t.Fatal(err)
			}
			snap = newSessionBeadSnapshotFromInfos([]sessionpkg.Info{info(rigLead)})
			if got := ns.classifyRow(row); got.Class != rowOwned {
				t.Fatalf("rig record open in the snapshot = %+v, want owned", got)
			}
			if err := store.Close(rigLead.ID); err != nil {
				t.Fatal(err)
			}
			snap = newSessionBeadSnapshotFromInfos(nil)
			fresh, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store)))
			if got := fresh.classifyRow(row); got.Class != rowOwned {
				t.Fatalf("fresh resolver after the close = %+v, want owned", got)
			}
			res.beginPass()
			if got := ns.classifyRow(row); got.Class != rowOwned {
				t.Fatalf("after the close = %+v, want owned, as a fresh resolver reads it", got)
			}
		})
	}
}

// rnsHeldListStore reads the store when List is called but holds the first
// answer until releaseFirst: a slow read that saw the store before a later
// write.
type rnsHeldListStore struct {
	*rnsCountingStore
	first   sync.Once
	read    chan struct{} // closed once the first List has read the store
	release chan struct{}
	unblock sync.Once
}

func (s *rnsHeldListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.rnsCountingStore.List(q)
	held := false
	s.first.Do(func() {
		held = true
		close(s.read)
	})
	if held {
		<-s.release
	}
	return rows, err
}

// releaseFirst lets the first List return.
func (s *rnsHeldListStore) releaseFirst() { s.unblock.Do(func() { close(s.release) }) }

// TestCitySessionResolverNameReadOlderThanSnapshotForgotten: a store read of a
// name that saw the store before the rig's record existed, and still runs when
// the snapshot answers with that record, is forgotten. Its answer is not
// remembered, and a later lookup does not wait on it, so neither hides the
// record once its session closes.
func TestCitySessionResolverNameReadOlderThanSnapshotForgotten(t *testing.T) {
	row := rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, "beta--lead"))
	// start holds a lookup's store read that saw no record, then opens the
	// rig's record in the snapshot, where another lookup accepts it, and
	// closes it again. The held read still runs when start returns.
	start := func(t *testing.T) (*rigNamespace, *citySessionResolver, *rnsHeldListStore, <-chan struct{}) {
		t.Helper()
		mem := beads.NewMemStoreFrom(100, nil, nil)
		mem.HonorExplicitIDs = true
		store := &rnsHeldListStore{
			rnsCountingStore: &rnsCountingStore{Store: mem, prefix: "gcx"},
			read:             make(chan struct{}),
			release:          make(chan struct{}),
		}
		var snap atomic.Pointer[sessionBeadSnapshot]
		snap.Store(newSessionBeadSnapshotFromInfos(nil))
		res := newControllerSessionResolver(cityPathB, func() sessionRecordIndex { return snap.Load() }, rnsStoreOpener(store))
		ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
		heldDone := make(chan struct{})
		go func() {
			defer close(heldDone)
			ns.classifyRow(row)
		}()
		t.Cleanup(func() {
			store.releaseFirst()
			<-heldDone
		})
		awaitClose(t, store.read, "the first store read")
		if _, err := store.Create(rnsSessionBead("gcx-22", "beta/lead", "beta--lead")); err != nil {
			t.Fatal(err)
		}
		snap.Store(newSessionBeadSnapshotFromInfos([]sessionpkg.Info{{
			ID: "gcx-22", Type: sessionpkg.BeadType, Template: "beta/lead",
			SessionName: "beta--lead", SessionNameMetadata: "beta--lead",
		}}))
		if got := ns.classifyRow(row); got.Class != rowOwned {
			t.Fatalf("rig record open in the snapshot = %+v, want owned", got)
		}
		if err := store.Close("gcx-22"); err != nil {
			t.Fatal(err)
		}
		snap.Store(newSessionBeadSnapshotFromInfos(nil))
		return ns, res, store, heldDone
	}
	t.Run("answer not remembered", func(t *testing.T) {
		ns, res, store, heldDone := start(t)
		store.releaseFirst()
		awaitClose(t, heldDone, "the held lookup")
		res.beginPass()
		if got := ns.classifyRow(row); got.Class != rowOwned {
			t.Fatalf("after the close = %+v, want owned: the read that saw no record was remembered", got)
		}
	})
	t.Run("not waited on", func(t *testing.T) {
		ns, _, _, _ := start(t)
		var got rowClassification
		done := make(chan struct{})
		go func() {
			defer close(done)
			got = ns.classifyRow(row)
		}()
		awaitClose(t, done, "a lookup after the close, while the read that saw no record is held")
		if got.Class != rowOwned {
			t.Fatalf("after the close = %+v, want owned", got)
		}
	})
}

// TestCitySessionResolverIDFastPathsDoNotHidePoolRecord: a record the
// snapshot or the caller's own session holds under a pool session's runtime
// name, but whose short id cannot be that session's record (the bead the
// adoption barrier makes for a runtime that outlived its record), does not
// hide the pool record in the session store, here a closed one.
func TestCitySessionResolverIDFastPathsDoNotHidePoolRecord(t *testing.T) {
	newStore := func(t *testing.T) *rnsCountingStore {
		t.Helper()
		store := rnsNewCountingStore(
			rnsSessionBead(poolIDB, templateB, poolNameB),
			rnsSessionBead("gcx-101", "", poolNameB),
		)
		if err := store.Close(poolIDB); err != nil {
			t.Fatal(err)
		}
		return store
	}
	row := rnsRow(poolNameB, nil)
	t.Run("snapshot", func(t *testing.T) {
		snap := newSessionBeadSnapshotFromInfos([]sessionpkg.Info{{
			ID: "gcx-101", Type: sessionpkg.BeadType,
			SessionName: poolNameB, SessionNameMetadata: poolNameB,
		}})
		res := newControllerSessionResolver(cityPathB, func() sessionRecordIndex { return snap }, rnsStoreOpener(newStore(t)))
		ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
		if got := ns.classifyRow(row); got.Class != rowOwned {
			t.Fatalf("pool record behind an adopted snapshot record = %+v, want owned", got)
		}
	})
	t.Run("own session", func(t *testing.T) {
		city := t.TempDir()
		env := map[string]string{"GC_CITY_PATH": city, "GC_SESSION_ID": "gcx-101", "GC_SESSION_NAME": poolNameB, "GC_TEMPLATE": templateB}
		res := newCitySessionResolver(citySessionResolverOptions{
			CityPath:  city,
			Getenv:    func(k string) string { return env[k] },
			OpenStore: rnsStoreOpener(newStore(t)),
		})
		ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
		if got := ns.classifyRow(row); got.Class != rowOwned {
			t.Fatalf("pool record behind the caller's adopted session = %+v, want owned", got)
		}
	})
}

func TestCitySessionResolverConfiguredPrefixTrimmed(t *testing.T) {
	store := rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, poolNameB))
	store.prefix = ""
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store), " gcx- "))
	if !ns.admits(poolIDB) || !ns.admits(poolNameB) {
		t.Fatal("configured prefix with a trailing dash not used")
	}
}

// TestCitySessionResolverSelfAnswersOnlyItsName: the caller's own session
// answers to its own name without a store read; other names still reach the
// store.
func TestCitySessionResolverSelfAnswersOnlyItsName(t *testing.T) {
	city := t.TempDir()
	env := map[string]string{"GC_CITY_PATH": city, "GC_SESSION_ID": "gcx-51", "GC_SESSION_NAME": "beta--lead", "GC_TEMPLATE": "beta/lead"}
	store := rnsNewCountingStore(rnsSessionBead("gcx-24", "beta/old", "beta--old"))
	res := newCitySessionResolver(citySessionResolverOptions{
		CityPath:  city,
		Getenv:    func(k string) string { return env[k] },
		OpenStore: rnsStoreOpener(store),
	})
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	if got := ns.classify(rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, "beta--lead"))); got != rowOwned || store.lists.Load() != 0 {
		t.Fatalf("own session's name classifies %s after %d store lists, want owned after none", got, store.lists.Load())
	}
	for v, want := range map[string]rowClass{"beta--lead": rowOwned, "beta--old": rowOwned, "beta--ghost": rowForeign} {
		if got := ns.classify(rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, v))); got != want {
			t.Errorf("session_name=%q classifies %s, want %s", v, got, want)
		}
	}
}

// TestCitySessionResolverSingleFlightSurvivesPanic: a panic in the store read
// is an error to every waiter, never a hit or a miss, and never leaves the
// key wedged.
func TestCitySessionResolverSingleFlightSurvivesPanic(t *testing.T) {
	store := &rnsPanicOnceStore{rnsCountingStore: rnsNewCountingStore(rnsSessionBead(poolIDB2, templateB, "crew.worker-"+poolIDB2))}
	res := newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store))
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	panicked := func() (p any) {
		defer func() { p = recover() }()
		ns.admits(poolIDB2)
		return nil
	}()
	if panicked == nil {
		t.Fatal("the store's panic was swallowed")
	}
	done := make(chan rigAdmission, 2)
	go func() {
		done <- ns.admission(poolIDB2)
		res.beginPass()
		done <- ns.admission(poolIDB2)
	}()
	first := <-done
	if first.Admitted() || first.Err == nil {
		t.Fatalf("lookup during the backoff after a panic = %+v, want refused with error", first)
	}
	if second := <-done; !second.Admitted() {
		t.Fatalf("own id refused after beginPass: %+v", second)
	}
}

type rnsPanicOnceStore struct {
	*rnsCountingStore
	mu       sync.Mutex
	panicked bool
}

func (s *rnsPanicOnceStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	first := !s.panicked
	s.panicked = true
	s.mu.Unlock()
	if first {
		panic("store read panicked")
	}
	return s.rnsCountingStore.Get(id)
}

// TestCitySessionResolverHitsKeyedByRecord: every spelling of one pool
// session name shares one remembered record.
func TestCitySessionResolverHitsKeyedByRecord(t *testing.T) {
	ns, store := rnsCityBNamespace(t)
	res := ns.sessions.(*citySessionResolver)
	variants := 0
	for _, sep := range []string{".", "_", "-", "..", "._", "_-", "...", "-_.", "....", "-.-.-"} {
		v := "crew" + sep + "worker-" + poolIDB2
		if !ns.admits(v) {
			t.Fatalf("variant %q not admitted", v)
		}
		variants++
	}
	res.mu.Lock()
	hits := len(res.hits)
	res.mu.Unlock()
	if hits != 1 {
		t.Fatalf("%d spellings left %d hit entries, want 1", variants, hits)
	}
	if got := store.gets.Load(); got != 1 {
		t.Fatalf("store read %d times, want 1", got)
	}
}

// TestCitySessionResolverNameHitExpires: a name answer is reused for the TTL,
// then read again, so a record added later is seen.
func TestCitySessionResolverNameHitExpires(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := rnsNewCountingStore(rnsSessionBead("gcx-31", "crew.lead", "beta--lead"))
	res := newCitySessionResolver(citySessionResolverOptions{CityPath: cityPathB, OpenStore: rnsStoreOpener(store), Now: func() time.Time { return now }})
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	row := rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, "beta--lead"))
	if got := ns.classify(row); got != rowForeign {
		t.Fatalf("before the rig record exists: %s", got)
	}
	if _, err := store.Create(rnsSessionBead("gcx-32", "beta/lead", "beta--lead")); err != nil {
		t.Fatal(err)
	}
	if got := ns.classify(row); got != rowForeign {
		t.Fatalf("within the TTL the remembered answer changed: %s", got)
	}
	now = now.Add(ownSessionNegativeTTL)
	if got := ns.classify(row); got != rowOwned {
		t.Fatalf("after the TTL the rig record is not seen: %s", got)
	}
}

func TestCitySessionResolverOverlongValueSkipsStore(t *testing.T) {
	var opens atomic.Int64
	store := rnsNewCountingStore()
	res := newControllerSessionResolver(cityPathB, nil, func() (beads.SessionStore, error) { opens.Add(1); return beadstest.Sessions(store), nil })
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	long := strings.Repeat("0", 600) + "-" + poolIDA
	if ns.admits(long) {
		t.Fatal("overlong value admitted")
	}
	if recs, err := res.lookupOwnSessionsByName("beta/"+strings.Repeat("x", 600), nil); recs != nil || err != nil {
		t.Fatalf("overlong name lookup = %v, %v", recs, err)
	}
	res.mu.Lock()
	misses := len(res.misses)
	res.mu.Unlock()
	if opens.Load() != 0 || misses != 0 {
		t.Fatalf("overlong values opened the store %d time(s) and left %d misses", opens.Load(), misses)
	}
}

// TestCitySessionResolverBackoffNeverAdmits: during the backoff no new id or
// name is admitted, remembered answers keep their meaning, and the backoff
// ends with time.
func TestCitySessionResolverBackoffNeverAdmits(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := rnsNewCountingStore(rnsSessionBead(poolIDB2, templateB, "crew.worker-"+poolIDB2), rnsSessionBead("gcx-21", "beta/lead", "beta--lead"))
	res := newCitySessionResolver(citySessionResolverOptions{CityPath: cityPathB, OpenStore: rnsStoreOpener(store), Now: func() time.Time { return now }})
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	if !ns.admits(poolIDB2) || ns.admits(poolIDA) {
		t.Fatal("baseline answers wrong")
	}
	store.setErr(errors.New("offline"))
	if a := ns.admission(poolIDB); a.Admitted() || a.Err == nil {
		t.Fatalf("unknown own-prefix id during outage: %+v", a)
	}
	store.setErr(nil)
	for _, v := range []string{poolIDB, "crew.worker-" + poolIDB} {
		if ns.admits(v) {
			t.Fatalf("%q admitted during the backoff", v)
		}
	}
	nameRow := rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, "beta--lead"))
	if got := ns.classify(nameRow); got != rowForeign {
		t.Fatalf("name stamp during the backoff: %s", got)
	}
	if !ns.admits(poolIDB2) || ns.admits(poolIDA) {
		t.Fatal("remembered answers changed during the backoff")
	}
	now = now.Add(ownSessionErrorBackoff)
	if got := ns.classify(nameRow); got != rowOwned {
		t.Fatalf("name stamp after the backoff: %s", got)
	}
}

// TestRigNamespaceNameQueryErrorFailsClosed: a failing session_name query
// leaves a name stamp Foreign with the error and starts the backoff.
func TestRigNamespaceNameQueryErrorFailsClosed(t *testing.T) {
	store := rnsNewCountingStore(rnsSessionBead("gcx-21", "beta/lead", "beta--lead"))
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store)))
	boom := errors.New("list failed")
	store.setErr(boom)
	row := rnsRow("", rnsMD(beadmeta.SessionNameMetadataKey, "beta--lead"))
	if got := ns.classifyRow(row); got.Class != rowForeign || !errors.Is(got.Err, boom) {
		t.Fatalf("name stamp with a failing query = %+v, want foreign with error", got)
	}
	store.setErr(nil)
	lists := store.lists.Load()
	if got := ns.classifyRow(row); got.Class != rowForeign || got.Err == nil {
		t.Fatalf("name stamp during the backoff = %+v, want foreign with error", got)
	}
	if store.lists.Load() != lists {
		t.Fatal("the store was queried during the backoff")
	}
	ns.sessions.(*citySessionResolver).beginPass()
	if got := ns.classify(row); got != rowOwned {
		t.Fatalf("name stamp after beginPass = %s, want owned", got)
	}
}

// TestCitySessionResolverIDPrefixPanicDoesNotWedge: a panic in the store's
// IDPrefix() reaches the caller without leaving the resolver locked; after
// beginPass the city's own id is admitted.
func TestCitySessionResolverIDPrefixPanicDoesNotWedge(t *testing.T) {
	store := &rnsPrefixPanicOnceStore{rnsCountingStore: rnsNewCountingStore(rnsSessionBead(poolIDB2, templateB, "crew.worker-"+poolIDB2))}
	res := newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store))
	ns, _ := newRigNamespace(rnsGuardedRig("beta"), res)
	first := make(chan any, 1)
	go func() {
		defer func() { first <- recover() }()
		ns.admits(poolIDB2)
	}()
	select {
	case p := <-first:
		if p == nil {
			t.Fatal("the IDPrefix panic was swallowed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("lookup whose store IDPrefix() panicked never returned")
	}
	second := make(chan bool, 1)
	go func() {
		res.beginPass()
		second <- ns.admits(poolIDB2)
	}()
	select {
	case got := <-second:
		if !got {
			t.Fatal("own id refused after the panic and beginPass")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a later lookup blocked after a panic in IDPrefix()")
	}
}

type rnsPrefixPanicOnceStore struct {
	*rnsCountingStore
	mu       sync.Mutex
	panicked bool
}

func (s *rnsPrefixPanicOnceStore) IDPrefix() string {
	s.mu.Lock()
	first := !s.panicked
	s.panicked = true
	s.mu.Unlock()
	if first {
		panic("IDPrefix panicked")
	}
	return s.rnsCountingStore.IDPrefix()
}

// TestCitySessionResolverPrefixesWithUnderscoreOrDot: a sessions store whose
// prefix holds '_' or '.' mints pool ids that are admitted, while an id of
// the same shape under another prefix stays refused.
func TestCitySessionResolverPrefixesWithUnderscoreOrDot(t *testing.T) {
	for _, prefix := range []string{"gc_", "my_city", "a.b"} {
		id := prefix + "-session-" + tokenB
		name := "crew.worker-" + id
		store := rnsNewCountingStore(rnsSessionBead(id, templateB, name))
		store.prefix = prefix
		ns, _ := newRigNamespace(rnsGuardedRig("beta"), newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store)))
		if !ns.admits(id) || !ns.admits(name) {
			t.Errorf("prefix %q: own pool id or name not admitted", prefix)
		}
		if got := ns.classify(rnsRow(id, nil)); got != rowOwned {
			t.Errorf("prefix %q: own pool claim classifies %s", prefix, got)
		}
		for _, foreign := range []string{"zz_-session-" + tokenA, "x.y-session-" + tokenA, "gc_-session-" + tokenA} {
			if ns.admits(foreign) || ns.admits("crew.worker-"+foreign) {
				t.Errorf("prefix %q: foreign id %q admitted", prefix, foreign)
			}
		}
	}
}

// TestNoSessionStoreSeesThroughTheHandle: the zero handle and a handle over
// a nil store are both no store, so the resolver never reads through one.
func TestNoSessionStoreSeesThroughTheHandle(t *testing.T) {
	if !noSessionStore(beads.SessionStore{}) {
		t.Error("the zero handle is a store")
	}
	if !noSessionStore(beadstest.Sessions((*beads.MemStore)(nil))) {
		t.Error("a handle over a nil *MemStore is a store")
	}
	if noSessionStore(beadstest.Sessions(beads.NewMemStore())) {
		t.Error("a handle over a MemStore is no store")
	}
}

// TestCitySessionResolverLearnsPrefixUnderPolicyLayer: the sessions handle a
// controller holds is over the bead-policy layer, which hides the store's
// IDPrefix. The resolver still learns the prefix, through the handle and the
// layer's declared resolve target, so a pool id under it is looked up.
func TestCitySessionResolverLearnsPrefixUnderPolicyLayer(t *testing.T) {
	store := rnsNewCountingStore(rnsSessionBead(poolIDB2, templateB, "crew.worker-"+poolIDB2))
	handle := beadstest.Sessions(wrapStoreWithBeadPolicies(store, &config.City{}))
	if _, exposed := beads.Store(handle).(explicitBeadIDStore); exposed {
		t.Fatal("fixture: the handle must hide IDPrefix")
	}
	if got := beads.IDPrefixFor(handle); got != "gcx" {
		t.Fatalf("beads.IDPrefixFor(handle over policy) = %q, want gcx", got)
	}
	if got := beads.IDPrefixFor(beads.SessionStore{}); got != "" {
		t.Fatalf("beads.IDPrefixFor(zero handle) = %q, want empty", got)
	}
	res := newControllerSessionResolver(cityPathB, nil, func() (beads.SessionStore, error) { return handle, nil })
	rec, ok, err := res.lookupOwnSession(poolIDB2)
	if err != nil || !ok || rec.ID != poolIDB2 {
		t.Fatalf("lookupOwnSession(%q) = %+v, %v, %v; want the record under the learned prefix", poolIDB2, rec, ok, err)
	}
}
