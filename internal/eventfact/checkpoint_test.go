package eventfact

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/journalops"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/ledgerfeed"
)

var cpAt = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func fingerprint(seq int64) ledgerfeed.Fingerprint {
	return ledgerfeed.Fingerprint{Seq: seq, IssueID: "gc-ev-x", TS: "2026-10-15 12:00:00", Op: "create", Digest: strings.Repeat("ab", 32)}
}

// longestFingerprint is the most text a tailer can hand out: the journal's
// VARCHAR(255) issue_id and VARCHAR(32) op full of four-byte characters, and
// the longest ts bd renders.
func longestFingerprint(seq int64) ledgerfeed.Fingerprint {
	last := fingerprint(seq)
	last.IssueID = strings.Repeat("\U0001F600", 255)
	last.Op = strings.Repeat("\U0001F600", 32)
	last.TS = "2026-10-15T12:00:00.123456789Z"
	return last
}

func sameCheckpoint(t *testing.T, got, want Checkpoint) {
	t.Helper()
	if !got.At.Equal(want.At) {
		t.Fatalf("At = %v, want %v", got.At, want.At)
	}
	got.At, want.At = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checkpoint mismatch:\n got  %+v\n want %+v", got, want)
	}
}

func TestCheckpointID(t *testing.T) {
	id, err := CheckpointID("my-city", "my-city")
	if err != nil || id != "my-city-evproj-zrmdzquvb6wyt462o74fvejjda" {
		t.Fatalf("CheckpointID = %s, %v", id, err)
	}
	for _, c := range []struct{ prefix, city string }{{"", "c"}, {"gc-", "c"}, {"gc", ""}, {"gc", "a city"}, {"gc", strings.Repeat("c", 201)}} {
		if _, err := CheckpointID(c.prefix, c.city); !errors.Is(err, ErrInvalid) {
			t.Errorf("CheckpointID(%q, %q) = %v, want ErrInvalid", c.prefix, c.city, err)
		}
	}
}

// Every shape a ledgerfeed.Tailer hands out round-trips whole.
func TestCheckpointRoundTrip(t *testing.T) {
	for name, feed := range map[string]ledgerfeed.Checkpoint{
		"empty journal":       {},
		"at a row":            {Seq: 41, Last: fingerprint(41)},
		"longest row text":    {Seq: 41, Last: longestFingerprint(41)},
		"unknown fingerprint": {Seq: 41},
		"lapsed":              {Seq: 41, Last: fingerprint(41), Lapsed: true},
	} {
		t.Run(name, func(t *testing.T) {
			cp := Checkpoint{City: "my-city", Feed: feed, At: cpAt}
			b, err := EncodeCheckpoint("gc", cp)
			if err != nil {
				t.Fatalf("EncodeCheckpoint: %v", err)
			}
			if b.Type != "op" || b.Status != "closed" || !b.NoHistory || len(b.Labels) != 1 || b.Labels[0] != LabelProjector {
				t.Fatalf("row shape = %+v", b)
			}
			got, err := DecodeCheckpoint(b)
			if err != nil {
				t.Fatalf("DecodeCheckpoint: %v", err)
			}
			sameCheckpoint(t, got, cp)
		})
	}
}

