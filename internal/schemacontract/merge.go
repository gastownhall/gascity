package schemacontract

import "fmt"

// Merge appends what the live schemas add to the baseline and never rewrites
// or deletes: a schema with no record is seeded, a new schema_version gets a
// new entry with empty note and reference for the author to fill, and at the
// newest recorded version only missing variants and field records are
// appended. A live version older than the newest recorded one is an error.
func Merge(b Baseline, live map[string]Entry) (Baseline, MergeSummary, error) {
	var summary MergeSummary
	out := Baseline{Format: baselineFormat, Schemas: map[string][]Entry{}}
	for path, entries := range b.Schemas {
		copied := make([]Entry, len(entries))
		for i, e := range entries {
			copied[i] = cloneEntry(e)
		}
		out.Schemas[path] = copied
	}
	for _, path := range sortedKeys(live) {
		entry := live[path]
		entries := out.Schemas[path]
		if len(entries) == 0 {
			out.Schemas[path] = []Entry{bare(entry)}
			summary.Seeded++
			continue
		}
		idx := -1
		for i, e := range entries {
			if e.SchemaVersion == entry.SchemaVersion {
				idx = i
			}
		}
		switch {
		case idx < 0:
			out.Schemas[path] = append(entries, bare(entry))
			summary.EntriesAdded++
		case idx < len(entries)-1:
			return Baseline{}, MergeSummary{}, fmt.Errorf("%s: schema_version %q went backwards: the baseline already records newer version %q", path, entry.SchemaVersion, entries[len(entries)-1].SchemaVersion)
		default:
			summary.RecordsAppended += appendMissing(&entries[idx], entry)
		}
	}
	return out, summary, nil
}

// bare copies a live entry as a new baseline entry with no note or reference.
func bare(e Entry) Entry {
	e.Note, e.Issue, e.PR = "", "", ""
	return cloneEntry(e)
}

func cloneEntry(e Entry) Entry {
	variants := make(map[string][]Field, len(e.Variants))
	for key, fields := range e.Variants {
		variants[key] = append([]Field(nil), fields...)
	}
	e.Variants = variants
	return e
}

// appendMissing adds the live variants and field records the recorded entry
// lacks and returns how many records it added.
func appendMissing(recorded *Entry, live Entry) int {
	added := 0
	for _, key := range sortedKeys(live.Variants) {
		have := map[string]bool{}
		for _, f := range recorded.Variants[key] {
			have[f.Path] = true
		}
		if recorded.Variants == nil {
			recorded.Variants = map[string][]Field{}
		}
		for _, f := range live.Variants[key] {
			if !have[f.Path] {
				recorded.Variants[key] = append(recorded.Variants[key], f)
				added++
			}
		}
	}
	return added
}
