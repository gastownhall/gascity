package citywriteauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"
)

// Cross-repo golden vector. A grant minted by the crucible CityWriteMinter must
// verify here. The identical token / pubkey / digest are pinned in the crucible
// citywritemint golden test (deterministic seed + fixed fields), so any drift in
// the wire contract on either side fails this test loudly.
func TestVerify_CrucibleGoldenVector(t *testing.T) {
	const (
		goldenToken     = "eyJraWQiOiJrMSIsImF1ZCI6ImdjLWNpdHktd3JpdGUiLCJjaXR5IjoiYWNtZSIsImVwb2NoIjo3LCJpYXQiOjE3MDAwMDAwMDAsImV4cCI6MTcwMDAwMDAzMCwianRpIjoianRpLWZpeGVkIiwicmVxIjoiYWRlZTY5YzgyOTI4ZGI2N2I3OGI5NTM5ZDNhYjllOTY2Yzk2OGExNDllZWQ0NjJlZDg1NzM5YzBhOGE4ZTZlOCJ9.h52S5KxNNJ0Q2lU-nRHvvqeDyxhFs4mYY057LDu-wHrcF0ttFyiohVSOOCUydyDC1fNLIyAMzBRDwydtdAWwDg"
		goldenPubStdB64 = "1hcioE4eYD4PsM66wVJ8oBErEfCTyNPt9Q/+ZT0drmk="
		goldenDigest    = "adee69c82928db67b78b9539d3ab9e966c968a149eed462ed85739c0a8a8e6e8"
	)
	pubRaw, err := base64.StdEncoding.DecodeString(goldenPubStdB64)
	if err != nil {
		t.Fatalf("pubkey: %v", err)
	}
	v, err := New(Options{
		Aud:    "gc-city-write",
		Keys:   map[string]ed25519.PublicKey{"k1": ed25519.PublicKey(pubRaw)},
		MaxTTL: time.Minute,
		Skew:   30 * time.Second,
		Now:    func() time.Time { return time.Unix(1_700_000_015, 0) }, // inside [iat, exp]
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g, err := v.Verify(goldenToken, Expect{City: "acme", ReqDigest: goldenDigest})
	if err != nil {
		t.Fatalf("crucible golden token must verify here: %v", err)
	}
	if g.JTI != "jti-fixed" || g.Epoch != 7 || g.Kid != "k1" {
		t.Fatalf("unexpected grant: %+v", g)
	}
}
