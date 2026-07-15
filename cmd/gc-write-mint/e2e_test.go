//go:build integration

package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/clientgrant"
)

// TestEndToEnd_MintThroughGrantSource drives the whole grant foundation through
// the real binary: clientgrant marshals the request binding into GC_GRANT_INFO,
// execs the built gc-write-mint (which re-validates, recomputes the digest, and
// signs with a key gc never sees), clientgrant shape-checks the returned token,
// and citywriteauth verifies it against the matching public key — the exact
// path a `gc --context prod sling` mutation will take.
func TestEndToEnd_MintThroughGrantSource(t *testing.T) {
	pub, priv := realKey()
	dir := t.TempDir()

	keyFile := filepath.Join(dir, "city.ed25519")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(priv.Seed())), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "gc-write-mint")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build gc-write-mint: %v\n%s", err, out)
	}

	// The client wires the built binary as a grant_command, city-pinned.
	src, err := clientgrant.NewGrantSource(bin + " --kid k1 --key " + keyFile + " --city mc")
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"source":"pr-42"}`)
	h := sha256.Sum256(body)
	digest := citywriteauth.ReqDigest("POST", "/v0/city/mc/sling", "", body)
	token, err := src.Mint(clientgrant.GrantInfo{
		Aud:        "gc-city-write",
		City:       "mc",
		Method:     "POST",
		Path:       "/v0/city/mc/sling",
		BodySHA256: hex.EncodeToString(h[:]),
		ReqDigest:  digest,
	})
	if err != nil {
		t.Fatalf("Mint via real binary: %v", err)
	}

	// Verify with a real-clock verifier (the binary stamps wall-clock iat/exp).
	v, err := citywriteauth.New(citywriteauth.Options{
		Aud:    "gc-city-write",
		Keys:   map[string]ed25519.PublicKey{"k1": pub},
		MaxTTL: 2 * time.Minute,
		Skew:   30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(token, citywriteauth.Expect{City: "mc", ReqDigest: digest}); err != nil {
		t.Fatalf("end-to-end minted grant must verify: %v", err)
	}

	// A second mint of the identical request must produce a distinct, still-valid
	// grant (fresh jti) — the single-use property the replay guard depends on.
	token2, err := src.Mint(clientgrant.GrantInfo{
		Aud: "gc-city-write", City: "mc", Method: "POST", Path: "/v0/city/mc/sling",
		BodySHA256: hex.EncodeToString(h[:]), ReqDigest: digest,
	})
	if err != nil {
		t.Fatalf("second Mint: %v", err)
	}
	if token2 == token {
		t.Fatal("two mints of the same request must differ (fresh jti)")
	}
	if _, err := v.Verify(token2, citywriteauth.Expect{City: "mc", ReqDigest: digest}); err != nil {
		t.Fatalf("second grant must also verify: %v", err)
	}

	// A city-pinned minter must refuse a request for a different city (the binary
	// exits non-zero, so Mint surfaces an error rather than a token).
	if _, err := src.Mint(clientgrant.GrantInfo{
		Aud: "gc-city-write", City: "other", Method: "POST", Path: "/v0/city/other/sling",
		BodySHA256: hex.EncodeToString(h[:]),
		ReqDigest:  citywriteauth.ReqDigest("POST", "/v0/city/other/sling", "", body),
	}); err == nil {
		t.Fatal("city-pinned minter must refuse a foreign city")
	}
}

// realKey returns a deterministic keypair usable with a wall-clock verifier.
func realKey() (ed25519.PublicKey, ed25519.PrivateKey) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(255 - i)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv.Public().(ed25519.PublicKey), priv
}
