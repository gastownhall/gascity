package productmetrics

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
)

// maxCanonicalPauseMessageBytes bounds the buffer canonicalPauseMessage
// allocates. The signed-pause body is capped at maxUploadResponseBytes before
// any field reaches the canonicalizer, and canonicalization only re-encodes
// fields parsed out of that body, so the capacity can never exceed the cap
// plus the domain prefix and JSON framing. One cap's worth of headroom keeps
// the assertion from tracking exact framing widths.
const maxCanonicalPauseMessageBytes = 2 * maxUploadResponseBytes

// TestSignedPauseBodyCapBoundsCanonicalAllocation pins the invariant that
// CodeQL's go/allocation-size-overflow alert on canonicalPauseMessage doubts:
// the capacity expression len(pauseDomainPrefix)+len(encoded) is bounded by
// the 4 KiB response cap, so the int addition cannot overflow.
//
// The alert's flagged operand is len(encoded) -- the marshaled canonical
// envelope. CodeQL treats it as attacker-scalable because it derives from the
// response body. It is not: verifySignedPauseWithKeySet rejects any body over
// maxUploadResponseBytes before canonicalization runs, and the only field that
// is not a fixed constant or a bounded integer (release_version) must equal
// the locally-prepared release version, which no responder controls.
func TestSignedPauseBodyCapBoundsCanonicalAllocation(t *testing.T) {
	if got := len(pauseDomainPrefix); got > maxUploadResponseBytes {
		t.Fatalf("domain prefix %d bytes exceeds the response cap %d", got, maxUploadResponseBytes)
	}

	// A body one byte over the cap must be refused before the allocation.
	oversized := make([]byte, maxUploadResponseBytes+1)
	for i := range oversized {
		oversized[i] = ' '
	}
	if _, err := verifySignedPauseWithKeySet(oversized, pauseExpectation{
		releaseVersion: testPauseRelease,
		metricsEpoch:   testPauseEpoch,
	}, pausePublicKeySet{}); err == nil {
		t.Fatal("verifySignedPauseWithKeySet accepted a body over maxUploadResponseBytes")
	}

	// Drive the largest canonical envelope that a capped body can describe:
	// a maximum-length key ID, the maximum JCS-safe epoch, and a release
	// version padded until the signed envelope fills the cap exactly.
	maxKeyID := strings.Repeat("k", maxPauseKeyIDBytes)
	if !validPauseKeyID(maxKeyID) {
		t.Fatalf("constructed key ID of %d bytes is not valid", len(maxKeyID))
	}

	_, privateKey := deterministicPauseKey()
	release := longestReleaseVersionWithinCap(t, maxKeyID, privateKey)

	message, err := canonicalPauseMessage(pauseUnsigned{
		SchemaVersion:  SchemaVersionV1,
		App:            AppGasCity,
		Action:         pauseAction,
		ReleaseVersion: release,
		MetricsEpoch:   maxJCSSafeInteger,
		KeyID:          maxKeyID,
	})
	if err != nil {
		t.Fatalf("canonicalPauseMessage on the maximal envelope: %v", err)
	}
	if len(message) > maxCanonicalPauseMessageBytes {
		t.Fatalf("canonical message is %d bytes, over the %d-byte bound", len(message), maxCanonicalPauseMessageBytes)
	}
	t.Logf("maximal canonical pause message: %d bytes (bound %d, cap %d)",
		len(message), maxCanonicalPauseMessageBytes, maxUploadResponseBytes)
}

// longestReleaseVersionWithinCap returns the longest valid release version
// whose signed envelope still fits inside maxUploadResponseBytes.
func longestReleaseVersionWithinCap(t *testing.T, keyID string, privateKey ed25519.PrivateKey) string {
	t.Helper()
	best := ""
	for pad := 0; pad <= maxUploadResponseBytes; pad++ {
		candidate := "0.31.0-" + strings.Repeat("a", pad)
		if pad == 0 {
			candidate = "0.31.0"
		}
		if !validPauseReleaseVersion(candidate) {
			continue
		}
		envelope := signedPauseEnvelope(candidate, maxJCSSafeInteger, keyID, privateKey)
		if len(envelope) > maxUploadResponseBytes {
			break
		}
		best = candidate
	}
	if best == "" {
		t.Fatal("no valid release version fits inside the response cap")
	}
	return best
}

