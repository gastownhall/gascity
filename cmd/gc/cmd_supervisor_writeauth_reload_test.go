package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
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
	wantStatus := func(t *testing.T, got writeAuthKeyReload, want writeAuthKeyReloadStatus) {
		t.Helper()
		if got.Status != want {
			t.Fatalf("reload outcome = %+v, want status %q", got, want)
		}
	}

	t.Run("swaps keys from supervisor.toml", func(t *testing.T) {
		setup(t)
		sm := newMux(t, k1.configEntry())
		writeSupervisorConfig(t, "[supervisor]\nwrite_auth_verify_key = \""+k2.configEntry()+"\"\n")
		var stderr bytes.Buffer
		got := reloadSupervisorWriteAuthKeys(sm, &stderr)
		want := writeAuthKeyReload{Status: writeAuthKeysReloaded, Added: []string{"bob"}, Removed: []string{"alice"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("reload outcome = %+v, want %+v", got, want)
		}
		if !strings.Contains(stderr.String(), "write-auth keys reloaded: added [bob], removed [alice]") {
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

	t.Run("replacing a kid's key is reported", func(t *testing.T) {
		setup(t)
		sm := newMux(t, k1.configEntry())
		rotated := newWriteAuthTestKey(t, "alice")
		writeSupervisorConfig(t, "[supervisor]\nwrite_auth_verify_key = \""+rotated.configEntry()+"\"\n")
		var stderr bytes.Buffer
		got := reloadSupervisorWriteAuthKeys(sm, &stderr)
		if want := (writeAuthKeyReload{Status: writeAuthKeysReloaded, Replaced: []string{"alice"}}); !reflect.DeepEqual(got, want) {
			t.Fatalf("reload outcome = %+v, want %+v", got, want)
		}
		if !strings.Contains(stderr.String(), "write-auth keys reloaded: replaced [alice]") {
			t.Fatalf("stderr = %q, want replaced summary", stderr.String())
		}
		h := sm.Handler()
		if !grantRejected(t, h, k1, "jti-alice-old") {
			t.Fatal("grant from the replaced key accepted after reload")
		}
		if grantRejected(t, h, rotated, "jti-alice-new") {
			t.Fatal("grant from the new key rejected after reload")
		}
	})

	t.Run("unchanged keys report no change", func(t *testing.T) {
		setup(t)
		sm := newMux(t, k1.configEntry())
		writeSupervisorConfig(t, "[supervisor]\nwrite_auth_verify_key = \""+k1.configEntry()+"\"\n")
		var stderr bytes.Buffer
		wantStatus(t, reloadSupervisorWriteAuthKeys(sm, &stderr), writeAuthKeysReloaded)
		if !strings.Contains(stderr.String(), "write-auth keys reloaded: no change") {
			t.Fatalf("stderr = %q, want no-change summary", stderr.String())
		}
	})

	t.Run("malformed key keeps old keys", func(t *testing.T) {
		setup(t)
		sm := newMux(t, k1.configEntry())
		writeSupervisorConfig(t, "[supervisor]\nwrite_auth_verify_key = \"bob:not-base64!\"\n")
		var stderr bytes.Buffer
		wantStatus(t, reloadSupervisorWriteAuthKeys(sm, &stderr), writeAuthKeysRefused)
		if !strings.Contains(stderr.String(), "write-auth keys not reloaded: parsing write_auth_verify_key") {
			t.Fatalf("stderr = %q, want refusal notice", stderr.String())
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
		wantStatus(t, reloadSupervisorWriteAuthKeys(sm, &stderr), writeAuthKeysRefused)
		if !strings.Contains(stderr.String(), "write-auth keys not reloaded: reading "+supervisor.ConfigPath()) {
			t.Fatalf("stderr = %q, want refusal naming the config file", stderr.String())
		}
		if grantRejected(t, sm.Handler(), k1, "jti-alice") {
			t.Fatal("old key no longer accepted after an unreadable config")
		}
	})

	t.Run("env override blocks reload of a file key", func(t *testing.T) {
		setup(t)
		t.Setenv("GC_CITY_WRITE_PUBKEY", k1.configEntry())
		sm := newMux(t, "")
		writeSupervisorConfig(t, "[supervisor]\nwrite_auth_verify_key = \""+k2.configEntry()+"\"\n")
		var stderr bytes.Buffer
		wantStatus(t, reloadSupervisorWriteAuthKeys(sm, &stderr), writeAuthKeysRefused)
		if !strings.Contains(stderr.String(), "GC_CITY_WRITE_PUBKEY") {
			t.Fatalf("stderr = %q, want env-override notice", stderr.String())
		}
		if !grantRejected(t, sm.Handler(), k2, "jti-bob") {
			t.Fatal("config key took effect despite GC_CITY_WRITE_PUBKEY override")
		}
	})

	t.Run("env-managed keys with no file key are silent", func(t *testing.T) {
		setup(t)
		t.Setenv("GC_CITY_WRITE_PUBKEY", k1.configEntry())
		sm := newMux(t, "")
		writeSupervisorConfig(t, "[supervisor]\n")
		var stderr bytes.Buffer
		wantStatus(t, reloadSupervisorWriteAuthKeys(sm, &stderr), writeAuthKeysNotManaged)
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q, want silence when GC_CITY_WRITE_PUBKEY manages the keys", stderr.String())
		}
		if grantRejected(t, sm.Handler(), k1, "jti-alice") {
			t.Fatal("env-managed key no longer accepted after reload")
		}
	})

	t.Run("write-auth off and no key is silent", func(t *testing.T) {
		setup(t)
		sm := newMux(t, "")
		writeSupervisorConfig(t, "[supervisor]\n")
		var stderr bytes.Buffer
		wantStatus(t, reloadSupervisorWriteAuthKeys(sm, &stderr), writeAuthKeysNotManaged)
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q, want silence when write-auth is not configured", stderr.String())
		}
	})

	t.Run("write-auth off cannot be enabled by reload", func(t *testing.T) {
		setup(t)
		sm := newMux(t, "")
		writeSupervisorConfig(t, "[supervisor]\nwrite_auth_verify_key = \""+k2.configEntry()+"\"\n")
		var stderr bytes.Buffer
		wantStatus(t, reloadSupervisorWriteAuthKeys(sm, &stderr), writeAuthKeysRefused)
		if !strings.Contains(stderr.String(), "not enabled at startup") {
			t.Fatalf("stderr = %q, want cannot-enable notice", stderr.String())
		}
	})
}

// serveSupervisorConn runs handleSupervisorConn on one end of a pipe and
// returns the client end.
func serveSupervisorConn(t *testing.T, reconcileCh chan reconcileRequest) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close() //nolint:errcheck // test cleanup
		server.Close() //nolint:errcheck // test cleanup
	})
	go handleSupervisorConn(server, func(supervisorShutdownMode, shutdownTrigger) bool { return false }, reconcileCh, nil)
	return client
}

