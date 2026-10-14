package opjournal

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Domain tags keep each hash scheme disjoint from every other one.
const (
	opIDTag     = "gascity/op-id/v1"
	factIDTag   = "gascity/fact-id/v1"
	opDigestTag = "gascity/op-digest/v1"
)

// Field limits.
const (
	maxConversationLen = 200
	maxPrincipalLen    = 256
	maxClientKeyLen    = 512
	maxActorLen        = 64
	maxFreeTextLen     = 512
	maxTokenLen        = 128
)

// opIDInfix and factIDInfix separate the mint prefix from the hash. They keep
// pinned ids out of SQLite's numeric sequence parsing.
const (
	opIDInfix   = "-op-"
	factIDInfix = "-fact-"
)

var (
	opIDRe   = regexp.MustCompile(`^[^\s,"@#]+-op-[a-z2-7]{26}$`)
	actorRe  = regexp.MustCompile(`^[a-z0-9._:/-]+$`)
	tokenRe  = regexp.MustCompile(`^[a-z0-9_.:/-]+$`)
	lowerB32 = base32.StdEncoding.WithPadding(base32.NoPadding)
)

// lp appends s to buf with a uvarint length prefix, so no two field lists
// that differ only at a boundary hash alike.
func lp(buf []byte, s string) []byte {
	buf = binary.AppendUvarint(buf, uint64(len(s)))
	return append(buf, s...)
}

// hash16 is the lowercased unpadded base32 of the first 16 bytes of
// sha256(lp(tag) lp(field)...): 26 characters of [a-z2-7].
func hash16(tag string, fields ...string) string {
	buf := lp(nil, tag)
	for _, f := range fields {
		buf = lp(buf, f)
	}
	sum := sha256.Sum256(buf)
	return strings.ToLower(lowerB32.EncodeToString(sum[:16]))
}

// DeriveID returns the pinned op id for (conversation, principal, clientKey)
// in the namespace prefix: <prefix>-op-<hash16>. prefix is the mint prefix of
// the store that holds the conversation row; it is never parsed from the
// conversation id.
func DeriveID(prefix, conversation, principal, clientKey string) (string, error) {
	if err := validatePrefix(prefix); err != nil {
		return "", err
	}
	if err := validateConversation(conversation); err != nil {
		return "", err
	}
	if err := validateText("principal", principal, maxPrincipalLen); err != nil {
		return "", err
	}
	if err := validateText("client key", clientKey, maxClientKeyLen); err != nil {
		return "", err
	}
	return prefix + opIDInfix + opIDSuffix(conversation, principal, clientKey), nil
}

func opIDSuffix(conversation, principal, clientKey string) string {
	return hash16(opIDTag, conversation, principal, clientKey)
}

// ValidateClientKey refuses a client key Propose would refuse: empty, longer
// than 512 bytes, not UTF-8, with a control character, or with leading or
// trailing space. Producers check a supplied key with it before proposing.
func ValidateClientKey(key string) error {
	return validateText("client key", key, maxClientKeyLen)
}

// InterruptKey is the client key of an interrupt that targets one turn of one
// session: stop:<gen>:<turn>. Two stops of the same turn collapse into one op.
// A producer without a turn uses its own unique key instead, because a
// session-only key would fold every later stop into the first.
func InterruptKey(gen int64, turn string) (string, error) {
	if gen < 1 {
		return "", invalidf("interrupt key: generation %d must be >= 1", gen)
	}
	if err := validateText("interrupt key turn", turn, maxClientKeyLen); err != nil {
		return "", err
	}
	return composedKey("interrupt key", "stop:"+strconv.FormatInt(gen, 10)+":"+turn)
}

// RespondKey is the client key of a respond to one interaction of one
// session: respond:<gen>:<request_id>. The generation is part of the key
// because some runtimes derive request ids from content. It is unused: in O0
// respond ops are keyed by the client key, and OJ-16 keys them by the
// interaction occurrence.
func RespondKey(gen int64, requestID string) (string, error) {
	if gen < 1 {
		return "", invalidf("respond key: generation %d must be >= 1", gen)
	}
	if err := validateText("respond key request id", requestID, maxClientKeyLen); err != nil {
		return "", err
	}
	return composedKey("respond key", "respond:"+strconv.FormatInt(gen, 10)+":"+requestID)
}

