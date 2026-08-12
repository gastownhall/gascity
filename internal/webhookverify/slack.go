package webhookverify

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

const (
	slackSignatureHeader = "X-Slack-Signature"
	slackTimestampHeader = "X-Slack-Request-Timestamp"
	slackVersionPrefix   = "v0="
	slackDefaultWindow   = 5 * time.Minute
)

// slackV0 verifies Slack's v0 signing scheme: an HMAC-SHA256 over
// "v0:{timestamp}:{body}", compared to X-Slack-Signature ("v0=<hex>"), with a
// replay window on X-Slack-Request-Timestamp. The Slack event type lives in the
// JSON payload and is E5's concern; this layer surfaces the signed timestamp
// for dedup.
type slackV0 struct {
	signatureHeader string
	timestampHeader string
	window          time.Duration
	now             func() time.Time
}

func newSlackV0(cfg config.WebhookVerify, opts Options) (Verifier, error) {
	// A pack authors [webhook.verify], so replay_window is untrusted: clamp it
	// down to the operator ceiling (maxReplayWindow) so a pack cannot widen the
	// freshness window it benefits from weakening.
	window, err := resolveReplayWindow(cfg.ReplayWindow, slackDefaultWindow)
	if err != nil {
		return nil, err
	}
	return &slackV0{
		signatureHeader: headerOrDefault(cfg.SignatureHeader, slackSignatureHeader),
		timestampHeader: headerOrDefault(cfg.TimestampHeader, slackTimestampHeader),
		window:          window,
		now:             opts.Now,
	}, nil
}

func (v *slackV0) Scheme() string { return "slack-v0" }

func (v *slackV0) Verify(_ context.Context, req VerifyRequest) (VerifyResult, error) {
	if len(req.Secret) == 0 {
		return VerifyResult{}, errors.New("webhookverify: slack-v0 requires a resolved secret")
	}
	tsRaw := strings.TrimSpace(req.Header.Get(v.timestampHeader))
	if tsRaw == "" {
		return failf("missing %s header", v.timestampHeader), nil
	}
	tsSecs, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return failf("%s is not a unix timestamp", v.timestampHeader), nil
	}
	if !withinReplayWindow(effectiveNow(req, v.now), tsSecs, v.window) {
		return failf("%s %d is outside the %s replay window", v.timestampHeader, tsSecs, v.window), nil
	}

	sig := strings.TrimSpace(req.Header.Get(v.signatureHeader))
	if sig == "" {
		return failf("missing %s signature header", v.signatureHeader), nil
	}
	rest, ok := strings.CutPrefix(sig, slackVersionPrefix)
	if !ok {
		return failf("%s is not in v0=<hex> form", v.signatureHeader), nil
	}
	provided, err := hex.DecodeString(rest)
	if err != nil {
		return failf("%s hex is malformed", v.signatureHeader), nil
	}

	base := make([]byte, 0, len(slackVersionPrefix)+len(tsRaw)+1+len(req.Body))
	base = append(base, "v0:"...)
	base = append(base, tsRaw...)
	base = append(base, ':')
	base = append(base, req.Body...)
	expected := hmacSHA256(req.Secret, base)
	if subtle.ConstantTimeCompare(provided, expected) != 1 {
		return failf("%s does not match", v.signatureHeader), nil
	}
	return VerifyResult{OK: true, DedupID: tsRaw, EventType: slackEventType(req.Body)}, nil
}

// slackEventType derives the rule-facing event type from a verified Slack
// payload: the nested "event.type" for an Events API event_callback (so a rule
// can select `event = "message"`), falling back to the top-level "type" for
// envelopes that carry no nested event (e.g. "url_verification"). It returns ""
// when the body is not the expected JSON object; the matcher then only matches a
// "*" rule. Parsing failures are not signature failures — the delivery already
// verified — so this never affects OK.
func slackEventType(body []byte) string {
	var p struct {
		Type  string `json:"type"`
		Event struct {
			Type string `json:"type"`
		} `json:"event"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return ""
	}
	if p.Event.Type != "" {
		return p.Event.Type
	}
	return p.Type
}