// serveOneReload stands in for runSupervisor's main loop for one reload
// request: it reports keys unless keys is nil and, when finish is set,
// completes the reconcile. It returns the request it served.
func serveOneReload(reconcileCh chan reconcileRequest, keys *writeAuthKeyReload, finish bool) <-chan reconcileRequest {
	served := make(chan reconcileRequest, 1)
	go func() {
		req := <-reconcileCh
		if keys != nil {
			req.reportWriteAuthKeys(*keys)
		}
		if finish && req.done != nil {
			close(req.done)
		}
		served <- req
	}()
	return served
}

// TestHandleSupervisorReloadReport runs each case in a synctest bubble, so the
// supervisor's real queue and wait timeouts elapse on the bubble's clock.
func TestHandleSupervisorReloadReport(t *testing.T) {
	refused := writeAuthKeyReload{Status: writeAuthKeysRefused, Reason: "parsing write_auth_verify_key: bad key"}

	t.Run("reports the key outcome once the reconcile finishes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			reconcileCh := make(chan reconcileRequest, 1)
			serveOneReload(reconcileCh, &refused, true)
			reply, err := exchangeSupervisorReloadReport(serveSupervisorConn(t, reconcileCh))
			if err != nil {
				t.Fatalf("exchangeSupervisorReloadReport: %v", err)
			}
			if want := (supervisorReloadReply{Outcome: supervisorReloadOK, WriteAuthKeys: &refused}); !reflect.DeepEqual(reply, want) {
				t.Fatalf("reply = %+v, want %+v", reply, want)
			}
		})
	})

	t.Run("a timed-out reconcile still carries the key outcome", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			reconcileCh := make(chan reconcileRequest, 1)
			reloaded := writeAuthKeyReload{Status: writeAuthKeysReloaded, Added: []string{"bob"}}
			serveOneReload(reconcileCh, &reloaded, false) // the reconcile never finishes
			start := time.Now()
			reply, err := exchangeSupervisorReloadReport(serveSupervisorConn(t, reconcileCh))
			if err != nil {
				t.Fatalf("exchangeSupervisorReloadReport: %v", err)
			}
			if want := (supervisorReloadReply{Outcome: supervisorReloadTimeout, WriteAuthKeys: &reloaded}); !reflect.DeepEqual(reply, want) {
				t.Fatalf("reply = %+v, want %+v", reply, want)
			}
			if waited := time.Since(start); waited != supervisorReloadWaitTimeout {
				t.Fatalf("replied after %s, want the %s reload wait timeout", waited, supervisorReloadWaitTimeout)
			}
		})
	})

	t.Run("a finished reconcile without a key outcome replies ok at once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			reconcileCh := make(chan reconcileRequest, 1)
			serveOneReload(reconcileCh, nil, true)
			start := time.Now()
			reply, err := exchangeSupervisorReloadReport(serveSupervisorConn(t, reconcileCh))
			if err != nil {
				t.Fatalf("exchangeSupervisorReloadReport: %v", err)
			}
			if want := (supervisorReloadReply{Outcome: supervisorReloadOK}); !reflect.DeepEqual(reply, want) {
				t.Fatalf("reply = %+v, want %+v", reply, want)
			}
			if waited := time.Since(start); waited != 0 {
				t.Fatalf("replied after %s, want no wait once the reconcile finished", waited)
			}
		})
	})

	t.Run("a full queue replies busy without a key outcome", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			reconcileCh := make(chan reconcileRequest) // no main loop drains it
			reply, err := exchangeSupervisorReloadReport(serveSupervisorConn(t, reconcileCh))
			if err != nil {
				t.Fatalf("exchangeSupervisorReloadReport: %v", err)
			}
			if want := (supervisorReloadReply{Outcome: supervisorReloadBusy}); !reflect.DeepEqual(reply, want) {
				t.Fatalf("reply = %+v, want %+v", reply, want)
			}
		})
	})

	t.Run("the bare reload verb still replies ok and asks for no report", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			reconcileCh := make(chan reconcileRequest, 1)
			served := serveOneReload(reconcileCh, &refused, true)
			client := serveSupervisorConn(t, reconcileCh)
			if _, err := io.WriteString(client, "reload\n"); err != nil {
				t.Fatalf("write reload: %v", err)
			}
			line, err := bufio.NewReader(client).ReadString('\n')
			if err != nil || line != "ok\n" {
				t.Fatalf("bare reload reply = %q, %v; want \"ok\\n\"", line, err)
			}
			if req := <-served; req.writeAuthKeys != nil {
				t.Fatal("bare reload request carries a write-auth report channel")
			}
		})
	})
}

