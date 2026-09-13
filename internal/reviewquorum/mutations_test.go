package reviewquorum

import (
	"reflect"
	"testing"
)

func TestMutationDeltaIgnoresPreExistingDirtyAndUntrackedEntries(t *testing.T) {
	before := `
 M existing.go
?? scratch.txt
`
	after := `
 M existing.go
?? scratch.txt
 M new-change.go
`

	got := MutationDeltaFromPorcelain(before, after)
	want := MutationsDelta{Changed: []StatusEntry{
		{Path: "new-change.go", Status: "M"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MutationDeltaFromPorcelain() = %+v, want %+v", got, want)
	}
}

func TestMutationDeltaReportsPreExistingEntryWhenStatusChanges(t *testing.T) {
	before := `
 M existing.go
?? scratch.txt
`
	after := `
MM existing.go
 M scratch.txt
`

	got := MutationDeltaFromPorcelain(before, after)
	want := MutationsDelta{Changed: []StatusEntry{
		{Path: "existing.go", Status: "MM"},
		{Path: "scratch.txt", Status: "M"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MutationDeltaFromPorcelain() = %+v, want %+v", got, want)
	}
}

func TestMutationDeltaReportsPreExistingEntryWhenRemovedFromStatus(t *testing.T) {
	before := `
 M existing.go
?? scratch.txt
`
	after := `
?? scratch.txt
`

	got := MutationDeltaFromPorcelain(before, after)
	want := MutationsDelta{Changed: []StatusEntry{
		{Path: "existing.go", Status: "clean"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MutationDeltaFromPorcelain() = %+v, want %+v", got, want)
	}
}

func TestParseStatusPorcelainUsesRenameDestination(t *testing.T) {
	got := ParseStatusPorcelain("R  old.go -> new.go\n")
	want := []StatusEntry{{Path: "new.go", Status: "R"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseStatusPorcelain() = %+v, want %+v", got, want)
	}
}

func TestParseStatusPorcelainZUsesRenameDestination(t *testing.T) {
	got := ParseStatusPorcelain("R  new -> literal.go\x00old -> literal.go\x00")
	want := []StatusEntry{{Path: "new -> literal.go", Status: "R"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseStatusPorcelain() = %+v, want %+v", got, want)
	}
}

func TestParseStatusPorcelainUnquotesQuotedPath(t *testing.T) {
	got := ParseStatusPorcelain(` M "dir/file with spaces.go"` + "\n")
	want := []StatusEntry{{Path: "dir/file with spaces.go", Status: "M"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseStatusPorcelain() = %+v, want %+v", got, want)
	}
}

func TestParseStatusPorcelainDoesNotSplitArrowInNonRenamePath(t *testing.T) {
	got := ParseStatusPorcelain(" M fixtures/name -> literal.go\n")
	want := []StatusEntry{{Path: "fixtures/name -> literal.go", Status: "M"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseStatusPorcelain() = %+v, want %+v", got, want)
	}
}
