package citysource

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// Migration classes. Every legacy city lands in exactly one, and the counts are
// reported rather than reduced to a success/failure — "we could not tell" is a
// result, not an omission.
const (
	// ClassMapped: the legacy cursor is provable against the log, so it becomes
	// an epoch-1 watermark with a real anchor.
	ClassMapped = "mapped"
	// ClassUnknown: the log could not be read. No state is emitted and nothing
	// is guessed.
	ClassUnknown = "unknown"
	// ClassConflicted: the legacy cursor claims more than the log holds. This is
	// the in-memory high-water hazard made visible — the old exporter's `high`
	// map and its up-to-10s durable-cursor lag could both leave a cursor ahead
	// of any durable record.
	ClassConflicted = "conflicted"
	// ClassQuarantined: the cursor is within the log's range but the record at
	// that seq is gone or unreadable, so the cursor did not come from this log.
	ClassQuarantined = "quarantined"
)

// ActorUnknown is the only actor resolution this producer will ever record
// without a verified binding. The legacy source records a caller-supplied
// free-form actor string with no principal binding of any kind, so there is
// nothing to promote — and promoting it would manufacture an uploader identity
// out of an unauthenticated field.
const ActorUnknown = "unknown"

// MigrationEntry is the per-city outcome.
type MigrationEntry struct {
	City      string `json:"city"`
	SourceID  string `json:"source_id"`
	LegacySeq uint64 `json:"legacy_seq"`
	Head      uint64 `json:"head"`
	Class     string `json:"class"`
	Detail    string `json:"detail,omitempty"`
	// ActorResolution is always ActorUnknown unless a VERIFIED uploader
	// principal was supplied by the caller. It is never derived from the legacy
	// Event.Actor string.
	ActorResolution string `json:"actor_resolution"`
	// State is the migrated durable state, present only for ClassMapped.
	State *CityState `json:"state,omitempty"`
}

// MigrationReport is the signed account of a legacy high-water migration.
//
// It is supervisor-level, not per-city: the legacy artifact is one
// events-export-cursor.json covering every city the supervisor exported, and
// the counts are only meaningful as one closed set over that whole file.
type MigrationReport struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Entries     []MigrationEntry `json:"entries"`
	Mapped      int              `json:"mapped"`
	Unknown     int              `json:"unknown"`
	Conflicted  int              `json:"conflicted"`
	Quarantined int              `json:"quarantined"`
	// Digest is the sha256 of the canonical report with Digest/KeyID/Signature
	// zeroed.
	Digest    string `json:"digest"`
	KeyID     string `json:"key_id"`
	Signature []byte `json:"signature"`
}

// MigrationRequest describes one migration run.
type MigrationRequest struct {
	// LegacyCursors is the persisted per-city acked seq from the old exporter's
	// events-export-cursor.json.
	LegacyCursors map[string]uint64
	// VerifiedUploader, when non-empty, is a principal the CALLER has verified
	// out of band. Empty is the honest default and the only value the legacy
	// source can justify.
	VerifiedUploader string
}

// MigrationConfig supplies what the migration needs that a single Producer
// cannot: the legacy file spans every city, and each city has its own enrolled
// source identity.
type MigrationConfig struct {
	// SourceIDFor maps a local city name to its enrolled, rotation-stable
	// source id. A city with no enrollment returns "" and is classified unknown
	// rather than being given a made-up identity.
	SourceIDFor func(city string) string
	// Now stamps the report.
	Now time.Time
}

