package genshell

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// jqRun runs runJQ over stdin and returns its streams and status.
func jqRun(t *testing.T, stdin string, args ...string) (stdout, stderr string, status uint8) {
	t.Helper()
	var out, errOut bytes.Buffer
	status = runJQ(context.Background(), args, strings.NewReader(stdin), &out, &errOut, []string{"GC_X=from-env"})
	return out.String(), errOut.String(), status
}

func TestJQ(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		stdin      string
		wantOut    string
		wantStatus uint8
		wantErr    string
	}{
		{
			name:    "pretty by default, keys sorted, nothing HTML-escaped",
			args:    []string{"."},
			stdin:   `{"b":1,"a":[1,{}],"c":"<&>","d":[]}`,
			wantOut: "{\n  \"a\": [\n    1,\n    {}\n  ],\n  \"b\": 1,\n  \"c\": \"<&>\",\n  \"d\": []\n}\n",
		},
		{
			name:    "compact",
			args:    []string{"-c", "."},
			stdin:   `{"b":1,"a":[1,2],"c":"<&>"}`,
			wantOut: "{\"a\":[1,2],\"b\":1,\"c\":\"<&>\"}\n",
		},
		{
			name:    "long compact",
			args:    []string{"--compact-output", ".[0]"},
			stdin:   `[{"id":"x"}]`,
			wantOut: "{\"id\":\"x\"}\n",
		},
		{
			name:    "raw string",
			args:    []string{"-r", ".[0].id // empty"},
			stdin:   `[{"id":"ga-1"}]`,
			wantOut: "ga-1\n",
		},
		{
			name:    "long raw string",
			args:    []string{"--raw-output", "."},
			stdin:   `"a\"b\\c"`,
			wantOut: "a\"b\\c\n",
		},
		{
			name:  "empty alternative writes nothing",
			args:  []string{"-r", ".[0].id // empty"},
			stdin: `[]`,
		},
		{
			name:    "raw number",
			args:    []string{"-r", "[.[] | select(((.status // \"\") | ascii_downcase) != \"closed\")] | length"},
			stdin:   `[{"status":"CLOSED"},{"status":"open"},{}]`,
			wantOut: "2\n",
		},
		{
			name:    "combined short flags",
			args:    []string{"-rc", ".[]"},
			stdin:   `["a",{"b":1}]`,
			wantOut: "a\n{\"b\":1}\n",
		},
		{
			name: "no input writes nothing",
			args: []string{"."},
		},
		{
			name:  "whitespace input writes nothing",
			args:  []string{"."},
			stdin: " \n\t\n",
		},
		{
			name:    "each input in turn",
			args:    []string{"-c", ". * 2"},
			stdin:   "1 2\n3",
			wantOut: "2\n4\n6\n",
		},
		{
			name:    "slurp",
			args:    []string{"-s", "(add // []) | unique_by(.id) | length"},
			stdin:   "[{\"id\":\"a\"},{\"id\":\"b\"}]\n[{\"id\":\"a\"}]\n\n",
			wantOut: "2\n",
		},
		{
			name:    "slurp of nothing",
			args:    []string{"-c", "-s", "."},
			wantOut: "[]\n",
		},
		{
			name:    "slurp of empty lines",
			args:    []string{"-s", "(add // []) | length"},
			stdin:   "\n\n\n",
			wantOut: "0\n",
		},
		{
			name:    "null input",
			args:    []string{"-n", "1 + 1"},
			stdin:   "ignored",
			wantOut: "2\n",
		},
		{
			name:    "arg",
			args:    []string{"--arg", "id", "rig1/worker", "-c", "[.[] | select((.assignee // \"\") == $id)] | .[:1]"},
			stdin:   `[{"id":"a","assignee":"other"},{"id":"b","assignee":"rig1/worker"},{"id":"c","assignee":"rig1/worker"}]`,
			wantOut: "[{\"assignee\":\"rig1/worker\",\"id\":\"b\"}]\n",
		},
		{
			name:    "argjson",
			args:    []string{"-c", "--argjson", "bb", `[{"id":"d","status":"open"}]`, "map(. + {blocked_by: $bb})"},
			stdin:   `[{"id":"a"}]`,
			wantOut: "[{\"blocked_by\":[{\"id\":\"d\",\"status\":\"open\"}],\"id\":\"a\"}]\n",
		},
		{
			name:    "a later arg replaces an earlier one",
			args:    []string{"--arg", "v", "first", "--arg", "v", "second", "-r", "$v"},
			stdin:   `null`,
			wantOut: "second\n",
		},
		{
			name:    "the generated blocker projection",
			args:    []string{"-c", `[.[0].dependencies[]? | select(.dependency_type == "blocks" or .dependency_type == "waits-for" or .dependency_type == "conditional-blocks") | {id, status}]`},
			stdin:   `[{"id":"x","dependencies":[{"id":"d1","status":"open","dependency_type":"blocks"},{"id":"d2","status":"closed","dependency_type":"parent-child"},{"id":"d3","dependency_type":"waits-for"}]}]`,
			wantOut: "[{\"id\":\"d1\",\"status\":\"open\"},{\"id\":\"d3\",\"status\":null}]\n",
		},
		{
			name:    "a missing dependency list",
			args:    []string{"-c", `[.[0].dependencies[]? | {id}]`},
			stdin:   `[{"id":"x"}]`,
			wantOut: "[]\n",
		},
		{
			name:    "sort_by over two keys",
			args:    []string{"-c", `sort_by(.created_at // "", .id // "") | map(.id)`},
			stdin:   `[{"id":"c","created_at":"2026-01-02"},{"id":"b","created_at":"2026-01-01"},{"id":"a","created_at":"2026-01-02"},{"id":"z"}]`,
			wantOut: "[\"z\",\"b\",\"a\",\"c\"]\n",
		},
		{
			name:    "graph-anchor selection",
			args:    []string{"-r", `.[0] | select(((.metadata["gc.kind"] // "") == "workflow") and ((.metadata["gc.formula_contract"] // "") == "graph.v2")) | .id // empty`},
			stdin:   `[{"id":"anchor","metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2"}}]`,
			wantOut: "anchor\n",
		},
		{
			name:    "numbers keep their text",
			args:    []string{"-c", "."},
			stdin:   `[12345678901234567890, 1.50, 0, -2]`,
			wantOut: "[12345678901234567890,1.50,0,-2]\n",
		},
		{
			name:    "strings outside ASCII and control characters",
			args:    []string{"-c", "."},
			stdin:   "\"é☃\\u0001\\u007f\\t\"",
			wantOut: "\"é☃\\u0001\\u007f\\t\"\n",
		},
		{
			name:    "environment",
			args:    []string{"-n", "-r", "$ENV.GC_X, env.GC_X"},
			wantOut: "from-env\nfrom-env\n",
		},
		{
			name:       "runtime error, then the next input",
			args:       []string{"-c", ".[]"},
			stdin:      `1 [2]`,
			wantOut:    "2\n",
			wantStatus: 5,
			wantErr:    "cannot iterate",
		},
		{
			name:       "halt_error",
			args:       []string{"-n", `"bye\n" | halt_error(1)`},
			wantStatus: 1,
			wantErr:    "bye\n",
		},
		{
			name:    "halt stops every input",
			args:    []string{"-c", "., halt"},
			stdin:   "1 2",
			wantOut: "1\n",
		},
		{
			name:       "input that is not JSON",
			args:       []string{"-c", "."},
			stdin:      `1 {`,
			wantOut:    "1\n",
			wantStatus: 2,
			wantErr:    "cannot parse input",
		},
		{
			name:       "compile error",
			args:       []string{".["},
			stdin:      `1`,
			wantStatus: 3,
			wantErr:    "compile error",
		},
		{
			name:       "undefined variable",
			args:       []string{"$nope"},
			stdin:      `1`,
			wantStatus: 3,
			wantErr:    "compile error",
		},
		{
			name:       "argjson that is not JSON",
			args:       []string{"--argjson", "bb", "{", "."},
			wantStatus: 2,
			wantErr:    "--argjson",
		},
		{
			name:       "argjson with two values",
			args:       []string{"--argjson", "bb", "1 2", "."},
			wantStatus: 2,
			wantErr:    "--argjson",
		},
		{
			name:       "arg without its value",
			args:       []string{".", "--arg", "id"},
			wantStatus: 2,
			wantErr:    "--arg takes two parameters",
		},
		{
			name:       "unsupported short option",
			args:       []string{"-e", "."},
			wantStatus: 2,
			wantErr:    "unsupported option",
		},
		{
			name:       "unsupported long option",
			args:       []string{"--tab", "."},
			wantStatus: 2,
			wantErr:    "unsupported option",
		},
		{
			name:       "input file",
			args:       []string{".", "beads.json"},
			wantStatus: 2,
			wantErr:    "input files are not supported",
		},
		{
			name:       "no filter",
			args:       []string{"-r"},
			wantStatus: 2,
			wantErr:    "no filter",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, status := jqRun(t, tc.stdin, tc.args...)
			if stdout != tc.wantOut {
				t.Errorf("stdout = %q, want %q", stdout, tc.wantOut)
			}
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d (stderr %q)", status, tc.wantStatus, stderr)
			}
			if tc.wantErr == "" && stderr != "" {
				t.Errorf("stderr = %q, want nothing", stderr)
			}
			if !strings.Contains(stderr, tc.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.wantErr)
			}
		})
	}
}

