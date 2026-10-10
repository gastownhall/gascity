package beads

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bdhttp "github.com/steveyegge/beads/backend/http"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// tokenHelper writes a command credential source that prints the token held
// in tokenFile and counts its runs in a sibling file. It uses only sh
// builtins.
func tokenHelper(t *testing.T, dir, token string) (source, tokenFile string, runs func() int) {
	t.Helper()
	tokenFile = filepath.Join(dir, "token")
	countFile := filepath.Join(dir, "runs")
	writeToken(t, tokenFile, token)
	helper := filepath.Join(dir, "helper.sh")
	script := "#!/bin/sh\necho run >> " + countFile + "\nread -r tok < " + tokenFile + "\necho \"$tok\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return "command:" + helper, tokenFile, func() int {
		data, err := os.ReadFile(countFile)
		if err != nil {
			return 0
		}
		return strings.Count(string(data), "run")
	}
}

func writeToken(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeCredentialClock pins sourceCredentialNow and returns an advance func.
func fakeCredentialClock(t *testing.T) func(time.Duration) {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	previous := sourceCredentialNow
	sourceCredentialNow = func() time.Time { return now }
	t.Cleanup(func() { sourceCredentialNow = previous })
	return func(d time.Duration) { now = now.Add(d) }
}

func childToken(t *testing.T, scope string) string {
	t.Helper()
	env := map[string]string{}
	applied, err := NewRemoteCredentialSubprocessEnv(scope, scope).Apply(context.Background(), env)
	if err != nil || !applied {
		t.Fatalf("Apply(%s) = %v, %v", scope, applied, err)
	}
	return env[beadsHTTPTokenEnv]
}

// TestScopeCredentialRotationReachesTheNextBdChild: a bd subprocess runner
// does not keep the token it first resolved. A file source is re-read for
// every child; a command source within its cache interval, then re-run.
func TestScopeCredentialRotationReachesTheNextBdChild(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	poisonAmbientCredentialLadder(t)
	advance := fakeCredentialClock(t)
	fileCity := remoteHTTPScope(t, server)
	cmdCity := remoteHTTPScope(t, server)
	fileToken := filepath.Join(t.TempDir(), "file-token")
	writeToken(t, fileToken, "file-1")
	cmdSource, cmdToken, runs := tokenHelper(t, t.TempDir(), "cmd-1")
	installRemoteCredentialLookup(t, map[string]RemoteCredentialConfig{
		fileCity: {Source: mustCredentialSource(t, "file:"+fileToken), Scope: "[beads]", FromCity: true},
		cmdCity:  {Source: mustCredentialSource(t, cmdSource), Scope: "[beads]", FromCity: true},
	})

	if got := childToken(t, fileCity); got != "127.0.0.1:9=file-1" {
		t.Fatalf("file child token = %q, want file-1", got)
	}
	writeToken(t, fileToken, "file-2")
	if got := childToken(t, fileCity); got != "127.0.0.1:9=file-2" {
		t.Fatalf("file child token after rotation = %q, want the rotated file-2", got)
	}

	if got := childToken(t, cmdCity); got != "127.0.0.1:9=cmd-1" {
		t.Fatalf("command child token = %q, want cmd-1", got)
	}
	writeToken(t, cmdToken, "cmd-2")
	if got := childToken(t, cmdCity); got != "127.0.0.1:9=cmd-1" {
		t.Fatalf("command child token inside the cache interval = %q, want the cached cmd-1", got)
	}
	if n := runs(); n != 1 {
		t.Fatalf("token command ran %d times for children inside one interval, want 1", n)
	}
	advance(scopeCommandCredentialMaxAge + time.Second)
	if got := childToken(t, cmdCity); got != "127.0.0.1:9=cmd-2" {
		t.Fatalf("command child token after the cache interval = %q, want the rotated cmd-2", got)
	}
}

// TestScopeCredentialProviderIsSharedPerScope: the token command runs once
// for a scope's native opens and bd children together, and a 401 refresh by
// the native store reaches the scope's next bd child at once.
func TestScopeCredentialProviderIsSharedPerScope(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	rotating := &rotatingAuthServer{inner: server}
	routeRemoteOpensThrough(t, rotating)
	_ = fakeCredentialClock(t)
	city := remoteHTTPScope(t, server)
	source, tokenFile, runs := tokenHelper(t, t.TempDir(), "token-1")
	installRemoteCredentialLookup(t, map[string]RemoteCredentialConfig{
		city: {Source: mustCredentialSource(t, source), Scope: "[beads]", FromCity: true},
	})

	first := openRemoteForTest(t, city, city)
	_ = openRemoteForTest(t, city, city)
	if got := childToken(t, city); got != "127.0.0.1:9=token-1" {
		t.Fatalf("child token = %q, want token-1", got)
	}
	if n := runs(); n != 1 {
		t.Fatalf("token command ran %d times for two opens and a child, want 1 (cached per scope)", n)
	}

	writeToken(t, tokenFile, "token-2")
	rotating.mu.Lock()
	rotating.retired = "token-1"
	rotating.mu.Unlock()
	if _, err := first.Get("gr-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after rotation error = %v, want ErrNotFound via the refreshed token", err)
	}
	if got := childToken(t, city); got != "127.0.0.1:9=token-2" {
		t.Fatalf("child token after the store's 401 refresh = %q, want token-2", got)
	}
}

