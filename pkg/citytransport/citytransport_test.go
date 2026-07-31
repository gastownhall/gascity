package citytransport

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func sampleUpload() Upload {
	return Upload{
		SourceID:             "src_city_7f2a",
		Epoch:                3,
		SchemaVersion:        SchemaVersion,
		SourceContractDigest: "sha256:aaaa",
		ContentPolicyDigest:  "sha256:bbbb",
		Events: []Offer{{
			Seq:          41,
			Type:         "bead.created",
			RecordTS:     "2026-07-30T01:31:44Z",
			SemanticHash: "sha256:cccc",
			ActorHash:    "0123456789abcdef",
			RunRef:       "run-1",
		}, {
			Seq:          42,
			Type:         "mail.sent",
			RecordTS:     "2026-07-30T01:31:45Z",
			SemanticHash: "sha256:dddd",
		}},
	}
}

// CITY-SHELL-1 happy: contract mock events round-trip byte-stably.
func TestEncodeDecodeRoundTripIsByteStable(t *testing.T) {
	up := sampleUpload()
	first, err := EncodeUpload(up)
	if err != nil {
		t.Fatalf("EncodeUpload: %v", err)
	}
	second, err := EncodeUpload(up)
	if err != nil {
		t.Fatalf("EncodeUpload (repeat): %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("encoding is not deterministic:\n%s\n%s", first, second)
	}
	back, err := DecodeUpload(first)
	if err != nil {
		t.Fatalf("DecodeUpload: %v", err)
	}
	again, err := EncodeUpload(back)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if string(again) != string(first) {
		t.Fatalf("round-trip is not byte-stable:\nwant %s\ngot  %s", first, again)
	}
	if strings.Contains(string(first), "\n") {
		t.Fatalf("canonical bytes must not contain a newline: %q", first)
	}
}

// CITY-SHELL-1 edge: an extra public field fails rather than being dropped.
func TestDecodeRejectsUnknownField(t *testing.T) {
	for name, body := range map[string]string{
		"upload": `{"source_id":"s","epoch":1,"schema_version":1,"source_contract_digest":"d","content_policy_digest":"c","events":[],"city_id":"leaked"}`,
		"offer":  `{"source_id":"s","epoch":1,"schema_version":1,"source_contract_digest":"d","content_policy_digest":"c","events":[{"seq":1,"type":"t","record_ts":"x","semantic_hash":"h","payload":{"raw":true}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeUpload([]byte(body)); !errors.Is(err, ErrMalformedResponse) {
				t.Fatalf("want ErrMalformedResponse for an unknown field, got %v", err)
			}
		})
	}
}

func TestDecodeAckRejectsTrailingContent(t *testing.T) {
	if _, err := DecodeAck([]byte(`{"request_id":"r"}{"request_id":"r2"}`)); !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("want ErrMalformedResponse for trailing content, got %v", err)
	}
}

// An unrecognized outcome must never read as acknowledged, or a server typo
// would advance a producer checkpoint.
func TestAcknowledgedIsClosedAndFailsClosed(t *testing.T) {
	ackd := []Outcome{OutcomeAdmit, OutcomeAckReplay, OutcomeAckDuplicate}
	for _, o := range ackd {
		if !Acknowledged(o) {
			t.Fatalf("%s must be acknowledged", o)
		}
	}
	notAckd := []Outcome{
		OutcomeAckStale, OutcomeParkGap, OutcomeQuarantineConflict,
		OutcomeQuarantineIncomparable, OutcomeQuarantineTimeBound,
		OutcomeQuarantineGapExpired, OutcomeEpochResetApplied,
		OutcomeRejectInvalidReset, OutcomeContractDigestMismatch,
		Outcome("ADMITTED"), Outcome(""), Outcome("admit"),
	}
	for _, o := range notAckd {
		if Acknowledged(o) {
			t.Fatalf("%q must NOT be acknowledged", o)
		}
	}
}

func newTestClient(t *testing.T, m *MockDoer, tok func(context.Context) (string, error)) *Client {
	t.Helper()
	c, err := NewClient(Config{
		BaseURL:       "https://bts.example/api/v1",
		TokenProvider: tok,
		MaxAttempts:   3,
		HTTP:          m,
		Sleep:         NoSleep,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// CITY-SHELL-3 happy: one successful mock response decodes.
func TestSendDecodesSuccess(t *testing.T) {
	m := &MockDoer{Responses: []MockResponse{{
		Status: 200,
		Body:   `{"request_id":"req-1","policy_digest":"sha256:aaaa","epoch":3,"results":[{"seq":41,"outcome":"ADMIT","accepted_record_id":"rec-41"},{"seq":42,"outcome":"ACK_DUPLICATE"}]}`,
	}}}
	ack, err := newTestClient(t, m, StaticToken("t1")).Send(context.Background(), "cityhash", sampleUpload())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ack.RequestID != "req-1" || ack.Epoch != 3 || len(ack.Results) != 2 {
		t.Fatalf("unexpected ack: %+v", ack)
	}
	if !Acknowledged(ack.Results[0].Outcome) || !Acknowledged(ack.Results[1].Outcome) {
		t.Fatalf("both results should be acknowledged: %+v", ack.Results)
	}
	if got := string(m.Bodies[0]); !strings.Contains(got, `"source_contract_digest":"sha256:aaaa"`) ||
		!strings.Contains(got, `"content_policy_digest":"sha256:bbbb"`) {
		t.Fatalf("both signed digests must ride on the wire, got %s", got)
	}
}

// CITY-SHELL-3: retries are bounded and a 5xx storm ends in a typed failure.
func TestSendBoundedRetryThenTypedFailure(t *testing.T) {
	m := &MockDoer{Responses: []MockResponse{{Status: 503, Body: `{"title":"unavailable","status":503}`}}}
	_, err := newTestClient(t, m, StaticToken("t1")).Send(context.Background(), "cityhash", sampleUpload())
	if !errors.Is(err, ErrRetriesExhausted) {
		t.Fatalf("want ErrRetriesExhausted, got %v", err)
	}
	if m.Attempts() != 3 {
		t.Fatalf("MaxAttempts=3 must cap attempts, got %d", m.Attempts())
	}
	var p *Problem
	if !errors.As(err, &p) || p.Status != 503 {
		t.Fatalf("the last problem must be preserved in the chain, got %v", err)
	}
}

// A 4xx is a decision the server already made; retrying it is pointless.
func TestSendDoesNotRetryClientProblem(t *testing.T) {
	m := &MockDoer{Responses: []MockResponse{{
		Status: 409,
		Body:   `{"type":"https://bts/problems/contract-digest-mismatch","title":"contract digest mismatch","status":409,"expected_digest":"sha256:new","offered_digest":"sha256:aaaa","request_id":"req-9"}`,
	}}}
	_, err := newTestClient(t, m, StaticToken("t1")).Send(context.Background(), "cityhash", sampleUpload())
	var p *Problem
	if !errors.As(err, &p) {
		t.Fatalf("want a *Problem, got %v", err)
	}
	if p.ExpectedDigest != "sha256:new" || p.OfferedDigest != "sha256:aaaa" {
		t.Fatalf("digest-mismatch evidence must survive decoding: %+v", p)
	}
	if m.Attempts() != 1 {
		t.Fatalf("a 4xx must not be retried, got %d attempts", m.Attempts())
	}
}

// CITY-SHELL-3 edge: a malformed 2xx is NOT a success.
func TestSendRejectsMalformed2xx(t *testing.T) {
	m := &MockDoer{Responses: []MockResponse{{Status: 200, Body: `{"request_id":"r","surprise":1}`}}}
	_, err := newTestClient(t, m, StaticToken("t1")).Send(context.Background(), "cityhash", sampleUpload())
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("want ErrMalformedResponse, got %v", err)
	}
	if m.Attempts() != 1 {
		t.Fatalf("a protocol break must not be retried, got %d", m.Attempts())
	}
}

func TestSendCancellationIsTyped(t *testing.T) {
	started := make(chan struct{}, 1)
	m := &MockDoer{
		Responses: []MockResponse{{Status: 200, Body: `{"request_id":"r"}`, Delay: time.Hour}},
		Started:   started,
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel once the attempt is genuinely in flight. Sleeping here would only
	// guess at that, and would either flake on a loaded machine or slow the
	// suite down to buy confidence it cannot actually provide.
	go func() { <-started; cancel() }()
	_, err := newTestClient(t, m, StaticToken("t1")).Send(ctx, "cityhash", sampleUpload())
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("want ErrCanceled, got %v", err)
	}
}

// A token that is unavailable is retryable and never escalates past the
// credential class — rotation is not a source event.
func TestSendTokenErrorIsRetryableCredentialFailure(t *testing.T) {
	calls := 0
	tok := func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "", errors.New("token file mid-rotation")
		}
		return "t2", nil
	}
	m := &MockDoer{Responses: []MockResponse{{Status: 200, Body: `{"request_id":"r","results":[]}`}}}
	ack, err := newTestClient(t, m, tok).Send(context.Background(), "cityhash", sampleUpload())
	if err != nil {
		t.Fatalf("a rotation blip must recover by retry: %v", err)
	}
	if ack.RequestID != "r" {
		t.Fatalf("unexpected ack %+v", ack)
	}
}

func TestSendRotatedCredentialChangesNothingButTheHeader(t *testing.T) {
	m := &MockDoer{Responses: []MockResponse{
		{Status: 503, Body: `{"title":"unavailable","status":503}`},
		{Status: 200, Body: `{"request_id":"r","epoch":3,"results":[{"seq":41,"outcome":"ADMIT"}]}`},
	}}
	up := sampleUpload()
	ack, err := newTestClient(t, m, RotatingToken("old", "new")).Send(context.Background(), "cityhash", up)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ack.Epoch != up.Epoch {
		t.Fatalf("rotation must not change the epoch: sent %d, acked %d", up.Epoch, ack.Epoch)
	}
	if string(m.Bodies[0]) != string(m.Bodies[1]) {
		t.Fatalf("the retried body must be byte-identical across a rotation:\n%s\n%s", m.Bodies[0], m.Bodies[1])
	}
}

func TestNewClientRequiresEndpointAndCredentialSource(t *testing.T) {
	if _, err := NewClient(Config{TokenProvider: StaticToken("t")}); err == nil {
		t.Fatal("want an error without BaseURL")
	}
	if _, err := NewClient(Config{BaseURL: "https://x/api/v1"}); err == nil {
		t.Fatal("want an error without TokenProvider")
	}
}

func TestSendTargetsTheFrozenOperationPath(t *testing.T) {
	var got string
	m := &MockDoer{Responses: []MockResponse{{Status: 200, Body: `{"request_id":"r","results":[]}`}}}
	c, err := NewClient(Config{
		BaseURL:       "https://bts.example/api/v1/",
		TokenProvider: StaticToken("t"),
		HTTP: doerFunc(func(req *http.Request) (*http.Response, error) {
			got = req.URL.Path
			return m.Do(req)
		}),
		Sleep: NoSleep,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.Send(context.Background(), "c0ffee", sampleUpload()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// The frozen operation matrix row: POST /api/v1/city/{city}/events.
	if want := "/api/v1/city/c0ffee/events"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
