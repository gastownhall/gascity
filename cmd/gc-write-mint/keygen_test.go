package main

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/citywriteauth"
)

// The keygen round trip: the generated private key is accepted by the --key
// parser, mints a grant, and the printed verify-key line configures the
// server's write-auth verifier, which accepts that grant under the same kid.
func TestKeygen_RoundTripMintsVerifiableGrant(t *testing.T) {
	t.Setenv("GC_CITY_WRITE_PUBKEY", "")
	t.Setenv("GC_CITY_WRITE_REQUIRED", "")
	t.Setenv("GC_CITY_WRITE_EPOCH_FLOOR", "")
	t.Setenv("GC_CITY_WRITE_CID", "")
	out := filepath.Join(t.TempDir(), "keys", "mc-alice.ed25519")

	line, err := keygen(keygenParams{Kid: "alice", Out: out, Rand: rand.Reader})
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	if !strings.HasPrefix(line, "alice:") {
		t.Fatalf("verify-key line = %q, want alice:<base64 pubkey>", line)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key mode = %o, want 600", perm)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("-----BEGIN PRIVATE KEY-----")) {
		t.Fatalf("key file is not a PEM PKCS#8 private key:\n%s", data)
	}
	priv, err := parseEd25519PrivateKey(data)
	if err != nil {
		t.Fatalf("--key parser rejects the generated key: %v", err)
	}

	v, err := api.ResolveWriteAuthVerifier(line, true)
	if err != nil {
		t.Fatalf("server rejects the printed verify key %q: %v", line, err)
	}

	in := infoFor("POST", "/v0/city/mc/sling", "", []byte(`{"source":"pr-42"}`))
	token, err := mint(mintParams{
		Kid:  "alice",
		TTL:  time.Minute,
		Aud:  defaultAud,
		City: "mc",
		Key:  priv,
		Now:  time.Now,
		Rand: rand.Reader,
	}, in)
	if err != nil {
		t.Fatalf("mint with generated key: %v", err)
	}
	g, err := v.Verify(token, citywriteauth.Expect{City: "mc", ReqDigest: in.ReqDigest})
	if err != nil {
		t.Fatalf("grant minted with the generated key must verify: %v", err)
	}
	if g.Kid != "alice" {
		t.Fatalf("verified kid = %q, want alice", g.Kid)
	}
}

func TestKeygen_RefusesOverwriteWithoutForce(t *testing.T) {
	out := filepath.Join(t.TempDir(), "k.ed25519")
	first, err := keygen(keygenParams{Kid: "alice", Out: out, Rand: rand.Reader})
	if err != nil {
		t.Fatalf("first keygen: %v", err)
	}
	before, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := keygen(keygenParams{Kid: "alice", Out: out, Rand: rand.Reader}); err == nil {
		t.Fatal("keygen overwrote an existing key without --force")
	} else if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("overwrite error = %v, want it to name --force", err)
	}
	after, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("refused keygen still changed the existing key")
	}

	second, err := keygen(keygenParams{Kid: "alice", Out: out, Force: true, Rand: rand.Reader})
	if err != nil {
		t.Fatalf("keygen --force: %v", err)
	}
	if second == first {
		t.Fatal("keygen --force printed the old verify key")
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, data) {
		t.Fatal("keygen --force left the old key in place")
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("forced key mode = %o, want 600", perm)
	}
	priv, err := parseEd25519PrivateKey(data)
	if err != nil {
		t.Fatalf("parsing forced key: %v", err)
	}
	if want := verifyKeyLine("alice", priv); second != want {
		t.Fatalf("printed line %q does not match the written key (%q)", second, want)
	}
}

func TestKeygen_RejectsBadKid(t *testing.T) {
	for _, kid := range []string{"", "  ", "a:b", "a,b", "a b", "a\tb"} {
		t.Run(kid, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "k.ed25519")
			if _, err := keygen(keygenParams{Kid: kid, Out: out, Rand: rand.Reader}); err == nil {
				t.Fatalf("keygen accepted kid %q", kid)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("keygen wrote a key for rejected kid %q (stat err %v)", kid, err)
			}
		})
	}
}

func TestRunKeygen_CLI(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "k.ed25519")

	var stdout, stderr bytes.Buffer
	if code := runKeygen([]string{"--kid", "alice", "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr.String())
	}
	line := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(line, "alice:") || strings.Count(stdout.String(), "\n") != 1 {
		t.Fatalf("stdout = %q, want exactly one alice:<base64> line", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runKeygen([]string{"--kid", "alice", "--out", out}, &stdout, &stderr); code != 1 {
		t.Fatalf("overwrite exit = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("refused keygen printed %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runKeygen([]string{"--kid", "alice"}, &stdout, &stderr); code != 2 {
		t.Fatalf("missing --out exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--out") {
		t.Fatalf("stderr = %q, want it to name --out", stderr.String())
	}
}
