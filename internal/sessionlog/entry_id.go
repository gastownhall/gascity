package sessionlog

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"
)

type stableSyntheticEntryIDSource struct {
	prefix       string
	recordDigest [sha256.Size]byte
	occurrence   uint64
}

type stableSyntheticEntryIDSequence struct {
	prefix      string
	occurrences map[[sha256.Size]byte]uint64
}

// newStableSyntheticEntryIDSource compacts and hashes one provider record once.
// Reuse the result when a single record emits multiple normalized entries.
func newStableSyntheticEntryIDSource(prefix string, raw []byte) stableSyntheticEntryIDSource {
	payload := bytes.TrimSpace(raw)
	var compact bytes.Buffer
	if json.Compact(&compact, payload) == nil {
		payload = compact.Bytes()
	}
	return stableSyntheticEntryIDSource{
		prefix:       strings.TrimSpace(prefix),
		recordDigest: sha256.Sum256(payload),
	}
}

func newStableSyntheticEntryIDSequence(prefix string) *stableSyntheticEntryIDSequence {
	return &stableSyntheticEntryIDSequence{
		prefix:      strings.TrimSpace(prefix),
		occurrences: make(map[[sha256.Size]byte]uint64),
	}
}

// ForRecord returns a source whose occurrence discriminator is stable as more
// records are appended. The first occurrence deliberately retains the
// content-only ID used before repeated provider records were supported.
func (s *stableSyntheticEntryIDSequence) ForRecord(raw []byte) stableSyntheticEntryIDSource {
	source := newStableSyntheticEntryIDSource(s.prefix, raw)
	source.occurrence = s.occurrences[source.recordDigest]
	s.occurrences[source.recordDigest] = source.occurrence + 1
	return source
}

// ID derives a cursor-safe ID from the record digest plus a discriminator for
// one normalized part. The full digest keeps collision risk independent of
// transcript length while making multi-part records O(record size + parts).
func (s stableSyntheticEntryIDSource) ID(part string) string {
	h := sha256.New()
	_, _ = h.Write(s.recordDigest[:])
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(part))
	if s.occurrence > 0 {
		var occurrence [8]byte
		binary.BigEndian.PutUint64(occurrence[:], s.occurrence)
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(occurrence[:])
	}
	return s.prefix + "-" + hex.EncodeToString(h.Sum(nil))
}

// RawRecordID identifies the provider record before it is expanded into one
// or more normalized entries. It lets raw consumers emit each source frame
// once without collapsing genuinely repeated byte-identical records.
func (s stableSyntheticEntryIDSource) RawRecordID() string {
	return s.ID("raw-record")
}

// stableSyntheticEntryID derives a cursor-safe ID for a provider record that
// emits one normalized entry.
func stableSyntheticEntryID(prefix string, raw []byte, part string) string {
	return newStableSyntheticEntryIDSource(prefix, raw).ID(part)
}
