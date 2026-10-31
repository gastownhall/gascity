package schemacontract

import (
	"strings"
	"testing"
)

const schemaPath = "x/result.schema.json"

// doc wraps properties in a versioned object schema.
func doc(version, required, props string) string {
	ver := ""
	if version != "" {
		ver = `"schema_version":{"const":"` + version + `"},`
	}
	return `{"type":"object","required":[` + required + `],"properties":{` + ver + props + `}}`
}

// baselineOf builds a one-schema baseline from the given entries.
func baselineOf(entries ...Entry) Baseline {
	return Baseline{Format: 1, Schemas: map[string][]Entry{schemaPath: entries}}
}

// check runs Check for the schema at schemaPath against a recorded document
// and a live document.
func check(t *testing.T, recorded, live string) []string {
	t.Helper()
	return Check(baselineOf(mustExtract(t, recorded)), map[string]Entry{schemaPath: mustExtract(t, live)})
}

func wantProblem(t *testing.T, problems []string, parts ...string) {
	t.Helper()
	for _, p := range problems {
		ok := true
		for _, part := range parts {
			if !strings.Contains(p, part) {
				ok = false
			}
		}
		if ok {
			return
		}
	}
	t.Fatalf("no problem mentions all of %q; problems: %q", parts, problems)
}

func wantClean(t *testing.T, problems []string) {
	t.Helper()
	if len(problems) != 0 {
		t.Fatalf("want no problems, got %q", problems)
	}
}

func TestCheckIdenticalIsClean(t *testing.T) {
	d := doc("1", `"a"`, `"a":{"type":"string"}`)
	wantClean(t, check(t, d, d))
}

func TestCheckBreakingMutants(t *testing.T) {
	base := doc("1", `"a"`, `"a":{"type":"string"},"b":{"type":"integer"},"c":{"type":"object","properties":{"d":{"type":"string"}}}`)
	cases := map[string]string{
		"property removed": doc("1", `"a"`, `"a":{"type":"string"},"c":{"type":"object","properties":{"d":{"type":"string"}}}`),
		"property renamed": doc("1", `"a"`, `"a":{"type":"string"},"b2":{"type":"integer"},"c":{"type":"object","properties":{"d":{"type":"string"}}}`),
		"type changed":     doc("1", `"a"`, `"a":{"type":"integer"},"b":{"type":"integer"},"c":{"type":"object","properties":{"d":{"type":"string"}}}`),
		"type widened":     doc("1", `"a"`, `"a":{"type":["string","null"]},"b":{"type":"integer"},"c":{"type":"object","properties":{"d":{"type":"string"}}}`),
		"nested removal":   doc("1", `"a"`, `"a":{"type":"string"},"b":{"type":"integer"},"c":{"type":"object"}`),
		"required relaxed": doc("1", ``, `"a":{"type":"string"},"b":{"type":"integer"},"c":{"type":"object","properties":{"d":{"type":"string"}}}`),
		"const changed":    doc("2", `"a"`, `"a":{"type":"string"},"b":{"type":"integer"},"c":{"type":"object","properties":{"d":{"type":"string"}}}`),
	}
	for name, live := range cases {
		t.Run(name, func(t *testing.T) {
			problems := check(t, base, live)
			if name == "const changed" {
				// A bumped version has no entry: that is the bump-without-entry rule,
				// not a shape comparison.
				wantProblem(t, problems, "bump without entry")
				return
			}
			wantProblem(t, problems, "BREAKING", schemaPath)
		})
	}
}

func TestCheckConstChangedWithinVersionIsBreaking(t *testing.T) {
	recorded := `{"type":"object","properties":{"schema_version":{"const":"1"},"kind":{"const":"a"}}}`
	live := `{"type":"object","properties":{"schema_version":{"const":"1"},"kind":{"const":"b"}}}`
	wantProblem(t, check(t, recorded, live), "BREAKING", "$.kind")
}

func TestCheckConstDroppedIsBreaking(t *testing.T) {
	recorded := `{"type":"object","properties":{"schema_version":{"const":"1"},"kind":{"const":"a"}}}`
	live := `{"type":"object","properties":{"schema_version":{"const":"1"},"kind":{"type":"string"}}}`
	wantProblem(t, check(t, recorded, live), "BREAKING", "$.kind")
}

func TestCheckNarrowingIsGreen(t *testing.T) {
	recorded := doc("1", `"a"`, `"a":{"type":["string","null"]},"b":{"type":"integer"}`)
	live := doc("1", `"a","b"`, `"a":{"type":"string"},"b":{"type":"integer"}`)
	wantClean(t, check(t, recorded, live))
}

func TestCheckScalarUnionWideningIsBreaking(t *testing.T) {
	recorded := doc("1", ``, `"n":{"oneOf":[{"type":"integer"},{"type":"null"}]}`)
	widened := doc("1", ``, `"n":{"oneOf":[{"type":"integer"},{"type":"null"},{"type":"string"}]}`)
	narrowed := doc("1", ``, `"n":{"type":"integer"}`)
	wantProblem(t, check(t, recorded, widened), "BREAKING", "$.n")
	wantClean(t, check(t, recorded, narrowed))
}

