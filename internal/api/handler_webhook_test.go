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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
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
	// A fresh timestamp: the discord verifier now enforces a replay window (FIX 1),
	// and this delivery flows through the real handler on the wall clock.
	ts := strconv.FormatInt(time.Now().Unix(), 10)
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

// --- E8: dedup, rate-limit, and webhook.received/rejected events ---

// newWebhookHandler builds the receiver against a caller-controlled *Server so a
// test can inject the limiter/dedup clock. It mirrors newTestCityHandler but
// hands back the Server whose per-city dedup cache + rate limiter persist across
// requests (the supervisor caches one Server per city in production).
func newWebhookHandler(t *testing.T, state *fakeState) (http.Handler, *Server) {
	t.Helper()
	srv := newServer(state, false)
	return newTestCityHandlerWith(t, state, srv), srv
}

func githubHeaders(sig, delivery string) map[string]string {
	return map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   delivery,
	}
}

func webhookReceivedEvents(t *testing.T, state State) []WebhookReceivedPayload {
	t.Helper()
	fake, ok := state.EventProvider().(*events.Fake)
	if !ok {
		t.Fatalf("event provider is %T, want *events.Fake", state.EventProvider())
	}
	var out []WebhookReceivedPayload
	for _, e := range fake.Events {
		if e.Type != events.WebhookReceived {
			continue
		}
		var p WebhookReceivedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode webhook.received payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// webhookRejectedEvents returns every emitted webhook.rejected payload.
func webhookRejectedEvents(t *testing.T, state State) []WebhookRejectedPayload {
	t.Helper()
	fake, ok := state.EventProvider().(*events.Fake)
	if !ok {
		t.Fatalf("event provider is %T, want *events.Fake", state.EventProvider())
	}
	var out []WebhookRejectedPayload
	for _, e := range fake.Events {
		if e.Type != events.WebhookRejected {
			continue
		}
		var p WebhookRejectedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode webhook.rejected payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func lastWebhookRejected(t *testing.T, state State) WebhookRejectedPayload {
	t.Helper()
	fake, ok := state.EventProvider().(*events.Fake)
	if !ok {
		t.Fatalf("event provider is %T, want *events.Fake", state.EventProvider())
	}
	for i := len(fake.Events) - 1; i >= 0; i-- {
		if fake.Events[i].Type != events.WebhookRejected {
			continue
		}
		var p WebhookRejectedPayload
		if err := json.Unmarshal(fake.Events[i].Payload, &p); err != nil {
			t.Fatalf("decode webhook.rejected payload: %v", err)
		}
		return p
	}
	t.Fatalf("no webhook.rejected event recorded")
	return WebhookRejectedPayload{}
}

// (E8-a) A duplicate delivery (same DedupID) is 2xx but dispatches the order
// exactly once across both; the second delivery emits a deduped received event.
func TestWebhookDedupSuppressesDuplicateDispatch(t *testing.T) {
	t.Setenv("GC_WEBHOOK_GITHUB_SECRET", "top-secret-webhook-key-dedup")
	secret := []byte("top-secret-webhook-key-dedup")

	disp := firedDispatcher()
	state := newWebhookState(t, githubWebhook("public"), prReviewOrder(), disp)
	h := newTestCityHandler(t, state)

	sig := githubSignature(secret, []byte(prLabeledPayload))
	hdrs := githubHeaders(sig, "dup-1")

	first := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", hdrs)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want 202 (body %s)", first.Code, first.Body.String())
	}
	second := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", hdrs)
	if second.Code < 200 || second.Code >= 300 {
		t.Fatalf("duplicate delivery status = %d, want 2xx", second.Code)
	}
	if !strings.Contains(second.Body.String(), `"deduped":true`) {
		t.Errorf("duplicate body = %s, want deduped:true", second.Body.String())
	}
	if disp.count() != 1 {
		t.Fatalf("dispatch count across duplicate deliveries = %d, want exactly 1", disp.count())
	}

	recvs := webhookReceivedEvents(t, state)
	if len(recvs) != 2 {
		t.Fatalf("webhook.received events = %d, want 2 (dispatch + deduped)", len(recvs))
	}
	if recvs[0].Deduped || !recvs[0].Dispatched {
		t.Errorf("first received = %+v, want dispatched=true deduped=false", recvs[0])
	}
	if !recvs[1].Deduped || recvs[1].Dispatched {
		t.Errorf("second received = %+v, want deduped=true dispatched=false", recvs[1])
	}
}

// slackWebhook is a public slack-v0 webhook that fires pr-review on any event
// (Event "*"), used to exercise the dedup-key derivation for a coarse signed id.
func slackWebhook() config.Webhook {
	return config.Webhook{
		Name:        "slack",
		Publication: config.ServicePublicationConfig{Visibility: "public"},
		Verify:      config.WebhookVerify{Scheme: "slack-v0", SecretEnv: "GC_WEBHOOK_SLACK_SECRET"},
		Rules:       []config.WebhookRule{{Event: "*", Order: prReviewOrderName}},
	}
}

func slackHeaders(secret []byte, ts, body string) map[string]string {
	base := []byte("v0:" + ts + ":" + body)
	mac := hmac.New(sha256.New, secret)
	mac.Write(base)
	return map[string]string{
		"X-Slack-Signature":         "v0=" + hex.EncodeToString(mac.Sum(nil)),
		"X-Slack-Request-Timestamp": ts,
	}
}

// (FIX 3) An attacker-mutable delivery id must not be part of the dedup key: two
// deliveries of the SAME signed body with DIFFERENT X-GitHub-Delivery values
// dedup to ONE dispatch. The key is the body hash (signature-covered), so minting
// a fresh delivery id cannot replay the order from the public endpoint.
func TestWebhookDedupIgnoresDeliveryHeader(t *testing.T) {
	t.Setenv("GC_WEBHOOK_GITHUB_SECRET", "top-secret-webhook-key-fix3")
	secret := []byte("top-secret-webhook-key-fix3")

	disp := firedDispatcher()
	state := newWebhookState(t, githubWebhook("public"), prReviewOrder(), disp)
	h := newTestCityHandler(t, state)

	sig := githubSignature(secret, []byte(prLabeledPayload))
	first := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", githubHeaders(sig, "fresh-A"))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want 202 (body %s)", first.Code, first.Body.String())
	}
	// Same signed body, a FRESH delivery id (the attacker's replay handle).
	second := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", githubHeaders(sig, "fresh-B"))
	if second.Code < 200 || second.Code >= 300 {
		t.Fatalf("replayed body status = %d, want 2xx", second.Code)
	}
	if disp.count() != 1 {
		t.Fatalf("same body + fresh delivery id must dedup to ONE dispatch (key ignores the header), got %d", disp.count())
	}
}

