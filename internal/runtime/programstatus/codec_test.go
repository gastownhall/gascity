package programstatus

import (
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// serverSanitizer is the tmux read format's filter,
// #{s/[^-+A-Za-z0-9=_.|%]//:@opt}: every byte outside the alphabet is removed.
var serverSanitizer = regexp.MustCompile(`[^-+A-Za-z0-9=_.|%]`)

func sanitize(v string) string { return serverSanitizer.ReplaceAllString(v, "") }

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// sameStatus compares two readings, times by instant.
func sameStatus(a, b runtime.ProgramStatus) bool {
	if !a.Since.Equal(b.Since) || !a.QuietSince.Equal(b.QuietSince) {
		return false
	}
	a.Since, a.QuietSince, b.Since, b.QuietSince = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	return a == b
}

func sampleStatus() runtime.ProgramStatus {
	return runtime.ProgramStatus{
		State:           runtime.ProgramBlocked,
		Kind:            runtime.ProgramBlockPermission,
		App:             "claude-code",
		Progress:        40,
		Since:           time.UnixMilli(1793408464412),
		QuietSince:      time.UnixMilli(1793408400000),
		Nonce:           "a1B2-c_3.%12",
		Seq:             17,
		TurnStarts:      3,
		TurnEnds:        2,
		Children:        2,
		WorkingChildren: 1,
		Probed:          true,
		LastError:       true,
		Reported:        true,
	}
}

func TestEncodeGoldenFormat(t *testing.T) {
	got, err := Encode(sampleStatus())
	if err != nil {
		t.Fatal(err)
	}
	const want = "v1|a1B2-c_3.%12|17|blocked|permission|1793408464412|1793408400000|3|2|40|1|2|PER|claude-code"
	if got != want {
		t.Fatalf("Encode\n got %s\nwant %s", got, want)
	}
	empty, err := Encode(runtime.ProgramStatus{Nonce: "r.%0", Progress: -1})
	if err != nil {
		t.Fatal(err)
	}
	if empty != "v1|r.%0|0|||0|0|0|0||0|0||" {
		t.Fatalf("empty record %q", empty)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	statuses := []runtime.ProgramStatus{
		sampleStatus(),
		{Nonce: "r.%0", Progress: -1},
		{Nonce: "r.%0", Progress: -1, State: runtime.ProgramIdle, App: "a+b.c_d-e", Since: time.UnixMilli(1), Seq: 1},
		{Nonce: "r.%0", Progress: 0, State: runtime.ProgramWorking, Since: time.UnixMilli(5), Seq: 2, Probed: true},
		{Nonce: "r.%0", Progress: -1, Seq: 9, LastError: true, TurnEnds: 4, TurnStarts: 4, QuietSince: time.UnixMilli(7)},
		{Nonce: "r.%0", Progress: -1, Seq: 3, Reported: true},
		{Nonce: "r.%0", Progress: -1, Seq: 3, Probed: true, Reported: true},
	}
	for _, s := range statuses {
		v, err := Encode(s)
		if err != nil {
			t.Fatalf("Encode(%+v): %v", s, err)
		}
		got, err := Decode(v)
		if err != nil {
			t.Fatalf("Decode(%q): %v", v, err)
		}
		if !sameStatus(got, s) {
			t.Fatalf("round trip %q\n got %+v\nwant %+v", v, got, s)
		}
	}
}

func TestEncodeSurvivesServerSanitizerUnchanged(t *testing.T) {
	s := sampleStatus()
	for _, app := range []string{"claude-code", "codex", "a+b", "x.y_z-1", strings.Repeat("Z", 32)} {
		s.App = app
		v, err := Encode(s)
		if err != nil {
			t.Fatal(err)
		}
		if sanitize(v) != v {
			t.Errorf("sanitizer changed %q to %q", v, sanitize(v))
		}
	}
	// Text of every byte value that survives the parser's rules.
	var all strings.Builder
	for r := rune(0x20); r < 0x2100; r++ {
		if r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			continue
		}
		all.WriteRune(r)
	}
	text := Text{Title: "日本 · title", Msg: all.String(), LastErrorMsg: "API Error: 400 ~!@#$%^&*()"}
	v, err := EncodeText(s.Nonce, text)
	if err != nil {
		t.Fatal(err)
	}
	if sanitize(v) != v {
		t.Fatalf("sanitizer changed the text value")
	}
	nonce, got, err := DecodeText(sanitize(v))
	if err != nil || nonce != s.Nonce {
		t.Fatalf("DecodeText: %q %v", nonce, err)
	}
	if got.Title != text.Title || got.LastErrorMsg != text.LastErrorMsg {
		t.Fatalf("text round trip %+v", got)
	}
	if len(got.Msg) > MaxEncodedText || !strings.HasPrefix(text.Msg, got.Msg) {
		t.Fatalf("msg not truncated to a %d-byte prefix: %d bytes", MaxEncodedText, len(got.Msg))
	}
}

func TestEncodeTextTruncatesAtRuneBoundary(t *testing.T) {
	msg := strings.Repeat("a", MaxEncodedText-1) + "é" // é straddles the limit
	v, err := EncodeText("r.%1", Text{Msg: msg})
	if err != nil {
		t.Fatal(err)
	}
	_, got, err := DecodeText(v)
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg != strings.Repeat("a", MaxEncodedText-1) {
		t.Fatalf("truncated to %d bytes: %q", len(got.Msg), got.Msg[len(got.Msg)-3:])
	}
	if v2, _ := EncodeText("r.%1", Text{}); v2 != "v1|r.%1|||" {
		t.Fatalf("empty text %q", v2)
	}
}

func TestEncodeRejectsValuesOutsideTheAlphabet(t *testing.T) {
	for _, nonce := range []string{"", "r", "r.3", "r.%", "r|x.%1", ".%1", "r .%1", "r.%1a"} {
		s := sampleStatus()
		s.Nonce = nonce
		if _, err := Encode(s); err == nil {
			t.Errorf("Encode accepted nonce %q", nonce)
		}
		if _, err := EncodeText(nonce, Text{}); err == nil {
			t.Errorf("EncodeText accepted nonce %q", nonce)
		}
	}
	s := sampleStatus()
	s.App = "bad|app"
	if _, err := Encode(s); err == nil {
		t.Error("Encode accepted an app outside the alphabet")
	}
	s = sampleStatus()
	s.State = "busy"
	if _, err := Encode(s); err == nil {
		t.Error("Encode accepted an unknown state")
	}
}

func TestDecodeRejectsForgedFields(t *testing.T) {
	good := "v1|a1B2-c_3.%12|17|blocked|permission|1793408464412|1793408400000|3|2|40|1|2|PE|claude-code"
	if _, err := Decode(good); err != nil {
		t.Fatalf("good record: %v", err)
	}
	set := func(i int, v string) string {
		f := strings.Split(good, "|")
		f[i] = v
		return strings.Join(f, "|")
	}
	forged := map[string]string{
		"too few fields":            strings.TrimSuffix(good, "|claude-code"),
		"too many fields":           good + "|x",
		"nonce without pane":        set(1, "a1B2"),
		"nonce bad pane":            set(1, "a1B2.%x"),
		"empty nonce":               set(1, ""),
		"negative seq":              set(2, "-1"),
		"plus seq":                  set(2, "+1"),
		"leading zero seq":          set(2, "017"),
		"seq overflow":              set(2, "18446744073709551616"),
		"empty seq":                 set(2, ""),
		"unknown state":             set(3, "busy"),
		"clear is not a state":      set(3, "clear"),
		"unknown kind":              set(4, "telepathy"),
		"kind without blocked":      set(3, "working"),
		"bad since":                 set(5, "1.5"),
		"bad quiet":                 set(6, "x"),
		"starts overflow":           set(7, "4294967296"),
		"ends negative":             set(8, "-2"),
		"progress over 100":         set(9, "101"),
		"progress leading zero":     set(9, "040"),
		"wchildren over children":   set(10, "3"),
		"children huge":             set(11, "99999999999"),
		"unknown flag":              set(12, "X"),
		"duplicate flag":            set(12, "PP"),
		"flags out of order":        set(12, "EP"),
		"reported before error":     set(12, "PRE"),
		"duplicate reported":        set(12, "RR"),
		"app too long":              set(13, strings.Repeat("a", 33)),
		"app outside charset":       set(13, "a=b"),
		"lowercase version":         "V1" + strings.TrimPrefix(good, "v1"),
		"garbage":                   "hello",
		"injected separator in app": good + "|",
	}
	for name, v := range forged {
		if _, err := Decode(v); !errors.Is(err, ErrUndecodable) {
			t.Errorf("%s: Decode(%q) err = %v, want ErrUndecodable", name, v, err)
		}
	}
	// Progress only with working or blocked.
	idle := strings.Replace(good, "|blocked|permission|", "|idle||", 1)
	if _, err := Decode(idle); !errors.Is(err, ErrUndecodable) {
		t.Errorf("progress on idle accepted: %v", err)
	}
	// Bytes the read sanitizer would strip never decode.
	for _, b := range []string{" ", ";", "#", "\n", "\x1b", "/", ":"} {
		if _, err := Decode(set(13, "claude"+b+"code")); !errors.Is(err, ErrUndecodable) {
			t.Errorf("byte %q accepted", b)
		}
	}
	// Text values are validated too.
	for _, v := range []string{
		"v1|r.%1||",
		"v1|r.%1||||",
		"v1|r.%1|!!||",
		"v1|r.%1|" + b64url("a\nb") + "||",
		"v1|r.%1|" + b64url("\x1b[31m") + "||",
		"v1|r.%1|||" + b64url(string([]byte{0xff})),
		"v1|r.%1|" + b64url(strings.Repeat("a", MaxEncodedText+1)) + "||",
	} {
		if _, _, err := DecodeText(v); !errors.Is(err, ErrUndecodable) {
			t.Errorf("DecodeText(%q) err = %v, want ErrUndecodable", v, err)
		}
	}
}

func TestDecodeUnknownVersionIsDistinct(t *testing.T) {
	for _, v := range []string{"v2|r.%1|whatever", "v10", "v0|x"} {
		_, err := Decode(v)
		if !errors.Is(err, ErrUnknownVersion) || errors.Is(err, ErrUndecodable) {
			t.Errorf("Decode(%q) err = %v, want ErrUnknownVersion only", v, err)
		}
		if RecordForm(err) != runtime.ProgramRecordUnknownVersion {
			t.Errorf("RecordForm(%v) = %q", err, RecordForm(err))
		}
		if _, _, err := DecodeText(v); !errors.Is(err, ErrUnknownVersion) {
			t.Errorf("DecodeText(%q) err = %v, want ErrUnknownVersion", v, err)
		}
	}
	if _, err := Decode(""); !errors.Is(err, ErrAbsent) || RecordForm(err) != runtime.ProgramRecordAbsent {
		t.Errorf("empty value: %v", err)
	}
	if _, err := Decode("v1|x"); RecordForm(err) != runtime.ProgramRecordUndecodable {
		t.Errorf("undecodable form: %q", RecordForm(err))
	}
	if RecordForm(nil) != runtime.ProgramRecordDecoded {
		t.Errorf("nil form: %q", RecordForm(nil))
	}
}

func TestStoreStatusEncodes(t *testing.T) {
	s := NewStore(testNonce)
	s.Probe()
	apply(t, s, "state=blocked:kind=question:app=claude-code:progress=5:msg="+b64("answer?"), at(1))
	apply(t, s, "state=working:id=sub:title="+b64("sub 1"), at(2))
	v, err := Encode(s.Status())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(v)
	if err != nil {
		t.Fatal(err)
	}
	if !sameStatus(got, s.Status()) {
		t.Fatalf("store status round trip\n got %+v\nwant %+v", got, s.Status())
	}
	tv, err := EncodeText(testNonce, s.Text())
	if err != nil {
		t.Fatal(err)
	}
	if _, txt, err := DecodeText(tv); err != nil || txt.Msg != "answer?" {
		t.Fatalf("text %+v %v", txt, err)
	}
}
