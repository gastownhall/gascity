package productmetrics

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

const (
	testPauseKeyID      = "pm-pause-test-01"
	testPauseRelease    = "0.31.0"
	testPauseEpoch      = uint64(7)
	maxSafeEpochForTest = uint64(1<<53 - 1)
)

func deterministicPauseKey() (ed25519.PublicKey, ed25519.PrivateKey) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	return append(ed25519.PublicKey(nil), privateKey[ed25519.SeedSize:]...), privateKey
}

func testPauseCatalog(publicKey ed25519.PublicKey) pausePublicKeyCatalog {
	return func(yield func(pausePublicKeyEntry)) {
		yield(pausePublicKeyEntry{id: testPauseKeyID, key: publicKey})
	}
}

func pauseCanonicalOracle(releaseVersion string, metricsEpoch uint64, keyID string) []byte {
	canonicalJSON := fmt.Sprintf(`{"action":"pause-through-metrics-epoch","app":"gascity","key_id":%q,"metrics_epoch":%d,"release_version":%q,"schema_version":1}`, keyID, metricsEpoch, releaseVersion)
	return append([]byte("gascity-product-metrics-pause-v1\x00"), canonicalJSON...)
}

func signedPauseEnvelope(releaseVersion string, metricsEpoch uint64, keyID string, privateKey ed25519.PrivateKey) string {
	signature := ed25519.Sign(privateKey, pauseCanonicalOracle(releaseVersion, metricsEpoch, keyID))
	// Deliberately use a different input field order from RFC 8785 order. The
	// verifier must canonicalize the six signed fields, not verify raw bytes.
	return fmt.Sprintf(`{"signature":%q,"metrics_epoch":%d,"action":"pause-through-metrics-epoch","schema_version":1,"key_id":%q,"app":"gascity","release_version":%q}`,
		base64.RawURLEncoding.EncodeToString(signature), metricsEpoch, keyID, releaseVersion)
}

func TestCanonicalPauseMessageMatchesRestrictedRFC8785Vector(t *testing.T) {
	message, err := canonicalPauseMessage(pauseUnsigned{
		SchemaVersion:  SchemaVersionV1,
		App:            AppGasCity,
		Action:         pauseAction,
		ReleaseVersion: testPauseRelease,
		MetricsEpoch:   testPauseEpoch,
		KeyID:          testPauseKeyID,
	})
	if err != nil {
		t.Fatalf("canonicalPauseMessage: %v", err)
	}
	if want := pauseCanonicalOracle(testPauseRelease, testPauseEpoch, testPauseKeyID); !bytes.Equal(message, want) {
		t.Fatalf("canonical message mismatch\n got: %q\nwant: %q", message, want)
	}
}

func TestVerifySignedPauseAcceptsReorderedExactEnvelope(t *testing.T) {
	publicKey, privateKey := deterministicPauseKey()
	body := signedPauseEnvelope(testPauseRelease, testPauseEpoch, testPauseKeyID, privateKey)

	verified, err := verifySignedPause([]byte(body), pauseExpectation{
		releaseVersion: testPauseRelease,
		metricsEpoch:   testPauseEpoch,
	}, testPauseCatalog(publicKey))
	if err != nil {
		t.Fatalf("verifySignedPause: %v", err)
	}
	if verified.releaseVersion != testPauseRelease || verified.metricsEpoch != testPauseEpoch || verified.keyID != testPauseKeyID {
		t.Fatalf("verified pause = %#v", verified)
	}

	withWhitespace := " \n\t" + body + " \r\n"
	if _, err := verifySignedPause([]byte(withWhitespace), pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, testPauseCatalog(publicKey)); err != nil {
		t.Fatalf("verifySignedPause(reformatted): %v", err)
	}

	maxSafe := signedPauseEnvelope(testPauseRelease, maxSafeEpochForTest, testPauseKeyID, privateKey)
	if _, err := verifySignedPause([]byte(maxSafe), pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: maxSafeEpochForTest}, testPauseCatalog(publicKey)); err != nil {
		t.Fatalf("verifySignedPause(maximum JCS-safe epoch): %v", err)
	}
}

