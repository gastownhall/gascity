package webhookverify

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

func discordSig(priv ed25519.PrivateKey, ts string, body []byte) string {
	msg := append([]byte(ts), body...)
	return hex.EncodeToString(ed25519.Sign(priv, msg))
}

// discordClockAt returns a clock pinned at the given unix-seconds timestamp so a
// fixed-ts signed vector stays inside the replay window deterministically.
func discordClockAt(unixSecs int64) func() time.Time {
	return func() time.Time { return time.Unix(unixSecs, 0) }
}

func TestDiscordEd25519_KnownGoodVector(t *testing.T) {
	// Deterministic vector: Ed25519 signing is deterministic, so a fixed seed
	// plus fixed message yields a stable, reproducible signature.
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	pubHex := hex.EncodeToString(pub)

	ts := "1700000000"
	body := []byte(`{"type":1}`)
	wantSig := discordSig(priv, ts, body)

	v, err := New("discord-ed25519", config.WebhookVerify{}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := v.Verify(context.Background(), VerifyRequest{
		Body:   body,
		Secret: []byte(pubHex), // operator supplies the app public key as hex
		Header: hdr(discordSignatureHeader, wantSig, discordTimestampHeader, ts),
		Now:    discordClockAt(1_700_000_000), // within the freshness window of ts
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("known-good vector must verify, reason %q", res.Reason)
	}
}

func TestDiscordEd25519_RawKeyBytes(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	ts := "1700000123"
	body := []byte(`{"type":2,"data":{}}`)
	v, _ := New("discord-ed25519", config.WebhookVerify{}, Options{})
	res, err := v.Verify(context.Background(), VerifyRequest{
		Body:   body,
		Secret: pub, // raw 32-byte public key
		Header: hdr(discordSignatureHeader, discordSig(priv, ts, body), discordTimestampHeader, ts),
		Now:    discordClockAt(1_700_000_123), // within the freshness window of ts
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("raw-key vector must verify, reason %q", res.Reason)
	}
}

func TestDiscordEd25519_BadSignatureRejected(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	ts := "1700000000"
	body := []byte(`{"type":2}`)
	good := discordSig(priv, ts, body)
	// flip the last hex nibble
	bad := good[:len(good)-1] + string("0123456789abcdef"[(hexVal(good[len(good)-1])+1)%16])

	v, _ := New("discord-ed25519", config.WebhookVerify{}, Options{})
	res, err := v.Verify(context.Background(), VerifyRequest{Body: body, Secret: pub, Header: hdr(discordSignatureHeader, bad, discordTimestampHeader, ts), Now: discordClockAt(1_700_000_000)})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("a corrupted signature must not verify")
	}
}

func TestDiscordEd25519_TamperedAndWrongKeyAndMissing(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	otherPub, _, _ := ed25519.GenerateKey(nil)
	ts := "1700000000"
	body := []byte(`{"type":2}`)
	sig := discordSig(priv, ts, body)
	v, _ := New("discord-ed25519", config.WebhookVerify{}, Options{})
	fresh := discordClockAt(1_700_000_000) // keep these inside the freshness window

	// tampered body
	res, _ := v.Verify(context.Background(), VerifyRequest{Body: []byte(`{"type":3}`), Secret: pub, Header: hdr(discordSignatureHeader, sig, discordTimestampHeader, ts), Now: fresh})
	if res.OK {
		t.Error("tampered body must not verify")
	}
	// wrong public key
	res, _ = v.Verify(context.Background(), VerifyRequest{Body: body, Secret: otherPub, Header: hdr(discordSignatureHeader, sig, discordTimestampHeader, ts), Now: fresh})
	if res.OK {
		t.Error("verification against the wrong public key must fail")
	}
	// missing signature header
	res, _ = v.Verify(context.Background(), VerifyRequest{Body: body, Secret: pub, Header: hdr(discordTimestampHeader, ts), Now: fresh})
	if res.OK {
		t.Error("missing signature header must not verify")
	}
	// missing timestamp header
	res, _ = v.Verify(context.Background(), VerifyRequest{Body: body, Secret: pub, Header: hdr(discordSignatureHeader, sig), Now: fresh})
	if res.OK {
		t.Error("missing timestamp header must not verify")
	}
}

func TestDiscordEd25519_BadPublicKeyIsError(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	ts := "1700000000"
	body := []byte(`{}`)
	v, _ := New("discord-ed25519", config.WebhookVerify{}, Options{})
	// operator supplied a garbage public key: operational error, not OK=false
	_, err := v.Verify(context.Background(), VerifyRequest{Body: body, Secret: []byte("not-a-valid-key"), Header: hdr(discordSignatureHeader, discordSig(priv, ts, body), discordTimestampHeader, ts)})
	if err == nil {
		t.Fatal("a malformed operator public key must be an operational error")
	}
}

// FIX 1: a validly-signed but STALE delivery is rejected on freshness (replay),
// while the same signature within the window verifies. The timestamp is part of
// the signed message, so ed25519 alone cannot catch a replay.
func TestDiscordEd25519_ReplaySkewRejected(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	signed := time.Unix(1_700_000_000, 0)
	ts := "1700000000"
	body := []byte(`{"type":2,"data":{}}`)
	sig := discordSig(priv, ts, body)
	v, _ := New("discord-ed25519", config.WebhookVerify{}, Options{})

	// 6 minutes later — outside the default 5-minute window even though ed25519 still verifies.
	stale, err := v.Verify(context.Background(), VerifyRequest{
		Body: body, Secret: pub,
		Header: hdr(discordSignatureHeader, sig, discordTimestampHeader, ts),
		Now:    func() time.Time { return signed.Add(6 * time.Minute) },
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if stale.OK {
		t.Fatal("a replayed (stale) delivery outside the window must be rejected")
	}

	// A fresh delivery with the same signature verifies.
	fresh, _ := v.Verify(context.Background(), VerifyRequest{
		Body: body, Secret: pub,
		Header: hdr(discordSignatureHeader, sig, discordTimestampHeader, ts),
		Now:    func() time.Time { return signed.Add(30 * time.Second) },
	})
	if !fresh.OK {
		t.Fatalf("a fresh delivery must verify, reason %q", fresh.Reason)
	}
}

// FIX 2: a pack-authored replay_window is clamped down to the operator ceiling
// (maxReplayWindow), so a pack cannot neuter the freshness control by widening
// the window it benefits from weakening.
func TestDiscordEd25519_ReplayWindowClampedToMax(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	signed := time.Unix(1_700_000_000, 0)
	ts := "1700000000"
	body := []byte(`{"type":2}`)
	sig := discordSig(priv, ts, body)

	// A pack asks for a 1000h window; it must be clamped to maxReplayWindow (15m).
	v, err := New("discord-ed25519", config.WebhookVerify{ReplayWindow: "1000h"}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Just outside the clamped max — still rejected despite the huge requested window.
	res, _ := v.Verify(context.Background(), VerifyRequest{
		Body: body, Secret: pub,
		Header: hdr(discordSignatureHeader, sig, discordTimestampHeader, ts),
		Now:    func() time.Time { return signed.Add(maxReplayWindow + time.Minute) },
	})
	if res.OK {
		t.Fatalf("a pack replay_window=1000h must be clamped to %s; a delivery past the max must be rejected", maxReplayWindow)
	}
}

// The Discord event type is derived from the verified interaction body's type so
// a rule can select a non-PING interaction (e.g. application_command).
func TestDiscordEd25519_EventTypeFromBody(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	ts := "1700000200"
	cases := map[string]string{
		`{"type":2,"data":{"name":"fix"}}`: "application_command",
		`{"type":3}`:                       "message_component",
		`{"type":5}`:                       "modal_submit",
		`{"type":1}`:                       "ping",
	}
	v, _ := New("discord-ed25519", config.WebhookVerify{}, Options{})
	for body, want := range cases {
		res, err := v.Verify(context.Background(), VerifyRequest{
			Body: []byte(body), Secret: pub,
			Header: hdr(discordSignatureHeader, discordSig(priv, ts, []byte(body)), discordTimestampHeader, ts),
			Now:    discordClockAt(1_700_000_200),
		})
		if err != nil {
			t.Fatalf("Verify(%s): %v", body, err)
		}
		if !res.OK {
			t.Fatalf("Verify(%s) not OK: %q", body, res.Reason)
		}
		if res.EventType != want {
			t.Errorf("body %s EventType = %q, want %q", body, res.EventType, want)
		}
	}
}

// Regression: a far-future signed timestamp must be rejected (the clamp-underflow
// path that a naive abs(skew) > window check would silently pass).
func TestDiscordEd25519_FarFutureTimestampRejected(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	ts := fmt.Sprintf("%d", int64(math.MaxInt64))
	body := []byte(`{"type":2}`)
	v, _ := New("discord-ed25519", config.WebhookVerify{}, Options{})
	res, err := v.Verify(context.Background(), VerifyRequest{
		Body: body, Secret: pub,
		Header: hdr(discordSignatureHeader, discordSig(priv, ts, body), discordTimestampHeader, ts),
		Now:    discordClockAt(1_700_000_000),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("a far-future signed timestamp must be rejected")
	}
}

func hexVal(b byte) int {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0')
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10
	default:
		return 0
	}
}
