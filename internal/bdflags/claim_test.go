package bdflags

import (
	"io"
	"reflect"
	"testing"

	"github.com/spf13/pflag"
)

func TestClaimFlag(t *testing.T) {
	cases := []struct {
		sub  string
		args []string
		want string
	}{
		{"update", []string{"x", "--claim"}, "--claim"},
		{"update", []string{"x", "--claim=true"}, "--claim"},
		{"update", []string{"x", "--claim=false"}, ""},
		{"update", []string{"x", "--claim=0"}, ""},
		{"update", []string{"x", "--claim=maybe"}, "--claim"}, // unparsable: set
		{"update", []string{"x", "--claim", "--claim=false"}, ""},
		{"update", []string{"x", "--claim=false", "--claim"}, "--claim"},
		{"update", []string{"x", "--", "--claim"}, ""},
		{"update", []string{"x", "--append-notes", "--claim"}, ""}, // a value
		{"ready", []string{"--claim"}, "--claim"},
		{"close", []string{"x", "--claim-next"}, "--claim-next"},
		{"close", []string{"x", "--continue"}, "--continue"},
		{"close", []string{"x", "--continue", "--no-auto"}, ""},
		{"close", []string{"x", "--continue", "--no-auto=true"}, ""},
		{"close", []string{"x", "--continue", "--no-auto=0"}, "--continue"},
		{"close", []string{"x", "--continue", "--no-auto=f"}, "--continue"},
		{"close", []string{"x", "--continue", "--no-auto=F"}, "--continue"},
		{"close", []string{"x", "--continue", "--no-auto=nope"}, "--continue"}, // unparsable: unset
		{"close", []string{"x", "--continue", "--no-auto", "--no-auto=false"}, "--continue"},
		{"close", []string{"x", "--continue", "--reason", "--no-auto"}, "--continue"},
		{"close", []string{"x", "--continue=false", "--claim-next=0"}, ""},
		{"close", []string{"x"}, ""},
		{"show", []string{"--claim"}, ""},
	}
	for _, tc := range cases {
		got, ok := ClaimFlag(tc.sub, tc.args)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("ClaimFlag(%q, %v) = %q, %v; want %q", tc.sub, tc.args, got, ok, tc.want)
		}
	}
}

func TestBoolFlagValueAndFlagValues(t *testing.T) {
	vf := map[string]bool{"--append-notes": true}
	if p, on, v := BoolFlagValue([]string{"--help=false"}, vf, "--help", "-h"); !p || on || !v {
		t.Errorf("--help=false = %v %v %v", p, on, v)
	}
	if p, _, _ := BoolFlagValue([]string{"--append-notes", "--help"}, vf, "--help", "-h"); p {
		t.Error("a --help in a value position read as the flag")
	}
	if p, on, v := BoolFlagValue([]string{"-h", "-h=0"}, vf, "--help", "-h"); !p || on || !v {
		t.Errorf("last -h=0 = %v %v %v", p, on, v)
	}
	got := FlagValues([]string{"--actor", "a", "--append-notes", "--actor", "--actor=b", "--", "--actor", "c"}, vf, "--actor")
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("FlagValues = %v", got)
	}
}

// FlagValues reads a shorthand in every spelling pflag accepts, inside a run
// of shorthands too, and leaves alone a value another flag of the run takes.
func TestFlagValuesReadsShorthandRuns(t *testing.T) {
	vf := map[string]bool{"-C": true, "--directory": true, "-a": true, "--append-notes": true}
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"-C", "/d"}, []string{"/d"}},
		{[]string{"-C=/d"}, []string{"/d"}},
		{[]string{"-C/d"}, []string{"/d"}},
		{[]string{"-C="}, []string{"="}}, // too short for "-C=value": "=" is attached
		{[]string{"-Cq"}, []string{"q"}},
		{[]string{"-qC", "/d"}, []string{"/d"}},
		{[]string{"-qC/d"}, []string{"/d"}},
		{[]string{"-qC=/d"}, []string{"/d"}},
		{[]string{"-qvC", "/d", "x"}, []string{"/d"}},
		{[]string{"-qC", "-C/e"}, []string{"-C/e"}}, // the next token is the value
		{[]string{"-qC", "--", "-C/e"}, []string{"--", "/e"}},
		{[]string{"-aC/d"}, nil},       // -a takes "C/d"
		{[]string{"-qa", "-C/d"}, nil}, // -a takes the next token
		{[]string{"-a=C/d"}, nil},
		{[]string{"-q=C/d"}, nil},
		{[]string{"--append-notes", "-C/d"}, nil},
		{[]string{"--", "-C/d"}, nil},
		{[]string{"-", "-C/d"}, []string{"/d"}},
		{[]string{"x", "-qC"}, nil}, // no value left
		{[]string{"-C/d", "--directory", "/e", "-qC=/f"}, []string{"/d", "/e", "/f"}},
	}
	for _, tc := range cases {
		if got := FlagValues(tc.args, vf, "-C", "--directory"); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("FlagValues(%q) = %q; want %q", tc.args, got, tc.want)
		}
	}
}

// TestFlagValuesAgreesWithPflag checks FlagValues against pflag itself: for
// every argv over a small token alphabet that pflag parses with the flags bd's
// update declares (in each pinned inventory), FlagValues reads, last, the -C
// value pflag keeps, and reads no -C where pflag reads none.
func TestFlagValuesAgreesWithPflag(t *testing.T) {
	alphabet := []string{
		"-C", "/d", "-C/d", "-C=/d", "-C=", "-Cq", "-qC", "-qC/d", "-qC=/d", "-q",
		"-qv", "-a", "-aC/d", "-a=C/d", "-qa", "--directory", "--directory=/e",
		"--append-notes", "--", "-", "x",
	}
	var argvs [][]string
	var gen func(prefix []string)
	gen = func(prefix []string) {
		if len(prefix) > 0 {
			argvs = append(argvs, append([]string(nil), prefix...))
		}
		if len(prefix) == 3 {
			return
		}
		for _, tok := range alphabet {
			gen(append(prefix, tok))
		}
	}
	gen(nil)
	for _, inv := range Inventories() {
		flags := inv.Flags("update")
		valueFlags := map[string]bool{}
		for _, f := range flags {
			if f.TakesValue() {
				valueFlags["--"+f.Name] = true
				if f.Shorthand != "" {
					valueFlags["-"+f.Shorthand] = true
				}
			}
		}
		parsed := 0
		for _, argv := range argvs {
			// A fresh flag set per argv: pflag keeps parse state on its flags.
			fs := pflag.NewFlagSet("update", pflag.ContinueOnError)
			fs.SetOutput(io.Discard)
			for _, f := range flags {
				addMirrorFlag(fs, f)
			}
			if err := fs.Parse(argv); err != nil {
				continue // bd runs nothing
			}
			parsed++
			got := FlagValues(argv, valueFlags, "-C", "--directory")
			dir := fs.Lookup("directory")
			switch {
			case !dir.Changed && len(got) > 0:
				t.Errorf("%s: %q: FlagValues reads -C %q, pflag reads none", inv.Name, argv, got)
			case dir.Changed && (len(got) == 0 || got[len(got)-1] != dir.Value.String()):
				t.Errorf("%s: %q: FlagValues reads -C %q, pflag keeps %q", inv.Name, argv, got, dir.Value.String())
			}
		}
		if parsed < 1000 {
			t.Fatalf("%s: only %d argvs parsed; the alphabet no longer exercises FlagValues", inv.Name, parsed)
		}
	}
}