func TestVerifySignedPauseRejectsHostileEnvelopes(t *testing.T) {
	publicKey, privateKey := deterministicPauseKey()
	valid := signedPauseEnvelope(testPauseRelease, testPauseEpoch, testPauseKeyID, privateKey)
	otherSeed := bytes.Repeat([]byte{0xff}, ed25519.SeedSize)
	otherPrivate := ed25519.NewKeyFromSeed(otherSeed)
	otherPublic := otherPrivate.Public().(ed25519.PublicKey)

	validSignatureText := extractPauseSignatureForTest(t, valid)
	signatureBytes, err := base64.RawURLEncoding.DecodeString(validSignatureText)
	if err != nil {
		t.Fatal(err)
	}
	signatureBytes[0] ^= 0x80
	bitFlipped := strings.Replace(valid, validSignatureText, base64.RawURLEncoding.EncodeToString(signatureBytes), 1)

	wrongReleaseSigned := signedPauseEnvelope("0.32.0", testPauseEpoch, testPauseKeyID, privateKey)
	wrongEpochSigned := signedPauseEnvelope(testPauseRelease, testPauseEpoch+1, testPauseKeyID, privateKey)
	zeroEpochSigned := signedPauseEnvelope(testPauseRelease, 0, testPauseKeyID, privateKey)
	unknownKeySigned := signedPauseEnvelope(testPauseRelease, testPauseEpoch, "pm-pause-unknown", privateKey)
	wrongSignature := signedPauseEnvelope(testPauseRelease, testPauseEpoch, testPauseKeyID, otherPrivate)
	unsafeEpoch := signedPauseEnvelope(testPauseRelease, maxSafeEpochForTest+1, testPauseKeyID, privateKey)

	tests := map[string]struct {
		body        string
		expectation pauseExpectation
		catalog     pausePublicKeyCatalog
	}{
		"empty":                 {expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"oversized":             {body: valid + strings.Repeat(" ", maxUploadResponseBytes-len(valid)+1), expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"null":                  {body: `null`, expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"trailing JSON":         {body: valid + `{}`, expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"unknown field":         {body: strings.Replace(valid, `}`, `,"extra":true}`, 1), expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"duplicate key":         {body: strings.Replace(valid, `"app":"gascity"`, `"app":"gascity","app":"gascity"`, 1), expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"case-folded field":     {body: strings.Replace(valid, `"app"`, `"APP"`, 1), expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"wrong schema":          {body: strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1), expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"wrong app":             {body: strings.Replace(valid, `"app":"gascity"`, `"app":"beads"`, 1), expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"wrong action":          {body: strings.Replace(valid, pauseAction, "pause", 1), expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"release mismatch":      {body: wrongReleaseSigned, expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"epoch mismatch":        {body: wrongEpochSigned, expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"zero epoch":            {body: zeroEpochSigned, expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: 0}, catalog: testPauseCatalog(publicKey)},
		"unknown key":           {body: unknownKeySigned, expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"wrong signing key":     {body: wrongSignature, expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"unsafe JSON epoch":     {body: unsafeEpoch, expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: maxSafeEpochForTest + 1}, catalog: testPauseCatalog(publicKey)},
		"bit-flipped signature": {body: bitFlipped, expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"padded signature":      {body: strings.Replace(valid, validSignatureText, validSignatureText+"=", 1), expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"short signature":       {body: strings.Replace(valid, validSignatureText, base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize-1)), 1), expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"missing signature":     {body: strings.Replace(valid, `"signature":"`+validSignatureText+`",`, "", 1), expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"fractional epoch":      {body: strings.Replace(valid, `"metrics_epoch":7`, `"metrics_epoch":7.0`, 1), expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(publicKey)},
		"nil catalog":           {body: valid, expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}},
		"wrong catalog key":     {body: valid, expectation: pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, catalog: testPauseCatalog(otherPublic)},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := verifySignedPause([]byte(test.body), test.expectation, test.catalog); err == nil {
				t.Fatal("verifySignedPause unexpectedly accepted an invalid envelope")
			}
		})
	}
}

func TestPauseKeyCatalogFailsClosedAndProductionSetIsEmpty(t *testing.T) {
	publicKey, privateKey := deterministicPauseKey()
	valid := []byte(signedPauseEnvelope(testPauseRelease, testPauseEpoch, testPauseKeyID, privateKey))
	expectation := pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}

	productionCount := 0
	productionPausePublicKeyCatalog(func(pausePublicKeyEntry) { productionCount++ })
	if productionCount != 0 {
		t.Fatalf("Stage 1a production pause-key catalog has %d entries, want zero", productionCount)
	}
	if _, err := verifySignedPause(valid, expectation, productionPausePublicKeyCatalog); err == nil {
		t.Fatal("endpoint-empty production key catalog verified a signed pause")
	}

	for name, catalog := range map[string]pausePublicKeyCatalog{
		"empty ID": func(yield func(pausePublicKeyEntry)) {
			yield(pausePublicKeyEntry{key: publicKey})
		},
		"oversized ID": func(yield func(pausePublicKeyEntry)) {
			yield(pausePublicKeyEntry{id: strings.Repeat("k", maxPauseKeyIDBytes+1), key: publicKey})
		},
		"wrong key size": func(yield func(pausePublicKeyEntry)) {
			yield(pausePublicKeyEntry{id: testPauseKeyID, key: publicKey[:8]})
		},
		"duplicate ID": func(yield func(pausePublicKeyEntry)) {
			yield(pausePublicKeyEntry{id: testPauseKeyID, key: publicKey})
			yield(pausePublicKeyEntry{id: testPauseKeyID, key: publicKey})
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verifySignedPause(valid, expectation, catalog); err == nil {
				t.Fatal("verifySignedPause accepted an invalid key catalog")
			}
		})
	}
}

func TestSignedPauseSharedValidVector(t *testing.T) {
	publicKey, privateKey := deterministicPauseKey()
	wantEnvelope := signedPauseEnvelope(testPauseRelease, testPauseEpoch, testPauseKeyID, privateKey) + "\n"
	wantPublicKey := base64.RawURLEncoding.EncodeToString(publicKey) + "\n"

	gotEnvelope, envelopeErr := os.ReadFile("testdata/pause-v1/valid.json")
	gotPublicKey, publicKeyErr := os.ReadFile("testdata/pause-v1/public-key.b64url")
	if envelopeErr != nil || publicKeyErr != nil {
		t.Fatalf("shared pause vector is missing; envelope=%s public_key=%s", strings.TrimSpace(wantEnvelope), strings.TrimSpace(wantPublicKey))
	}
	if string(gotEnvelope) != wantEnvelope {
		t.Fatalf("valid vector drift\n got: %s\nwant: %s", gotEnvelope, wantEnvelope)
	}
	if string(gotPublicKey) != wantPublicKey {
		t.Fatalf("public-key vector drift\n got: %s\nwant: %s", gotPublicKey, wantPublicKey)
	}
	if _, err := verifySignedPause(bytes.TrimSpace(gotEnvelope), pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, testPauseCatalog(publicKey)); err != nil {
		t.Fatalf("verify shared pause vector: %v", err)
	}
}

func TestSignedPauseSharedInvalidVectors(t *testing.T) {
	publicKey, _ := deterministicPauseKey()
	for _, name := range []string{"bit-flipped.json", "duplicate-key.json", "unknown-key.json", "padded-signature.json"} {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile("testdata/pause-v1/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := verifySignedPause(bytes.TrimSpace(body), pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, testPauseCatalog(publicKey)); err == nil {
				t.Fatal("invalid shared vector verified")
			}
		})
	}
}

func extractPauseSignatureForTest(t *testing.T, envelope string) string {
	t.Helper()
	const marker = `"signature":"`
	start := strings.Index(envelope, marker)
	if start < 0 {
		t.Fatal("test envelope has no signature")
	}
	start += len(marker)
	end := strings.IndexByte(envelope[start:], '"')
	if end < 0 {
		t.Fatal("test envelope has an unterminated signature")
	}
	return envelope[start : start+end]
}

func TestPauseDTOsHaveClosedShapes(t *testing.T) {
	for typ, want := range map[reflect.Type][]string{
		reflect.TypeOf(pauseUnsigned{}):     {"SchemaVersion", "App", "Action", "ReleaseVersion", "MetricsEpoch", "KeyID"},
		reflect.TypeOf(pauseEnvelopeWire{}): {"SchemaVersion", "App", "Action", "ReleaseVersion", "MetricsEpoch", "KeyID", "Signature"},
	} {
		if typ.NumField() != len(want) {
			t.Fatalf("%s has %d fields, want %d", typ, typ.NumField(), len(want))
		}
		for i, field := range want {
			if typ.Field(i).Name != field {
				t.Errorf("%s field %d = %s, want %s", typ, i, typ.Field(i).Name, field)
			}
		}
		assertNoOpenDTOType(t, typ, map[reflect.Type]bool{})
	}
}

func FuzzVerifySignedPause(f *testing.F) {
	publicKey, privateKey := deterministicPauseKey()
	f.Add([]byte(signedPauseEnvelope(testPauseRelease, testPauseEpoch, testPauseKeyID, privateKey)))
	f.Add([]byte(`{}`))
	f.Add(bytes.Repeat([]byte{'x'}, maxUploadResponseBytes+1))
	f.Fuzz(func(t *testing.T, body []byte) {
		verified, err := verifySignedPause(body, pauseExpectation{releaseVersion: testPauseRelease, metricsEpoch: testPauseEpoch}, testPauseCatalog(publicKey))
		if err == nil && (verified.releaseVersion != testPauseRelease || verified.metricsEpoch != testPauseEpoch || verified.keyID != testPauseKeyID) {
			t.Fatalf("successful verifier returned %#v", verified)
		}
	})
}