// CheckpointMetadata names every key, so the update that moves the row clears
// what the previous checkpoint set.
func TestCheckpointMetadataClearsTheEarlierCheckpoint(t *testing.T) {
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	first := Checkpoint{City: "my-city", Feed: ledgerfeed.Checkpoint{Seq: 41, Last: fingerprint(41), Lapsed: true}, At: cpAt}
	b, err := EncodeCheckpoint("gc", first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(b); err != nil {
		t.Fatalf("Create: %v", err)
	}
	next := Checkpoint{City: "my-city", Feed: ledgerfeed.Checkpoint{Seq: 90}, At: cpAt.Add(time.Second)}
	patch, err := CheckpointMetadata(next)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(b.ID, beads.UpdateOpts{Metadata: patch}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	stored, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeCheckpoint(stored)
	if err != nil {
		t.Fatalf("DecodeCheckpoint: %v", err)
	}
	sameCheckpoint(t, got, next)
}

func TestCheckpointRefuses(t *testing.T) {
	for name, cp := range map[string]Checkpoint{
		"no city":               {Feed: ledgerfeed.Checkpoint{Seq: 1}, At: cpAt},
		"negative seq":          {City: "c", Feed: ledgerfeed.Checkpoint{Seq: -1}, At: cpAt},
		"fingerprint elsewhere": {City: "c", Feed: ledgerfeed.Checkpoint{Seq: 5, Last: fingerprint(4)}, At: cpAt},
		"fingerprint, no seq":   {City: "c", Feed: ledgerfeed.Checkpoint{Seq: 5, Last: ledgerfeed.Fingerprint{IssueID: "x"}}, At: cpAt},
		"digest not sha256 hex": {City: "c", Feed: ledgerfeed.Checkpoint{Seq: 5, Last: ledgerfeed.Fingerprint{Seq: 5, Digest: "xyz"}}, At: cpAt},
		"zero time":             {City: "c", Feed: ledgerfeed.Checkpoint{Seq: 1}},
		// The year is checked in UTC, the zone the row stores.
		"zoned time past 9999 in utc":     {City: "c", Feed: ledgerfeed.Checkpoint{Seq: 1}, At: time.Date(9999, 12, 31, 23, 0, 0, 0, time.FixedZone("", -5*3600))},
		"zoned time before year 0 in utc": {City: "c", Feed: ledgerfeed.Checkpoint{Seq: 1}, At: time.Date(0, 1, 1, 1, 0, 0, 0, time.FixedZone("", 5*3600))},
	} {
		if _, err := EncodeCheckpoint("gc", cp); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: EncodeCheckpoint = %v, want ErrInvalid", name, err)
		}
	}

	good, err := EncodeCheckpoint("gc", Checkpoint{City: "my-city", Feed: ledgerfeed.Checkpoint{Seq: 41, Last: fingerprint(41)}, At: cpAt})
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*beads.Bead){
		"not a checkpoint row": func(b *beads.Bead) { b.Labels = []string{"gc:event-fact"} },
		"lapsed yes":           func(b *beads.Bead) { b.Metadata["gc.evproj.lapsed"] = "yes" },
		"no seq":               func(b *beads.Bead) { delete(b.Metadata, "gc.evproj.seq") },
		"another city":         func(b *beads.Bead) { b.Metadata["gc.evproj.city"] = "other-city" },
		"fingerprint moved":    func(b *beads.Bead) { b.Metadata["gc.evproj.last_seq"] = "40" },
		"no time":              func(b *beads.Bead) { delete(b.Metadata, "gc.evproj.at") },
	} {
		b := good
		b.Labels = append([]string(nil), good.Labels...)
		b.Metadata = maps.Clone(good.Metadata)
		mutate(&b)
		if _, err := DecodeCheckpoint(b); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: DecodeCheckpoint = %v, want ErrCorrupt", name, err)
		}
	}
	newer := good
	newer.Metadata = maps.Clone(good.Metadata)
	newer.Metadata["gc.evproj.schema"] = "2"
	if _, err := DecodeCheckpoint(newer); !errors.Is(err, ErrUnsupportedSchema) {
		t.Errorf("schema 2: %v, want ErrUnsupportedSchema", err)
	}
}

// A fingerprint's text fits the journal column it came from, so neither the
// projector nor a row it reads back hands the tailer an unbounded value.
func TestCheckpointRefusesFingerprintText(t *testing.T) {
	good, err := EncodeCheckpoint("gc", Checkpoint{City: "my-city", Feed: ledgerfeed.Checkpoint{Seq: 41, Last: fingerprint(41)}, At: cpAt})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []struct {
		key string
		set func(*ledgerfeed.Fingerprint, string)
		max int
	}{
		{"gc.evproj.last_issue", func(l *ledgerfeed.Fingerprint, v string) { l.IssueID = v }, maxLastIssueLen},
		{"gc.evproj.last_ts", func(l *ledgerfeed.Fingerprint, v string) { l.TS = v }, maxLastTSLen},
		{"gc.evproj.last_op", func(l *ledgerfeed.Fingerprint, v string) { l.Op = v }, maxLastOpLen},
	} {
		for name, v := range map[string]string{
			"over its limit": strings.Repeat("x", field.max+1),
			"not UTF-8":      "gc-\xff",
		} {
			t.Run(field.key+" "+name, func(t *testing.T) {
				last := fingerprint(41)
				field.set(&last, v)
				cp := Checkpoint{City: "my-city", Feed: ledgerfeed.Checkpoint{Seq: 41, Last: last}, At: cpAt}
				if _, err := EncodeCheckpoint("gc", cp); !errors.Is(err, ErrInvalid) {
					t.Errorf("EncodeCheckpoint = %v, want ErrInvalid", err)
				}
				b := good
				b.Metadata = maps.Clone(good.Metadata)
				b.Metadata[field.key] = v
				if _, err := DecodeCheckpoint(b); !errors.Is(err, ErrCorrupt) {
					t.Errorf("DecodeCheckpoint = %v, want ErrCorrupt", err)
				}
			})
		}
	}
}

// memJournal is a journal of fixed rows, seq 1..len(rows).
type memJournal struct{ rows []journalops.Row }

func (j *memJournal) ReadEventsJournalPage(_ context.Context, since int64, limit int) (journalops.Page, error) {
	head := int64(len(j.rows))
	var out []journalops.Row
	for _, r := range j.rows {
		if r.Seq > since && (limit == 0 || len(out) < limit) {
			out = append(out, r)
		}
	}
	return journalops.Page{Rows: out, Head: head}, nil
}

