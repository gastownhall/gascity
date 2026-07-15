package productmetrics

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/Masterminds/semver/v3"
)

const (
	pauseAction        = "pause-through-metrics-epoch"
	pauseDomainPrefix  = "gascity-product-metrics-pause-v1\x00"
	maxPauseKeyIDBytes = 64
	maxJCSSafeInteger  = uint64(1<<53 - 1)
)

type pausePublicKeyEntry struct {
	id  string
	key ed25519.PublicKey
}

type pausePublicKeyCatalog func(func(pausePublicKeyEntry))

type pausePublicKeySet map[string]ed25519.PublicKey

// productionPausePublicKeyCatalog remains empty until an approved activation
// manifest supplies B3 key-custody evidence. Test keys are injected only into
// same-package tests and must never be added here.
func productionPausePublicKeyCatalog(func(pausePublicKeyEntry)) {}

type pauseUnsigned struct {
	SchemaVersion  int    `json:"schema_version"`
	App            string `json:"app"`
	Action         string `json:"action"`
	ReleaseVersion string `json:"release_version"`
	MetricsEpoch   uint64 `json:"metrics_epoch"`
	KeyID          string `json:"key_id"`
}

type pauseEnvelopeWire struct {
	SchemaVersion  int    `json:"schema_version"`
	App            string `json:"app"`
	Action         string `json:"action"`
	ReleaseVersion string `json:"release_version"`
	MetricsEpoch   uint64 `json:"metrics_epoch"`
	KeyID          string `json:"key_id"`
	Signature      string `json:"signature"`
}

type pauseExpectation struct {
	releaseVersion string
	metricsEpoch   uint64
}

type verifiedPause struct {
	releaseVersion string
	metricsEpoch   uint64
	keyID          string
}

func verifySignedPause(body []byte, expectation pauseExpectation, catalog pausePublicKeyCatalog) (verifiedPause, error) {
	keys, err := indexPausePublicKeyCatalog(catalog)
	if err != nil {
		return verifiedPause{}, err
	}
	return verifySignedPauseWithKeySet(body, expectation, keys)
}

