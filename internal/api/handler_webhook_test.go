package api

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orderdispatch"
	"github.com/gastownhall/gascity/internal/orders"
)

// recordingDispatcher is a fake orderdispatch.Dispatcher that records every
// dispatch and returns a canned result, so webhook receiver tests can assert
// whether — and with what vars — the sink fired an order without a live city.
type recordingDispatcher struct {
	mu     sync.Mutex
	calls  []orderdispatch.DispatchRequest
	result orderdispatch.DispatchResult
	err    error
}

func (d *recordingDispatcher) Dispatch(_ context.Context, req orderdispatch.DispatchRequest) (orderdispatch.DispatchResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, req)
	if d.err != nil {
		return orderdispatch.DispatchResult{}, d.err
	}
	res := d.result
	if res.ScopedName == "" {
		res.ScopedName = req.Order.ScopedName()
	}
	return res, nil
}

func (d *recordingDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

func (d *recordingDispatcher) last() orderdispatch.DispatchRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[len(d.calls)-1]
}

// firedDispatcher returns a dispatcher whose result reports a launched order.
func firedDispatcher() *recordingDispatcher {
	return &recordingDispatcher{result: orderdispatch.DispatchResult{Fired: true, TrackingID: "track-1"}}
}

func githubSignature(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

const prReviewOrderName = "pr-review"

// githubWebhook is a public github-hmac webhook that fires pr-review on a
// labeled pull_request, extracting repo + pr from the payload (E5 args).
func githubWebhook(visibility string) config.Webhook {
	return config.Webhook{
		Name:        "github",
		Publication: config.ServicePublicationConfig{Visibility: visibility},
		Verify: config.WebhookVerify{
			Scheme:      "github-hmac-sha256",
			SecretEnv:   "GC_WEBHOOK_GITHUB_SECRET",
			EventHeader: "X-GitHub-Event",
			DedupHeader: "X-GitHub-Delivery",
		},
		Rules: []config.WebhookRule{{
			Event: "pull_request",
			Match: map[string]string{"action": "labeled"},
			Order: prReviewOrderName,
			Args: map[string]string{
				"repo": "{{repository.full_name}}",
				"pr":   "{{pull_request.number}}",
			},
		}},
	}
}

func prReviewOrder() orders.Order {
	return orders.Order{Name: prReviewOrderName, Trigger: "webhook", Formula: "pr-review-formula"}
}

const prLabeledPayload = `{"action":"labeled","repository":{"full_name":"acme/widgets"},"pull_request":{"number":1347}}`

// newWebhookState builds a fakeState with a single webhook, order, and injected
// dispatcher for receiver tests.
func newWebhookState(t *testing.T, hook config.Webhook, order orders.Order, disp orderdispatch.Dispatcher) *fakeState {
	t.Helper()
	st := newFakeState(t)
	st.cfg.Webhooks = []config.Webhook{hook}
	st.autos = []orders.Order{order}
	st.webhookDispatcher = disp
	return st
}

func postHook(t *testing.T, h http.Handler, state State, name, body, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, cityURL(state, "/hook/"+name), strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// (a) A private webhook POSTed from a non-loopback RemoteAddr is refused with a
// 404 (never leaking that the hook exists); from loopback it proceeds through
// verify and dispatches.
func TestWebhookPrivateRejectsExternalAllowsLoopback(t *testing.T) {
	t.Setenv("GC_WEBHOOK_GITHUB_SECRET", "top-secret-webhook-key-01")
	secret := []byte("top-secret-webhook-key-01")

	disp := firedDispatcher()
	state := newWebhookState(t, githubWebhook("private"), prReviewOrder(), disp)
	h := newTestCityHandler(t, state)

	sig := githubSignature(secret, []byte(prLabeledPayload))
	hdrs := map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   "d-1",
	}

	// External (non-loopback) → 404, dispatcher never touched.
	extRec := postHook(t, h, state, "github", prLabeledPayload, "198.51.100.10:9000", hdrs)
	if extRec.Code != http.StatusNotFound {
		t.Fatalf("external private status = %d, want 404", extRec.Code)
	}
	if disp.count() != 0 {
		t.Fatalf("external private must not dispatch, got %d calls", disp.count())
	}

	// Loopback → proceeds to verify and dispatches (202).
	loRec := postHook(t, h, state, "github", prLabeledPayload, "127.0.0.1:9000", hdrs)
	if loRec.Code != http.StatusAccepted {
		t.Fatalf("loopback private status = %d, want 202", loRec.Code)
	}
	if disp.count() != 1 {
		t.Fatalf("loopback private dispatch count = %d, want 1", disp.count())
	}
}

// (b) A public webhook with a valid GitHub signature dispatches with the
// E5-extracted, R4-namespaced vars; a tampered body verifies to 401.
func TestWebhookPublicValidDispatchesTamperedRejected(t *testing.T) {
	t.Setenv("GC_WEBHOOK_GITHUB_SECRET", "top-secret-webhook-key-02")
	secret := []byte("top-secret-webhook-key-02")

	disp := firedDispatcher()
	state := newWebhookState(t, githubWebhook("public"), prReviewOrder(), disp)
	h := newTestCityHandler(t, state)

	sig := githubSignature(secret, []byte(prLabeledPayload))
	hdrs := map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   "d-2",
	}

	// Valid signature from the edge (non-loopback) → dispatch.
	rec := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", hdrs)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid public status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	if disp.count() != 1 {
		t.Fatalf("valid public dispatch count = %d, want 1", disp.count())
	}
	call := disp.last()
	if got := call.Vars["repo"]; got != "acme/widgets" {
		t.Errorf("dispatched Vars[repo] = %q, want acme/widgets", got)
	}
	if got := call.Vars["pr"]; got != "1347" {
		t.Errorf("dispatched Vars[pr] = %q, want 1347", got)
	}
	// R4: exec-env overlay is namespaced under GC_WEBHOOK_ARG_.
	if got := call.ExecEnv["GC_WEBHOOK_ARG_repo"]; got != "acme/widgets" {
		t.Errorf("dispatched ExecEnv[GC_WEBHOOK_ARG_repo] = %q, want acme/widgets", got)
	}
	if _, raw := call.ExecEnv["repo"]; raw {
		t.Errorf("exec env must not carry the raw (un-namespaced) arg key repo")
	}
	if call.Source != orderdispatch.SourceWebhook {
		t.Errorf("dispatch Source = %q, want %q", call.Source, orderdispatch.SourceWebhook)
	}

	// Tampered body (signature no longer matches) → 401, no new dispatch.
	tampered := strings.Replace(prLabeledPayload, "1347", "9999", 1)
	tamRec := postHook(t, h, state, "github", tampered, "203.0.113.7:443", hdrs)
	if tamRec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered public status = %d, want 401", tamRec.Code)
	}
	if disp.count() != 1 {
		t.Fatalf("tampered delivery must not dispatch, count = %d, want 1", disp.count())
	}
}