// Migrate classifies every legacy cursor against the live log and returns an
// unsigned report. Sign it with SignMigrationReport before it is treated as
// evidence.
//
// Migration never advances anything: a mapped city inherits exactly the seq the
// old exporter had durably acked, at epoch 1, with an anchor proving the log it
// came from. A city that cannot be proven gets no state at all, so the producer
// treats it as a fresh source rather than resuming on a cursor nobody can vouch
// for. That is the deliberate trade — a fresh source floors at head and misses
// history, which is visible and bounded, whereas trusting an unprovable cursor
// is silent and unbounded.
func Migrate(probe LineageProbe, cfg MigrationConfig, req MigrationRequest) (MigrationReport, error) {
	if probe == nil {
		return MigrationReport{}, fmt.Errorf("citysource: migration needs a LineageProbe to prove any cursor")
	}
	if cfg.SourceIDFor == nil {
		return MigrationReport{}, fmt.Errorf("citysource: migration needs SourceIDFor; identities are never invented")
	}
	actor := ActorUnknown
	if req.VerifiedUploader != "" {
		actor = req.VerifiedUploader
	}

	cities := make([]string, 0, len(req.LegacyCursors))
	for c := range req.LegacyCursors {
		cities = append(cities, c)
	}
	sort.Strings(cities)

	rep := MigrationReport{
		GeneratedAt: cfg.Now.UTC(),
		Entries:     make([]MigrationEntry, 0, len(cities)),
	}

	for _, city := range cities {
		legacy := req.LegacyCursors[city]
		sourceID := cfg.SourceIDFor(city)
		e := MigrationEntry{
			City:            city,
			SourceID:        sourceID,
			LegacySeq:       legacy,
			ActorResolution: actor,
		}
		if sourceID == "" {
			e.Class, e.Detail = ClassUnknown, "city has no enrolled source identity"
			rep.Entries = append(rep.Entries, e)
			continue
		}

		head, err := probe.Head(city)
		if err != nil {
			e.Class, e.Detail = ClassUnknown, fmt.Sprintf("head unreadable: %v", err)
			rep.Entries = append(rep.Entries, e)
			continue
		}
		e.Head = head

		switch {
		case legacy == 0:
			// Never acked. There is nothing to prove and nothing to lose: start
			// clean at epoch 1, watermark 0, no anchor.
			e.Class = ClassMapped
			e.State = &CityState{SourceID: sourceID, Epoch: 1}
		case legacy > head:
			e.Class = ClassConflicted
			e.Detail = fmt.Sprintf("legacy cursor %d exceeds log head %d", legacy, head)
		default:
			at, ok, err := probe.OfferAt(city, legacy)
			switch {
			case err != nil:
				e.Class, e.Detail = ClassUnknown, fmt.Sprintf("seq %d unreadable: %v", legacy, err)
			case !ok:
				e.Class = ClassQuarantined
				e.Detail = fmt.Sprintf("seq %d absent below head %d: the cursor did not come from this log", legacy, head)
			default:
				e.Class = ClassMapped
				e.State = &CityState{
					SourceID:   sourceID,
					Epoch:      1,
					Watermark:  legacy,
					AnchorSeq:  legacy,
					AnchorHash: SemanticHash(at),
				}
			}
		}
		rep.Entries = append(rep.Entries, e)
	}

	for _, e := range rep.Entries {
		switch e.Class {
		case ClassMapped:
			rep.Mapped++
		case ClassUnknown:
			rep.Unknown++
		case ClassConflicted:
			rep.Conflicted++
		case ClassQuarantined:
			rep.Quarantined++
		}
	}
	if got := rep.Mapped + rep.Unknown + rep.Conflicted + rep.Quarantined; got != len(rep.Entries) {
		return MigrationReport{}, fmt.Errorf("citysource: migration counts %d != %d entries; a city escaped classification", got, len(rep.Entries))
	}
	return rep, nil
}

// reportPreimage is the canonical bytes the digest and signature cover.
func reportPreimage(r MigrationReport) ([]byte, error) {
	r.Digest, r.KeyID, r.Signature = "", "", nil
	return canonical(r)
}

// SignMigrationReport stamps the report's digest and signs it. An unsigned
// report is a draft; only a signed one is evidence.
func SignMigrationReport(r MigrationReport, keyID string, priv ed25519.PrivateKey) (MigrationReport, error) {
	pre, err := reportPreimage(r)
	if err != nil {
		return MigrationReport{}, err
	}
	sum := sha256.Sum256(pre)
	r.Digest = "sha256:" + hex.EncodeToString(sum[:])
	r.KeyID = keyID
	// Sign the digest-stamped form so digest and signature cannot disagree.
	pre2, err := canonical(withoutSignature(r))
	if err != nil {
		return MigrationReport{}, err
	}
	r.Signature = ed25519.Sign(priv, pre2)
	return r, nil
}

// VerifyMigrationReport checks a report's digest and signature.
func VerifyMigrationReport(r MigrationReport, trusted map[string]ed25519.PublicKey) error {
	key, ok := trusted[r.KeyID]
	if !ok {
		return fmt.Errorf("%w: migration report signed by unknown key_id %q", ErrPolicyUnknown, r.KeyID)
	}
	pre, err := reportPreimage(r)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(pre)
	if want := "sha256:" + hex.EncodeToString(sum[:]); want != r.Digest {
		return fmt.Errorf("%w: migration report digest %s, recomputed %s", ErrPolicyMismatch, r.Digest, want)
	}
	pre2, err := canonical(withoutSignature(r))
	if err != nil {
		return err
	}
	if !ed25519.Verify(key, pre2, r.Signature) {
		return fmt.Errorf("%w: migration report signature does not verify", ErrPolicyUnknown)
	}
	return nil
}

func withoutSignature(r MigrationReport) MigrationReport {
	r.Signature = nil
	return r
}