// (FIX 4) Two DISTINCT slack deliveries in the same wall-clock second (same signed
// ts) must BOTH dispatch: the key is the per-delivery body hash, so distinct
// bodies never collide. The old ts-granular key silently dropped one with a 2xx.
func TestWebhookSlackDistinctBodiesSameTsBothDispatch(t *testing.T) {
	t.Setenv("GC_WEBHOOK_SLACK_SECRET", "slack-webhook-secret-abcdefgh")
	secret := []byte("slack-webhook-secret-abcdefgh")

	disp := firedDispatcher()
	state := newWebhookState(t, slackWebhook(), prReviewOrder(), disp)
	h := newTestCityHandler(t, state)

	ts := strconv.FormatInt(time.Now().Unix(), 10) // fresh: inside the replay window
	bodyA := `{"event":{"type":"message"},"n":1}`
	bodyB := `{"event":{"type":"message"},"n":2}`

	recA := postHook(t, h, state, "slack", bodyA, "203.0.113.7:443", slackHeaders(secret, ts, bodyA))
	if recA.Code != http.StatusAccepted {
		t.Fatalf("delivery A status = %d, want 202 (body %s)", recA.Code, recA.Body.String())
	}
	recB := postHook(t, h, state, "slack", bodyB, "203.0.113.7:443", slackHeaders(secret, ts, bodyB))
	if recB.Code != http.StatusAccepted {
		t.Fatalf("delivery B status = %d, want 202 (body %s)", recB.Code, recB.Body.String())
	}
	if disp.count() != 2 {
		t.Fatalf("two distinct bodies sharing a slack ts must both dispatch, got %d", disp.count())
	}
}

