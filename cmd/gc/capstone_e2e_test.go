package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/ssrf"
	"github.com/google/uuid"
)

// The capstone wire E2E (G23). It stands up a REAL hardened city — real
// controllerState + real SupervisorMux + real write-auth over an httptest TLS
// server — and drives the flagship one-liner (`rig add --git-url` then `sling`)
// through the remote CLI paths with an in-process ed25519 grant signer. It stubs
// exactly two boundaries: the git-fetch (rigCloneGit) so no network clone runs,
// and the SSRF DNS resolver (ssrf.HostResolver) so the fence runs for real
// against a fence-passing fake-public host. Everything else — admission locks,
// the request_id state machine, the durable idem record, the fence, rig.Provision
// (real beads init + config append), the G17 visibility barrier, typed events,
// the real SSE stream, the G18 grant editor, the writeAuthMiddleware, TLS, and
// the CLI rendering — is exercised end to end. No subprocess, tmux, Dolt, or
// network (per TESTING.md).

// Stable metadata keys for the durable idempotency record (mirrors the
// unexported internal/api constants; kept here as literals because the E2E
// asserts across the package boundary).
const (
	capstoneMetaIdemRequestID = "gc.idem.request_id"
	capstoneMetaIdemState     = "gc.idem.state"
	capstoneMetaIdemResultRig = "gc.idem.result.rig"
	capstoneIdemSucceeded     = "succeeded"
	capstoneIdemRolledBack    = "rolled_back"
	capstoneRoutedToKey       = "gc.routed_to"

	// capstonePublicHost resolves (via the stubbed resolver) to TEST-NET-2, which
	// IsInternalIP classifies public, so the strict SSRF fence passes for real;
	// the address is guaranteed-unrouted if a regression ever let a dial escape.
	capstonePublicHost  = "capstone.example.test"
	capstoneBlockedHost = "capstone-internal.example.test"
)

func capstonePublicGitURL() string  { return "https://" + capstonePublicHost + "/repo.git" }
func capstoneBlockedGitURL() string { return "https://" + capstoneBlockedHost + "/repo.git" }

// capstoneHarness is the shared per-test rig: a live hardened server, an
// in-process grant signer (with a mint counter that never fires on the SSE), and
// the two stubbed boundaries.
type capstoneHarness struct {
	t        *testing.T
	cs       *controllerState
	srv      *httptest.Server
	caPath   string
	cityName string
	cityPath string
	priv     ed25519.PrivateKey

	grantCount atomic.Int64 // # of X-GC-City-Write grants minted (mutations only)
	cloneCount atomic.Int64 // # of times the git-fetch boundary ran
	failClones atomic.Int64 // fail the first N clones (after materializing the dir)

	// gate, when set, blocks the clone until released — for the in-flight
	// concurrency scenario. cloneEntered signals the clone is parked on the gate.
	gate         atomic.Pointer[chan struct{}]
	cloneEntered chan struct{}
}