func TestCheckAdditiveFieldAsksForGenerator(t *testing.T) {
	recorded := doc("1", `"a"`, `"a":{"type":"string"}`)
	live := doc("1", `"a"`, `"a":{"type":"string"},"b":{"type":"integer"}`)
	problems := check(t, recorded, live)
	wantProblem(t, problems, "run the generator", schemaPath)
	for _, p := range problems {
		if strings.Contains(p, "BREAKING") || strings.Contains(p, "bump") {
			t.Fatalf("additive change must not demand a bump: %q", p)
		}
	}
}

func TestCheckAllOfMergeAndBooleanSchema(t *testing.T) {
	recorded := `{"allOf":[{"type":"object","required":["a"],"properties":{"a":{"type":"string"}}},{"properties":{"b":true}}]}`
	removed := `{"allOf":[{"type":"object","required":["a"],"properties":{"a":{"type":"string"}}}]}`
	wantProblem(t, check(t, recorded, removed), "BREAKING", "$.b")
	wantClean(t, check(t, recorded, recorded))
}

func TestCheckVariants(t *testing.T) {
	recorded := `{"oneOf":[{"$ref":"#/$defs/local"},{"$ref":"#/$defs/api"}],"$defs":{
		"local":{"type":"object","properties":{"l":{"type":"string"}}},
		"api":{"type":"object","properties":{"a":{"type":"string"}}}}}`
	removed := `{"oneOf":[{"$ref":"#/$defs/local"}],"$defs":{"local":{"type":"object","properties":{"l":{"type":"string"}}}}}`
	renamed := `{"oneOf":[{"$ref":"#/$defs/local"},{"$ref":"#/$defs/remote"}],"$defs":{
		"local":{"type":"object","properties":{"l":{"type":"string"}}},
		"remote":{"type":"object","properties":{"a":{"type":"string"}}}}}`
	added := `{"oneOf":[{"$ref":"#/$defs/local"},{"$ref":"#/$defs/api"},{"$ref":"#/$defs/extra"}],"$defs":{
		"local":{"type":"object","properties":{"l":{"type":"string"}}},
		"api":{"type":"object","properties":{"a":{"type":"string"}}},
		"extra":{"type":"object","properties":{"e":{"type":"string"}}}}}`
	wantProblem(t, check(t, recorded, removed), "BREAKING", "api")
	wantProblem(t, check(t, recorded, renamed), "BREAKING", "api")
	problems := check(t, recorded, added)
	wantProblem(t, problems, "run the generator", "extra")
	for _, p := range problems {
		if strings.Contains(p, "BREAKING") {
			t.Fatalf("adding a variant is additive: %q", p)
		}
	}
}

func TestCheckHelperDefRenameIsGreen(t *testing.T) {
	recorded := `{"type":"object","properties":{"row":{"$ref":"#/$defs/rowDef"}},"$defs":{"rowDef":{"type":"object","properties":{"id":{"type":"string"}}}}}`
	live := `{"type":"object","properties":{"row":{"$ref":"#/$defs/rowShape"}},"$defs":{"rowShape":{"type":"object","properties":{"id":{"type":"string"}}}}}`
	wantClean(t, check(t, recorded, live))
}

func TestCheckFramingChangeIsBreaking(t *testing.T) {
	plain := `{"type":"object","properties":{"a":{"type":"string"}}}`
	jsonl := `{"type":"object","x-gc-jsonl":{},"properties":{"a":{"type":"string"}}}`
	raw := `{"type":"object","x-gc-raw-json":true,"properties":{"a":{"type":"string"}}}`
	wantProblem(t, check(t, plain, jsonl), "BREAKING", "jsonl")
	wantProblem(t, check(t, jsonl, plain), "BREAKING", "jsonl")
	wantProblem(t, check(t, plain, raw), "BREAKING", "raw_json")
}

func TestCheckIfThenElseAreIgnored(t *testing.T) {
	recorded := `{"type":"object","properties":{"a":{"type":"string"}}}`
	live := `{"type":"object","properties":{"a":{"type":"string"}},"if":{"required":["a"]},"then":{"required":["a"]},"else":{}}`
	wantClean(t, check(t, recorded, live))
}

func TestCheckMissingRecordAsksForGenerator(t *testing.T) {
	live := map[string]Entry{schemaPath: mustExtract(t, doc("1", ``, `"a":{"type":"string"}`))}
	problems := Check(Baseline{Format: 1, Schemas: map[string][]Entry{}}, live)
	wantProblem(t, problems, schemaPath, "run the generator")
}

func completeEntry(t *testing.T, version, props string) Entry {
	t.Helper()
	e := mustExtract(t, doc(version, ``, props))
	e.Note = "renamed a"
	e.Issue = "#8453"
	return e
}