func TestExchangeSupervisorReloadReport(t *testing.T) {
	// serve answers one command line on the server end of a pipe with reply;
	// an empty reply closes the connection without answering, as a supervisor
	// that predates the report verb does.
	serve := func(t *testing.T, reply string) (net.Conn, <-chan string) {
		t.Helper()
		client, server := net.Pipe()
		t.Cleanup(func() {
			client.Close() //nolint:errcheck // test cleanup
			server.Close() //nolint:errcheck // test cleanup
		})
		got := make(chan string, 1)
		go func() {
			defer server.Close() //nolint:errcheck // test cleanup
			line, _ := bufio.NewReader(server).ReadString('\n')
			got <- line
			if reply != "" {
				io.WriteString(server, reply) //nolint:errcheck // best-effort test reply
			}
		}()
		return client, got
	}

	t.Run("sends the report verb and decodes the reply", func(t *testing.T) {
		client, got := serve(t, `{"outcome":"ok","write_auth_keys":{"status":"reloaded","removed":["alice"]}}`+"\n")
		reply, err := exchangeSupervisorReloadReport(client)
		if err != nil {
			t.Fatalf("exchangeSupervisorReloadReport: %v", err)
		}
		if line := <-got; line != supervisorReloadReportCommand+"\n" {
			t.Fatalf("command = %q, want %q", line, supervisorReloadReportCommand+"\n")
		}
		want := supervisorReloadReply{Outcome: supervisorReloadOK, WriteAuthKeys: &writeAuthKeyReload{Status: writeAuthKeysReloaded, Removed: []string{"alice"}}}
		if !reflect.DeepEqual(reply, want) {
			t.Fatalf("reply = %+v, want %+v", reply, want)
		}
	})

	t.Run("a supervisor that predates the verb sends no report", func(t *testing.T) {
		client, _ := serve(t, "")
		if _, err := exchangeSupervisorReloadReport(client); !errors.Is(err, errNoSupervisorReloadReport) {
			t.Fatalf("err = %v, want errNoSupervisorReloadReport", err)
		}
	})

	t.Run("an undecodable reply is an error, not a fallback", func(t *testing.T) {
		client, _ := serve(t, "ok\n")
		_, err := exchangeSupervisorReloadReport(client)
		if err == nil || errors.Is(err, errNoSupervisorReloadReport) {
			t.Fatalf("err = %v, want a decode error", err)
		}
	})
}

