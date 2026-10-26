package eventfact

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/ledgerfeed"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// Checkpoint row shape.
const (
	// LabelProjector marks the projector's checkpoint row.
	LabelProjector = "gc:event-projector"
	// CheckpointSchemaRev is the checkpoint-row schema revision this build
	// writes. It is apart from FactSchemaRev, so a fact-schema bump leaves
	// the checkpoint readable; a reader reads every revision from 1 up to it.
	CheckpointSchemaRev = 1

	checkpointFamily = "evproj"
	checkpointTag    = "gascity/event-projector/v1"
	maxCityLen       = 200

	// Fingerprint field limits, in bytes. The journal's issue_id and op are
	// VARCHAR(255) and VARCHAR(32), which count characters, and its ts is a
	// DATETIME that bd renders as UTC RFC 3339, 30 bytes at most.
	maxLastIssueLen = 255 * utf8.UTFMax
	maxLastOpLen    = 32 * utf8.UTFMax
	maxLastTSLen    = 64
)

// Checkpoint metadata keys: the whole ledgerfeed.Checkpoint, so a resumed
// tailer re-checks the row it names (a seq alone cannot see a journal reset)
// and carries a lapse across a restart.
const (
	keyCPSchema     = "gc.evproj.schema"
	keyCPCity       = "gc.evproj.city"
	keyCPSeq        = "gc.evproj.seq"
	keyCPLastSeq    = "gc.evproj.last_seq"
	keyCPLastIssue  = "gc.evproj.last_issue"
	keyCPLastTS     = "gc.evproj.last_ts"
	keyCPLastOp     = "gc.evproj.last_op"
	keyCPLastDigest = "gc.evproj.last_digest"
	keyCPLapsed     = "gc.evproj.lapsed"
	keyCPAt         = "gc.evproj.at"
)

var digestRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Checkpoint is the projector's durable resume point for one city.
type Checkpoint struct {
	City string
	// Feed is the tailer's checkpoint: every journal row at or below Feed.Seq
	// is applied.
	Feed ledgerfeed.Checkpoint
	// At is when the projector wrote it.
	At time.Time
}

// CheckpointID returns the pinned id of city's checkpoint row in the
// namespace prefix, the sessions store's mint prefix.
func CheckpointID(prefix, city string) (string, error) {
	if err := validateCity(city); err != nil {
		return "", err
	}
	id, err := opjournal.PinnedID(prefix, checkpointFamily, checkpointTag, city)
	if err != nil {
		return "", fmt.Errorf("%w: checkpoint id: %w", ErrInvalid, err)
	}
	return id, nil
}

// EncodeCheckpoint renders cp as its row for the first write: a closed,
// no-history create.
func EncodeCheckpoint(prefix string, cp Checkpoint) (beads.Bead, error) {
	id, err := CheckpointID(prefix, cp.City)
	if err != nil {
		return beads.Bead{}, err
	}
	meta, err := CheckpointMetadata(cp)
	if err != nil {
		return beads.Bead{}, err
	}
	meta[keyCPCity] = cp.City
	for k, v := range meta {
		if v == "" {
			delete(meta, k)
		}
	}
	return beads.Bead{
		ID:        id,
		Title:     "event projector checkpoint",
		Status:    beadStatusClosed,
		Type:      BeadType,
		Labels:    []string{LabelProjector},
		Metadata:  meta,
		NoHistory: true,
	}, nil
}

// CheckpointMetadata is the metadata that moves an existing checkpoint row to
// cp, for the projector's revision-guarded update. It names every key the
// update moves, the absent ones as "", so no value of an earlier checkpoint
// survives. It leaves out the city: the row id pins it and the create wrote
// it, and an update reaches bd as --set-metadata k=v, where bd types a value
// by its spelling, so a city named "null" would read back empty.
func CheckpointMetadata(cp Checkpoint) (map[string]string, error) {
	if err := validateCity(cp.City); err != nil {
		return nil, err
	}
	if err := validateFeed(cp.Feed); err != nil {
		return nil, err
	}
	if cp.At.IsZero() || cp.At.UTC().Year() < 0 || cp.At.UTC().Year() > 9999 {
		return nil, invalidf("checkpoint time %v is zero or outside RFC 3339's years", cp.At)
	}
	last := cp.Feed.Last
	meta := map[string]string{
		keyCPSchema:     strconv.Itoa(CheckpointSchemaRev),
		keyCPSeq:        strconv.FormatInt(cp.Feed.Seq, 10),
		keyCPLastSeq:    "",
		keyCPLastIssue:  "",
		keyCPLastTS:     "",
		keyCPLastOp:     "",
		keyCPLastDigest: "",
		keyCPLapsed:     "",
		keyCPAt:         cp.At.UTC().Format(time.RFC3339Nano),
	}
	if last.Seq != 0 {
		meta[keyCPLastSeq] = strconv.FormatInt(last.Seq, 10)
		meta[keyCPLastIssue] = last.IssueID
		meta[keyCPLastTS] = last.TS
		meta[keyCPLastOp] = last.Op
		meta[keyCPLastDigest] = last.Digest
	}
	if cp.Feed.Lapsed {
		meta[keyCPLapsed] = "true"
	}
	return meta, nil
}

