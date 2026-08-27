package nudgequeue

import (
	"bytes"
	"strings"
	"testing"
)

func TestRestoreAnchorCodecRoundTripIsCanonical(t *testing.T) {
	anchor := RestoreAnchor{
		Store:                   CommandStoreBinding{StoreUUID: "123e4567-e89b-12d3-a456-426614174000", RestoreEpoch: 7},
		HighestAcceptedRevision: 42,
		HighestAcceptedSequence: 40,
	}
	wire, err := EncodeRestoreAnchor(anchor)
	if err != nil {
		t.Fatalf("EncodeRestoreAnchor: %v", err)
	}
	original := append([]byte(nil), wire...)
	const want = "{\"version\":1,\"store\":{\"store_uuid\":\"123e4567-e89b-12d3-a456-426614174000\",\"restore_epoch\":7},\"highest_accepted_revision\":42,\"highest_accepted_sequence\":40,\"checksum_sha256\":\"284525a2a97548323231587ff1ba445955f83da1bc2b0a6e48477461a536a579\"}\n"
	if got := string(wire); got != want {
		t.Fatalf("encoded record = %q, want exact golden %q", got, want)
	}
	decoded, err := DecodeRestoreAnchor(wire)
	if err != nil {
		t.Fatalf("DecodeRestoreAnchor: %v", err)
	}
	if decoded != anchor {
		t.Fatalf("decoded = %#v, want %#v", decoded, anchor)
	}
	if !bytes.Equal(wire, original) {
		t.Fatalf("DecodeRestoreAnchor mutated input: got %q, want %q", wire, original)
	}
	reencoded, err := EncodeRestoreAnchor(decoded)
	if err != nil {
		t.Fatalf("EncodeRestoreAnchor(decoded): %v", err)
	}
	if !bytes.Equal(reencoded, wire) {
		t.Fatalf("re-encoded = %q, want %q", reencoded, wire)
	}
}

func TestDecodeRestoreAnchorRejectsUntrustedWire(t *testing.T) {
	valid, err := EncodeRestoreAnchor(RestoreAnchor{
		Store:                   CommandStoreBinding{StoreUUID: "123e4567-e89b-12d3-a456-426614174000", RestoreEpoch: 2},
		HighestAcceptedRevision: 9,
		HighestAcceptedSequence: 5,
	})
	if err != nil {
		t.Fatalf("EncodeRestoreAnchor fixture: %v", err)
	}
	object := strings.TrimSuffix(string(valid), "\n")
	checksum := checksumFromRestoreAnchorWire(t, valid)
	legalPadding := append([]byte(" \t\r\n"), valid...)
	legalPadding = append(legalPadding, " \t\r\n"...)
	if _, err := DecodeRestoreAnchor(legalPadding); err != nil {
		t.Fatalf("DecodeRestoreAnchor rejected legal JSON whitespace: %v", err)
	}
	tests := []struct {
		name string
		wire []byte
	}{
		{name: "empty", wire: nil},
		{name: "oversized", wire: bytes.Repeat([]byte("x"), MaxRestoreAnchorBytes+1)},
		{name: "invalid UTF-8", wire: []byte{0xff}},
		{name: "vertical tab prefix", wire: append([]byte{'\v'}, valid...)},
		{name: "vertical tab suffix", wire: append(append([]byte(nil), valid...), '\v')},
		{name: "form feed prefix", wire: append([]byte{'\f'}, valid...)},
		{name: "form feed suffix", wire: append(append([]byte(nil), valid...), '\f')},
		{name: "NBSP prefix", wire: append([]byte("\u00a0"), valid...)},
		{name: "NBSP suffix", wire: append(append([]byte(nil), valid...), "\u00a0"...)},
		{name: "Unicode em space prefix", wire: append([]byte("\u2003"), valid...)},
		{name: "Unicode em space suffix", wire: append(append([]byte(nil), valid...), "\u2003"...)},
		{name: "non-object", wire: []byte("[]")},
		{name: "malformed", wire: []byte("{")},
		{name: "trailing value", wire: append(append([]byte(nil), valid...), []byte("{}")...)},
		{name: "unknown top field", wire: []byte(strings.TrimSuffix(object, "}") + `,"extra":true}`)},
		{name: "case variant top field", wire: []byte(strings.Replace(object, `"version"`, `"Version"`, 1))},
		{name: "duplicate top field", wire: []byte(strings.Replace(object, `"version":1`, `"version":1,"version":1`, 1))},
		{name: "unknown nested field", wire: []byte(strings.Replace(object, `"restore_epoch":2`, `"restore_epoch":2,"extra":true`, 1))},
		{name: "case variant nested field", wire: []byte(strings.Replace(object, `"restore_epoch"`, `"Restore_Epoch"`, 1))},
		{name: "duplicate nested field", wire: []byte(strings.Replace(object, `"restore_epoch":2`, `"restore_epoch":2,"restore_epoch":2`, 1))},
		{name: "missing field", wire: []byte(strings.Replace(object, `,"highest_accepted_sequence":5`, "", 1))},
		{name: "unsupported version", wire: []byte(strings.Replace(object, `"version":1`, `"version":2`, 1))},
		{name: "invalid UUID", wire: []byte(strings.Replace(object, "123e4567-e89b-12d3-a456-426614174000", "123E4567-e89b-12d3-a456-426614174000", 1))},
		{name: "zero epoch", wire: []byte(strings.Replace(object, `"restore_epoch":2`, `"restore_epoch":0`, 1))},
		{name: "sequence exceeds revision", wire: []byte(strings.Replace(object, `"highest_accepted_sequence":5`, `"highest_accepted_sequence":10`, 1))},
		{name: "changed checksum coverage", wire: []byte(strings.Replace(object, `"highest_accepted_revision":9`, `"highest_accepted_revision":8`, 1))},
		{name: "uppercase checksum", wire: []byte(strings.Replace(object, checksum, strings.ToUpper(checksum), 1))},
		{name: "short checksum", wire: []byte(strings.Replace(object, checksum, "00", 1))},
		{name: "unpaired surrogate", wire: []byte(strings.Replace(object, checksum, `\ud800`, 1))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]byte(nil), tc.wire...)
			if decoded, err := DecodeRestoreAnchor(tc.wire); err == nil {
				t.Fatalf("DecodeRestoreAnchor(%q) = %#v, nil error", tc.wire, decoded)
			}
			if !bytes.Equal(tc.wire, before) {
				t.Fatalf("DecodeRestoreAnchor mutated input: got %q, want %q", tc.wire, before)
			}
		})
	}
}

