package webhookverify

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

const (
	discordSignatureHeader = "X-Signature-Ed25519"
	discordTimestampHeader = "X-Signature-Timestamp"
	// discordDefaultWindow mirrors slack's default replay window (5 min): Discord
	// signs "{timestamp}{body}", so the timestamp is signature-covered and can
	// gate replays exactly as slack's does.
	discordDefaultWindow = 5 * time.Minute
)

// discordEd25519 verifies Discord's interactions signature: an Ed25519
// signature over "{timestamp}{rawbody}" against the application's public key.
// The "secret material" here is a public key — still operator-provided — carried
// in VerifyRequest.Secret as hex (Discord's portal form) or raw 32 bytes.
//
// Discord PING (interaction type 1) handling and the PONG response are the
// receiver's concern (E3); this layer only authenticates the request. When a
// dedup header is configured it is surfaced so the receiver can dedup; Discord
// carries no native delivery id.
type discordEd25519 struct {
	signatureHeader string
	timestampHeader string
	dedupHeader     string
	window          time.Duration
	now             func() time.Time
}

func newDiscordEd25519(cfg config.WebhookVerify, opts Options) (Verifier, error) {
	// replay_window is pack-authored, so clamp it down to the operator ceiling
	// (maxReplayWindow) — a pack must not be able to widen the freshness window.
	window, err := resolveReplayWindow(cfg.ReplayWindow, discordDefaultWindow)
	if err != nil {
		return nil, err
	}
	return &discordEd25519{
		signatureHeader: headerOrDefault(cfg.SignatureHeader, discordSignatureHeader),
		timestampHeader: headerOrDefault(cfg.TimestampHeader, discordTimestampHeader),
		dedupHeader:     strings.TrimSpace(cfg.DedupHeader),
		window:          window,
		now:             opts.Now,
	}, nil
}

func (v *discordEd25519) Scheme() string { return "discord-ed25519" }

func (v *discordEd25519) Verify(_ context.Context, req VerifyRequest) (VerifyResult, error) {
	pub, err := decodeEd25519PublicKey(req.Secret)
	if err != nil {
		return VerifyResult{}, err
	}
	sigHex := strings.TrimSpace(req.Header.Get(v.signatureHeader))
	if sigHex == "" {
		return failf("missing %s signature header", v.signatureHeader), nil
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return failf("%s hex is malformed", v.signatureHeader), nil
	}
	if len(sig) != ed25519.SignatureSize {
		return failf("%s is not a %d-byte signature", v.signatureHeader, ed25519.SignatureSize), nil
	}
	ts := strings.TrimSpace(req.Header.Get(v.timestampHeader))
	if ts == "" {
		return failf("missing %s header", v.timestampHeader), nil
	}
	// Freshness: the signed timestamp is part of the signed message, so a captured
	// valid delivery replayed outside the window is rejected even though ed25519
	// still verifies. Mirrors slack-v0's replay defense (parse the unix seconds,
	// reject when the skew exceeds the window) using the injectable clock.
	tsSecs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return failf("%s is not a unix timestamp", v.timestampHeader), nil
	}
	if !withinReplayWindow(effectiveNow(req, v.now), tsSecs, v.window) {
		return failf("%s %d is outside the %s replay window", v.timestampHeader, tsSecs, v.window), nil
	}
	msg := make([]byte, 0, len(ts)+len(req.Body))
	msg = append(msg, ts...)
	msg = append(msg, req.Body...)
	if !ed25519.Verify(pub, msg, sig) {
		return failf("%s does not match", v.signatureHeader), nil
	}
	res := VerifyResult{OK: true, EventType: discordEventType(req.Body)}
	if v.dedupHeader != "" {
		res.DedupID = strings.TrimSpace(req.Header.Get(v.dedupHeader))
	}
	return res, nil
}

// discordEventType derives the rule-facing event type from a verified Discord
// interaction body: its interaction "type", mapped to a stable lowercase name
// so a rule can select a non-PING interaction (e.g. `event =
// "application_command"`) and narrow further with a match on `data.name`.
// Unknown/future types fall back to "interaction_<n>" so the value is always
// non-empty and legible; a body that is not the expected JSON object yields "".
// (Type 1 PING is short-circuited to PONG by the receiver before matching, so
// "ping" here is only ever surfaced for observability.)
func discordEventType(body []byte) string {
	var p struct {
		Type json.Number `json:"type"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return ""
	}
	switch n := p.Type.String(); n {
	case "":
		return ""
	case "1":
		return "ping"
	case "2":
		return "application_command"
	case "3":
		return "message_component"
	case "4":
		return "application_command_autocomplete"
	case "5":
		return "modal_submit"
	default:
		return "interaction_" + n
	}
}

// decodeEd25519PublicKey interprets operator-provided public-key material as
// either hex (Discord's portal form, 64 hex chars) or raw 32 bytes. A malformed
// key is an operator fault, so it returns an error rather than a failed result.
func decodeEd25519PublicKey(material []byte) (ed25519.PublicKey, error) {
	if len(material) == 0 {
		return nil, errors.New("webhookverify: discord-ed25519 requires a public key")
	}
	trimmed := strings.TrimSpace(string(material))
	if decoded, err := hex.DecodeString(trimmed); err == nil && len(decoded) == ed25519.PublicKeySize {
		return ed25519.PublicKey(decoded), nil
	}
	if len(material) == ed25519.PublicKeySize {
		return ed25519.PublicKey(material), nil
	}
	return nil, fmt.Errorf("webhookverify: discord-ed25519 public key is not %d-byte hex or raw", ed25519.PublicKeySize)
}