// (FIX 6) The built verifier is memoized per webhook so the jwt-jwks JWKS cache
// persists across deliveries (fetched once, not rebuilt+refetched per request).
// Two builds with an unchanged config fingerprint return the SAME verifier
// instance; a changed fingerprint rebuilds. (The complementary "JWKS fetched once
// across two Verify calls on one verifier" is asserted in
// webhookverify.TestJWTJWKS_ValidToken.)
func TestWebhookVerifierMemoizedPerHook(t *testing.T) {
	state := newFakeState(t)
	hook := config.Webhook{
		Name:   "idp",
		Verify: config.WebhookVerify{Scheme: "jwt-jwks"},
		Rules:  []config.WebhookRule{{Event: "*", Order: "o"}},
	}
	state.cfg.Webhooks = []config.Webhook{hook}
	state.cfg.WebhookPolicy.JWTPolicies = []config.WebhookJWTPolicy{{
		Name: "idp", Issuer: "https://idp.example", Audience: "supervisor",
		JWKSURL: "https://idp.example/.well-known/jwks.json",
	}}
	srv := newServer(state, false)
	cfg := state.Config()

	v1, _, err := srv.buildWebhookVerifier(cfg, hook)
	if err != nil {
		t.Fatalf("build 1: %v", err)
	}
	v2, _, err := srv.buildWebhookVerifier(cfg, hook)
	if err != nil {
		t.Fatalf("build 2: %v", err)
	}
	if v1 != v2 {
		t.Fatal("same-config deliveries must reuse ONE verifier instance so the JWKS cache persists")
	}

	// A config hot-reload that changes a fingerprinted field rebuilds the verifier.
	hook2 := hook
	hook2.Verify.SignatureHeader = "X-Other-Token"
	v3, _, err := srv.buildWebhookVerifier(cfg, hook2)
	if err != nil {
		t.Fatalf("build 3: %v", err)
	}
	if v1 == v3 {
		t.Fatal("a changed config fingerprint must rebuild the verifier")
	}
}

// (E8-b) Over the per-webhook rate limit → 429 with Retry-After and no dispatch.
// FIX 7: the over-limit path is deliberately NOT evented (it would be an
// un-throttled per-request write on a flood); the 429 + Retry-After is the signal.
func TestWebhookRateLimitReturns429(t *testing.T) {
	t.Setenv("GC_WEBHOOK_GITHUB_SECRET", "top-secret-webhook-key-rl")
	secret := []byte("top-secret-webhook-key-rl")

	disp := firedDispatcher()
	state := newWebhookState(t, githubWebhook("public"), prReviewOrder(), disp)
	// Operator ceiling: one delivery per minute, burst 1 → 2nd back-to-back is 429.
	state.cfg.WebhookPolicy.RateLimit = &config.WebhookRateLimitConfig{PerMinute: 1, Burst: 1}
	h, srv := newWebhookHandler(t, state)
	now := time.Now()
	srv.webhookLimiter.now = func() time.Time { return now } // freeze: no refill mid-test

	sig := githubSignature(secret, []byte(prLabeledPayload))

	first := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", githubHeaders(sig, "rl-1"))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want 202", first.Code)
	}
	second := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", githubHeaders(sig, "rl-2"))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit status = %d, want 429", second.Code)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Errorf("429 response missing Retry-After header")
	}
	if disp.count() != 1 {
		t.Fatalf("dispatch count = %d, want 1 (the 429 must not dispatch)", disp.count())
	}
	// FIX 7: the over-limit request emits NO webhook.rejected event.
	if rejs := webhookRejectedEvents(t, state); len(rejs) != 0 {
		t.Errorf("over-limit request must emit no webhook.rejected event, got %d: %+v", len(rejs), rejs)
	}
}