func TestEncodeRestoreAnchorRejectsInvalidValues(t *testing.T) {
	tests := []RestoreAnchor{
		{},
		{Store: CommandStoreBinding{StoreUUID: "123e4567-e89b-12d3-a456-426614174000"}},
		{Store: CommandStoreBinding{StoreUUID: "123e4567-e89b-12d3-a456-426614174000", RestoreEpoch: 1}, HighestAcceptedRevision: 1, HighestAcceptedSequence: 2},
	}
	for _, anchor := range tests {
		if wire, err := EncodeRestoreAnchor(anchor); err == nil {
			t.Errorf("EncodeRestoreAnchor(%#v) = %q, nil error", anchor, wire)
		}
	}
}

func FuzzDecodeRestoreAnchor(f *testing.F) {
	valid, err := EncodeRestoreAnchor(RestoreAnchor{Store: CommandStoreBinding{StoreUUID: "123e4567-e89b-12d3-a456-426614174000", RestoreEpoch: 1}, HighestAcceptedRevision: 3, HighestAcceptedSequence: 2})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Add([]byte("{}"))
	f.Add(bytes.Repeat([]byte("x"), MaxRestoreAnchorBytes+1))
	f.Add([]byte(`{"version":1,"version":1}`))
	f.Fuzz(func(t *testing.T, wire []byte) {
		anchor, err := DecodeRestoreAnchor(wire)
		if err != nil {
			return
		}
		reencoded, err := EncodeRestoreAnchor(anchor)
		if err != nil {
			t.Fatalf("accepted anchor cannot encode: %#v: %v", anchor, err)
		}
		decodedAgain, err := DecodeRestoreAnchor(reencoded)
		if err != nil || decodedAgain != anchor {
			t.Fatalf("canonical round trip = (%#v, %v), want %#v", decodedAgain, err, anchor)
		}
	})
}

func checksumFromRestoreAnchorWire(t *testing.T, wire []byte) string {
	t.Helper()
	const marker = `"checksum_sha256":"`
	start := bytes.Index(wire, []byte(marker))
	if start < 0 {
		t.Fatalf("fixture has no checksum: %q", wire)
	}
	start += len(marker)
	end := bytes.IndexByte(wire[start:], '"')
	if end < 0 {
		t.Fatalf("fixture has unterminated checksum: %q", wire)
	}
	return string(wire[start : start+end])
}
