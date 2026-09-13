package webhookmatch

import "testing"

func mustParse(t *testing.T, raw string) map[string]any {
	t.Helper()
	body, err := ParseBody([]byte(raw))
	if err != nil {
		t.Fatalf("ParseBody(%s): %v", raw, err)
	}
	return body
}

func TestParseBody_RejectsNonObject(t *testing.T) {
	for _, raw := range []string{`[1,2,3]`, `"a string"`, `42`, `true`, `null`, ``, `{bad`} {
		if _, err := ParseBody([]byte(raw)); err == nil {
			t.Errorf("ParseBody(%q) = nil error, want error", raw)
		}
	}
}

func TestResolvePath(t *testing.T) {
	body := mustParse(t, `{
		"action": "opened",
		"pull_request": {"number": 1347, "state": "open", "draft": false, "merged_at": null},
		"issue": {"labels": [{"name": "bug"}, {"name": "status/needs-review"}]},
		"repository": {"full_name": "octo/hello"}
	}`)

	cases := []struct {
		path    string
		want    string
		wantOK  bool
		wantNil bool // resolved value is JSON null (ok=true, value=nil)
	}{
		{path: "action", want: "opened", wantOK: true},
		{path: "pull_request.number", want: "1347", wantOK: true},
		{path: "pull_request.state", want: "open", wantOK: true},
		{path: "pull_request.draft", want: "false", wantOK: true},
		{path: "repository.full_name", want: "octo/hello", wantOK: true},
		{path: "issue.labels.0.name", want: "bug", wantOK: true},
		{path: "issue.labels.1.name", want: "status/needs-review", wantOK: true},
		{path: "pull_request.merged_at", want: "", wantOK: true, wantNil: true},
		// misses — never panic, always (nil,false)
		{path: "", wantOK: false},
		{path: "missing", wantOK: false},
		{path: "pull_request.missing", wantOK: false},
		{path: "issue.labels.9.name", wantOK: false},   // index out of range
		{path: "issue.labels.-1.name", wantOK: false},  // negative index
		{path: "issue.labels.name", wantOK: false},     // non-numeric index into array
		{path: "action.nope", wantOK: false},           // segment past a scalar
		{path: "pull_request.number.x", wantOK: false}, // segment past a number
	}
	for _, tc := range cases {
		got, ok := resolvePath(body, tc.path)
		if ok != tc.wantOK {
			t.Errorf("resolvePath(%q) ok = %v, want %v", tc.path, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if tc.wantNil && got != nil {
			t.Errorf("resolvePath(%q) = %v, want JSON null", tc.path, got)
		}
		s, err := coerceToString(got)
		if err != nil {
			t.Errorf("coerceToString(%q): %v", tc.path, err)
			continue
		}
		if s != tc.want {
			t.Errorf("resolvePath(%q) coerced = %q, want %q", tc.path, s, tc.want)
		}
	}
}

func TestCoerceToString_Types(t *testing.T) {
	body := mustParse(t, `{
		"s": "text", "n": 1347, "big": 10000000000, "f": 3.5,
		"b": true, "z": null,
		"obj": {"a": 1}, "arr": [1, "two", false]
	}`)
	cases := map[string]string{
		"s":   "text",
		"n":   "1347",
		"big": "10000000000",
		"f":   "3.5",
		"b":   "true",
		"z":   "",
		"obj": `{"a":1}`,
		"arr": `[1,"two",false]`,
	}
	for path, want := range cases {
		v, ok := resolvePath(body, path)
		if !ok {
			t.Errorf("resolvePath(%q) missing", path)
			continue
		}
		got, err := coerceToString(v)
		if err != nil {
			t.Errorf("coerceToString(%q): %v", path, err)
			continue
		}
		if got != want {
			t.Errorf("coerceToString(%q) = %q, want %q", path, got, want)
		}
	}
}
