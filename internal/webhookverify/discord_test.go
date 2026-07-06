package webhookverify

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func discordSig(priv ed25519.PrivateKey, ts string, body []byte) string {
	msg := append([]byte(ts), body...)
	return hex.EncodeToString(ed25519.Sign(priv, msg))
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
	res, err := v.Verify(context.Background(), VerifyRequest{Body: body, Secret: pub, Header: hdr(discordSignatureHeader, bad, discordTimestampHeader, ts)})
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

	// tampered body
	res, _ := v.Verify(context.Background(), VerifyRequest{Body: []byte(`{"type":3}`), Secret: pub, Header: hdr(discordSignatureHeader, sig, discordTimestampHeader, ts)})
	if res.OK {
		t.Error("tampered body must not verify")
	}
	// wrong public key
	res, _ = v.Verify(context.Background(), VerifyRequest{Body: body, Secret: otherPub, Header: hdr(discordSignatureHeader, sig, discordTimestampHeader, ts)})
	if res.OK {
		t.Error("verification against the wrong public key must fail")
	}
	// missing signature header
	res, _ = v.Verify(context.Background(), VerifyRequest{Body: body, Secret: pub, Header: hdr(discordTimestampHeader, ts)})
	if res.OK {
		t.Error("missing signature header must not verify")
	}
	// missing timestamp header
	res, _ = v.Verify(context.Background(), VerifyRequest{Body: body, Secret: pub, Header: hdr(discordSignatureHeader, sig)})
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
