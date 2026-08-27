package nudgequeue

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"
)

const (
	// MaxRestoreAnchorBytes bounds bytes accepted by the restore-anchor decoder.
	MaxRestoreAnchorBytes                = 4 << 10
	restoreAnchorWireVersionV1    uint32 = 1
	restoreAnchorChecksumDomainV1        = "gascity.nudge-command.restore-anchor.v1"
)

type restoreAnchorWire struct {
	Version                 uint32                 `json:"version"`
	Store                   restoreAnchorStoreWire `json:"store"`
	HighestAcceptedRevision uint64                 `json:"highest_accepted_revision"`
	HighestAcceptedSequence uint64                 `json:"highest_accepted_sequence"`
	ChecksumSHA256          string                 `json:"checksum_sha256"`
}

type restoreAnchorStoreWire struct {
	StoreUUID    string `json:"store_uuid"`
	RestoreEpoch uint64 `json:"restore_epoch"`
}

// EncodeRestoreAnchor returns one canonical, checksummed, newline-terminated record.
func EncodeRestoreAnchor(anchor RestoreAnchor) ([]byte, error) {
	if !validRestoreAnchor(anchor) {
		return nil, errors.New("encoding restore anchor: invalid anchor")
	}
	checksum, err := computeRestoreAnchorChecksum(anchor)
	if err != nil {
		return nil, fmt.Errorf("encoding restore anchor: %w", err)
	}
	wire, err := json.Marshal(restoreAnchorWire{
		Version:                 restoreAnchorWireVersionV1,
		Store:                   restoreAnchorStoreWire(anchor.Store),
		HighestAcceptedRevision: anchor.HighestAcceptedRevision,
		HighestAcceptedSequence: anchor.HighestAcceptedSequence,
		ChecksumSHA256:          checksum,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding restore anchor: %w", err)
	}
	wire = append(wire, '\n')
	if len(wire) > MaxRestoreAnchorBytes {
		return nil, fmt.Errorf("encoding restore anchor: record exceeds %d bytes", MaxRestoreAnchorBytes)
	}
	return wire, nil
}

// DecodeRestoreAnchor totally validates one bounded v1 restore-anchor record without mutation.
func DecodeRestoreAnchor(wire []byte) (RestoreAnchor, error) {
	if len(wire) == 0 {
		return RestoreAnchor{}, errors.New("decoding restore anchor: record is empty")
	}
	if len(wire) > MaxRestoreAnchorBytes {
		return RestoreAnchor{}, fmt.Errorf("decoding restore anchor: record exceeds %d bytes", MaxRestoreAnchorBytes)
	}
	if !utf8.Valid(wire) {
		return RestoreAnchor{}, errors.New("decoding restore anchor: record is not valid UTF-8")
	}
	trimmed := bytes.Trim(wire, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return RestoreAnchor{}, errors.New("decoding restore anchor: record is not a JSON object")
	}
	if err := validateRestoreAnchorUnicodeEscapes(trimmed); err != nil {
		return RestoreAnchor{}, fmt.Errorf("decoding restore anchor: invalid JSON string: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	anchor, checksum, err := decodeRestoreAnchorObject(decoder)
	if err != nil {
		return RestoreAnchor{}, fmt.Errorf("decoding restore anchor: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return RestoreAnchor{}, errors.New("decoding restore anchor: trailing JSON value")
	}
	if !validRestoreAnchor(anchor) {
		return RestoreAnchor{}, errors.New("decoding restore anchor: invalid anchor")
	}
	want, err := computeRestoreAnchorChecksum(anchor)
	if err != nil {
		return RestoreAnchor{}, fmt.Errorf("decoding restore anchor: %w", err)
	}
	if err := validateRestoreAnchorChecksum(checksum, want); err != nil {
		return RestoreAnchor{}, fmt.Errorf("decoding restore anchor: %w", err)
	}
	return anchor, nil
}

func decodeRestoreAnchorObject(decoder *json.Decoder) (RestoreAnchor, string, error) {
	if err := expectRestoreAnchorDelimiter(decoder, '{'); err != nil {
		return RestoreAnchor{}, "", errors.New("record is not a JSON object")
	}
	var anchor RestoreAnchor
	var checksum string
	seen := make(map[string]bool, 5)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return RestoreAnchor{}, "", errors.New("invalid JSON object")
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return RestoreAnchor{}, "", errors.New("duplicate or invalid JSON field")
		}
		seen[name] = true
		switch name {
		case "version":
			version, err := decodeRestoreAnchorUint32(decoder)
			if err != nil || version != restoreAnchorWireVersionV1 {
				return RestoreAnchor{}, "", errors.New("unsupported wire version")
			}
		case "store":
			store, err := decodeRestoreAnchorStore(decoder)
			if err != nil {
				return RestoreAnchor{}, "", err
			}
			anchor.Store = store
		case "highest_accepted_revision":
			value, err := decodeRestoreAnchorUint64(decoder)
			if err != nil {
				return RestoreAnchor{}, "", errors.New("invalid highest accepted revision")
			}
			anchor.HighestAcceptedRevision = value
		case "highest_accepted_sequence":
			value, err := decodeRestoreAnchorUint64(decoder)
			if err != nil {
				return RestoreAnchor{}, "", errors.New("invalid highest accepted sequence")
			}
			anchor.HighestAcceptedSequence = value
		case "checksum_sha256":
			token, err := decoder.Token()
			var ok bool
			checksum, ok = token.(string)
			if err != nil || !ok {
				return RestoreAnchor{}, "", errors.New("invalid checksum")
			}
		default:
			return RestoreAnchor{}, "", errors.New("unknown or noncanonical JSON field")
		}
	}
	if err := expectRestoreAnchorDelimiter(decoder, '}'); err != nil || len(seen) != 5 {
		return RestoreAnchor{}, "", errors.New("required field is missing")
	}
	return anchor, checksum, nil
}

func decodeRestoreAnchorStore(decoder *json.Decoder) (CommandStoreBinding, error) {
	if err := expectRestoreAnchorDelimiter(decoder, '{'); err != nil {
		return CommandStoreBinding{}, errors.New("store is not a JSON object")
	}
	var store CommandStoreBinding
	seen := make(map[string]bool, 2)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return CommandStoreBinding{}, errors.New("invalid store")
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return CommandStoreBinding{}, errors.New("duplicate or invalid store field")
		}
		seen[name] = true
		switch name {
		case "store_uuid":
			token, err := decoder.Token()
			var ok bool
			store.StoreUUID, ok = token.(string)
			if err != nil || !ok {
				return CommandStoreBinding{}, errors.New("invalid store UUID")
			}
		case "restore_epoch":
			value, err := decodeRestoreAnchorUint64(decoder)
			if err != nil {
				return CommandStoreBinding{}, errors.New("invalid restore epoch")
			}
			store.RestoreEpoch = value
		default:
			return CommandStoreBinding{}, errors.New("unknown or noncanonical store field")
		}
	}
	if err := expectRestoreAnchorDelimiter(decoder, '}'); err != nil || len(seen) != 2 {
		return CommandStoreBinding{}, errors.New("required store field is missing")
	}
	return store, nil
}