// DecodeCheckpoint reads a checkpoint row. A malformed row, or one whose id is
// not its city's under a valid prefix, is ErrCorrupt; a newer schema is
// ErrUnsupportedSchema.
func DecodeCheckpoint(b beads.Bead) (Checkpoint, error) {
	if b.Type != BeadType || !slices.Contains(b.Labels, LabelProjector) {
		return Checkpoint{}, corruptf(b.ID, "not an event-projector row (type %q, labels %v)", b.Type, b.Labels)
	}
	d := metaReader{id: b.ID, meta: b.Metadata}
	if err := d.schema(keyCPSchema, CheckpointSchemaRev); err != nil {
		return Checkpoint{}, err
	}
	cp := Checkpoint{
		City: d.required(keyCPCity),
		Feed: ledgerfeed.Checkpoint{
			Seq: d.int64(keyCPSeq),
			Last: ledgerfeed.Fingerprint{
				Seq:     d.int64(keyCPLastSeq),
				IssueID: d.meta[keyCPLastIssue],
				TS:      d.meta[keyCPLastTS],
				Op:      d.meta[keyCPLastOp],
				Digest:  d.meta[keyCPLastDigest],
			},
		},
		At: d.time(keyCPAt),
	}
	d.required(keyCPSeq)
	d.required(keyCPAt)
	switch lapsed := d.meta[keyCPLapsed]; lapsed {
	case "":
	case "true":
		cp.Feed.Lapsed = true
	default:
		d.fail(keyCPLapsed, "%q is not true or absent", lapsed)
	}
	if d.err != nil {
		return Checkpoint{}, d.err
	}
	if err := validateCity(cp.City); err != nil {
		return Checkpoint{}, corruptf(b.ID, "%v", err)
	}
	if err := validateFeed(cp.Feed); err != nil {
		return Checkpoint{}, corruptf(b.ID, "%v", err)
	}
	cut := strings.LastIndex(b.ID, "-"+checkpointFamily+"-")
	if cut < 0 {
		return Checkpoint{}, corruptf(b.ID, "id is not <prefix>-%s-<hash>", checkpointFamily)
	}
	want, err := CheckpointID(b.ID[:cut], cp.City)
	if err != nil {
		return Checkpoint{}, corruptf(b.ID, "id prefix: %v", err)
	}
	if want != b.ID {
		return Checkpoint{}, corruptf(b.ID, "id does not match its city (want %s)", want)
	}
	return cp, nil
}

func validateCity(city string) error {
	switch {
	case city == "":
		return invalidf("city is empty")
	case len(city) > maxCityLen:
		return invalidf("city is %d bytes, max %d", len(city), maxCityLen)
	case !utf8.ValidString(city):
		return invalidf("city %q is not valid UTF-8", city)
	case strings.IndexFunc(city, isSpaceOrControl) >= 0:
		return invalidf("city %q contains whitespace or control characters", city)
	}
	return nil
}

// validateFeed accepts the shapes a ledgerfeed.Tailer hands out: a seq, and
// either no fingerprint or the fingerprint of the row at that seq, whose text
// fits the journal's columns.
func validateFeed(cp ledgerfeed.Checkpoint) error {
	last := cp.Last
	for _, field := range []struct {
		name, value string
		max         int
	}{
		{"checkpoint fingerprint issue", last.IssueID, maxLastIssueLen},
		{"checkpoint fingerprint ts", last.TS, maxLastTSLen},
		{"checkpoint fingerprint op", last.Op, maxLastOpLen},
	} {
		if err := checkText(field.name, field.value, field.max); err != nil {
			return err
		}
	}
	switch {
	case cp.Seq < 0:
		return invalidf("checkpoint seq %d is negative", cp.Seq)
	case last.Seq == 0:
		if last != (ledgerfeed.Fingerprint{}) {
			return invalidf("checkpoint fingerprint without a seq: %+v", last)
		}
	case last.Seq != cp.Seq:
		return invalidf("checkpoint fingerprint seq %d is not the checkpoint seq %d", last.Seq, cp.Seq)
	case !digestRe.MatchString(last.Digest):
		return invalidf("checkpoint fingerprint digest %q is not a sha256 hex digest", last.Digest)
	}
	return nil
}