// A checkpoint stored and read back resumes a tailer with no gap: the stored
// fingerprint is the whole one the tailer re-checks.
func TestStoredCheckpointResumesWithoutAGap(t *testing.T) {
	journal := &memJournal{}
	for i := int64(1); i <= 5; i++ {
		journal.rows = append(journal.rows, journalops.Row{Seq: i, Op: "create", IssueID: "gc-x", TS: "2026-10-15 12:00:0" + string(rune('0'+i)), Actor: "gascity", IssueJSON: `{"id":"gc-x"}`})
	}
	var gaps []ledgerfeed.Gap
	var applied []int64
	newTailer := func() *ledgerfeed.Tailer {
		return &ledgerfeed.Tailer{
			Source:    journal,
			PageLimit: 3,
			Apply: func(_ context.Context, page journalops.Page) error {
				for _, r := range page.Rows {
					applied = append(applied, r.Seq)
				}
				return nil
			},
			OnGap: func(_ context.Context, gap ledgerfeed.Gap) error {
				gaps = append(gaps, gap)
				return nil
			},
		}
	}
	ctx := context.Background()
	first := newTailer()
	if _, err := first.Step(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := EncodeCheckpoint("gc", Checkpoint{City: "c", Feed: first.Checkpoint(), At: cpAt})
	if err != nil {
		t.Fatal(err)
	}
	cp, err := DecodeCheckpoint(b)
	if err != nil {
		t.Fatal(err)
	}
	second := newTailer()
	if err := second.Resume(cp.Feed); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 0 {
		t.Fatalf("resuming from the stored checkpoint saw gaps: %+v", gaps)
	}
	if want := []int64{1, 2, 3, 4, 5}; !reflect.DeepEqual(applied, want) {
		t.Fatalf("applied %v, want %v", applied, want)
	}
}

// bdSetMetadata applies patch the way `bd update --set-metadata k=v` stores
// it (BdStore.UpdateIfMatch): bd types each value by its spelling, so "null"
// reads back as "" and a number or boolean spelling reads back as its JSON
// text.
func bdSetMetadata(t *testing.T, meta map[string]string, patch map[string]string) {
	t.Helper()
	for k, v := range patch {
		var scalar any
		if err := json.Unmarshal([]byte(v), &scalar); err == nil {
			switch scalar.(type) {
			case nil:
				v = ""
			case bool, float64:
				data, _ := json.Marshal(scalar)
				v = string(data)
			}
		}
		if v == "" {
			delete(meta, k)
			continue
		}
		meta[k] = v
	}
}

// The update that moves the row leaves the city alone: the row id pins it and
// the create wrote it. A city whose name reads as a JSON scalar ("null",
// "true", "42") then survives every update through bd.
func TestCheckpointUpdateLeavesTheCity(t *testing.T) {
	for _, city := range []string{"my-city", "null", "true", "42"} {
		t.Run(city, func(t *testing.T) {
			first := Checkpoint{City: city, Feed: ledgerfeed.Checkpoint{Seq: 41, Last: fingerprint(41)}, At: cpAt}
			b, err := EncodeCheckpoint("gc", first)
			if err != nil {
				t.Fatal(err)
			}
			if b.Metadata[keyCPCity] != city {
				t.Fatalf("the create carries city %q, want %q", b.Metadata[keyCPCity], city)
			}
			next := Checkpoint{City: city, Feed: ledgerfeed.Checkpoint{Seq: 90}, At: cpAt.Add(time.Second)}
			patch, err := CheckpointMetadata(next)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := patch[keyCPCity]; ok {
				t.Fatalf("the update patch rewrites the city: %v", patch)
			}
			bdSetMetadata(t, b.Metadata, patch)
			got, err := DecodeCheckpoint(b)
			if err != nil {
				t.Fatalf("DecodeCheckpoint after the update: %v", err)
			}
			sameCheckpoint(t, got, next)
		})
	}
}

func TestDecodeCheckpointSchemaAndCanonicalForm(t *testing.T) {
	good, err := EncodeCheckpoint("gc", Checkpoint{City: "my-city", Feed: ledgerfeed.Checkpoint{Seq: 41, Last: fingerprint(41)}, At: cpAt})
	if err != nil {
		t.Fatal(err)
	}
	if good.Metadata[keyCPSchema] != "1" || CheckpointSchemaRev != 1 {
		t.Fatalf("schema = %q, CheckpointSchemaRev = %d", good.Metadata[keyCPSchema], CheckpointSchemaRev)
	}
	for name, value := range map[string]map[string]string{
		"schema 0":           {keyCPSchema: "0"},
		"at with an offset":  {keyCPAt: "2026-10-15T14:00:00+02:00"},
		"at trailing zeroes": {keyCPAt: "2026-10-15T12:00:00.500Z"},
		"seq non-canonical":  {keyCPSeq: "041", keyCPLastSeq: "041"},
	} {
		b := good
		b.Metadata = maps.Clone(good.Metadata)
		maps.Copy(b.Metadata, value)
		if _, err := DecodeCheckpoint(b); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: DecodeCheckpoint = %v, want ErrCorrupt", name, err)
		}
	}
	newer := good
	newer.Metadata = maps.Clone(good.Metadata)
	newer.Metadata[keyCPSchema] = "2"
	if _, err := DecodeCheckpoint(newer); !errors.Is(err, ErrUnsupportedSchema) {
		t.Errorf("schema 2: %v, want ErrUnsupportedSchema", err)
	}
}