func TestWriteSupervisorReloadReply(t *testing.T) {
	reloaded := &writeAuthKeyReload{Status: writeAuthKeysReloaded, Added: []string{"bob"}, Removed: []string{"alice"}}
	refused := &writeAuthKeyReload{Status: writeAuthKeysRefused, Reason: "parsing write_auth_verify_key: bad key"}
	const (
		triggered     = "Reconciliation triggered.\n"
		reloadedLine  = "write-auth keys reloaded: added [bob], removed [alice]\n"
		refusedStderr = "gc supervisor reload: write-auth keys not reloaded: parsing write_auth_verify_key: bad key\n"
		jsonResult    = `{"schema_version":"1","ok":true,"command":"supervisor reload","action":"reload","message":"Reconciliation triggered."}` + "\n"
	)
	cases := []struct {
		name       string
		reply      supervisorReloadReply
		jsonOut    bool
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "ok without a key report", reply: supervisorReloadReply{Outcome: supervisorReloadOK}, wantStdout: triggered},
		{name: "ok with keys not managed by the file", reply: supervisorReloadReply{Outcome: supervisorReloadOK, WriteAuthKeys: &writeAuthKeyReload{Status: writeAuthKeysNotManaged}}, wantStdout: triggered},
		{name: "ok with reloaded keys", reply: supervisorReloadReply{Outcome: supervisorReloadOK, WriteAuthKeys: reloaded}, wantStdout: triggered + reloadedLine},
		{name: "ok with refused keys fails", reply: supervisorReloadReply{Outcome: supervisorReloadOK, WriteAuthKeys: refused}, wantCode: 1, wantStdout: triggered, wantStderr: refusedStderr},
		{name: "json ok with reloaded keys", reply: supervisorReloadReply{Outcome: supervisorReloadOK, WriteAuthKeys: reloaded}, jsonOut: true, wantStdout: jsonResult, wantStderr: "gc supervisor reload: " + reloadedLine},
		{name: "json ok with refused keys fails without a result", reply: supervisorReloadReply{Outcome: supervisorReloadOK, WriteAuthKeys: refused}, jsonOut: true, wantCode: 1, wantStderr: refusedStderr},
		{name: "timeout after the key step reports keys", reply: supervisorReloadReply{Outcome: supervisorReloadTimeout, WriteAuthKeys: reloaded}, wantCode: 1, wantStdout: reloadedLine, wantStderr: "gc supervisor reload: reconcile did not finish before timeout\n"},
		{name: "busy", reply: supervisorReloadReply{Outcome: supervisorReloadBusy}, wantCode: 1, wantStderr: "gc supervisor reload: reconcile queue is busy; try again shortly\n"},
		{name: "unknown outcome", reply: supervisorReloadReply{Outcome: "later"}, wantCode: 1, wantStderr: "gc supervisor reload: unexpected supervisor reply outcome \"later\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := writeSupervisorReloadReply(&stdout, &stderr, tc.jsonOut, tc.reply); code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", code, tc.wantCode)
			}
			if stdout.String() != tc.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tc.wantStdout)
			}
			if stderr.String() != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}