func newCapstoneHarness(t *testing.T) *capstoneHarness {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_HOME", t.TempDir())

	cityName := "capstone-city"
	cityPath := t.TempDir()
	// Explicit HQ prefix "hq" keeps the city store's own bead prefix distinct from
	// the rig prefix the sling scenario routes on. The single agent is declared the
	// schema-2 way (agents/<name>/agent.toml) so the post-provision config reload
	// (loadCityConfigWithBuiltinPacks) accepts it — an inline [[agent]] is PackV1
	// and is rejected on reload. It is city-scoped (cross-store eligible) and has no
	// sling_query, so the built-in store router handles routing in-process. ZERO
	// hardcoded roles: the name is arbitrary config.
	cityToml := "[workspace]\nname = \"capstone-city\"\nprefix = \"hq\"\n"
	writeSchema2RigCity(t, cityPath, cityName, cityToml, "")
	agentDir := filepath.Join(cityPath, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte("scope = \"city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}

	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), cityName, cityPath)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	apiMux := api.NewSupervisorMux(&singleCityStateResolver{state: cs}, nil, false, "controller", "test", time.Now())
	apiMux.WithAnyHostAllowed()
	// G10 boots the write-auth gate BECAUSE the key is present (non-loopback +
	// allow_mutations). A sibling assertion (TestCapstoneBootGateRefusesKeyless)
	// proves the keyless refusal.
	keyCfg := "k1:" + base64.StdEncoding.EncodeToString(pub)
	if err := api.InstallWriteAuth(apiMux, keyCfg, false, api.WriteAuthBindContext{NonLocal: true, AllowMutations: true}); err != nil {
		t.Fatalf("install write auth: %v", err)
	}

	srv := httptest.NewTLSServer(apiMux.Handler())
	t.Cleanup(srv.Close)

	h := &capstoneHarness{
		t:            t,
		cs:           cs,
		srv:          srv,
		caPath:       writeCapstoneServerCA(t, srv),
		cityName:     cityName,
		cityPath:     cityPath,
		priv:         priv,
		cloneEntered: make(chan struct{}, 1),
	}

	// Seam #1: stub the single git-fetch boundary. Every other provisioning step
	// stays real. Save/restore so no other test sees the stub.
	origClone := rigCloneGit
	t.Cleanup(func() { rigCloneGit = origClone })
	rigCloneGit = func(_ context.Context, _, dst string, _ git.CloneOptions) error {
		if gp := h.gate.Load(); gp != nil {
			select {
			case h.cloneEntered <- struct{}{}:
			default:
			}
			<-*gp
		}
		n := h.cloneCount.Add(1)
		// Materialize a plain working tree (dir + README): the rig-add contract
		// treats the git check as informational, so rig.Provision warn-and-continues
		// on a non-repo dir. Materialize BEFORE a simulated failure so the rollback
		// has a staged dir to remove (the realistic mid-clone failure).
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, "README.md"), []byte("capstone fixture\n"), 0o644); err != nil {
			return err
		}
		if n <= h.failClones.Load() {
			return fmt.Errorf("simulated clone failure #%d", n)
		}
		return nil
	}

	// Seam #2: stub the SSRF resolver so the strict fence runs for real against a
	// fence-passing fake-public host (and a fence-failing internal one).
	origResolver := ssrf.HostResolver
	t.Cleanup(func() { ssrf.HostResolver = origResolver })
	ssrf.HostResolver = func(host string) ([]net.IP, error) {
		switch host {
		case capstonePublicHost:
			return []net.IP{net.ParseIP("198.51.100.7")}, nil // TEST-NET-2 (public)
		case capstoneBlockedHost:
			return []net.IP{net.ParseIP("10.0.0.7")}, nil // RFC1918 (blocked)
		default:
			return nil, fmt.Errorf("capstone resolver: unexpected host %q", host)
		}
	}

	return h
}

