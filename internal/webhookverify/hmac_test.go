package webhookverify

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

var testSecret = []byte("super-secret-shared-hmac-key-0123456789")

func TestGitHubHMAC_Valid(t *testing.T) {
	body := []byte(`{"action":"labeled","number":7}`)
	sig := "sha256=" + hex.EncodeToString(hmacSHA256(testSecret, body))
	v, err := New("github-hmac-sha256", config.WebhookVerify{}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := v.Verify(context.Background(), VerifyRequest{
		Body:   body,
		Secret: testSecret,
		Header: hdr(githubSignatureHeader, sig, githubEventHeader, "pull_request", githubDeliveryHeader, "del-123"),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected OK, got reason %q", res.Reason)
	}
	if res.EventType != "pull_request" {
		t.Errorf("EventType = %q, want pull_request", res.EventType)
	}
	if res.DedupID != "del-123" {
		t.Errorf("DedupID = %q, want del-123", res.DedupID)
	}
}

func TestGitHubHMAC_TamperedBody(t *testing.T) {
	body := []byte(`{"action":"labeled"}`)
	sig := "sha256=" + hex.EncodeToString(hmacSHA256(testSecret, body))
	v, _ := New("github-hmac-sha256", config.WebhookVerify{}, Options{})
	res, err := v.Verify(context.Background(), VerifyRequest{
		Body:   []byte(`{"action":"closed"}`), // tampered after signing
		Secret: testSecret,
		Header: hdr(githubSignatureHeader, sig),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("tampered body must not verify")
	}
}

func TestGitHubHMAC_WrongSecret(t *testing.T) {
	body := []byte(`{"action":"labeled"}`)
	sig := "sha256=" + hex.EncodeToString(hmacSHA256([]byte("a-different-secret-key-abcdefabcdef"), body))
	v, _ := New("github-hmac-sha256", config.WebhookVerify{}, Options{})
	res, _ := v.Verify(context.Background(), VerifyRequest{Body: body, Secret: testSecret, Header: hdr(githubSignatureHeader, sig)})
	if res.OK {
		t.Fatal("signature from a different secret must not verify")
	}
}

func TestGitHubHMAC_MissingHeader(t *testing.T) {
	v, _ := New("github-hmac-sha256", config.WebhookVerify{}, Options{})
	res, err := v.Verify(context.Background(), VerifyRequest{Body: []byte(`{}`), Secret: testSecret, Header: hdr()})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("missing signature header must not verify")
	}
}

func TestGitHubHMAC_MissingSecretIsError(t *testing.T) {
	body := []byte(`{}`)
	sig := "sha256=" + hex.EncodeToString(hmacSHA256(testSecret, body))
	v, _ := New("github-hmac-sha256", config.WebhookVerify{}, Options{})
	_, err := v.Verify(context.Background(), VerifyRequest{Body: body, Header: hdr(githubSignatureHeader, sig)})
	if err == nil {
		t.Fatal("an unresolved secret must be an operational error, not a silent pass/fail")
	}
}

func TestGitHubHMAC_MalformedSignature(t *testing.T) {
	v, _ := New("github-hmac-sha256", config.WebhookVerify{}, Options{})
	for _, sig := range []string{"deadbeef", "sha256=nothex", "sha1=" + hex.EncodeToString(hmacSHA256(testSecret, []byte("{}")))} {
		res, err := v.Verify(context.Background(), VerifyRequest{Body: []byte("{}"), Secret: testSecret, Header: hdr(githubSignatureHeader, sig)})
		if err != nil {
			t.Fatalf("Verify(%q): %v", sig, err)
		}
		if res.OK {
			t.Errorf("malformed signature %q must not verify", sig)
		}
	}
}

func TestGenericHMAC_ConstructionRequiresHeader(t *testing.T) {
	if _, err := New("hmac-sha256", config.WebhookVerify{}, Options{}); err == nil {
		t.Fatal("hmac-sha256 without signature_header must fail construction")
	}
}

func TestGenericHMAC_ValidHexAndBase64(t *testing.T) {
	body := []byte(`{"event":"issue.created"}`)
	mac := hmacSHA256(testSecret, body)
	cfg := config.WebhookVerify{SignatureHeader: "X-Plane-Signature", EventHeader: "X-Plane-Event", DedupHeader: "X-Plane-Delivery"}
	v, err := New("hmac-sha256", cfg, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	encodings := map[string]string{
		"hex":       hex.EncodeToString(mac),
		"hexPrefix": "sha256=" + hex.EncodeToString(mac),
		"base64":    base64.StdEncoding.EncodeToString(mac),
	}
	for name, sig := range encodings {
		res, err := v.Verify(context.Background(), VerifyRequest{
			Body:   body,
			Secret: testSecret,
			Header: hdr("X-Plane-Signature", sig, "X-Plane-Event", "issue", "X-Plane-Delivery", "pd-9"),
		})
		if err != nil {
			t.Fatalf("Verify(%s): %v", name, err)
		}
		if !res.OK {
			t.Errorf("encoding %s: expected OK, reason %q", name, res.Reason)
		}
		if res.EventType != "issue" || res.DedupID != "pd-9" {
			t.Errorf("encoding %s: event/dedup = %q/%q", name, res.EventType, res.DedupID)
		}
	}
}

func TestGenericHMAC_TamperedAndWrongSecretAndMissing(t *testing.T) {
	body := []byte(`payload-bytes-here`)
	mac := hmacSHA256(testSecret, body)
	cfg := config.WebhookVerify{SignatureHeader: "X-Sig"}
	v, _ := New("hmac-sha256", cfg, Options{})

	// tampered body
	res, _ := v.Verify(context.Background(), VerifyRequest{Body: []byte("other"), Secret: testSecret, Header: hdr("X-Sig", hex.EncodeToString(mac))})
	if res.OK {
		t.Error("tampered body must not verify")
	}
	// wrong secret
	res, _ = v.Verify(context.Background(), VerifyRequest{Body: body, Secret: []byte("wrong-secret-wrong-secret-wrong!!"), Header: hdr("X-Sig", hex.EncodeToString(mac))})
	if res.OK {
		t.Error("wrong secret must not verify")
	}
	// missing header
	res, _ = v.Verify(context.Background(), VerifyRequest{Body: body, Secret: testSecret, Header: hdr()})
	if res.OK {
		t.Error("missing signature header must not verify")
	}
}
