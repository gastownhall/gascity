package webhookverify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

const (
	githubSignatureHeader = "X-Hub-Signature-256"
	githubEventHeader     = "X-GitHub-Event"
	githubDeliveryHeader  = "X-GitHub-Delivery"
	sha256Prefix          = "sha256="
)

// hmacSHA256 computes the HMAC-SHA256 of msg under key.
func hmacSHA256(key, msg []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(msg)
	return mac.Sum(nil)
}

// githubHMAC verifies GitHub's X-Hub-Signature-256 scheme: a lowercase-hex
// HMAC-SHA256 of the raw body, prefixed "sha256=". Event and delivery ids come
// from fixed GitHub headers.
type githubHMAC struct {
	eventHeader     string
	dedupHeader     string
	signatureHeader string
}

func newGitHubHMAC(cfg config.WebhookVerify, _ Options) (Verifier, error) {
	return &githubHMAC{
		eventHeader:     headerOrDefault(cfg.EventHeader, githubEventHeader),
		dedupHeader:     headerOrDefault(cfg.DedupHeader, githubDeliveryHeader),
		signatureHeader: headerOrDefault(cfg.SignatureHeader, githubSignatureHeader),
	}, nil
}

func (v *githubHMAC) Scheme() string { return "github-hmac-sha256" }

func (v *githubHMAC) Verify(_ context.Context, req VerifyRequest) (VerifyResult, error) {
	if len(req.Secret) == 0 {
		return VerifyResult{}, errors.New("webhookverify: github-hmac-sha256 requires a resolved secret")
	}
	sig := strings.TrimSpace(req.Header.Get(v.signatureHeader))
	if sig == "" {
		return failf("missing %s signature header", v.signatureHeader), nil
	}
	rest, ok := strings.CutPrefix(sig, sha256Prefix)
	if !ok {
		return failf("%s is not in sha256=<hex> form", v.signatureHeader), nil
	}
	provided, err := hex.DecodeString(rest)
	if err != nil {
		return failf("%s hex is malformed", v.signatureHeader), nil
	}
	expected := hmacSHA256(req.Secret, req.Body)
	if subtle.ConstantTimeCompare(provided, expected) != 1 {
		return failf("%s does not match", v.signatureHeader), nil
	}
	return VerifyResult{
		OK:        true,
		EventType: strings.TrimSpace(req.Header.Get(v.eventHeader)),
		DedupID:   strings.TrimSpace(req.Header.Get(v.dedupHeader)),
	}, nil
}

// genericHMAC verifies a plain HMAC-SHA256 of the raw body against a
// configurable header. The signature encoding is auto-detected: an optional
// "sha256=" prefix is stripped, then hex and base64 (std/raw) are tried. This
// covers providers like Plane that sign the raw body but do not follow GitHub's
// exact header shape.
type genericHMAC struct {
	signatureHeader string
	eventHeader     string
	dedupHeader     string
}

func newGenericHMAC(cfg config.WebhookVerify, _ Options) (Verifier, error) {
	header := strings.TrimSpace(cfg.SignatureHeader)
	if header == "" {
		return nil, errors.New("webhookverify: hmac-sha256 requires verify.signature_header")
	}
	return &genericHMAC{
		signatureHeader: header,
		eventHeader:     strings.TrimSpace(cfg.EventHeader),
		dedupHeader:     strings.TrimSpace(cfg.DedupHeader),
	}, nil
}

func (v *genericHMAC) Scheme() string { return "hmac-sha256" }

func (v *genericHMAC) Verify(_ context.Context, req VerifyRequest) (VerifyResult, error) {
	if len(req.Secret) == 0 {
		return VerifyResult{}, errors.New("webhookverify: hmac-sha256 requires a resolved secret")
	}
	raw := strings.TrimSpace(req.Header.Get(v.signatureHeader))
	if raw == "" {
		return failf("missing %s signature header", v.signatureHeader), nil
	}
	provided, ok := decodeSignature(raw)
	if !ok {
		return failf("%s is not valid hex or base64", v.signatureHeader), nil
	}
	expected := hmacSHA256(req.Secret, req.Body)
	if subtle.ConstantTimeCompare(provided, expected) != 1 {
		return failf("%s does not match", v.signatureHeader), nil
	}
	res := VerifyResult{OK: true}
	if v.eventHeader != "" {
		res.EventType = strings.TrimSpace(req.Header.Get(v.eventHeader))
	}
	if v.dedupHeader != "" {
		res.DedupID = strings.TrimSpace(req.Header.Get(v.dedupHeader))
	}
	return res, nil
}

// decodeSignature decodes a signature that may be hex or base64, with an
// optional "sha256=" prefix. It returns the raw signature bytes and whether
// decoding succeeded.
func decodeSignature(s string) ([]byte, bool) {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, sha256Prefix); ok {
		s = rest
	}
	if b, err := hex.DecodeString(s); err == nil {
		return b, true
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, true
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, true
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, true
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, true
	}
	return nil, false
}

// headerOrDefault returns the trimmed override header name, or def when empty.
func headerOrDefault(override, def string) string {
	if h := strings.TrimSpace(override); h != "" {
		return h
	}
	return def
}
