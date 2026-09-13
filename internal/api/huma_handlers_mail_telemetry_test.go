package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// installManualMeterReader swaps the global MeterProvider for one backed by a
// manual reader and re-arms the telemetry recorder's lazy instrument binding
// so Record* calls land in the reader. Cleanup restores the previous provider
// and re-arms the binding again so later tests do not write to a dead reader.
// Tests using this helper must NOT call t.Parallel() — the MeterProvider and
// instrument binding are process-global.
func installManualMeterReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	telemetry.ResetInstrumentsForTest()
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		telemetry.ResetInstrumentsForTest()
	})
	return reader
}

// collectMailOpCounts collects from the manual reader and returns the
// gc.mail.operations.total datapoints keyed by (operation, status). Other
// metrics (e.g. middleware's gc.http.*) are ignored.
func collectMailOpCounts(t *testing.T, reader *sdkmetric.ManualReader) map[[2]string]int64 {
	t.Helper()
	var out metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	counts := make(map[[2]string]int64)
	for _, sm := range out.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "gc.mail.operations.total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("gc.mail.operations.total data = %T, want Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				var op, status string
				for _, kv := range dp.Attributes.ToSlice() {
					switch kv.Key {
					case "operation":
						op = kv.Value.AsString()
					case "status":
						status = kv.Value.AsString()
					}
				}
				counts[[2]string{op, status}] += dp.Value
			}
		}
	}
	return counts
}

// doMailRequest drives one HTTP request through the full Huma stack and
// asserts the response status.
func doMailRequest(t *testing.T, h http.Handler, req *http.Request, wantStatus int) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s %s status = %d, want %d; body: %s",
			req.Method, req.URL.Path, rec.Code, wantStatus, rec.Body.String())
	}
	return rec
}

// newDeleteRequest creates a DELETE httptest request with the X-GC-Request
// CSRF header set, mirroring newPostRequest.
func newDeleteRequest(url string) *http.Request {
	req := httptest.NewRequest("DELETE", url, nil)
	req.Header.Set("X-GC-Request", "true")
	return req
}

// failingMutationMailProvider embeds a real provider (so Get works and
// findMailProviderForMessage resolves) but fails every mutation operation.
type failingMutationMailProvider struct {
	mail.Provider
	failErr error
}

func (p *failingMutationMailProvider) Send(string, string, string, string) (mail.Message, error) {
	return mail.Message{}, p.failErr
}

func (p *failingMutationMailProvider) MarkRead(string) error { return p.failErr }

func (p *failingMutationMailProvider) MarkUnread(string) error { return p.failErr }

func (p *failingMutationMailProvider) Archive(string) error { return p.failErr }

func (p *failingMutationMailProvider) Reply(string, string, string, string) (mail.Message, error) {
	return mail.Message{}, p.failErr
}

func (p *failingMutationMailProvider) Delete(string) error { return p.failErr }

// TestMailMutationHandlersRecordMailOpOnSuccess verifies that each of the six
// supervisor API mail mutation handlers records exactly one
// gc.mail.operations.total datapoint with status "ok" and the same operation
// labels the CLI uses, so CLI and API traffic aggregate under one series.
func TestMailMutationHandlersRecordMailOpOnSuccess(t *testing.T) {
	reader := installManualMeterReader(t)
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	rec := doMailRequest(t, h, newPostRequest(cityURL(state, "/mail"),
		bytes.NewBufferString(`{"from":"mayor","to":"worker","subject":"s","body":"b"}`)),
		http.StatusCreated)
	var sent mail.Message
	if err := json.NewDecoder(rec.Body).Decode(&sent); err != nil {
		t.Fatalf("decode sent message: %v", err)
	}

	doMailRequest(t, h, newPostRequest(cityURL(state, "/mail/")+sent.ID+"/read", nil), http.StatusOK)
	doMailRequest(t, h, newPostRequest(cityURL(state, "/mail/")+sent.ID+"/mark-unread", nil), http.StatusOK)

	rec = doMailRequest(t, h, newPostRequest(cityURL(state, "/mail/")+sent.ID+"/reply",
		bytes.NewBufferString(`{"from":"worker","subject":"re","body":"r"}`)),
		http.StatusCreated)
	var reply mail.Message
	if err := json.NewDecoder(rec.Body).Decode(&reply); err != nil {
		t.Fatalf("decode reply message: %v", err)
	}

	doMailRequest(t, h, newPostRequest(cityURL(state, "/mail/")+sent.ID+"/archive", nil), http.StatusOK)
	doMailRequest(t, h, newDeleteRequest(cityURL(state, "/mail/")+reply.ID), http.StatusOK)

	got := collectMailOpCounts(t, reader)
	want := map[[2]string]int64{
		{"send", "ok"}:        1,
		{"mark_read", "ok"}:   1,
		{"mark_unread", "ok"}: 1,
		{"reply", "ok"}:       1,
		{"archive", "ok"}:     1,
		{"delete", "ok"}:      1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mail op counts = %v, want %v", got, want)
	}
}