func TestCheckVersionBumpRules(t *testing.T) {
	v1 := mustExtract(t, doc("1", ``, `"a":{"type":"string"}`))
	v2 := completeEntry(t, "2", `"b":{"type":"string"}`)
	live2 := mustExtract(t, doc("2", ``, `"b":{"type":"string"}`))
	live3 := mustExtract(t, doc("3", ``, `"b":{"type":"string"}`))
	live1 := mustExtract(t, doc("1", ``, `"a":{"type":"string"}`))

	t.Run("bump with a complete entry is clean", func(t *testing.T) {
		wantClean(t, Check(baselineOf(v1, v2), map[string]Entry{schemaPath: live2}))
	})
	t.Run("bump without an entry fails", func(t *testing.T) {
		wantProblem(t, Check(baselineOf(v1), map[string]Entry{schemaPath: live2}), "bump without entry", "run the generator")
	})
	t.Run("version went backwards", func(t *testing.T) {
		wantProblem(t, Check(baselineOf(v1, v2), map[string]Entry{schemaPath: live1}), "went backwards")
	})
	t.Run("live version newer than every entry fails", func(t *testing.T) {
		wantProblem(t, Check(baselineOf(v1, v2), map[string]Entry{schemaPath: live3}), "bump without entry")
	})
	t.Run("bumped entry without note fails", func(t *testing.T) {
		bad := v2
		bad.Note = ""
		wantProblem(t, Check(baselineOf(v1, bad), map[string]Entry{schemaPath: live2}), "note")
	})
	t.Run("bumped entry without a reference fails", func(t *testing.T) {
		bad := v2
		bad.Issue, bad.PR = "", ""
		wantProblem(t, Check(baselineOf(v1, bad), map[string]Entry{schemaPath: live2}), "issue", "pr")
	})
	t.Run("bumped entry with a malformed reference fails", func(t *testing.T) {
		bad := v2
		bad.Issue = "see slack"
		wantProblem(t, Check(baselineOf(v1, bad), map[string]Entry{schemaPath: live2}), "reference")
	})
	t.Run("a pr URL reference is accepted", func(t *testing.T) {
		ok := v2
		ok.Issue = ""
		ok.PR = "https://github.com/gastownhall/gascity/pull/9000"
		wantClean(t, Check(baselineOf(v1, ok), map[string]Entry{schemaPath: live2}))
	})
	t.Run("the first entry needs no note", func(t *testing.T) {
		wantClean(t, Check(baselineOf(v1), map[string]Entry{schemaPath: live1}))
	})
	t.Run("duplicate version in one schema fails", func(t *testing.T) {
		dup := v1
		dup.Note, dup.Issue = "again", "#1"
		wantProblem(t, Check(baselineOf(v1, dup), map[string]Entry{schemaPath: live1}), "duplicate")
	})
}

func TestCheckUnversionedToConstBump(t *testing.T) {
	unversioned := mustExtract(t, doc("", ``, `"a":{"type":"string"}`))
	versioned := mustExtract(t, doc("1", ``, `"a":{"type":"string"}`))
	versioned.Note = "declared schema_version"
	versioned.Issue = "#8453"
	live := map[string]Entry{schemaPath: versioned}

	wantClean(t, Check(baselineOf(unversioned, versioned), live))
	wantProblem(t, Check(baselineOf(unversioned), live), "bump without entry")
}

func TestCheckBreakingPersistsAgainstNewestEntryOnly(t *testing.T) {
	// A breaking change is judged against the newest entry for the live version,
	// so merging it into the baseline would not hide it: Merge never rewrites.
	v1 := mustExtract(t, doc("1", `"a"`, `"a":{"type":"string"},"b":{"type":"string"}`))
	live := mustExtract(t, doc("1", `"a"`, `"a":{"type":"string"}`))
	wantProblem(t, Check(baselineOf(v1), map[string]Entry{schemaPath: live}), "BREAKING", "$.b")
}

func TestCheckOrphanedRecordFails(t *testing.T) {
	v1 := mustExtract(t, doc("1", ``, `"a":{"type":"string"}`))
	problems := Check(baselineOf(v1), map[string]Entry{})
	wantProblem(t, problems, schemaPath, "schema removed", "BREAKING CHANGE:")
}

func TestCheckProblemsAreSorted(t *testing.T) {
	b := Baseline{Format: 1, Schemas: map[string][]Entry{}}
	live := map[string]Entry{
		"z/result.schema.json": mustExtract(t, doc("1", ``, `"a":{"type":"string"}`)),
		"a/result.schema.json": mustExtract(t, doc("1", ``, `"a":{"type":"string"}`)),
	}
	problems := Check(b, live)
	if len(problems) != 2 || !strings.Contains(problems[0], "a/result") || !strings.Contains(problems[1], "z/result") {
		t.Fatalf("problems not sorted by schema path: %q", problems)
	}
}
