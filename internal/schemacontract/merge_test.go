package schemacontract

import (
	"reflect"
	"strings"
	"testing"
)

func liveOf(t *testing.T, docs map[string]string) map[string]Entry {
	t.Helper()
	live := map[string]Entry{}
	for path, d := range docs {
		live[path] = mustExtract(t, d)
	}
	return live
}

func TestMergeSeedsSchemaWithNoRecord(t *testing.T) {
	live := liveOf(t, map[string]string{schemaPath: doc("1", `"a"`, `"a":{"type":"string"}`)})
	got, summary, err := Merge(Baseline{Format: 1}, live)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if summary.Seeded != 1 || summary.EntriesAdded != 0 || summary.RecordsAppended != 0 {
		t.Fatalf("summary = %+v, want one seeded schema", summary)
	}
	entries := got.Schemas[schemaPath]
	if len(entries) != 1 || entries[0].SchemaVersion != "1" || entries[0].Note != "" {
		t.Fatalf("seeded entries = %+v, want one bare version-1 entry", entries)
	}
	wantClean(t, Check(got, live))
}

func TestMergeAppendsOnlyMissingRecordsAtSameVersion(t *testing.T) {
	recorded := mustExtract(t, doc("1", `"a"`, `"a":{"type":"string"}`))
	live := liveOf(t, map[string]string{schemaPath: doc("1", `"a"`, `"a":{"type":"string"},"b":{"type":"integer"}`)})
	got, summary, err := Merge(baselineOf(recorded), live)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if summary.RecordsAppended != 1 || summary.Seeded != 0 || summary.EntriesAdded != 0 {
		t.Fatalf("summary = %+v, want one appended record", summary)
	}
	if n := len(got.Schemas[schemaPath]); n != 1 {
		t.Fatalf("entries = %d, want the existing entry extended, not a new one", n)
	}
	if !hasField(got.Schemas[schemaPath][0], "", "$.b") {
		t.Fatalf("$.b was not appended: %+v", got.Schemas[schemaPath][0])
	}
	wantClean(t, Check(got, live))
}

func TestMergeAppendsMissingVariant(t *testing.T) {
	recorded := mustExtract(t, `{"oneOf":[{"$ref":"#/$defs/local"}],"$defs":{"local":{"type":"object"}}}`)
	live := liveOf(t, map[string]string{schemaPath: `{"oneOf":[{"$ref":"#/$defs/local"},{"$ref":"#/$defs/api"}],"$defs":{"local":{"type":"object"},"api":{"type":"object"}}}`})
	got, summary, err := Merge(baselineOf(recorded), live)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if summary.RecordsAppended == 0 {
		t.Fatalf("summary = %+v, want the new variant's records appended", summary)
	}
	if _, ok := got.Schemas[schemaPath][0].Variants["api"]; !ok {
		t.Fatalf("variant api not appended: %+v", got.Schemas[schemaPath][0].Variants)
	}
}

func TestMergeNeverHidesABreakingChange(t *testing.T) {
	recorded := mustExtract(t, doc("1", `"a"`, `"a":{"type":"string"},"b":{"type":"integer"}`))
	live := liveOf(t, map[string]string{schemaPath: doc("1", `"a"`, `"a":{"type":"integer"}`)})
	got, _, err := Merge(baselineOf(recorded), live)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !reflect.DeepEqual(got.Schemas[schemaPath][0].Variants[""], recorded.Variants[""]) {
		t.Fatalf("Merge rewrote or deleted records: %+v", got.Schemas[schemaPath][0].Variants[""])
	}
	wantProblem(t, Check(got, live), "BREAKING")
}

func TestMergeNewVersionAddsEntryWithEmptyPlaceholders(t *testing.T) {
	v1 := mustExtract(t, doc("1", ``, `"a":{"type":"string"}`))
	live := liveOf(t, map[string]string{schemaPath: doc("2", ``, `"b":{"type":"string"}`)})
	got, summary, err := Merge(baselineOf(v1), live)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if summary.EntriesAdded != 1 {
		t.Fatalf("summary = %+v, want one entry added", summary)
	}
	entries := got.Schemas[schemaPath]
	if len(entries) != 2 || entries[1].SchemaVersion != "2" {
		t.Fatalf("entries = %+v, want v1 then v2", entries)
	}
	if entries[1].Note != "" || entries[1].Issue != "" || entries[1].PR != "" {
		t.Fatalf("new entry must leave note and reference empty for the author: %+v", entries[1])
	}
	problems := Check(got, live)
	wantProblem(t, problems, "note")
}

func TestMergeUnversionedToConstBumpAddsEntry(t *testing.T) {
	unversioned := mustExtract(t, doc("", ``, `"a":{"type":"string"}`))
	live := liveOf(t, map[string]string{schemaPath: doc("1", ``, `"a":{"type":"string"}`)})
	got, summary, err := Merge(baselineOf(unversioned), live)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if summary.EntriesAdded != 1 || len(got.Schemas[schemaPath]) != 2 {
		t.Fatalf("got %+v / %+v, want a second entry for version 1", summary, got.Schemas[schemaPath])
	}
}

func TestMergeRefusesOlderVersion(t *testing.T) {
	v1 := mustExtract(t, doc("1", ``, `"a":{"type":"string"}`))
	v2 := completeEntry(t, "2", `"b":{"type":"string"}`)
	live := liveOf(t, map[string]string{schemaPath: doc("1", ``, `"a":{"type":"string"}`)})
	_, _, err := Merge(baselineOf(v1, v2), live)
	if err == nil || !strings.Contains(err.Error(), "went backwards") || !strings.Contains(err.Error(), schemaPath) {
		t.Fatalf("Merge error = %v, want a went-backwards error naming the schema", err)
	}
}

func TestMergeIsIdempotent(t *testing.T) {
	live := liveOf(t, map[string]string{
		schemaPath:             doc("1", `"a"`, `"a":{"type":"string"}`),
		"y/result.schema.json": doc("", ``, `"z":{"type":"string"}`),
	})
	first, _, err := Merge(Baseline{Format: 1}, live)
	if err != nil {
		t.Fatal(err)
	}
	second, summary, err := Merge(first, live)
	if err != nil {
		t.Fatal(err)
	}
	if summary != (MergeSummary{}) {
		t.Fatalf("second merge summary = %+v, want zero", summary)
	}
	a, _ := Marshal(first)
	b, _ := Marshal(second)
	if string(a) != string(b) {
		t.Fatalf("second merge changed the baseline:\n%s\n---\n%s", a, b)
	}
}

func TestMergeKeepsRecordsOfSchemasThatAreGone(t *testing.T) {
	v1 := mustExtract(t, doc("1", ``, `"a":{"type":"string"}`))
	got, _, err := Merge(baselineOf(v1), map[string]Entry{})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(got.Schemas[schemaPath]) != 1 {
		t.Fatalf("Merge deleted the record of a removed schema")
	}
	wantProblem(t, Check(got, map[string]Entry{}), "schema removed")
}

func TestMergeDoesNotMutateItsInput(t *testing.T) {
	recorded := mustExtract(t, doc("1", `"a"`, `"a":{"type":"string"}`))
	in := baselineOf(recorded)
	before, _ := Marshal(in)
	live := liveOf(t, map[string]string{schemaPath: doc("1", `"a"`, `"a":{"type":"string"},"b":{"type":"integer"}`)})
	if _, _, err := Merge(in, live); err != nil {
		t.Fatal(err)
	}
	after, _ := Marshal(in)
	if string(before) != string(after) {
		t.Fatalf("Merge mutated its input baseline")
	}
}