// (c) A read-only server refuses a public webhook dispatch (dispatch is a
// mutation) even with a valid signature.
func TestWebhookReadOnlyRefusesPublicDispatch(t *testing.T) {
	t.Setenv("GC_WEBHOOK_GITHUB_SECRET", "top-secret-webhook-key-03")
	secret := []byte("top-secret-webhook-key-03")

	disp := firedDispatcher()
	state := newWebhookState(t, githubWebhook("public"), prReviewOrder(), disp)
	h := newTestCityHandlerReadOnly(t, state)

	sig := githubSignature(secret, []byte(prLabeledPayload))
	rec := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "pull_request",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-only public status = %d, want 403", rec.Code)
	}
	if disp.count() != 0 {
		t.Fatalf("read-only must not dispatch, count = %d, want 0", disp.count())
	}
}

// (d) An unknown webhook name is a 404.
func TestWebhookUnknownName404(t *testing.T) {
	disp := firedDispatcher()
	state := newWebhookState(t, githubWebhook("public"), prReviewOrder(), disp)
	h := newTestCityHandler(t, state)

	rec := postHook(t, h, state, "does-not-exist", `{}`, "127.0.0.1:9000", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown name status = %d, want 404", rec.Code)
	}
	if disp.count() != 0 {
		t.Fatalf("unknown name must not dispatch, count = %d", disp.count())
	}
}

