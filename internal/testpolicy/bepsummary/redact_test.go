package bepsummary

import (
	_ "embed"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// redactProgram is the jq program bazel.yml runs on each lane's BEP file
// before uploading it: only the fields bepEvent decodes leave the runner.
//
//go:embed redact.jq
var redactProgram string

// jsonFieldNames collects the JSON names of every field reachable from t.
func jsonFieldNames(t reflect.Type, into map[string]bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name != "" && name != "-" {
			into[name] = true
		}
		jsonFieldNames(f.Type, into)
	}
}

// A field bep.go starts reading but redact.jq drops would silently zero that
// column in bazel.yml's report: the redacted upload is all the bep-summary
// job sees.
func TestRedactProgramKeepsEveryDecodedField(t *testing.T) {
	names := map[string]bool{}
	jsonFieldNames(reflect.TypeOf(bepEvent{}), names)
	if len(names) < 10 {
		t.Fatalf("found only %d JSON field names in bepEvent; the walk is broken", len(names))
	}
	var missing []string
	for name := range names {
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(redactProgram) {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("redact.jq does not keep BEP field(s) %v that bep.go decodes; add them to its projection", missing)
	}
}