// TestCanonicalPauseEncodingHasNoUnboundedField documents, field by field,
// why every input to the canonical marshal is bounded. If a future field is
// added without a bound, this test is where that shows up.
func TestCanonicalPauseEncodingHasNoUnboundedField(t *testing.T) {
	encoded, err := json.Marshal(struct {
		Action         string `json:"action"`
		App            string `json:"app"`
		KeyID          string `json:"key_id"`
		MetricsEpoch   uint64 `json:"metrics_epoch"`
		ReleaseVersion string `json:"release_version"`
		SchemaVersion  int    `json:"schema_version"`
	}{
		Action:         pauseAction,                                 // fixed constant
		App:            AppGasCity,                                  // fixed constant
		KeyID:          strings.Repeat("k", maxPauseKeyIDBytes),     // <= 64 bytes
		MetricsEpoch:   maxJCSSafeInteger,                           // <= 2^53-1
		ReleaseVersion: strings.Repeat("v", maxUploadResponseBytes), // <= body cap
		SchemaVersion:  SchemaVersionV1,                             // fixed constant
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Even with every variable field at its ceiling the encoding stays within
	// one cap plus framing, orders of magnitude below any int overflow.
	if len(encoded) > maxCanonicalPauseMessageBytes {
		t.Fatalf("worst-case canonical encoding is %d bytes, over the %d-byte bound", len(encoded), maxCanonicalPauseMessageBytes)
	}
	t.Logf("worst-case canonical encoding: %d bytes", len(encoded))
}

// TestCanonicalPauseMessageIsNotSelfBounding records a disproven hypothesis so
// a later reader does not re-walk it: that canonicalPauseMessage bounds its own
// allocation, which would make CodeQL's alert straightforwardly wrong.
//
// It does not. validatePauseUnsigned constrains the action, app, schema version,
// key ID and epoch, but it accepts any strict semver release version, and semver
// prerelease identifiers have no length limit. Called directly with a large
// release version the function happily builds a large message -- so the alert is
// not flagging something structurally impossible.
//
// The bound is real but it lives in the caller: verifySignedPauseWithKeySet
// refuses a body over maxUploadResponseBytes, and that is the only production
// path to this function. That makes the alert a false positive *as reached in
// production*, and it makes the cap a load-bearing invariant rather than an
// incidental one -- which is why the bound above is pinned by a test instead of
// left implicit. A second caller that skipped the cap would turn this alert
// true, and would break TestSignedPauseBodyCapBoundsCanonicalAllocation only if
// that caller were also routed through the capped entry point.
func TestCanonicalPauseMessageIsNotSelfBounding(t *testing.T) {
	const oversizedRelease = 64 * 1024

	release := "0.31.0-" + strings.Repeat("a", oversizedRelease)
	if !validPauseReleaseVersion(release) {
		t.Skipf("semver rejected a %d-byte prerelease; the hypothesis is moot", len(release))
	}

	message, err := canonicalPauseMessage(pauseUnsigned{
		SchemaVersion:  SchemaVersionV1,
		App:            AppGasCity,
		Action:         pauseAction,
		ReleaseVersion: release,
		MetricsEpoch:   testPauseEpoch,
		KeyID:          testPauseKeyID,
	})
	if err != nil {
		t.Fatalf("canonicalPauseMessage rejected an oversized release version: %v", err)
	}
	if len(message) <= maxCanonicalPauseMessageBytes {
		t.Fatalf("expected the unguarded call to exceed %d bytes, got %d -- if the function "+
			"gained its own bound, update this test and the pause.go:166 alert rationale",
			maxCanonicalPauseMessageBytes, len(message))
	}
	t.Logf("unguarded canonical message: %d bytes -- the bound is the caller's cap, not this function's",
		len(message))
}