func TestJQWithNoStdinIsEmptyInput(t *testing.T) {
	var out, errOut bytes.Buffer
	if status := runJQ(context.Background(), []string{"-s", "-c", "."}, nil, &out, &errOut, nil); status != 0 {
		t.Fatalf("status = %d, want 0 (stderr %q)", status, errOut.String())
	}
	if out.String() != "[]\n" {
		t.Errorf("stdout = %q, want %q", out.String(), "[]\n")
	}
}

func TestJQInAScriptPipeline(t *testing.T) {
	script := `r='[{"id":"ga-1","labels":["hold:mayor"]},{"id":"ga-2"}]'; ` +
		`n=$(printf "%s" "$r" | jq -r '[ (.[0].labels // [])[] | select(. == "hold:mayor" or . == "hold:external") ] | length' 2>/dev/null); ` +
		`id=$(printf "%s" "$r" | jq -r ".[1].id // empty"); ` +
		`none=$(printf "%s" "" | jq -r ".[0].id // empty"); ` +
		`printf "%s\n%s\n%s\n" "$r" "[]" "" | jq -s "(add // []) | unique_by(.id) | length" > /dev/null || exit $?; ` +
		`bad=$(printf "%s" "{" | jq . 2>/dev/null) || printf "bad:%s\n" "$?"; ` +
		`printf "n=%s id=%s none=[%s]\n" "$n" "$id" "$none"`
	stdout, stderr, err := runScript(t, script, Options{})
	if err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, stderr)
	}
	if want := "bad:2\nn=1 id=ga-2 none=[]\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}
