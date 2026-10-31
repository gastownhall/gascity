package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// writeAuthRejectedDetail is the generic 403 detail the write-auth gate
// returns for any grant that fails verification (unknown kid included).
const writeAuthRejectedDetail = "write grant rejected"

// writeAuthProbePath is a city-scoped mutation path that no handler serves: the
// write-auth gate still covers it, so a probe exercises verification without
// mutating the city when the grant is accepted.
const writeAuthProbePath = "/v0/city/bright-lights/write-auth-reload-probe"

type writeAuthTestKey struct {
	kid  string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newWriteAuthTestKey(t *testing.T, kid string) writeAuthTestKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return writeAuthTestKey{kid: kid, pub: pub, priv: priv}
}

// configEntry renders the key as a write_auth_verify_key entry.
func (k writeAuthTestKey) configEntry() string {
	return k.kid + ":" + base64.StdEncoding.EncodeToString(k.pub)
}

// probeRequest builds a CSRF-carrying POST to writeAuthProbePath bearing a
// fresh grant signed by k.
func (k writeAuthTestKey) probeRequest(t *testing.T, baseURL, jti string) *http.Request {
	t.Helper()
	body := []byte(`{}`)
	now := time.Now()
	g := citywriteauth.Grant{
		Kid: k.kid, Aud: "gc-city-write.v2", City: "bright-lights",
		IAT: now.Unix(), Exp: now.Add(time.Minute).Unix(), JTI: jti,
		Req: citywriteauth.ReqDigest(http.MethodPost, writeAuthProbePath, "", body),
	}
	payload, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal grant: %v", err)
	}
	token := base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(k.priv, payload))
	req, err := http.NewRequest(http.MethodPost, baseURL+writeAuthProbePath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-GC-City-Write", token)
	req.Header.Set("X-GC-Request", "1")
	return req
}

// grantRejected serves a probe signed by k through h and reports whether the
// write-auth gate rejected the grant.
func grantRejected(t *testing.T, h http.Handler, k writeAuthTestKey, jti string) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, k.probeRequest(t, "http://127.0.0.1", jti))
	return rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), writeAuthRejectedDetail)
}