// composedKey returns key when it is a client key Propose accepts, so a
// helper never hands out a key the store then refuses.
func composedKey(what, key string) (string, error) {
	if err := validateText(what, key, maxClientKeyLen); err != nil {
		return "", err
	}
	return key, nil
}

// validatePrefix refuses an empty prefix or one that would make the id
// ambiguous: whitespace, @, #, a dot, or a trailing hyphen.
func validatePrefix(prefix string) error {
	switch {
	case prefix == "":
		return invalidf("id prefix is empty")
	case strings.HasSuffix(prefix, "-"):
		return invalidf("id prefix %q ends with '-'", prefix)
	case strings.ContainsAny(prefix, "@#.,\""):
		return invalidf("id prefix %q contains a reserved character", prefix)
	case strings.IndexFunc(prefix, unicode.IsSpace) >= 0 || hasControl(prefix):
		return invalidf("id prefix %q contains whitespace", prefix)
	}
	return nil
}

// validateConversation refuses conversation ids that cannot sit inside a
// label, a lease or an id: empty, too long, whitespace, control characters,
// '@', '#', ',' or '"'.
func validateConversation(conv string) error {
	switch {
	case conv == "":
		return invalidf("conversation is empty")
	case len(conv) > maxConversationLen:
		return invalidf("conversation is %d bytes, max %d", len(conv), maxConversationLen)
	case !utf8.ValidString(conv):
		return invalidf("conversation %q is not valid UTF-8", conv)
	case strings.ContainsAny(conv, "@#,\""):
		return invalidf("conversation %q contains a reserved character", conv)
	case strings.IndexFunc(conv, unicode.IsSpace) >= 0 || hasControl(conv):
		return invalidf("conversation %q contains whitespace or control characters", conv)
	}
	return nil
}

// validateText refuses an empty, oversized, non-UTF-8 or control-bearing
// value, or one with leading or trailing space. Values are refused, never
// trimmed.
func validateText(what, v string, maxLen int) error {
	switch {
	case v == "":
		return invalidf("%s is empty", what)
	case len(v) > maxLen:
		return invalidf("%s is %d bytes, max %d", what, len(v), maxLen)
	case !utf8.ValidString(v):
		return invalidf("%s is not valid UTF-8", what)
	case hasControl(v):
		return invalidf("%s contains control characters", what)
	case strings.TrimSpace(v) != v:
		return invalidf("%s %q has leading or trailing space", what, v)
	}
	return nil
}

// validateOptionalText is validateText for a free-text field that may be
// empty.
func validateOptionalText(what, v string) error {
	if v == "" {
		return nil
	}
	return validateText(what, v, maxFreeTextLen)
}

// validateUpdateText refuses a value an update would not store as a string.
// Fenced updates reach bd as --set-metadata k=v, and bd types v by its
// spelling: "null" reads back as "", and a number spelling can read back
// normalized. Text written by an update must therefore not be a JSON null,
// boolean or number.
func validateUpdateText(what, v string) error {
	var scalar any
	if err := json.Unmarshal([]byte(v), &scalar); err != nil {
		return nil
	}
	switch scalar.(type) {
	case nil, bool, float64:
		return invalidf("%s %q reads as a JSON %s once stored; quote or reword it", what, v, jsonKind(scalar))
	}
	return nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	default:
		return "number"
	}
}

// validateToken refuses a value outside the token alphabet [a-z0-9_.:/-].
func validateToken(what, v string) error {
	if len(v) > maxTokenLen || !tokenRe.MatchString(v) {
		return invalidf("%s %q is not a token", what, v)
	}
	return nil
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// validOpID reports whether id has the shape of a pinned op id.
func validOpID(id string) bool { return opIDRe.MatchString(id) }