// TestRunSupervisorReloadSwapsWriteAuthKeysWithoutStoppingCities drives a real
// runSupervisor against a temp GC_HOME: rewriting write_auth_verify_key and
// running `gc supervisor reload` (then SIGHUP) swaps the verifier keys served
// on the API without stopping or restarting the registered city, and the
// command reports the key changes and fails on a refused key list.
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

	// Enroll bob and revoke alice, then run `gc supervisor reload`: it reports
	// the key changes the supervisor made.
	writeSupervisorConfig(t, baseCfg+"write_auth_verify_key = \""+k2.configEntry()+"\"\n")
	var reloadOut, reloadErr bytes.Buffer
	if code := reloadSupervisorReport(&reloadOut, &reloadErr, false); code != 0 {
		t.Fatalf("reloadSupervisorReport = %d; stderr=%q supervisor stderr=%q", code, reloadErr.String(), stderr.String())
	}
	const enrolled = "write-auth keys reloaded: added [bob], removed [alice]"
	if !strings.Contains(reloadOut.String(), enrolled) {
		t.Fatalf("reload stdout = %q, want %q", reloadOut.String(), enrolled)
	}
	if !strings.Contains(stderr.String(), enrolled) {
		t.Fatalf("supervisor stderr = %q, want %q", stderr.String(), enrolled)
	}
	if rejected(k2, "jti-bob-2") {
		t.Fatal("enrolled key rejected after reload")
	}
	if !rejected(k1, "jti-alice-2") {
		t.Fatal("revoked key accepted after reload")
	}

	// SIGHUP runs the same key reload: enroll carol next to bob.
	k3 := newWriteAuthTestKey(t, "carol")
	writeSupervisorConfig(t, baseCfg+"write_auth_verify_key = \""+k2.configEntry()+","+k3.configEntry()+"\"\n")
	sigCh <- syscall.SIGHUP
	waitFor(&stderr, "write-auth keys reloaded: added [carol]")
	if rejected(k3, "jti-carol-1") {
		t.Fatal("key enrolled by SIGHUP rejected")
	}

	// A malformed edit keeps the current keys, and the command says so and
	// exits non-zero instead of reporting success.
	writeSupervisorConfig(t, baseCfg+"write_auth_verify_key = \"dave:not-base64!\"\n")
	reloadOut.Reset()
	reloadErr.Reset()
	if code := reloadSupervisorReport(&reloadOut, &reloadErr, false); code != 1 {
		t.Fatalf("reloadSupervisorReport after a malformed edit = %d, want 1; stdout=%q stderr=%q", code, reloadOut.String(), reloadErr.String())
	}
	if !strings.Contains(reloadErr.String(), "gc supervisor reload: write-auth keys not reloaded: parsing write_auth_verify_key") {
		t.Fatalf("reload stderr = %q, want the refusal", reloadErr.String())
	}
	if rejected(k2, "jti-bob-3") || rejected(k3, "jti-carol-2") {
		t.Fatal("current keys rejected after a refused reload")
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
