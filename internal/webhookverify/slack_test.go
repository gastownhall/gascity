package webhookverify

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

const slackTestSecret = "slack-signing-secret-0123456789ab"

func slackSign(ts string, body []byte) string {
	base := []byte("v0:" + ts + ":")
	base = append(base, body...)
	return "v0=" + hex.EncodeToString(hmacSHA256([]byte(slackTestSecret), base))
}

func TestSlackV0_Valid(t *testing.T) {
	secret := slackTestSecret
	body := []byte(`{"type":"event_callback"}`)
	now := time.Unix(1_700_000_000, 0)
	ts := fmt.Sprintf("%d", now.Unix())
	sig := slackSign(ts, body)

	v, err := New("slack-v0", config.WebhookVerify{}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := v.Verify(context.Background(), VerifyRequest{
		Body:   body,
		Secret: []byte(secret),
		Header: hdr(slackSignatureHeader, sig, slackTimestampHeader, ts),
		Now:    func() time.Time { return now.Add(30 * time.Second) },
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected OK, reason %q", res.Reason)
	}
	if res.DedupID != ts {
		t.Errorf("DedupID = %q, want the signed timestamp %q", res.DedupID, ts)
	}
}

func TestSlackV0_ReplaySkewRejected(t *testing.T) {
	secret := "slack-signing-secret-0123456789ab"
	body := []byte(`{"type":"event_callback"}`)
	signed := time.Unix(1_700_000_000, 0)
	ts := fmt.Sprintf("%d", signed.Unix())
	sig := slackSign(ts, body)

	v, _ := New("slack-v0", config.WebhookVerify{}, Options{})
	res, err := v.Verify(context.Background(), VerifyRequest{
		Body:   body,
		Secret: []byte(secret),
		Header: hdr(slackSignatureHeader, sig, slackTimestampHeader, ts),
		// 6 minutes later — outside the 5-minute replay window even though the
		// signature itself is valid.
		Now: func() time.Time { return signed.Add(6 * time.Minute) },
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("a replayed delivery outside the window must be rejected")
	}
}

func TestSlackV0_TamperedWrongSecretMissing(t *testing.T) {
	secret := "slack-signing-secret-0123456789ab"
	body := []byte(`{"type":"event_callback"}`)
	now := time.Unix(1_700_000_000, 0)
	ts := fmt.Sprintf("%d", now.Unix())
	sig := slackSign(ts, body)
	clock := func() time.Time { return now.Add(10 * time.Second) }
	v, _ := New("slack-v0", config.WebhookVerify{}, Options{})

	// tampered body
	res, _ := v.Verify(context.Background(), VerifyRequest{Body: []byte("tampered"), Secret: []byte(secret), Header: hdr(slackSignatureHeader, sig, slackTimestampHeader, ts), Now: clock})
	if res.OK {
		t.Error("tampered body must not verify")
	}
	// wrong secret
	res, _ = v.Verify(context.Background(), VerifyRequest{Body: body, Secret: []byte("wrong-secret-wrong-secret-wrong!"), Header: hdr(slackSignatureHeader, sig, slackTimestampHeader, ts), Now: clock})
	if res.OK {
		t.Error("wrong secret must not verify")
	}
	// missing signature header
	res, _ = v.Verify(context.Background(), VerifyRequest{Body: body, Secret: []byte(secret), Header: hdr(slackTimestampHeader, ts), Now: clock})
	if res.OK {
		t.Error("missing signature header must not verify")
	}
	// missing timestamp header
	res, _ = v.Verify(context.Background(), VerifyRequest{Body: body, Secret: []byte(secret), Header: hdr(slackSignatureHeader, sig), Now: clock})
	if res.OK {
		t.Error("missing timestamp header must not verify")
	}
}

func TestSlackV0_ConfigurableWindow(t *testing.T) {
	secret := "slack-signing-secret-0123456789ab"
	body := []byte(`{}`)
	now := time.Unix(1_700_000_000, 0)
	ts := fmt.Sprintf("%d", now.Unix())
	sig := slackSign(ts, body)

	// A 1-minute window rejects a 2-minute-old delivery that the default 5m
	// window would accept.
	v, err := New("slack-v0", config.WebhookVerify{ReplayWindow: "1m"}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, _ := v.Verify(context.Background(), VerifyRequest{Body: body, Secret: []byte(secret), Header: hdr(slackSignatureHeader, sig, slackTimestampHeader, ts), Now: func() time.Time { return now.Add(2 * time.Minute) }})
	if res.OK {
		t.Fatal("delivery outside the configured 1m window must be rejected")
	}
}

// FIX 2: a pack-authored replay_window above the operator ceiling is clamped down
// to maxReplayWindow — a pack cannot widen the freshness window it benefits from
// weakening. A delivery just outside the clamped max is still rejected.
func TestSlackV0_ReplayWindowClampedToMax(t *testing.T) {
	secret := slackTestSecret
	body := []byte(`{}`)
	now := time.Unix(1_700_000_000, 0)
	ts := fmt.Sprintf("%d", now.Unix())
	sig := slackSign(ts, body)

	// A pack asks for a 1000h window; the effective window is clamped to 15m.
	v, err := New("slack-v0", config.WebhookVerify{ReplayWindow: "1000h"}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, _ := v.Verify(context.Background(), VerifyRequest{
		Body: body, Secret: []byte(secret),
		Header: hdr(slackSignatureHeader, sig, slackTimestampHeader, ts),
		Now:    func() time.Time { return now.Add(maxReplayWindow + time.Minute) },
	})
	if res.OK {
		t.Fatalf("a pack replay_window=1000h must be clamped to %s; a stale delivery past the max must be rejected", maxReplayWindow)
	}
}