// (e) A Discord PING (interaction type 1) with a valid ed25519 signature is
// answered {"type":1} without dispatching.
func TestWebhookDiscordPingPongNoDispatch(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	t.Setenv("GC_WEBHOOK_DISCORD_PUBKEY", hex.EncodeToString(pub))

	hook := config.Webhook{
		Name:        "discord",
		Publication: config.ServicePublicationConfig{Visibility: "public"},
		Verify: config.WebhookVerify{
			Scheme:    "discord-ed25519",
			SecretEnv: "GC_WEBHOOK_DISCORD_PUBKEY",
		},
		Rules: []config.WebhookRule{{Event: "*", Order: prReviewOrderName}},
	}
	disp := firedDispatcher()
	state := newWebhookState(t, hook, prReviewOrder(), disp)
	h := newTestCityHandler(t, state)

	ping := []byte(`{"type":1}`)
	ts := "1700000000"
	msg := append([]byte(ts), ping...)
	sig := hex.EncodeToString(ed25519.Sign(priv, msg))

	rec := postHook(t, h, state, "discord", string(ping), "203.0.113.9:443", map[string]string{
		"X-Signature-Ed25519":   sig,
		"X-Signature-Timestamp": ts,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("discord ping status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var pong struct {
		Type int `json:"type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pong); err != nil {
		t.Fatalf("decode pong: %v", err)
	}
	if pong.Type != 1 {
		t.Fatalf("pong type = %d, want 1", pong.Type)
	}
	if disp.count() != 0 {
		t.Fatalf("discord PING must not dispatch, count = %d", disp.count())
	}
}

// (f) R1: a webhook whose secret_env is outside the operator GC_WEBHOOK_*
// namespace (or unset) is an operator fault → 503, not a 401.
func TestWebhookR1SecretFaultIs503(t *testing.T) {
	t.Run("wrong namespace", func(t *testing.T) {
		t.Setenv("MY_SECRET", "top-secret-webhook-key-04")
		hook := githubWebhook("public")
		hook.Verify.SecretEnv = "MY_SECRET" // not GC_WEBHOOK_*
		disp := firedDispatcher()
		state := newWebhookState(t, hook, prReviewOrder(), disp)
		h := newTestCityHandler(t, state)

		rec := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", map[string]string{
			"X-Hub-Signature-256": "sha256=deadbeef",
			"X-GitHub-Event":      "pull_request",
		})
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("out-of-namespace secret status = %d, want 503", rec.Code)
		}
		if disp.count() != 0 {
			t.Fatalf("must not dispatch on operator fault, count = %d", disp.count())
		}
	})

	t.Run("unset", func(t *testing.T) {
		hook := githubWebhook("public")
		hook.Verify.SecretEnv = "GC_WEBHOOK_DEFINITELY_UNSET"
		disp := firedDispatcher()
		state := newWebhookState(t, hook, prReviewOrder(), disp)
		h := newTestCityHandler(t, state)

		rec := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", map[string]string{
			"X-Hub-Signature-256": "sha256=deadbeef",
			"X-GitHub-Event":      "pull_request",
		})
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("unset secret status = %d, want 503", rec.Code)
		}
	})
}

// (g) A verified delivery that matches no rule is a 2xx no-op — never a 4xx —
// and never dispatches.
func TestWebhookNoMatchIsNoOp(t *testing.T) {
	t.Setenv("GC_WEBHOOK_GITHUB_SECRET", "top-secret-webhook-key-05")
	secret := []byte("top-secret-webhook-key-05")

	disp := firedDispatcher()
	state := newWebhookState(t, githubWebhook("public"), prReviewOrder(), disp)
	h := newTestCityHandler(t, state)

	// A valid signature but an event no rule matches (rule wants pull_request).
	sig := githubSignature(secret, []byte(prLabeledPayload))
	rec := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "issues",
	})
	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("no-match status = %d, want 2xx no-op", rec.Code)
	}
	if disp.count() != 0 {
		t.Fatalf("no-match must not dispatch, count = %d", disp.count())
	}
}

// (h) The typed /order/{name}/run route refuses under read-only (write-auth path)
// and refuses a non-webhook-trigger order.
func TestOrderRunTypedGuards(t *testing.T) {
	t.Run("read-only refuses", func(t *testing.T) {
		disp := firedDispatcher()
		state := newWebhookState(t, githubWebhook("public"), prReviewOrder(), disp)
		h := newTestCityHandlerReadOnly(t, state)

		req := newPostRequest(cityURL(state, "/order/"+prReviewOrderName+"/run"), strings.NewReader(`{"vars":{}}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("read-only order run status = %d, want 403", rec.Code)
		}
		if disp.count() != 0 {
			t.Fatalf("read-only order run must not dispatch, count = %d", disp.count())
		}
	})

	t.Run("non-webhook-trigger refused", func(t *testing.T) {
		disp := firedDispatcher()
		manual := orders.Order{Name: "manual-order", Trigger: "manual", Formula: "f"}
		state := newWebhookState(t, githubWebhook("public"), manual, disp)
		h := newTestCityHandler(t, state)

		req := newPostRequest(cityURL(state, "/order/manual-order/run"), strings.NewReader(`{"vars":{}}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("non-webhook-trigger run status = %d, want 422 (body %s)", rec.Code, rec.Body.String())
		}
		if disp.count() != 0 {
			t.Fatalf("non-webhook-trigger must not dispatch, count = %d", disp.count())
		}
	})

	t.Run("webhook-trigger dispatches", func(t *testing.T) {
		disp := firedDispatcher()
		state := newWebhookState(t, githubWebhook("public"), prReviewOrder(), disp)
		h := newTestCityHandler(t, state)

		req := newPostRequest(cityURL(state, "/order/"+prReviewOrderName+"/run"), strings.NewReader(`{"vars":{"repo":"acme/widgets"}}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("typed run status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
		}
		if disp.count() != 1 {
			t.Fatalf("typed run dispatch count = %d, want 1", disp.count())
		}
		if got := disp.last().Vars["repo"]; got != "acme/widgets" {
			t.Errorf("typed run Vars[repo] = %q, want acme/widgets", got)
		}
	})
}