func expectRestoreAnchorDelimiter(decoder *json.Decoder, want json.Delim) error {
	token, err := decoder.Token()
	if err != nil || token != want {
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func decodeRestoreAnchorUint32(decoder *json.Decoder) (uint32, error) {
	value, err := decodeRestoreAnchorUint64(decoder)
	if err != nil || value > uint64(^uint32(0)) {
		return 0, errors.New("invalid unsigned integer")
	}
	return uint32(value), nil
}

func decodeRestoreAnchorUint64(decoder *json.Decoder) (uint64, error) {
	token, err := decoder.Token()
	number, ok := token.(json.Number)
	if err != nil || !ok {
		return 0, errors.New("invalid unsigned integer")
	}
	return strconv.ParseUint(string(number), 10, 64)
}

func computeRestoreAnchorChecksum(anchor RestoreAnchor) (string, error) {
	payload, err := json.Marshal(struct {
		Domain                  string                 `json:"domain"`
		Version                 uint32                 `json:"version"`
		Store                   restoreAnchorStoreWire `json:"store"`
		HighestAcceptedRevision uint64                 `json:"highest_accepted_revision"`
		HighestAcceptedSequence uint64                 `json:"highest_accepted_sequence"`
	}{
		Domain:                  restoreAnchorChecksumDomainV1,
		Version:                 restoreAnchorWireVersionV1,
		Store:                   restoreAnchorStoreWire(anchor.Store),
		HighestAcceptedRevision: anchor.HighestAcceptedRevision,
		HighestAcceptedSequence: anchor.HighestAcceptedSequence,
	})
	if err != nil {
		return "", fmt.Errorf("computing checksum payload: %w", err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func validateRestoreAnchorChecksum(got, want string) error {
	if len(got) != sha256.Size*2 {
		return errors.New("checksum is not a canonical SHA-256 digest")
	}
	gotBytes, err := hex.DecodeString(got)
	if err != nil || hex.EncodeToString(gotBytes) != got {
		return errors.New("checksum is not a canonical SHA-256 digest")
	}
	wantBytes, err := hex.DecodeString(want)
	if err != nil || subtle.ConstantTimeCompare(gotBytes, wantBytes) != 1 {
		return errors.New("checksum does not match anchor fields")
	}
	return nil
}

func validateRestoreAnchorUnicodeEscapes(wire []byte) error {
	for i := 0; i < len(wire); i++ {
		if wire[i] != '\\' || i+1 >= len(wire) || wire[i+1] != 'u' {
			continue
		}
		high, ok := restoreAnchorCodeUnit(wire, i+2)
		if !ok {
			return errors.New("invalid Unicode escape")
		}
		i += 5
		if high < 0xd800 || high > 0xdfff {
			continue
		}
		if high >= 0xdc00 || i+6 >= len(wire) || wire[i+1] != '\\' || wire[i+2] != 'u' {
			return errors.New("unpaired surrogate")
		}
		low, ok := restoreAnchorCodeUnit(wire, i+3)
		if !ok || low < 0xdc00 || low > 0xdfff {
			return errors.New("unpaired surrogate")
		}
		i += 6
	}
	return nil
}

func restoreAnchorCodeUnit(wire []byte, start int) (uint16, bool) {
	if start+4 > len(wire) {
		return 0, false
	}
	var value uint16
	for _, b := range wire[start : start+4] {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value |= uint16(b - '0')
		case b >= 'a' && b <= 'f':
			value |= uint16(b-'a') + 10
		case b >= 'A' && b <= 'F':
			value |= uint16(b-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}