// TestMailMutationHandlersRecordMailOpOnError verifies that provider failures
// in each of the six mail mutation handlers record exactly one
// gc.mail.operations.total datapoint with status "error".
func TestMailMutationHandlersRecordMailOpOnError(t *testing.T) {
	reader := installManualMeterReader(t)
	state := newFakeState(t)

	// Seed via the real provider so Get (and thus provider resolution for the
	// per-message endpoints) succeeds; only the mutations fail.
	msg, err := state.cityMailProv.Send("mayor", "worker", "s", "b")
	if err != nil {
		t.Fatalf("seed Send: %v", err)
	}
	state.cityMailProv = &failingMutationMailProvider{
		Provider: state.cityMailProv,
		failErr:  errors.New("provider down"),
	}
	h := newTestCityHandler(t, state)

	doMailRequest(t, h, newPostRequest(cityURL(state, "/mail"),
		bytes.NewBufferString(`{"from":"mayor","to":"worker","subject":"s","body":"b"}`)),
		http.StatusInternalServerError)
	doMailRequest(t, h, newPostRequest(cityURL(state, "/mail/")+msg.ID+"/read", nil), http.StatusInternalServerError)
	doMailRequest(t, h, newPostRequest(cityURL(state, "/mail/")+msg.ID+"/mark-unread", nil), http.StatusInternalServerError)
	doMailRequest(t, h, newPostRequest(cityURL(state, "/mail/")+msg.ID+"/reply",
		bytes.NewBufferString(`{"from":"worker","subject":"re","body":"r"}`)),
		http.StatusInternalServerError)
	doMailRequest(t, h, newPostRequest(cityURL(state, "/mail/")+msg.ID+"/archive", nil), http.StatusInternalServerError)
	doMailRequest(t, h, newDeleteRequest(cityURL(state, "/mail/")+msg.ID), http.StatusInternalServerError)

	got := collectMailOpCounts(t, reader)
	want := map[[2]string]int64{
		{"send", "error"}:        1,
		{"mark_read", "error"}:   1,
		{"mark_unread", "error"}: 1,
		{"reply", "error"}:       1,
		{"archive", "error"}:     1,
		{"delete", "error"}:      1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mail op counts = %v, want %v", got, want)
	}
}

// TestMailSendIdempotentReplayRecordsNoMailOp pins that an Idempotency-Key
// replay of POST /mail serves the cached response without calling the
// provider and records no additional gc.mail.operations.total datapoint, so
// client retries cannot double-count sends.
func TestMailSendIdempotentReplayRecordsNoMailOp(t *testing.T) {
	reader := installManualMeterReader(t)
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	body := `{"from":"mayor","to":"worker","subject":"s","body":"b"}`
	for range 2 {
		req := newPostRequest(cityURL(state, "/mail"), bytes.NewBufferString(body))
		req.Header.Set("Idempotency-Key", "mail-telemetry-replay-1")
		doMailRequest(t, h, req, http.StatusCreated)
	}

	got := collectMailOpCounts(t, reader)
	want := map[[2]string]int64{
		{"send", "ok"}: 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mail op counts = %v, want %v", got, want)
	}
}

// TestMailArchiveDeleteIdempotentRepeatRecordsNoMailOp pins CLI-parity
// semantics: an idempotent repeat archive/delete (message already gone)
// returns 200 but records no mail-op datapoint, matching the CLI which skips
// telemetry for already-archived messages.
func TestMailArchiveDeleteIdempotentRepeatRecordsNoMailOp(t *testing.T) {
	reader := installManualMeterReader(t)
	state := newFakeState(t)

	msgA, err := state.cityMailProv.Send("mayor", "worker", "a", "body-a")
	if err != nil {
		t.Fatalf("seed Send A: %v", err)
	}
	msgB, err := state.cityMailProv.Send("mayor", "worker", "b", "body-b")
	if err != nil {
		t.Fatalf("seed Send B: %v", err)
	}
	h := newTestCityHandler(t, state)

	for range 2 {
		doMailRequest(t, h, newPostRequest(cityURL(state, "/mail/")+msgA.ID+"/archive", nil), http.StatusOK)
	}
	for range 2 {
		doMailRequest(t, h, newDeleteRequest(cityURL(state, "/mail/")+msgB.ID), http.StatusOK)
	}

	got := collectMailOpCounts(t, reader)
	want := map[[2]string]int64{
		{"archive", "ok"}: 1,
		{"delete", "ok"}:  1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mail op counts = %v, want %v", got, want)
	}
}