func verifySignedPauseWithKeySet(body []byte, expectation pauseExpectation, keys pausePublicKeySet) (verifiedPause, error) {
	if len(body) == 0 || len(body) > maxUploadResponseBytes {
		return verifiedPause{}, fmt.Errorf("productmetrics: signed pause body is empty or oversized")
	}
	if !validPauseReleaseVersion(expectation.releaseVersion) || !validMetricsEpoch(expectation.metricsEpoch) {
		return verifiedPause{}, fmt.Errorf("productmetrics: signed pause expectation is invalid")
	}

	var envelope pauseEnvelopeWire
	if err := strictUnmarshalObject(body, &envelope, exactPauseEnvelopeField); err != nil {
		return verifiedPause{}, fmt.Errorf("productmetrics: signed pause JSON is invalid")
	}
	unsigned := pauseUnsigned{
		SchemaVersion:  envelope.SchemaVersion,
		App:            envelope.App,
		Action:         envelope.Action,
		ReleaseVersion: envelope.ReleaseVersion,
		MetricsEpoch:   envelope.MetricsEpoch,
		KeyID:          envelope.KeyID,
	}
	if err := validatePauseUnsigned(unsigned); err != nil {
		return verifiedPause{}, err
	}
	if unsigned.ReleaseVersion != expectation.releaseVersion || unsigned.MetricsEpoch != expectation.metricsEpoch {
		return verifiedPause{}, fmt.Errorf("productmetrics: signed pause does not match the upload permit")
	}

	publicKey, ok := keys[unsigned.KeyID]
	if !ok {
		return verifiedPause{}, fmt.Errorf("productmetrics: signed pause uses an unapproved key ID")
	}
	if envelope.Signature == "" {
		return verifiedPause{}, fmt.Errorf("productmetrics: signed pause has no signature")
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != envelope.Signature {
		return verifiedPause{}, fmt.Errorf("productmetrics: signed pause signature is not canonical base64url Ed25519")
	}
	message, err := canonicalPauseMessage(unsigned)
	if err != nil {
		return verifiedPause{}, err
	}
	if !ed25519.Verify(publicKey, message, signature) {
		return verifiedPause{}, fmt.Errorf("productmetrics: signed pause signature verification failed")
	}
	return verifiedPause{
		releaseVersion: unsigned.ReleaseVersion,
		metricsEpoch:   unsigned.MetricsEpoch,
		keyID:          unsigned.KeyID,
	}, nil
}

func canonicalPauseMessage(unsigned pauseUnsigned) ([]byte, error) {
	if err := validatePauseUnsigned(unsigned); err != nil {
		return nil, err
	}
	// This field order is RFC 8785 lexicographic key order. All accepted string
	// domains are ASCII tokens or canonical semver, so encoding/json emits the
	// same JSON string representation as JCS without a general-purpose
	// canonicalizer or an open map-shaped DTO.
	canonical := struct {
		Action         string `json:"action"`
		App            string `json:"app"`
		KeyID          string `json:"key_id"`
		MetricsEpoch   uint64 `json:"metrics_epoch"`
		ReleaseVersion string `json:"release_version"`
		SchemaVersion  int    `json:"schema_version"`
	}{
		Action:         unsigned.Action,
		App:            unsigned.App,
		KeyID:          unsigned.KeyID,
		MetricsEpoch:   unsigned.MetricsEpoch,
		ReleaseVersion: unsigned.ReleaseVersion,
		SchemaVersion:  unsigned.SchemaVersion,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("productmetrics: canonicalize signed pause: %w", err)
	}
	message := make([]byte, 0, len(pauseDomainPrefix)+len(encoded))
	message = append(message, pauseDomainPrefix...)
	message = append(message, encoded...)
	return message, nil
}

func validatePauseUnsigned(unsigned pauseUnsigned) error {
	if unsigned.SchemaVersion != SchemaVersionV1 {
		return fmt.Errorf("productmetrics: signed pause schema version must be %d", SchemaVersionV1)
	}
	if unsigned.App != AppGasCity {
		return fmt.Errorf("productmetrics: signed pause app must be %q", AppGasCity)
	}
	if unsigned.Action != pauseAction {
		return fmt.Errorf("productmetrics: signed pause action is invalid")
	}
	if !validPauseReleaseVersion(unsigned.ReleaseVersion) {
		return fmt.Errorf("productmetrics: signed pause release version is invalid")
	}
	if !validMetricsEpoch(unsigned.MetricsEpoch) {
		return fmt.Errorf("productmetrics: signed pause metrics epoch is outside the canonical JSON domain")
	}
	if !validPauseKeyID(unsigned.KeyID) {
		return fmt.Errorf("productmetrics: signed pause key ID is invalid")
	}
	return nil
}

func validPauseReleaseVersion(value string) bool {
	version, err := semver.StrictNewVersion(value)
	return err == nil && version.String() == value
}

func validMetricsEpoch(value uint64) bool {
	return value > 0 && value <= maxJCSSafeInteger
}

func validPauseKeyID(value string) bool {
	if len(value) == 0 || len(value) > maxPauseKeyIDBytes {
		return false
	}
	for i := range len(value) {
		character := value[i]
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func indexPausePublicKeyCatalog(catalog pausePublicKeyCatalog) (pausePublicKeySet, error) {
	if catalog == nil {
		return nil, fmt.Errorf("productmetrics: nil signed-pause key catalog")
	}
	keys := make(pausePublicKeySet)
	var catalogErr error
	catalog(func(entry pausePublicKeyEntry) {
		if catalogErr != nil {
			return
		}
		if !validPauseKeyID(entry.id) {
			catalogErr = fmt.Errorf("productmetrics: invalid signed-pause key ID")
			return
		}
		if len(entry.key) != ed25519.PublicKeySize {
			catalogErr = fmt.Errorf("productmetrics: signed-pause public key has invalid length")
			return
		}
		if _, exists := keys[entry.id]; exists {
			catalogErr = fmt.Errorf("productmetrics: duplicate signed-pause key ID")
			return
		}
		keys[entry.id] = append(ed25519.PublicKey(nil), entry.key...)
	})
	if catalogErr != nil {
		return nil, catalogErr
	}
	return keys, nil
}

func exactPauseEnvelopeField(field string) bool {
	switch field {
	case "schema_version", "app", "action", "release_version", "metrics_epoch", "key_id", "signature":
		return true
	default:
		return false
	}
}