// (E8-c) A pack cannot raise its own rate limit above the operator ceiling: a
// pack-contributed webhook with a huge MaxPerMinute is still limited at the tiny
// operator ceiling and 429s on the second back-to-back delivery.
func TestWebhookPackCannotRaiseRateLimitCeiling(t *testing.T) {
	t.Setenv("GC_WEBHOOK_GITHUB_SECRET", "top-secret-webhook-key-clamp")
	secret := []byte("top-secret-webhook-key-clamp")

	hook := githubWebhook("public")
	hook.SourceDir = "packs/evil" // pack-contributed provenance
	hook.MaxPerMinute = 1_000_000 // pack tries to grant itself a huge limit
	disp := firedDispatcher()
	state := newWebhookState(t, hook, prReviewOrder(), disp)
	// Operator ceiling is tiny; the pack value must be clamped to it.
	state.cfg.WebhookPolicy.RateLimit = &config.WebhookRateLimitConfig{PerMinute: 1, Burst: 1}
	h, srv := newWebhookHandler(t, state)
	now := time.Now()
	srv.webhookLimiter.now = func() time.Time { return now }

	sig := githubSignature(secret, []byte(prLabeledPayload))

	if rec := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", githubHeaders(sig, "c-1")); rec.Code != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want 202", rec.Code)
	}
	rec := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", githubHeaders(sig, "c-2"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second delivery status = %d, want 429 — a pack MaxPerMinute must not raise the operator ceiling", rec.Code)
	}
	if disp.count() != 1 {
		t.Fatalf("dispatch count = %d, want 1", disp.count())
	}
}

// (E8-d) A successful delivery emits webhook.received with dispatched=true and
// the right fields, carrying no arg values.
func TestWebhookReceivedEventOnDispatch(t *testing.T) {
	t.Setenv("GC_WEBHOOK_GITHUB_SECRET", "top-secret-webhook-key-recv")
	secret := []byte("top-secret-webhook-key-recv")

	disp := firedDispatcher() // TrackingID "track-1"
	state := newWebhookState(t, githubWebhook("public"), prReviewOrder(), disp)
	h := newTestCityHandler(t, state)

	sig := githubSignature(secret, []byte(prLabeledPayload))
	rec := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", githubHeaders(sig, "recv-1"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}

	recvs := webhookReceivedEvents(t, state)
	if len(recvs) != 1 {
		t.Fatalf("webhook.received events = %d, want 1", len(recvs))
	}
	ev := recvs[0]
	if ev.Webhook != "github" {
		t.Errorf("Webhook = %q, want github", ev.Webhook)
	}
	if ev.Scheme != "github-hmac-sha256" {
		t.Errorf("Scheme = %q, want github-hmac-sha256", ev.Scheme)
	}
	if ev.EventType != "pull_request" {
		t.Errorf("EventType = %q, want pull_request", ev.EventType)
	}
	if ev.DedupID != "recv-1" {
		t.Errorf("DedupID = %q, want recv-1", ev.DedupID)
	}
	if !ev.Dispatched || !ev.Matched || ev.Deduped {
		t.Errorf("flags = {dispatched:%v matched:%v deduped:%v}, want {true,true,false}", ev.Dispatched, ev.Matched, ev.Deduped)
	}
	if ev.RuleIndex != 0 {
		t.Errorf("RuleIndex = %d, want 0", ev.RuleIndex)
	}
	if ev.Order != prReviewOrderName {
		t.Errorf("Order = %q, want %q", ev.Order, prReviewOrderName)
	}
	if ev.TrackingID != "track-1" {
		t.Errorf("TrackingID = %q, want track-1", ev.TrackingID)
	}
	if ev.BodySize != len(prLabeledPayload) {
		t.Errorf("BodySize = %d, want %d", ev.BodySize, len(prLabeledPayload))
	}
}

