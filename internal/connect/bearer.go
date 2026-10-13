package connect

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Bearer and sentinel wire prefixes. The version digit moves when the MAC
// input changes, so a relay can tell formats apart without a key.
const (
	BearerPrefix   = "gcb1."
	SentinelPrefix = "gcs1."
)

// Domain tags keep the bearer and sentinel MACs disjoint.
const (
	bearerTag   = "gc.connect.bearer.v1"
	sentinelTag = "gc.connect.sentinel.v1"
)

// MinKeyLen is the shortest key K (DES-CN D7) MintBearer and Sentinel accept.
// The sentinel is itself a live bd serve token, so a short key would make it
// a guessable credential.
const MinKeyLen = 32

// macLen is the encoded length of a 32-byte HMAC-SHA256 in unpadded base64url.
const macLen = 43

var (
	sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	cityNameRe  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	macEncoding = base64.RawURLEncoding
)

// ValidSessionID reports whether id is a session id a bearer can carry:
// [A-Za-z0-9][A-Za-z0-9._-]{0,127}.
func ValidSessionID(id string) bool {
	return sessionIDRe.MatchString(id)
}

// ValidCityName reports whether name is a city name a bundle or descriptor
// can carry: [a-zA-Z0-9][a-zA-Z0-9._-]*, the supervisor's registry rule
// (internal/supervisor/registry.go). The city becomes a path component of the
// box's connect directory (§3.7) and of the default key path (D7), so it can
// never be empty, ".", "..", or hold a separator or control character.
func ValidCityName(name string) bool {
	return cityNameRe.MatchString(name)
}

// lp appends s to buf with a uvarint length prefix, so no two field lists
// that differ only at a boundary MAC alike.
func lp(buf []byte, s string) []byte {
	buf = binary.AppendUvarint(buf, uint64(len(s)))
	return append(buf, s...)
}

func mac(key, msg []byte) string {
	h := hmac.New(sha256.New, key)
	h.Write(msg) //nolint:errcheck // hash.Hash.Write never returns an error
	return macEncoding.EncodeToString(h.Sum(nil))
}

// MintBearer returns the per-box bearer for one session incarnation:
//
//	gcb1.<session id>.<generation>.<b64url(mac)>
//	mac = HMAC-SHA256(key, "gc.connect.bearer.v1" ‖ lp(city) ‖ lp(session id) ‖ lp(generation))
//
// The generation is MACed as its decimal string. The key must be at least
// MinKeyLen bytes, the city is required, the session id must satisfy ValidSessionID, and the generation
// must be at least 1.
func MintBearer(key []byte, city, sessionID string, generation int) (string, error) {
	switch {
	case len(key) < MinKeyLen:
		return "", fmt.Errorf("minting bearer: key is %d bytes, below %d", len(key), MinKeyLen)
	case city == "":
		return "", errors.New("minting bearer: city is empty")
	case !ValidSessionID(sessionID):
		return "", fmt.Errorf("minting bearer: session id %q is not [A-Za-z0-9][A-Za-z0-9._-]{0,127}", sessionID)
	case generation < 1:
		return "", fmt.Errorf("minting bearer for session %q: generation %d is below 1", sessionID, generation)
	}
	gen := strconv.Itoa(generation)
	msg := []byte(bearerTag)
	msg = lp(msg, city)
	msg = lp(msg, sessionID)
	msg = lp(msg, gen)
	return BearerPrefix + sessionID + "." + gen + "." + mac(key, msg), nil
}

// ParseBearer splits a bearer into its session id and generation. It takes
// the MAC and then the generation from the right, so session ids containing
// dots parse. It checks the form only; verifying the MAC needs the key.
func ParseBearer(s string) (sessionID string, generation int, ok bool) {
	rest, found := strings.CutPrefix(s, BearerPrefix)
	if !found {
		return "", 0, false
	}
	i := strings.LastIndexByte(rest, '.')
	if i < 0 || !validMAC(rest[i+1:]) {
		return "", 0, false
	}
	rest = rest[:i]
	i = strings.LastIndexByte(rest, '.')
	if i < 0 {
		return "", 0, false
	}
	gen, ok := parseGeneration(rest[i+1:])
	if !ok {
		return "", 0, false
	}
	id := rest[:i]
	if !ValidSessionID(id) {
		return "", 0, false
	}
	return id, gen, true
}

// Sentinel returns the token-file sentinel for a city:
//
//	gcs1.<b64url(HMAC-SHA256(key, "gc.connect.sentinel.v1" ‖ lp(city)))>
//
// It is never handed to a box; it keeps the rendered token file non-empty
// and serves as the doctor's canary. Because every non-empty token-file line
// is a live bd serve credential, the key must be at least MinKeyLen bytes and
// the city is required.
func Sentinel(key []byte, city string) (string, error) {
	switch {
	case len(key) < MinKeyLen:
		return "", fmt.Errorf("deriving sentinel: key is %d bytes, below %d", len(key), MinKeyLen)
	case city == "":
		return "", errors.New("deriving sentinel: city is empty")
	}
	return SentinelPrefix + mac(key, lp([]byte(sentinelTag), city)), nil
}

// validMAC accepts exactly the canonical unpadded base64url of 32 bytes.
func validMAC(s string) bool {
	if len(s) != macLen {
		return false
	}
	raw, err := macEncoding.Strict().DecodeString(s)
	return err == nil && len(raw) == sha256.Size
}

// parseGeneration accepts a canonical positive decimal: no sign, no leading
// zero, within int.
func parseGeneration(s string) (int, bool) {
	if s == "" || s[0] == '0' {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}
