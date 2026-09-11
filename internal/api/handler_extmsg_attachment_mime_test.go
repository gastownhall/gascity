package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/extmsg"
)

// gp-sgu7 (citadel, 2026-09-08 → 09-11): a Slack voice memo reached the
// adapter with no mimetype, the adapter forwarded the attachment without
// mime_type, and this endpoint answered 422 "expected required property
// mime_type to be present" — the same answer for 34,000 retries, holding
// every later message in the channel behind it. An unknown file type must
// never be able to refuse a founder's text: mime_type is optional on an
// inbound attachment.

func inboundNormalizedBodyWithAttachment(t *testing.T, mimeType string) []byte {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(inboundNormalizedBody(t)), &body); err != nil {
		t.Fatalf("Unmarshal(fixture): %v", err)
	}
	att := map[string]any{
		"provider_id": "F0C0W3GM8KA",
		"url":         "file:///tmp/gc-slack-adapter/inbound/C0AP0KV9S9E/1788826440.901429-Audio Clip.m4a",
	}
	if mimeType != "" {
		att["mime_type"] = mimeType
	}
	body["message"].(map[string]any)["attachments"] = []any{att}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal(body): %v", err)
	}
	return raw
}

func postInboundStatus(t *testing.T, raw []byte) (int, string) {
	t.Helper()
	fs := newSessionFakeState(t)
	srv := New(fs)
	t.Cleanup(srv.waitForBackground)
	services := extmsg.NewServices(fs.cityBeadStore)
	fs.extmsgSvc = &services
	fs.adapterReg = extmsg.NewAdapterRegistry()

	req := newPostRequest(cityURL(fs, "/extmsg/inbound"), bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestHandleExtMsgInboundAttachmentWithoutMIMETypeIsNotRefused(t *testing.T) {
	withCode, _ := postInboundStatus(t, inboundNormalizedBodyWithAttachment(t, "audio/mp4"))
	withoutCode, withoutBody := postInboundStatus(t, inboundNormalizedBodyWithAttachment(t, ""))
	if withoutCode == http.StatusUnprocessableEntity || strings.Contains(withoutBody, "mime_type") {
		t.Fatalf("an attachment without mime_type must not be refused by the schema: status=%d body=%s", withoutCode, withoutBody)
	}
	if withoutCode != withCode {
		t.Fatalf("mime_type presence must not change the outcome: with=%d without=%d body=%s", withCode, withoutCode, withoutBody)
	}
}

func TestOpenAPIExternalAttachmentMIMETypeOptional(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json returned %d: %s", rec.Code, rec.Body.String())
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Required   []string                   `json:"required"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	att, ok := spec.Components.Schemas["ExternalAttachment"]
	if !ok {
		t.Fatal("ExternalAttachment schema missing from the spec")
	}
	if _, ok := att.Properties["mime_type"]; !ok {
		t.Fatal("mime_type must remain a declared property")
	}
	for _, r := range att.Required {
		if r == "mime_type" {
			t.Fatalf("mime_type must be optional; required = %v", att.Required)
		}
	}
	for _, want := range []string{"provider_id", "url"} {
		found := false
		for _, r := range att.Required {
			found = found || r == want
		}
		if !found {
			t.Fatalf("%s must stay required; required = %v", want, att.Required)
		}
	}
}