// TestHandshakeCacheKeepsCredentialsApartOnOneActivation covers the
// credential in the handshake cache key: a rig with its own credential that
// is served by the city's activation (same .beads, same server address and
// project) is verified with its own token, and so is a city whose credential
// configuration changed. Neither reuses a handshake another credential won.
func TestHandshakeCacheKeepsCredentialsApartOnOneActivation(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	recorder := &authRecordingTransport{inner: server}
	routeRemoteOpensThrough(t, recorder)
	poisonAmbientCredentialLadder(t)
	city := remoteHTTPScope(t, server)
	inheritingRig := t.TempDir() // no metadata: served by the city's activation
	t.Setenv("CITY_A_TOKEN", "token-a")
	t.Setenv("CITY_A_NEW_TOKEN", "token-a2")
	t.Setenv("RIG_TOKEN", "token-rig")
	lookup := map[string]RemoteCredentialConfig{
		city:          {Source: mustCredentialSource(t, "env:CITY_A_TOKEN"), Scope: "[beads]", FromCity: true},
		inheritingRig: {Source: mustCredentialSource(t, "env:RIG_TOKEN"), Scope: `rig "r"`},
	}
	installRemoteCredentialLookup(t, lookup)
	// The store's own open also reads the server context, so the handshake
	// dials are recorded at the handshake seam itself.
	var dialedWith []string
	previousDial := remoteHandshakeDial
	remoteHandshakeDial = func(ctx context.Context, beadsDir string, target bdhttp.Target, opts bdhttp.Options) (*bdhttp.ServerSnapshot, error) {
		probe, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.BaseURL.String(), nil)
		if err := opts.Credential.Authorize(ctx, probe); err != nil {
			return nil, err
		}
		dialedWith = append(dialedWith, probe.Header.Get("Authorization"))
		return previousDial(ctx, beadsDir, target, opts)
	}
	t.Cleanup(func() { remoteHandshakeDial = previousDial })
	expectHandshake := func(who, want string) {
		t.Helper()
		if len(dialedWith) == 0 || dialedWith[len(dialedWith)-1] != want {
			t.Errorf("%s: handshake dials %v, want a fresh handshake with %q (not another credential's cached one)", who, dialedWith, want)
		}
		dialedWith = nil
	}

	requests, auth := openAndReadAs(t, recorder, city, city)
	assertAllAuth(t, "city", requests, auth, "Bearer token-a")
	expectHandshake("city", "Bearer token-a")
	requests, auth = openAndReadAs(t, recorder, inheritingRig, city)
	assertAllAuth(t, "rig with its own credential on the city's activation", requests, auth, "Bearer token-rig")
	expectHandshake("rig with its own credential on the city's activation", "Bearer token-rig")

	lookup[city] = RemoteCredentialConfig{Source: mustCredentialSource(t, "env:CITY_A_NEW_TOKEN"), Scope: "[beads]", FromCity: true}
	requests, auth = openAndReadAs(t, recorder, city, city)
	assertAllAuth(t, "city after a credential change", requests, auth, "Bearer token-a2")
	expectHandshake("city after a credential change", "Bearer token-a2")

	// The same credential again is answered from the cache.
	_, _ = openAndReadAs(t, recorder, city, city)
	if len(dialedWith) != 0 {
		t.Errorf("handshake re-dialed %v for an unchanged credential, want the cached answer", dialedWith)
	}
}