func writeSupervisorConfig(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(supervisor.ConfigPath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReloadSupervisorWriteAuthKeys(t *testing.T) {
	k1 := newWriteAuthTestKey(t, "alice")
	k2 := newWriteAuthTestKey(t, "bob")

	newMux := func(t *testing.T, configKey string) *api.SupervisorMux {
		t.Helper()
		sm := api.NewSupervisorMux(nil, nil, false, "test", "", time.Now()).WithAnyHostAllowed()
		if err := api.InstallWriteAuth(sm, configKey, false, api.WriteAuthBindContext{}); err != nil {
			t.Fatalf("InstallWriteAuth: %v", err)
		}
		return sm
	}
	setup := func(t *testing.T) {
		t.Helper()
		t.Setenv("GC_HOME", shortTempDir(t, "gc-home-"))
		t.Setenv("GC_CITY_WRITE_PUBKEY", "")
		t.Setenv("GC_CITY_WRITE_REQUIRED", "")
	}

	t.Run("swaps keys from supervisor.toml", func(t *testing.T) {
		setup(t)
		sm := newMux(t, k1.configEntry())
		writeSupervisorConfig(t, "[supervisor]\nwrite_auth_verify_key = \""+k2.configEntry()+"\"\n")
		var stderr bytes.Buffer
		reloadSupervisorWriteAuthKeys(sm, &stderr)
		if !strings.Contains(stderr.String(), "1 added [bob], 1 removed [alice]") {
			t.Fatalf("stderr = %q, want added/removed summary", stderr.String())
		}
		h := sm.Handler()
		if grantRejected(t, h, k2, "jti-bob") {
			t.Fatal("grant from added kid rejected after reload")
		}
		if !grantRejected(t, h, k1, "jti-alice") {
			t.Fatal("grant from removed kid accepted after reload")
		}
	})

	t.Run("malformed key keeps old keys", func(t *testing.T) {
		setup(t)
		sm := newMux(t, k1.configEntry())
		writeSupervisorConfig(t, "[supervisor]\nwrite_auth_verify_key = \"bob:not-base64!\"\n")
		var stderr bytes.Buffer
		reloadSupervisorWriteAuthKeys(sm, &stderr)
		if !strings.Contains(stderr.String(), "write-auth keys unchanged") {
			t.Fatalf("stderr = %q, want unchanged notice", stderr.String())
		}
		if grantRejected(t, sm.Handler(), k1, "jti-alice") {
			t.Fatal("old key no longer accepted after a malformed reload")
		}
	})

	t.Run("unreadable config keeps old keys", func(t *testing.T) {
		setup(t)
		sm := newMux(t, k1.configEntry())
		writeSupervisorConfig(t, "[supervisor\n")
		var stderr bytes.Buffer
		reloadSupervisorWriteAuthKeys(sm, &stderr)
		if !strings.Contains(stderr.String(), "write-auth keys unchanged") {
			t.Fatalf("stderr = %q, want unchanged notice", stderr.String())
		}
		if grantRejected(t, sm.Handler(), k1, "jti-alice") {
			t.Fatal("old key no longer accepted after an unreadable config")
		}
	})

	t.Run("env override blocks reload", func(t *testing.T) {
		setup(t)
		t.Setenv("GC_CITY_WRITE_PUBKEY", k1.configEntry())
		sm := newMux(t, "")
		writeSupervisorConfig(t, "[supervisor]\nwrite_auth_verify_key = \""+k2.configEntry()+"\"\n")
		var stderr bytes.Buffer
		reloadSupervisorWriteAuthKeys(sm, &stderr)
		if !strings.Contains(stderr.String(), "GC_CITY_WRITE_PUBKEY") {
			t.Fatalf("stderr = %q, want env-override notice", stderr.String())
		}
		if !grantRejected(t, sm.Handler(), k2, "jti-bob") {
			t.Fatal("config key took effect despite GC_CITY_WRITE_PUBKEY override")
		}
	})

	t.Run("write-auth off and no key is silent", func(t *testing.T) {
		setup(t)
		sm := newMux(t, "")
		writeSupervisorConfig(t, "[supervisor]\n")
		var stderr bytes.Buffer
		reloadSupervisorWriteAuthKeys(sm, &stderr)
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q, want silence when write-auth is not configured", stderr.String())
		}
	})

	t.Run("write-auth off cannot be enabled by reload", func(t *testing.T) {
		setup(t)
		sm := newMux(t, "")
		writeSupervisorConfig(t, "[supervisor]\nwrite_auth_verify_key = \""+k2.configEntry()+"\"\n")
		var stderr bytes.Buffer
		reloadSupervisorWriteAuthKeys(sm, &stderr)
		if !strings.Contains(stderr.String(), "not enabled at startup") {
			t.Fatalf("stderr = %q, want cannot-enable notice", stderr.String())
		}
	})
}

// TestRunSupervisorReloadSwapsWriteAuthKeysWithoutStoppingCities drives a real
// runSupervisor against a temp GC_HOME: rewriting write_auth_verify_key and
// sending the socket "reload" (then SIGHUP) swaps the verifier keys served on
// the API without stopping or restarting the registered city.
func TestRunSupervisorReloadSwapsWriteAuthKeysWithoutStoppingCities(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("HOME", filepath.Dir(gcHome))
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_CITY_WRITE_PUBKEY", "")
	t.Setenv("GC_CITY_WRITE_REQUIRED", "")

	k1 := newWriteAuthTestKey(t, "alice")
	k2 := newWriteAuthTestKey(t, "bob")
	port := freeLoopbackPort(t)
	baseCfg := "[supervisor]\nport = " + port + "\npatrol_interval = \"10m\"\n"
	writeSupervisorConfig(t, baseCfg+"write_auth_verify_key = \""+k1.configEntry()+"\"\n")

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	sigChReady := make(chan chan<- os.Signal, 1)
	oldSignalNotify := supervisorSignalNotify
	supervisorSignalNotify = func(c chan<- os.Signal, _ ...os.Signal) {
		sigChReady <- c
	}
	t.Cleanup(func() {
		supervisorSignalNotify = oldSignalNotify
	})

	var stdout, stderr lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- runSupervisor(&stdout, &stderr)
	}()
	var sigCh chan<- os.Signal
	select {
	case sigCh = <-sigChReady:
	case <-time.After(hangBudget):
		t.Fatalf("timed out waiting for supervisor signal hook; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	waitFor := func(buf *lockedBuffer, want string) {
		t.Helper()
		deadline := time.Now().Add(hangBudget)
		for time.Now().Before(deadline) && !strings.Contains(buf.String(), want) {
			time.Sleep(10 * time.Millisecond)
		}
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("timed out waiting for %q; stdout=%q stderr=%q", want, stdout.String(), stderr.String())
		}
	}
	waitFor(&stdout, "Launching city 'bright-lights'")

	baseURL := "http://127.0.0.1:" + port
	rejected := func(k writeAuthTestKey, jti string) bool {
		t.Helper()
		resp, err := http.DefaultClient.Do(k.probeRequest(t, baseURL, jti))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode == http.StatusForbidden && strings.Contains(string(body), writeAuthRejectedDetail)
	}
	if rejected(k1, "jti-alice-1") {
		t.Fatal("startup key rejected before reload")
	}
	if !rejected(k2, "jti-bob-1") {
		t.Fatal("unenrolled key accepted before reload")
	}

	// Enroll bob and revoke alice, then reload over the control socket.
	writeSupervisorConfig(t, baseCfg+"write_auth_verify_key = \""+k2.configEntry()+"\"\n")
	var reloadOut, reloadErr bytes.Buffer
	if code := reloadSupervisor(&reloadOut, &reloadErr); code != 0 {
		t.Fatalf("reloadSupervisor = %d; stderr=%q supervisor stderr=%q", code, reloadErr.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "1 added [bob], 1 removed [alice]") {
		t.Fatalf("supervisor stderr = %q, want key reload summary", stderr.String())
	}
	if rejected(k2, "jti-bob-2") {
		t.Fatal("enrolled key rejected after reload")
	}
	if !rejected(k1, "jti-alice-2") {
		t.Fatal("revoked key accepted after reload")
	}

	// A malformed edit followed by SIGHUP keeps the current keys.
	writeSupervisorConfig(t, baseCfg+"write_auth_verify_key = \"carol:not-base64!\"\n")
	sigCh <- syscall.SIGHUP
	waitFor(&stderr, "write-auth keys unchanged")
	if rejected(k2, "jti-bob-3") {
		t.Fatal("current key rejected after a malformed SIGHUP reload")
	}

	if got := stdout.String(); strings.Contains(got, "Stopping city") || strings.Count(got, "Launching city 'bright-lights'") != 1 {
		t.Fatalf("reload restarted or stopped the city; stdout=%q", got)
	}

	sigCh <- syscall.SIGTERM
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("runSupervisor code = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("runSupervisor did not exit after SIGTERM; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
