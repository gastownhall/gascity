// Command gc-write-mint is the reference X-GC-City-Write grant minter for a
// direct hardened city. gc invokes it as a context's grant_command: gc computes
// the request binding and hands it over as JSON in the GC_GRANT_INFO environment
// variable (never on argv). gc-write-mint re-validates the audience and city,
// independently recomputes the request digest and refuses if it does not match
// the client's claim, stamps the remaining claims, ed25519-signs, and prints the
// token to stdout.
//
// It is a re-validating signer, not a blind oracle: the signing key never enters
// gc, and the minter refuses to sign a request whose binding it cannot itself
// reconstruct. It is a dev/reference tool and deliberately lives outside the
// verify-only internal/citywriteauth package.
//
// Usage:
//
//	grant_command = "gc-write-mint --kid k1 --key ~/.gc/keys/city.ed25519 --city example-city"
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/clientgrant"
)

// defaultAud is the audience the capstone hardened city expects. It matches the
// verifier's configured Aud; a grant minted for a different audience is refused
// by citywriteauth, so the minter refuses it up front.
const defaultAud = citywriteauth.AudienceCityWrite

// maxTTL caps a grant's lifetime. A grant is single-use and request-bound, so a
// short window bounds the replay exposure; the server enforces its own MaxTTL
// too, but the minter never issues one longer than this.
const maxTTL = 2 * time.Minute

// minTTL is the smallest grant lifetime the minter will issue. iat and exp are
// stamped as whole Unix seconds (see mint), so a sub-second TTL could truncate
// to exp == iat, which the server rejects as a non-positive validity window
// (citywriteauth.Verify requires exp strictly after iat). One whole second
// guarantees floor(now+ttl) >= floor(now)+1 > iat for any sub-second clock
// offset, and a network mutation could never use a shorter grant anyway.
const minTTL = time.Second

// mintParams are the minter's own configuration: the signing identity and
// policy. They come from flags, never from the untrusted request.
type mintParams struct {
	Kid   string             // key id stamped into the grant; must match a server key
	Epoch int64              // epoch counter; must be >= the server's floor
	TTL   time.Duration      // grant lifetime (iat..exp); capped at maxTTL
	City  string             // if non-empty, refuse a request for a different city
	Aud   string             // expected audience; refuse a different one
	Key   ed25519.PrivateKey // the signing key; never leaves this process
	Now   func() time.Time   // injectable clock
	Rand  io.Reader          // injectable randomness for the jti
}

func main() {
	kid := flag.String("kid", "", "key id (kid) stamped into the grant; must match a server verifying key")
	keyPath := flag.String("key", "", "path to the ed25519 private key (PEM PKCS#8, or a raw/hex/base64 32-byte seed or 64-byte private key)")
	epoch := flag.Int64("epoch", 0, "epoch counter stamped into the grant (must be >= the server's floor)")
	ttl := flag.Duration("ttl", maxTTL, "grant lifetime; must be in ["+minTTL.String()+", "+maxTTL.String()+"]")
	city := flag.String("city", "", "if set, refuse to mint unless the request's city matches")
	aud := flag.String("aud", defaultAud, "expected audience; refuse to mint a request with a different audience")
	flag.Parse()

	if strings.TrimSpace(*keyPath) == "" {
		fatal(errors.New("--key is required"))
	}
	keyData, err := os.ReadFile(*keyPath)
	if err != nil {
		fatal(fmt.Errorf("reading --key: %w", err))
	}
	priv, err := parseEd25519PrivateKey(keyData)
	if err != nil {
		fatal(err)
	}
	info, err := loadGrantInfo(os.Getenv)
	if err != nil {
		fatal(err)
	}
	token, err := mint(mintParams{
		Kid:   *kid,
		Epoch: *epoch,
		TTL:   *ttl,
		City:  *city,
		Aud:   *aud,
		Key:   priv,
		Now:   time.Now,
		Rand:  rand.Reader,
	}, info)
	if err != nil {
		fatal(err)
	}
	fmt.Println(token)
}