// TestRemoteHandshakeRefusesAScopeOtherThanThePlannedOne: the plan's
// credential and plaintext grant were decided for its own activation; a
// handshake asked for another scope is refused before any request.
func TestRemoteHandshakeRefusesAScopeOtherThanThePlannedOne(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	planned := remoteHTTPScope(t, server)
	other := remoteHTTPScope(t, server)
	plan, err := remoteOpenPlanFor(context.Background(), "", planned, planned)
	if err != nil {
		t.Fatal(err)
	}
	_, err = remoteWireHandshakeWith(other, plan)
	var wireErr *contract.PreflightWireError
	if !errors.As(err, &wireErr) {
		t.Fatalf("handshake for another scope = %v, want a *PreflightWireError refusal", err)
	}
	if got := server.requests(); len(got) != 0 {
		t.Fatalf("requests %v sent for a scope the plan was not made for", got)
	}
}

// TestAmbientPlanHonoursBeadsHTTPCAFile: with no per-scope credential, the
// native open trusts the CA BEADS_HTTP_CA_FILE names for its target exactly
// as the bd child does (host-scoped, refused when it disagrees with the
// sidecar); a scope with a per-scope credential trusts the sidecar alone, and
// its bd child gets the variable blanked.
func TestAmbientPlanHonoursBeadsHTTPCAFile(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	scope := remoteHTTPScope(t, server)
	caDir := t.TempDir()
	envCA := filepath.Join(caDir, "env-ca.pem")
	sidecarCA := filepath.Join(caDir, "sidecar-ca.pem")
	for _, p := range []string{envCA, sidecarCA} {
		if err := os.WriteFile(p, []byte("pem"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plan := func(scope string) remoteOpenPlan {
		t.Helper()
		p, err := remoteOpenPlanFor(context.Background(), "", scope, scope)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	for _, value := range []string{"127.0.0.1=" + envCA, "127.0.0.1:9=" + envCA} {
		t.Setenv(beadsHTTPCAFileEnv, value)
		if p := plan(scope); p.targetErr != nil || p.target.CAFile != envCA {
			t.Errorf("%s: plan CAFile = %q (err %v), want %q", value, p.target.CAFile, p.targetErr, envCA)
		}
	}
	for _, value := range []string{"other.example=" + envCA, "127.0.0.1:10=" + envCA} {
		t.Setenv(beadsHTTPCAFileEnv, value)
		if p := plan(scope); p.targetErr != nil || p.target.CAFile != "" {
			t.Errorf("%s: plan CAFile = %q (err %v), want none (another target)", value, p.target.CAFile, p.targetErr)
		}
	}
	for _, value := range []string{"127.0.0.1", "127.0.0.1=relative.pem"} {
		t.Setenv(beadsHTTPCAFileEnv, value)
		if p := plan(scope); p.targetErr == nil {
			t.Errorf("%s: plan accepted a malformed %s", value, beadsHTTPCAFileEnv)
		}
	}

	withSidecar := remoteHTTPScope(t, server)
	target, err := bdhttp.LoadTarget(filepath.Join(withSidecar, ".beads"))
	if err != nil {
		t.Fatal(err)
	}
	target.CAFile = sidecarCA
	if err := bdhttp.SaveTarget(filepath.Join(withSidecar, ".beads"), target); err != nil {
		t.Fatal(err)
	}
	t.Setenv(beadsHTTPCAFileEnv, "127.0.0.1="+envCA)
	if p := plan(withSidecar); p.targetErr == nil {
		t.Errorf("plan accepted %s naming a different CA than the sidecar's", beadsHTTPCAFileEnv)
	}

	t.Setenv("CITY_TOKEN", "token-city")
	installRemoteCredentialLookup(t, map[string]RemoteCredentialConfig{
		scope: {Source: mustCredentialSource(t, "env:CITY_TOKEN"), Scope: "[beads]", FromCity: true},
	})
	t.Setenv(beadsHTTPCAFileEnv, "127.0.0.1="+envCA)
	if p := plan(scope); p.target.CAFile != "" {
		t.Errorf("scoped plan CAFile = %q, want the sidecar's alone (none)", p.target.CAFile)
	}
	env := map[string]string{}
	if _, err := NewRemoteCredentialSubprocessEnv(scope, scope).Apply(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if got, ok := env[beadsHTTPCAFileEnv]; !ok || got != "" {
		t.Errorf("scoped bd child %s = %q (set %v), want blanked", beadsHTTPCAFileEnv, got, ok)
	}
}

// TestEnvCredentialSourceLeavesTheProcessEnvironment: an env: source is moved
// out of the process environment, by the composition root's early call or by
// its first resolution, so no child process inherits it; the scope still
// resolves it and its own bd child still gets it, scoped to its server.
func TestEnvCredentialSourceLeavesTheProcessEnvironment(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	early := remoteHTTPScope(t, server)
	lazy := remoteHTTPScope(t, server)
	t.Setenv("EARLY_CITY_TOKEN", "token-early-value")
	t.Setenv("LAZY_CITY_TOKEN", "token-lazy-value")
	installRemoteCredentialLookup(t, map[string]RemoteCredentialConfig{
		early: {Source: mustCredentialSource(t, "env:EARLY_CITY_TOKEN"), Scope: "[beads]", FromCity: true},
		lazy:  {Source: mustCredentialSource(t, "env:LAZY_CITY_TOKEN"), Scope: "[beads]", FromCity: true},
	})

	SequesterRemoteCredentialEnv("EARLY_CITY_TOKEN")
	if _, ok := os.LookupEnv("EARLY_CITY_TOKEN"); ok {
		t.Fatal("EARLY_CITY_TOKEN still in the process environment after SequesterRemoteCredentialEnv")
	}
	if got := childToken(t, early); got != "127.0.0.1:9=token-early-value" {
		t.Fatalf("early scope child token = %q, want its own token", got)
	}
	if got := childToken(t, lazy); got != "127.0.0.1:9=token-lazy-value" {
		t.Fatalf("lazy scope child token = %q, want its own token", got)
	}
	if _, ok := os.LookupEnv("LAZY_CITY_TOKEN"); ok {
		t.Fatal("LAZY_CITY_TOKEN still in the process environment after its first resolution")
	}
	// What every child process gc spawns inherits.
	for _, entry := range os.Environ() {
		for _, leaked := range []string{"token-early-value", "token-lazy-value"} {
			if strings.Contains(entry, leaked) {
				t.Errorf("the environment children inherit still carries an env: credential")
			}
		}
	}
	// The scope keeps resolving it after it left the environment.
	if got := childToken(t, early); got != "127.0.0.1:9=token-early-value" {
		t.Fatalf("early scope child token after sequestration = %q, want its own token", got)
	}
}
