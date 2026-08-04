package citywriteauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

// Cross-repo golden vectors. A grant minted by the crucible CityWriteMinter must
// verify here. The identical tokens / pubkey / digest are pinned in the crucible
// citywritemint golden test (deterministic seed + fixed fields), so any drift in
// the wire contract on either side fails this test loudly.
//
// goldenTokenV2 is the cid+v2 cutover token: aud "gc-city-write.v2" plus the
// cid tenancy claim. goldenTokenV1 is the pre-cid token (aud "gc-city-write",
// no cid), retained to pin the legacy-acceptance and cid-fail-closed behavior
// against a real legacy wire artifact (the field is genuinely absent from its
// payload, not empty).
const (
	goldenTokenV2 = "eyJraWQiOiJrMSIsImF1ZCI6ImdjLWNpdHktd3JpdGUudjIiLCJjaXR5IjoiYWNtZSIsImNpZCI6ImNpdHlfYWNtZSIsImVwb2NoIjo3LCJpYXQiOjE3MDAwMDAwMDAsImV4cCI6MTcwMDAwMDAzMCwianRpIjoianRpLWZpeGVkIiwicmVxIjoiYWRlZTY5YzgyOTI4ZGI2N2I3OGI5NTM5ZDNhYjllOTY2Yzk2OGExNDllZWQ0NjJlZDg1NzM5YzBhOGE4ZTZlOCJ9.yFUNyRHlJ_lkPFy98GkiqFb1yO-CdOSi6KHSnCTa0VGCHiR7RNIMvb8DnsM4XDDbyh8XrHgjsqLAxfL2_c8QAw"
	goldenTokenV1 = "eyJraWQiOiJrMSIsImF1ZCI6ImdjLWNpdHktd3JpdGUiLCJjaXR5IjoiYWNtZSIsImVwb2NoIjo3LCJpYXQiOjE3MDAwMDAwMDAsImV4cCI6MTcwMDAwMDAzMCwianRpIjoianRpLWZpeGVkIiwicmVxIjoiYWRlZTY5YzgyOTI4ZGI2N2I3OGI5NTM5ZDNhYjllOTY2Yzk2OGExNDllZWQ0NjJlZDg1NzM5YzBhOGE4ZTZlOCJ9.h52S5KxNNJ0Q2lU-nRHvvqeDyxhFs4mYY057LDu-wHrcF0ttFyiohVSOOCUydyDC1fNLIyAMzBRDwydtdAWwDg"

	goldenPubStdB64 = "1hcioE4eYD4PsM66wVJ8oBErEfCTyNPt9Q/+ZT0drmk="
	goldenDigest    = "adee69c82928db67b78b9539d3ab9e966c968a149eed462ed85739c0a8a8e6e8"
	goldenCID       = "city_acme"
)

// goldenVerifier builds a verifier shaped like the production writeauth wiring:
// v2 primary audience, optional legacy v1 audience, cid enforced when set.
func goldenVerifier(t *testing.T, cid, legacyAud string) *Verifier {
	t.Helper()
	pubRaw, err := base64.StdEncoding.DecodeString(goldenPubStdB64)
	if err != nil {
		t.Fatalf("pubkey: %v", err)
	}
	v, err := New(Options{
		Aud:       "gc-city-write.v2",
		LegacyAud: legacyAud,
		CID:       cid,
		Keys:      map[string]ed25519.PublicKey{"k1": ed25519.PublicKey(pubRaw)},
		MaxTTL:    time.Minute,
		Skew:      30 * time.Second,
		Now:       func() time.Time { return time.Unix(1_700_000_015, 0) }, // inside [iat, exp]
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

func TestVerify_CrucibleGoldenVectorV2(t *testing.T) {
	v := goldenVerifier(t, goldenCID, "gc-city-write")
	g, err := v.Verify(goldenTokenV2, Expect{City: "acme", ReqDigest: goldenDigest})
	if err != nil {
		t.Fatalf("crucible v2 golden token must verify here: %v", err)
	}
	if g.JTI != "jti-fixed" || g.Epoch != 7 || g.Kid != "k1" || g.CID != goldenCID {
		t.Fatalf("unexpected grant: %+v", g)
	}
}

// The tenancy binding: the v2 golden token was minted for city_acme, so a
// verifier configured with a different cid (another org's controller) must
// reject it even though every other claim checks out.
func TestVerify_CrucibleGoldenVectorV2_RejectedByOtherTenant(t *testing.T) {
	v := goldenVerifier(t, "city_other", "gc-city-write")
	if _, err := v.Verify(goldenTokenV2, Expect{City: "acme", ReqDigest: goldenDigest}); !errors.Is(err, ErrCIDMismatch) {
		t.Fatalf("v2 golden token vs other tenant: got %v, want ErrCIDMismatch", err)
	}
}

// Legacy v1 grants (pre-cid aud, no cid claim) stay accepted on deployments
// that are not tenancy-scoped, so nothing already minting v1 breaks.
func TestVerify_CrucibleGoldenVectorV1_LegacyAccepted(t *testing.T) {
	v := goldenVerifier(t, "", "gc-city-write")
	g, err := v.Verify(goldenTokenV1, Expect{City: "acme", ReqDigest: goldenDigest})
	if err != nil {
		t.Fatalf("legacy v1 golden token must verify when LegacyAud is configured: %v", err)
	}
	if g.CID != "" {
		t.Fatalf("legacy grant must carry no cid, got %q", g.CID)
	}
}

// On a tenancy-scoped verifier (cid configured) the legacy audience is not
// honored at all, so a legacy v1 grant is rejected outright on the audience
// gate — the v2 cutover forcing function itself, ahead of the cid gate. This is
// what closes the mis-minted "legacy audience + matching cid" hole: because the
// legacy audience is refused under cid, dual-accept can never reopen the tenancy
// window the v2 audience cutover closed, even for a grant that carries a
// matching cid.
func TestVerify_CrucibleGoldenVectorV1_RejectedWhenCIDConfigured(t *testing.T) {
	v := goldenVerifier(t, goldenCID, "gc-city-write")
	if _, err := v.Verify(goldenTokenV1, Expect{City: "acme", ReqDigest: goldenDigest}); !errors.Is(err, ErrAudience) {
		t.Fatalf("v1 golden token vs cid-configured verifier: got %v, want ErrAudience", err)
	}
}

// Without LegacyAud the verifier is v2-only and the v1 audience is rejected
// outright — the pre-cutover hard-reject behavior remains reachable.
func TestVerify_CrucibleGoldenVectorV1_RejectedWithoutLegacyAud(t *testing.T) {
	v := goldenVerifier(t, "", "")
	if _, err := v.Verify(goldenTokenV1, Expect{City: "acme", ReqDigest: goldenDigest}); !errors.Is(err, ErrAudience) {
		t.Fatalf("v1 golden token vs v2-only verifier: got %v, want ErrAudience", err)
	}
}
