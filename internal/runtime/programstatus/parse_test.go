package programstatus

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func mustParse(t *testing.T, body string) Report {
	t.Helper()
	r, err := ParseBody(body)
	if err != nil {
		t.Fatalf("ParseBody(%q): %v", body, err)
	}
	return r
}

func mustDrop(t *testing.T, body string) {
	t.Helper()
	r, err := ParseBody(body)
	if !errors.Is(err, ErrDropped) {
		t.Fatalf("ParseBody(%q) = %+v, %v; want ErrDropped", body, r, err)
	}
}

func TestParseBodyCapturedReports(t *testing.T) {
	cases := []struct {
		body string
		want Report
	}{
		{"state=idle:app=claude-code", Report{State: runtime.ProgramIdle, App: "claude-code", Progress: -1}},
		{"state=blocked:app=codex:kind=permission", Report{State: runtime.ProgramBlocked, Kind: runtime.ProgramBlockPermission, App: "codex", Progress: -1}},
		{
			"state=blocked:app=claude-code:kind=question:msg=YW5zd2VyOiBXaGljaCBjb2xvciBkbyB5b3UgcHJlZmVyPyAoUmVkIMK3IEJsdWUp",
			Report{State: runtime.ProgramBlocked, Kind: runtime.ProgramBlockQuestion, App: "claude-code", Progress: -1, Msg: "answer: Which color do you prefer? (Red · Blue)"},
		},
		{
			"state=working:app=claude-code:id=ab1d92669c348ddab:title=bW9jayBzdWJhZ2VudCAx",
			Report{State: runtime.ProgramWorking, ID: "ab1d92669c348ddab", App: "claude-code", Progress: -1, Title: "mock subagent 1"},
		},
		{"state=working:id=root/task-1:progress=40:app=gen", Report{State: runtime.ProgramWorking, ID: "root/task-1", Progress: 40, App: "gen"}},
		{"state=clear:id=ab1d92669c348ddab", Report{Clear: true, ID: "ab1d92669c348ddab", Progress: -1}},
		{"state=clear", Report{Clear: true, Progress: -1}},
		{"state=error:app=claude-code:msg=QVBJIEVycm9yOiA0MDAgbW9jayBpbnZhbGlkX3JlcXVlc3RfZXJyb3I=", Report{State: runtime.ProgramError, App: "claude-code", Progress: -1, Msg: "API Error: 400 mock invalid_request_error"}},
	}
	for _, tc := range cases {
		if got := mustParse(t, tc.body); got != tc.want {
			t.Errorf("ParseBody(%q)\n got %+v\nwant %+v", tc.body, got, tc.want)
		}
	}
}

func TestParseBodyProbe(t *testing.T) {
	if _, err := ParseBody("?"); !errors.Is(err, ErrProbe) {
		t.Fatalf("ParseBody(?) err = %v, want ErrProbe", err)
	}
}

func TestParseBodyStateIsRequiredAndKnown(t *testing.T) {
	for _, body := range []string{"", "app=x", "state=", "state=busy", "state=IDLE", "state=idle extra"} {
		mustDrop(t, body)
	}
}

func TestParseBodyPairRules(t *testing.T) {
	// Whitespace around keys and values is trimmed.
	if got := mustParse(t, " state = working : app = x "); got.State != runtime.ProgramWorking || got.App != "x" {
		t.Errorf("trim: got %+v", got)
	}
	// Last duplicate wins.
	if got := mustParse(t, "state=idle:state=working:app=a:app=b"); got.State != runtime.ProgramWorking || got.App != "b" {
		t.Errorf("last wins: got %+v", got)
	}
	// Unknown keys are ignored.
	if got := mustParse(t, "state=idle:future=1:zz=y"); got.State != runtime.ProgramIdle {
		t.Errorf("unknown keys: got %+v", got)
	}
	// Malformed pairs are skipped: no '=', empty key, key outside [a-z], value byte outside the charset.
	got := mustParse(t, "state=done:noequals:=v:Key=v:app=a;b:app2=x:title=a b")
	if got.State != runtime.ProgramDone || got.App != "" || got.Title != "" {
		t.Errorf("malformed: got %+v", got)
	}
	// A malformed later duplicate does not erase the earlier valid value.
	if got := mustParse(t, "state=idle:app=good:app=bad value"); got.App != "good" {
		t.Errorf("malformed duplicate: got %+v", got)
	}
	// A malformed state pair leaves state missing: dropped.
	mustDrop(t, "state=id le")
}

func TestParseBodyKeyLimit(t *testing.T) {
	if got := mustParse(t, "state=idle:"+strings.Repeat("k", 16)+"=1"); got.State != runtime.ProgramIdle {
		t.Errorf("16-byte key: got %+v", got)
	}
	mustDrop(t, "state=idle:"+strings.Repeat("k", 17)+"=1")
}