// (E8-e) webhook.rejected fires with the correct reason on a verify failure and
// on a perimeter denial.
func TestWebhookRejectedEventReasons(t *testing.T) {
	t.Run("verify failure", func(t *testing.T) {
		t.Setenv("GC_WEBHOOK_GITHUB_SECRET", "top-secret-webhook-key-vf")
		disp := firedDispatcher()
		state := newWebhookState(t, githubWebhook("public"), prReviewOrder(), disp)
		h := newTestCityHandler(t, state)

		rec := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", githubHeaders("sha256=deadbeef", "vf-1"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("verify-failure status = %d, want 401", rec.Code)
		}
		if disp.count() != 0 {
			t.Fatalf("verify failure must not dispatch, count = %d", disp.count())
		}
		rej := lastWebhookRejected(t, state)
		if rej.Reason != reasonVerifyFailed {
			t.Errorf("reason = %q, want %q", rej.Reason, reasonVerifyFailed)
		}
		if rej.Status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rej.Status)
		}
	})

	t.Run("perimeter denial", func(t *testing.T) {
		t.Setenv("GC_WEBHOOK_GITHUB_SECRET", "top-secret-webhook-key-pd")
		disp := firedDispatcher()
		// A private webhook denies an external (non-loopback) delivery at the perimeter.
		state := newWebhookState(t, githubWebhook("private"), prReviewOrder(), disp)
		h := newTestCityHandler(t, state)

		rec := postHook(t, h, state, "github", prLabeledPayload, "198.51.100.10:9000", map[string]string{"X-GitHub-Event": "pull_request"})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("perimeter-denied status = %d, want 404", rec.Code)
		}
		if disp.count() != 0 {
			t.Fatalf("perimeter denial must not dispatch, count = %d", disp.count())
		}
		rej := lastWebhookRejected(t, state)
		if rej.Reason != reasonPerimeterDenied {
			t.Errorf("reason = %q, want %q", rej.Reason, reasonPerimeterDenied)
		}
	})
}

// (E8-f) No secret, signature, or raw body ever appears in an emitted event.
func TestWebhookEventsNeverLeakSecrets(t *testing.T) {
	const secretStr = "top-secret-webhook-key-leak"
	t.Setenv("GC_WEBHOOK_GITHUB_SECRET", secretStr)
	secret := []byte(secretStr)

	disp := firedDispatcher()
	state := newWebhookState(t, githubWebhook("public"), prReviewOrder(), disp)
	h := newTestCityHandler(t, state)

	sig := githubSignature(secret, []byte(prLabeledPayload))
	// A successful dispatch (rich received event) …
	if rec := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", githubHeaders(sig, "leak-1")); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch status = %d, want 202", rec.Code)
	}
	// … and a verify failure (rejected event).
	if rec := postHook(t, h, state, "github", prLabeledPayload, "203.0.113.7:443", githubHeaders("sha256=deadbeef", "leak-2")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("verify-failure status = %d, want 401", rec.Code)
	}

	fake, ok := state.EventProvider().(*events.Fake)
	if !ok {
		t.Fatalf("event provider is %T, want *events.Fake", state.EventProvider())
	}
	var asserted int
	for _, e := range fake.Events {
		if e.Type != events.WebhookReceived && e.Type != events.WebhookRejected {
			continue
		}
		asserted++
		blob := string(e.Payload)
		if strings.Contains(blob, secretStr) {
			t.Errorf("%s event leaks the secret: %s", e.Type, blob)
		}
		if strings.Contains(blob, sig) {
			t.Errorf("%s event leaks the signature: %s", e.Type, blob)
		}
		if strings.Contains(blob, prLabeledPayload) {
			t.Errorf("%s event leaks the raw body: %s", e.Type, blob)
		}
		// The extracted arg value (payload body content) must not appear either.
		if strings.Contains(blob, "acme/widgets") {
			t.Errorf("%s event leaks payload content: %s", e.Type, blob)
		}
	}
	if asserted == 0 {
		t.Fatal("expected webhook events to assert against")
	}
}