// grantedClient builds a remote client whose mutations carry an in-process grant.
func (h *capstoneHarness) grantedClient() *api.Client {
	h.t.Helper()
	c, err := api.NewRemoteCityScopedClient(h.srv.URL, h.cityName, api.RemoteOptions{
		CAFile: h.caPath,
		Grant:  h.grantSource(),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

// grantSource is the in-process ed25519 signer standing in for gc-write-mint. It
// increments grantCount on every mint, so the test can prove a grant rode each
// mutation and none rode the SSE.
func (h *capstoneHarness) grantSource() api.GrantSource {
	return func(b api.GrantBinding) (string, error) {
		n := h.grantCount.Add(1)
		now := time.Now()
		g := citywriteauth.Grant{
			Kid:  "k1",
			Aud:  citywriteauth.AudienceCityWrite,
			City: h.cityName,
			IAT:  now.Unix(),
			Exp:  now.Add(time.Minute).Unix(),
			JTI:  fmt.Sprintf("cap-jti-%d", n),
			Req:  b.ReqDigest,
		}
		payload, err := json.Marshal(g)
		if err != nil {
			return "", err
		}
		sig := ed25519.Sign(h.priv, payload)
		return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
	}
}

func (h *capstoneHarness) target() *remoteTarget {
	return &remoteTarget{BaseURL: h.srv.URL, CityName: h.cityName, Source: remoteSourceURLFlag}
}

func (h *capstoneHarness) idemRecord(t *testing.T, requestID string) (*beads.Bead, bool) {
	t.Helper()
	matches, err := h.cs.CityBeadStore().List(beads.ListQuery{
		Metadata:      map[string]string{capstoneMetaIdemRequestID: requestID},
		IncludeClosed: true,
		Live:          true,
	})
	if err != nil {
		t.Fatalf("idem lookup: %v", err)
	}
	if len(matches) == 0 {
		return nil, false
	}
	return &matches[0], true
}

// assertEventPair scans the real event log for the progress + terminal-success
// frames carrying this request_id.
func (h *capstoneHarness) assertEventPair(t *testing.T, requestID string) {
	t.Helper()
	fake, ok := h.cs.EventProvider().(*events.Fake)
	if !ok {
		t.Fatalf("event provider is %T, want *events.Fake", h.cs.EventProvider())
	}
	all, err := fake.List(events.Filter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	sawProgress, sawTerminal := false, false
	for _, e := range all {
		var p struct {
			RequestID string `json:"request_id"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		if p.RequestID != requestID {
			continue
		}
		switch e.Type {
		case events.RigProvisionProgress:
			sawProgress = true
		case events.RequestResultRigCreate:
			sawTerminal = true
		}
	}
	if !sawProgress {
		t.Errorf("no rig.provision.progress event for request_id=%s", requestID)
	}
	if !sawTerminal {
		t.Errorf("no request.result.rig.create event for request_id=%s", requestID)
	}
}

func writeCapstoneServerCA(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	cert := srv.Certificate()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	p := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func capstoneRigInConfig(cs *controllerState, name string) bool {
	cfg := cs.Config()
	if cfg == nil {
		return false
	}
	for _, r := range cfg.Rigs {
		if r.Name == name {
			return true
		}
	}
	return false
}

// rigAdd/sling arg thunks keep the long positional call sites readable. Every
// scenario adds the same rig identity (name "web", prefix "gc" so the file
// store's gc-N bead IDs prefix-route to it, branch "main"); only the request_id
// and output buffers vary, so they are the only parameters.
func capstoneRigAdd(h *capstoneHarness, c *api.Client, reqID string, stdout, stderr *bytes.Buffer) int {
	return cmdRigAddRemote(c, h.target(), nil, capstonePublicGitURL(), reqID, "web", "gc", "main", nil, false, false, false, stdout, stderr)
}

func capstoneSling(c *api.Client, target, beadID string, stdout, stderr *bytes.Buffer) int {
	return cmdSlingRemote(c, []string{target, beadID}, false, false, false, "", nil, "", false, false, false, "", false, false, false, "", "", false, stdout, stderr)
}

// Scenario A + B — the capstone one-liner and the idempotent replay.
func TestCapstoneOneLinerAndIdempotentReplay(t *testing.T) {
	h := newCapstoneHarness(t)
	client := h.grantedClient()
	reqID := uuid.NewString()

	// --- Scenario A: rig add --git-url ---
	var addOut, addErr bytes.Buffer
	if code := capstoneRigAdd(h, client, reqID, &addOut, &addErr); code != 0 {
		t.Fatalf("rig add exit=%d\nstdout=%s\nstderr=%s", code, addOut.String(), addErr.String())
	}
	if !strings.Contains(addOut.String(), "Cloning rig working tree from git") {
		t.Errorf("missing clone progress line:\n%s", addOut.String())
	}
	if !strings.Contains(addOut.String(), "provisioned → web (prefix gc, branch main)") {
		t.Errorf("missing terminal provisioned line:\n%s", addOut.String())
	}
	if !strings.Contains(addErr.String(), "target: capstone-city @ ") {
		t.Errorf("missing target echo on stderr:\n%s", addErr.String())
	}

	// Server state: rig visible (G17), provisioned store exists and is writable.
	if !capstoneRigInConfig(h.cs, "web") {
		t.Fatalf("web rig absent from config after provision")
	}
	webStore := h.cs.BeadStore("web")
	if webStore == nil {
		t.Fatalf("BeadStore(web) is nil — the provisioned store is missing")
	}
	// Durable idem record reached succeeded (the G17 barrier's durable half).
	if rec, ok := h.idemRecord(t, reqID); !ok {
		t.Fatalf("no durable idem record for request_id=%s", reqID)
	} else if rec.Metadata[capstoneMetaIdemState] != capstoneIdemSucceeded {
		t.Fatalf("idem state=%q want succeeded", rec.Metadata[capstoneMetaIdemState])
	} else if rec.Metadata[capstoneMetaIdemResultRig] != "web" {
		t.Errorf("idem result rig=%q want web", rec.Metadata[capstoneMetaIdemResultRig])
	}
	h.assertEventPair(t, reqID)

	if got := h.cloneCount.Load(); got != 1 {
		t.Fatalf("cloneCount=%d after one add, want 1", got)
	}
	if got := h.grantCount.Load(); got != 1 {
		t.Fatalf("grantCount=%d after rig add, want 1 (the POST; the SSE mints none)", got)
	}

	// --- Scenario A (cont.): sling the new rig ---
	seeded, err := webStore.Create(beads.Bead{Title: "capstone work", Type: "task"})
	if err != nil {
		t.Fatalf("seed bead into provisioned store: %v", err)
	}
	var slOut, slErr bytes.Buffer
	if code := capstoneSling(client, "worker", seeded.ID, &slOut, &slErr); code != 0 {
		t.Fatalf("sling exit=%d\nstdout=%s\nstderr=%s", code, slOut.String(), slErr.String())
	}
	if !strings.Contains(slOut.String(), "→ worker") {
		t.Errorf("sling output missing route target:\n%s", slOut.String())
	}
	routed, err := webStore.Get(seeded.ID)
	if err != nil {
		t.Fatalf("re-read routed bead: %v", err)
	}
	if got := routed.Metadata[capstoneRoutedToKey]; got == "" {
		t.Fatalf("gc.routed_to not set on the slung bead; metadata=%v", routed.Metadata)
	} else if !strings.Contains(got, "worker") {
		t.Errorf("gc.routed_to=%q, want it to name the worker agent", got)
	}

	// The grant rode BOTH mutations and never the SSE: exactly 2 mints.
	if got := h.grantCount.Load(); got != 2 {
		t.Fatalf("grantCount=%d after add+sling, want exactly 2", got)
	}

	// A grant-less client against the same hardened city is refused, non-fallbackably.
	noGrant, err := api.NewRemoteCityScopedClient(h.srv.URL, h.cityName, api.RemoteOptions{CAFile: h.caPath})
	if err != nil {
		t.Fatal(err)
	}
	_, ngErr := noGrant.RigCreate(api.RigCreateRequest{Name: "web2", Prefix: "w2", DefaultBranch: "main", GitURL: capstonePublicGitURL(), RequestID: uuid.NewString()}, nil)
	if ngErr == nil {
		t.Fatal("grant-less mutation against a hardened city must fail")
	}
	if api.ShouldFallback(noGrant, ngErr) {
		t.Errorf("a remote write-auth rejection must be non-fallbackable (gate G1): %v", ngErr)
	}
	if !strings.Contains(ngErr.Error(), "grant") {
		t.Errorf("grant-less error should name the missing grant: %v", ngErr)
	}

	// --- Scenario B: idempotent replay (same request_id, same digest flags) ---
	var reOut, reErr bytes.Buffer
	if code := capstoneRigAdd(h, client, reqID, &reOut, &reErr); code != 0 {
		t.Fatalf("replay rig add exit=%d\nstdout=%s\nstderr=%s", code, reOut.String(), reErr.String())
	}
	if !strings.Contains(reOut.String(), "exists → web (idempotent replay)") {
		t.Errorf("replay must render the idempotent-exists line:\n%s", reOut.String())
	}
	if got := h.cloneCount.Load(); got != 1 {
		t.Fatalf("cloneCount=%d after replay, want 1 (no double-clone)", got)
	}
}

// Scenario D — rollback capstone: a failed clone rolls back to no-rig, then the
// same request_id retry re-clones cleanly.
func TestCapstoneRollbackThenSameRequestIDReclones(t *testing.T) {
	h := newCapstoneHarness(t)
	client := h.grantedClient()
	reqID := uuid.NewString()
	h.failClones.Store(1) // fail the first clone (after it materializes the dir)

	var out1, err1 bytes.Buffer
	if code := capstoneRigAdd(h, client, reqID, &out1, &err1); code != 1 {
		t.Fatalf("failed rig add exit=%d want 1\nstdout=%s\nstderr=%s", code, out1.String(), err1.String())
	}
	if !strings.Contains(err1.String(), "clone_failed") {
		t.Errorf("failure must classify clone_failed:\n%s", err1.String())
	}
	if !strings.Contains(err1.String(), "Retry the same request_id") {
		t.Errorf("failure must print the same-request_id re-clone recipe:\n%s", err1.String())
	}
	// Rolled back: no rig in config, staged dir gone, durable record rolled_back.
	if capstoneRigInConfig(h.cs, "web") {
		t.Errorf("web rig must NOT be in config after a rolled-back provision")
	}
	if _, statErr := os.Stat(filepath.Join(h.cityPath, "rigs", "web")); !os.IsNotExist(statErr) {
		t.Errorf("staged rig dir must be gone after rollback (stat err=%v)", statErr)
	}
	if rec, ok := h.idemRecord(t, reqID); !ok {
		t.Fatalf("no idem record after failure")
	} else if rec.Metadata[capstoneMetaIdemState] != capstoneIdemRolledBack {
		t.Fatalf("idem state=%q want rolled_back", rec.Metadata[capstoneMetaIdemState])
	}
	if got := h.cloneCount.Load(); got != 1 {
		t.Fatalf("cloneCount=%d after one failed add, want 1", got)
	}

	// Retry the exact same request_id: the rolled_back record purges, so it
	// re-clones cleanly and provisions.
	h.failClones.Store(0)
	var out2, err2 bytes.Buffer
	if code := capstoneRigAdd(h, client, reqID, &out2, &err2); code != 0 {
		t.Fatalf("retry rig add exit=%d want 0\nstdout=%s\nstderr=%s", code, out2.String(), err2.String())
	}
	if !strings.Contains(out2.String(), "provisioned → web") {
		t.Errorf("retry must provision:\n%s", out2.String())
	}
	if got := h.cloneCount.Load(); got != 2 {
		t.Fatalf("cloneCount=%d after retry, want 2 (the re-clone)", got)
	}
	if !capstoneRigInConfig(h.cs, "web") {
		t.Errorf("web rig must be in config after the successful retry")
	}
}

// Scenario E — fence negative: a git_url whose host resolves internal fails
// closed with blocked_host, before any clone or dir creation.
func TestCapstoneBlockedHostFailsClosed(t *testing.T) {
	h := newCapstoneHarness(t)
	client := h.grantedClient()
	reqID := uuid.NewString()

	_, err := client.RigCreate(api.RigCreateRequest{
		Name: "blocked", Prefix: "b", DefaultBranch: "main",
		GitURL: capstoneBlockedGitURL(), RequestID: reqID,
	}, nil)
	if err == nil {
		t.Fatal("a blocked-host clone must fail")
	}
	var failed *api.RigCreateFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("want RigCreateFailedError, got %T: %v", err, err)
	}
	if failed.Code != "blocked_host" {
		t.Errorf("failure code=%q want blocked_host", failed.Code)
	}
	if got := h.cloneCount.Load(); got != 0 {
		t.Errorf("cloneCount=%d, want 0 (the fence runs before the clone)", got)
	}
	if _, statErr := os.Stat(filepath.Join(h.cityPath, "rigs", "blocked")); !os.IsNotExist(statErr) {
		t.Errorf("no rig dir may exist for a fence-blocked add (stat err=%v)", statErr)
	}
	if capstoneRigInConfig(h.cs, "blocked") {
		t.Errorf("a fence-blocked rig must not appear in config")
	}
}

// Scenario C — in-flight replay + name conflict (Client level, gated clone).
func TestCapstoneInflightReplayAndNameConflict(t *testing.T) {
	h := newCapstoneHarness(t)
	client := h.grantedClient()
	idA := uuid.NewString()
	idB := uuid.NewString()

	gate := make(chan struct{})
	h.gate.Store(&gate)

	reqA := api.RigCreateRequest{Name: "web", Prefix: "gc", DefaultBranch: "main", GitURL: capstonePublicGitURL(), RequestID: idA}

	type outcome struct {
		res api.RigCreateResult
		err error
	}
	firstCh := make(chan outcome, 1)
	go func() { r, e := client.RigCreate(reqA, nil); firstCh <- outcome{r, e} }()

	// Wait until the first provision is parked on the gate inside the clone.
	select {
	case <-h.cloneEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("first provision never reached the gated clone")
	}

	// (i) A second POST with the SAME request_id + body replays the in-flight
	// provision (no second clone). It also blocks on the SSE for the terminal, so
	// run it concurrently and release the gate below.
	secondCh := make(chan outcome, 1)
	go func() { r, e := client.RigCreate(reqA, nil); secondCh <- outcome{r, e} }()

	// (ii) A POST with a DIFFERENT request_id for the same rig name is a
	// structured 409 rig_name_conflict carrying the in-flight request_id — no SSE
	// wait, so this returns immediately.
	_, confErr := client.RigCreate(api.RigCreateRequest{Name: "web", Prefix: "gc", DefaultBranch: "main", GitURL: capstonePublicGitURL(), RequestID: idB}, nil)
	var conflict *api.RigCreateConflictError
	if !errors.As(confErr, &conflict) {
		t.Fatalf("want RigCreateConflictError, got %T: %v", confErr, confErr)
	}
	if conflict.Code != "rig_name_conflict" {
		t.Errorf("conflict code=%q want rig_name_conflict", conflict.Code)
	}
	if conflict.InFlightRequestID != idA {
		t.Errorf("conflict in-flight id=%q want %q", conflict.InFlightRequestID, idA)
	}

	// Release the held provision; both same-request_id calls now complete.
	close(gate)
	h.gate.Store(nil)

	first := <-firstCh
	if first.err != nil {
		t.Fatalf("held provision must complete: %v", first.err)
	}
	if first.res.Status != "provisioned" {
		t.Errorf("first status=%q want provisioned", first.res.Status)
	}
	second := <-secondCh
	if second.err != nil {
		t.Fatalf("in-flight replay must complete: %v", second.err)
	}
	// The replay must not have driven a second clone.
	if got := h.cloneCount.Load(); got != 1 {
		t.Fatalf("cloneCount=%d, want 1 (the in-flight replay must not double-clone)", got)
	}
}

// TestCapstoneBootGateRefusesKeyless proves the G10 boot gate sibling: the same
// hardened bind (non-loopback + allow_mutations) with NO verify key and no ack
// refuses to install write-auth, so the wire E2E's key-present success is
// meaningful.
func TestCapstoneBootGateRefusesKeyless(t *testing.T) {
	mux := api.NewSupervisorMux(nil, nil, false, "controller", "test", time.Now())
	err := api.InstallWriteAuth(mux, "", false, api.WriteAuthBindContext{NonLocal: true, AllowMutations: true})
	if err == nil {
		t.Fatal("a keyless non-loopback allow_mutations bind must refuse to boot (G10)")
	}
	if !strings.Contains(err.Error(), "unauthenticated write plane") {
		t.Errorf("refusal should name the unauthenticated write plane: %v", err)
	}
	// The ack knob lets it boot (behind a trusted network front).
	if err := api.InstallWriteAuth(mux, "", false, api.WriteAuthBindContext{NonLocal: true, AllowMutations: true, AllowUnverified: true}); err != nil {
		t.Errorf("the ack knob must permit a keyless boot: %v", err)
	}
}