func TestParseBodyKindProgressApp(t *testing.T) {
	// Unknown kind reads as absent; kind only applies with blocked.
	if got := mustParse(t, "state=blocked:kind=telepathy"); got.Kind != runtime.ProgramBlockUnspecified {
		t.Errorf("unknown kind: %+v", got)
	}
	if got := mustParse(t, "state=working:kind=auth"); got.Kind != runtime.ProgramBlockUnspecified {
		t.Errorf("kind on working: %+v", got)
	}
	if got := mustParse(t, "state=blocked:kind=auth"); got.Kind != runtime.ProgramBlockAuth {
		t.Errorf("auth: %+v", got)
	}
	progress := map[string]int{
		"state=working:progress=0":    0,
		"state=working:progress=100":  100,
		"state=blocked:progress=7":    7,
		"state=working:progress=101":  -1,
		"state=working:progress=-1":   -1,
		"state=working:progress=+5":   -1,
		"state=working:progress=4.5":  -1,
		"state=working:progress=":     -1,
		"state=idle:progress=50":      -1,
		"state=done:progress=100":     -1,
		"state=working:progress=0050": -1,
	}
	for body, want := range progress {
		if got := mustParse(t, body); got.Progress != want {
			t.Errorf("%s: progress %d, want %d", body, got.Progress, want)
		}
	}
	for app, want := range map[string]string{
		"claude-code":           "claude-code",
		"a.b_c+d-e":             "a.b_c+d-e",
		"a/b":                   "",
		"a,b":                   "",
		"x=y":                   "",
		"":                      "",
		strings.Repeat("a", 32): strings.Repeat("a", 32),
	} {
		if got := mustParse(t, "state=idle:app="+app); got.App != want {
			t.Errorf("app %q: got %q, want %q", app, got.App, want)
		}
	}
	// app longer than its 32-byte limit breaks a limit: the report is discarded whole.
	mustDrop(t, "state=idle:app="+strings.Repeat("a", 33))
}

func TestParseBodyIDGrammar(t *testing.T) {
	seg32 := strings.Repeat("s", 32)
	for _, id := range []string{"a", "build/test", "a.b_c+d-e/f", seg32, "1/2/3/4/5/6/7/8"} {
		if got := mustParse(t, "state=working:id="+id); got.ID != id {
			t.Errorf("id %q: got %+v", id, got)
		}
	}
	long := strings.Repeat(strings.Repeat("x", 31)+"/", 4) + "x" // 129 bytes, 5 levels
	for _, id := range []string{"", "/a", "a/", "a//b", seg32 + "s", "1/2/3/4/5/6/7/8/9", "a,b", "a=b", long} {
		mustDrop(t, "state=working:id="+id)
	}
}

func TestParseBodyTextLimits(t *testing.T) {
	// Padding is optional.
	if got := mustParse(t, "state=idle:msg=aGk"); got.Msg != "hi" {
		t.Errorf("unpadded: %+v", got)
	}
	if got := mustParse(t, "state=idle:msg=aGk="); got.Msg != "hi" {
		t.Errorf("padded: %+v", got)
	}
	// Invalid base64 drops the report.
	mustDrop(t, "state=idle:msg=a")
	mustDrop(t, "state=idle:msg=a=Gk")
	mustDrop(t, "state=idle:title=ab,c")
	// Decoded limits: msg 2048, title 192.
	if got := mustParse(t, "state=idle:msg="+b64(strings.Repeat("m", 2048))); len(got.Msg) != 2048 {
		t.Errorf("msg at limit: %d", len(got.Msg))
	}
	mustDrop(t, "state=idle:msg="+b64(strings.Repeat("m", 2049)))
	if got := mustParse(t, "state=idle:title="+b64(strings.Repeat("t", 192))); len(got.Title) != 192 {
		t.Errorf("title at limit: %d", len(got.Title))
	}
	mustDrop(t, "state=idle:title="+b64(strings.Repeat("t", 193)))
	// Encoded limits are checked before decoding.
	mustDrop(t, "state=idle:msg="+strings.Repeat("A", 2733))
	mustDrop(t, "state=idle:title="+strings.Repeat("A", 257))
	// Decoded text must be UTF-8.
	mustDrop(t, "state=idle:msg="+base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}))
}

func TestParseBodyDropsReportWithControlCharsInText(t *testing.T) {
	for _, text := range []string{"line\nbreak", "tab\there", "nul\x00", "esc\x1b[31m", "del\x7f", "c1\u0085", "csi\u009b"} {
		mustDrop(t, "state=blocked:msg="+b64(text))
		mustDrop(t, "state=working:title="+b64(text))
	}
}

func TestParseBodyStripsBidiFromText(t *testing.T) {
	in := "a\u202ebidi\u202c \u200bzero\u200f\u061c\u2066iso\u2069\ufeffend"
	got := mustParse(t, "state=blocked:msg="+b64(in)+":title="+b64(in))
	const want = "abidi zeroisoend"
	if got.Msg != want || got.Title != want {
		t.Errorf("stripped msg %q title %q, want %q", got.Msg, got.Title, want)
	}
	// Ordinary non-ASCII text is kept.
	if got := mustParse(t, "state=idle:msg="+b64("café · 日本")); got.Msg != "café · 日本" {
		t.Errorf("non-ASCII: %q", got.Msg)
	}
}

// TestParseBodyStripsControlAndBidiFromText is the bead's name for the two
// text rules together: control characters drop the whole report (the spec
// says discard), bidi and invisible formatting characters are stripped.
func TestParseBodyStripsControlAndBidiFromText(t *testing.T) {
	mustDrop(t, "state=idle:msg="+b64("x\u202e\n"))
	if got := mustParse(t, "state=idle:msg="+b64("x\u202ey")); got.Msg != "xy" {
		t.Errorf("got %q", got.Msg)
	}
}

func TestParseBodyWholeSequenceLimit(t *testing.T) {
	// The whole sequence (ESC ] 7501 ; body BEL) may be 4096 bytes.
	pad := func(n int) string { return "state=idle:" + "zz=" + strings.Repeat("a", n-len("state=idle:zz=")) }
	limit := MaxSequence - len(oscPrefix7501) - 1
	if _, err := ParseBody(pad(limit)); err != nil {
		t.Fatalf("body at limit: %v", err)
	}
	mustDrop(t, pad(limit+1))
}