// mint re-validates info against the minter's policy, recomputes the request
// digest, and returns a signed X-GC-City-Write token. Every failure returns an
// error and mints nothing, so the minter never issues a grant it could not
// itself reconstruct from the request parts.
func mint(p mintParams, info clientgrant.GrantInfo) (string, error) {
	if len(p.Key) != ed25519.PrivateKeySize {
		return "", errors.New("gc-write-mint: signing key not loaded")
	}
	if strings.TrimSpace(p.Kid) == "" {
		return "", errors.New("gc-write-mint: --kid is required")
	}
	if p.TTL < minTTL || p.TTL > maxTTL {
		return "", fmt.Errorf("gc-write-mint: --ttl must be in [%s, %s], got %s", minTTL, maxTTL, p.TTL)
	}
	aud := p.Aud
	if aud == "" {
		aud = defaultAud
	}

	// Validate the versioned contract and the request binding before signing.
	if info.Version != clientgrant.Version {
		return "", fmt.Errorf("gc-write-mint: grant info version %q != %q", info.Version, clientgrant.Version)
	}
	if info.Aud != aud {
		return "", fmt.Errorf("gc-write-mint: audience %q != expected %q", info.Aud, aud)
	}
	if strings.TrimSpace(info.City) == "" {
		return "", errors.New("gc-write-mint: grant info missing city")
	}
	if p.City != "" && info.City != p.City {
		return "", fmt.Errorf("gc-write-mint: request city %q != pinned --city %q", info.City, p.City)
	}
	if info.Method == "" || info.Path == "" {
		return "", errors.New("gc-write-mint: grant info missing method/path")
	}
	if !isHexSHA256(info.BodySHA256) {
		return "", fmt.Errorf("gc-write-mint: body_sha256 %q is not a hex sha256", info.BodySHA256)
	}
	if info.ReqDigest == "" {
		return "", errors.New("gc-write-mint: grant info missing req_digest")
	}

	// Re-validate: recompute the request digest independently from the parts and
	// refuse if the client's claim does not match. This is what makes the minter a
	// re-validating signer rather than a blind oracle — a captured grant request
	// cannot be repurposed by lying about the digest.
	want := citywriteauth.ReqDigestFromBodyHash(info.Method, info.Path, info.CanonicalQuery, info.BodySHA256)
	if want != info.ReqDigest {
		return "", fmt.Errorf("gc-write-mint: req_digest mismatch: client claimed %s, recomputed %s", info.ReqDigest, want)
	}

	now := p.Now().UTC()
	jti, err := randomJTI(p.Rand)
	if err != nil {
		return "", err
	}
	grant := citywriteauth.Grant{
		Kid:   p.Kid,
		Aud:   aud,
		City:  info.City,
		Epoch: p.Epoch,
		IAT:   now.Unix(),
		Exp:   now.Add(p.TTL).Unix(),
		JTI:   jti,
		Req:   want, // sign the minter's own recomputation, not the client's claim
	}
	payload, err := json.Marshal(grant)
	if err != nil {
		return "", fmt.Errorf("gc-write-mint: encoding grant: %w", err)
	}
	sig := ed25519.Sign(p.Key, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// loadGrantInfo reads and parses the request binding from GC_GRANT_INFO. The
// request never arrives on argv, so a `ps` snapshot cannot reveal it.
func loadGrantInfo(getenv func(string) string) (clientgrant.GrantInfo, error) {
	raw := strings.TrimSpace(getenv(clientgrant.GrantInfoEnv))
	if raw == "" {
		return clientgrant.GrantInfo{}, fmt.Errorf("gc-write-mint: %s is not set (gc-write-mint is invoked by gc as a grant_command)", clientgrant.GrantInfoEnv)
	}
	var info clientgrant.GrantInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return clientgrant.GrantInfo{}, fmt.Errorf("gc-write-mint: parsing %s: %w", clientgrant.GrantInfoEnv, err)
	}
	return info, nil
}

// randomJTI returns a fresh 128-bit token id as hex. A unique jti per mint is
// what lets the server's replay guard enforce single-use.
func randomJTI(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, 16)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("gc-write-mint: generating jti: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// isHexSHA256 reports whether s is a 64-character hex string (a SHA-256 digest).
func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// parseEd25519PrivateKey loads an ed25519 private key from PEM (PKCS#8) or, for a
// non-PEM file, from a raw / hex / base64 encoding of a 32-byte seed or a 64-byte
// private key. The input must be a PRIVATE key; a 32-byte value is treated as a
// seed.
func parseEd25519PrivateKey(data []byte) (ed25519.PrivateKey, error) {
	if block, _ := pem.Decode(data); block != nil {
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("gc-write-mint: parsing PEM private key: %w", err)
		}
		priv, ok := key.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("gc-write-mint: PEM key is %T, not ed25519", key)
		}
		return priv, nil
	}
	for _, cand := range keyCandidates(data) {
		switch len(cand) {
		case ed25519.SeedSize:
			return ed25519.NewKeyFromSeed(cand), nil
		case ed25519.PrivateKeySize:
			return append(ed25519.PrivateKey(nil), cand...), nil
		}
	}
	return nil, errors.New("gc-write-mint: --key is not PEM, nor a 32-byte seed / 64-byte ed25519 key (raw, hex, or base64)")
}

// keyCandidates returns the plausible decodings of key material: the trimmed
// hex and base64 forms first (a key file usually holds encoded text with a
// trailing newline), then the raw bytes as-is.
func keyCandidates(data []byte) [][]byte {
	var out [][]byte
	trimmed := strings.TrimSpace(string(data))
	if b, err := hex.DecodeString(trimmed); err == nil {
		out = append(out, b)
	}
	if b, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
		out = append(out, b)
	}
	if b, err := base64.RawURLEncoding.DecodeString(trimmed); err == nil {
		out = append(out, b)
	}
	out = append(out, data)
	return out
}

// fatal writes an error to stderr and exits non-zero, so gc's grant_command exec
// sees a failed mint rather than a malformed token on stdout.
func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
