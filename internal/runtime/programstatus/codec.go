package programstatus

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The pane-option codec. Every value survives the read sanitizer
// #{s/[^-+A-Za-z0-9=_.|%]//:@opt} unchanged:
//
//	@gc_ps      v1|<rand>.%N|seq|state|kind|since_ms|quiet_ms|starts|ends|progress|wchildren|children|flags|app
//	@gc_ps_text v1|<rand>.%N|b64url(title)|b64url(msg)|b64url(last_error_msg)
//
// flags is a subset of "PE" in that order (P: probed, E: last error); an
// absent progress is empty; a zero time is 0.
const (
	// CodecVersion is the codec version this package reads and writes.
	CodecVersion = 1
	// MaxEncodedText bounds each text field, in bytes, before encoding;
	// longer text is truncated at a rune boundary.
	MaxEncodedText = 512

	codecTag     = "v1"
	recordFields = 14
	textFields   = 5
)

var (
	// ErrAbsent is returned by Decode and DecodeText for an empty value:
	// nothing was published.
	ErrAbsent = errors.New("programstatus: no record published")
	// ErrUndecodable wraps every reason a published value is rejected.
	ErrUndecodable = errors.New("programstatus: undecodable record")
	// ErrUnknownVersion is returned for a value written by a codec version
	// this package does not know. It is distinct from ErrUndecodable: a
	// reader seeing it under a live tap knows the tap is not ours to read.
	ErrUnknownVersion = errors.New("programstatus: unknown codec version")
)

// RecordForm maps a Decode or DecodeText result to the record form
// [runtime.JudgeProgramStatus] reads.
func RecordForm(err error) runtime.ProgramRecordForm {
	switch {
	case err == nil:
		return runtime.ProgramRecordDecoded
	case errors.Is(err, ErrAbsent):
		return runtime.ProgramRecordAbsent
	case errors.Is(err, ErrUnknownVersion):
		return runtime.ProgramRecordUnknownVersion
	default:
		return runtime.ProgramRecordUndecodable
	}
}

// Encode returns the @gc_ps value for s. Mode, Validity and ReaderVerified
// are judged at read time and not encoded. It fails when a field cannot be
// represented: an invalid nonce, an unknown state or kind, or an invalid app.
func Encode(s runtime.ProgramStatus) (string, error) {
	if !ValidNonce(s.Nonce) {
		return "", fmt.Errorf("encoding program status: invalid nonce %q", s.Nonce)
	}
	if s.State != "" && !knownState(s.State) {
		return "", fmt.Errorf("encoding program status: unknown state %q", s.State)
	}
	if !knownKind(s.Kind) {
		return "", fmt.Errorf("encoding program status: unknown kind %q", s.Kind)
	}
	if s.App != "" && !validApp(s.App) {
		return "", fmt.Errorf("encoding program status: invalid app %q", s.App)
	}
	if s.Progress > 100 {
		return "", fmt.Errorf("encoding program status: progress %d over 100", s.Progress)
	}
	if s.Children < 0 || s.WorkingChildren < 0 || s.WorkingChildren > s.Children {
		return "", fmt.Errorf("encoding program status: %d working of %d children", s.WorkingChildren, s.Children)
	}
	return encodeFields(s), nil
}

// encodeFields formats s without validating it.
func encodeFields(s runtime.ProgramStatus) string {
	progress := ""
	if s.Progress >= 0 {
		progress = strconv.Itoa(s.Progress)
	}
	flags := ""
	if s.Probed {
		flags += "P"
	}
	if s.LastError {
		flags += "E"
	}
	return strings.Join([]string{
		codecTag,
		s.Nonce,
		strconv.FormatUint(s.Seq, 10),
		string(s.State),
		string(s.Kind),
		encodeTime(s.Since),
		encodeTime(s.QuietSince),
		strconv.FormatUint(uint64(s.TurnStarts), 10),
		strconv.FormatUint(uint64(s.TurnEnds), 10),
		progress,
		strconv.Itoa(s.WorkingChildren),
		strconv.Itoa(s.Children),
		flags,
		s.App,
	}, "|")
}

// Decode parses an @gc_ps value. It returns [ErrAbsent] for an empty value,
// [ErrUnknownVersion] for another codec version, and an error wrapping
// [ErrUndecodable] for anything else that is not exactly what [Encode]
// writes: a wrong field count, a byte outside the alphabet, a non-canonical
// number, an unknown state, kind or flag, a kind without blocked, a progress
// without working or blocked, or more working children than children.
func Decode(v string) (runtime.ProgramStatus, error) {
	f, err := splitValue(v, recordFields)
	if err != nil {
		return runtime.ProgramStatus{}, err
	}
	bad := func(field, val string) (runtime.ProgramStatus, error) {
		return runtime.ProgramStatus{}, fmt.Errorf("%w: %s %q", ErrUndecodable, field, val)
	}
	s := runtime.ProgramStatus{Nonce: f[1], Progress: -1}
	if !ValidNonce(s.Nonce) {
		return bad("nonce", f[1])
	}
	if s.Seq, err = parseCanonical(f[2], 64); err != nil {
		return bad("seq", f[2])
	}
	s.State = runtime.ProgramState(f[3])
	if s.State != "" && !knownState(s.State) {
		return bad("state", f[3])
	}
	s.Kind = runtime.ProgramBlockKind(f[4])
	if !knownKind(s.Kind) || (s.Kind != "" && s.State != runtime.ProgramBlocked) {
		return bad("kind", f[4])
	}
	if s.Since, err = decodeTime(f[5]); err != nil {
		return bad("since", f[5])
	}
	if s.QuietSince, err = decodeTime(f[6]); err != nil {
		return bad("quiet", f[6])
	}
	starts, err := parseCanonical(f[7], 32)
	if err != nil {
		return bad("starts", f[7])
	}
	ends, err := parseCanonical(f[8], 32)
	if err != nil {
		return bad("ends", f[8])
	}
	s.TurnStarts, s.TurnEnds = uint32(starts), uint32(ends)
	if f[9] != "" {
		if s.Progress = parseProgress(f[9]); s.Progress < 0 || !busy(s.State) {
			return bad("progress", f[9])
		}
	}
	wchildren, err := parseCanonical(f[10], 31)
	if err != nil {
		return bad("wchildren", f[10])
	}
	children, err := parseCanonical(f[11], 31)
	if err != nil || wchildren > children {
		return bad("children", f[11])
	}
	s.WorkingChildren, s.Children = int(wchildren), int(children)
	switch f[12] {
	case "":
	case "P":
		s.Probed = true
	case "E":
		s.LastError = true
	case "PE":
		s.Probed, s.LastError = true, true
	default:
		return bad("flags", f[12])
	}
	if f[13] != "" && !validApp(f[13]) {
		return bad("app", f[13])
	}
	s.App = f[13]
	return s, nil
}

// EncodeText returns the @gc_ps_text value for nonce and t, truncating each
// field to [MaxEncodedText] bytes at a rune boundary.
func EncodeText(nonce string, t Text) (string, error) {
	if !ValidNonce(nonce) {
		return "", fmt.Errorf("encoding program status text: invalid nonce %q", nonce)
	}
	enc := func(s string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(truncateRunes(s, MaxEncodedText)))
	}
	return strings.Join([]string{codecTag, nonce, enc(t.Title), enc(t.Msg), enc(t.LastErrorMsg)}, "|"), nil
}

// DecodeText parses an @gc_ps_text value, with Decode's errors. Each field
// must be unpadded base64url of at most [MaxEncodedText] bytes of UTF-8 text
// with no control characters.
func DecodeText(v string) (nonce string, t Text, err error) {
	f, err := splitValue(v, textFields)
	if err != nil {
		return "", Text{}, err
	}
	if !ValidNonce(f[1]) {
		return "", Text{}, fmt.Errorf("%w: nonce %q", ErrUndecodable, f[1])
	}
	out := [3]string{}
	for i, enc := range f[2:] {
		raw, err := base64.RawURLEncoding.DecodeString(enc)
		if err != nil {
			return "", Text{}, fmt.Errorf("%w: text field %d: %w", ErrUndecodable, i, err)
		}
		if len(raw) > MaxEncodedText {
			return "", Text{}, fmt.Errorf("%w: text field %d over %d bytes", ErrUndecodable, i, MaxEncodedText)
		}
		if err := checkText(string(raw)); err != nil {
			return "", Text{}, fmt.Errorf("%w: text field %d: %w", ErrUndecodable, i, err)
		}
		out[i] = string(raw)
	}
	return f[1], Text{Title: out[0], Msg: out[1], LastErrorMsg: out[2]}, nil
}

// ValidNonce reports whether nonce has the "<rand>.%<pane number>" form,
// rand being 1-64 bytes of [A-Za-z0-9_-].
func ValidNonce(nonce string) bool {
	rand, pane, ok := strings.Cut(nonce, ".%")
	if !ok || rand == "" || len(rand) > 64 || pane == "" || len(pane) > 10 {
		return false
	}
	for i := 0; i < len(rand); i++ {
		if !isAlnum(rand[i]) && rand[i] != '_' && rand[i] != '-' {
			return false
		}
	}
	for i := 0; i < len(pane); i++ {
		if pane[i] < '0' || pane[i] > '9' {
			return false
		}
	}
	return true
}

// splitValue checks the version tag and alphabet of a published value and
// splits it into exactly n fields.
func splitValue(v string, n int) ([]string, error) {
	if v == "" {
		return nil, ErrAbsent
	}
	tag, _, _ := strings.Cut(v, "|")
	if tag != codecTag {
		if isVersionTag(tag) {
			return nil, fmt.Errorf("%w: %q", ErrUnknownVersion, tag)
		}
		return nil, fmt.Errorf("%w: version tag %q", ErrUndecodable, tag)
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; !isAlnum(c) && !strings.ContainsRune("-+=_.|%", rune(c)) {
			return nil, fmt.Errorf("%w: byte %q outside the alphabet", ErrUndecodable, c)
		}
	}
	f := strings.Split(v, "|")
	if len(f) != n {
		return nil, fmt.Errorf("%w: %d fields, want %d", ErrUndecodable, len(f), n)
	}
	return f, nil
}

// isVersionTag reports whether tag is "v" and a decimal.
func isVersionTag(tag string) bool {
	if len(tag) < 2 || tag[0] != 'v' {
		return false
	}
	for i := 1; i < len(tag); i++ {
		if tag[i] < '0' || tag[i] > '9' {
			return false
		}
	}
	return true
}

// parseCanonical parses an unsigned decimal of at most bits bits written
// exactly as strconv.FormatUint writes it.
func parseCanonical(s string, bits int) (uint64, error) {
	n, err := strconv.ParseUint(s, 10, bits)
	if err != nil {
		return 0, err
	}
	if strconv.FormatUint(n, 10) != s {
		return 0, fmt.Errorf("non-canonical number %q", s)
	}
	return n, nil
}

func encodeTime(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.UnixMilli(), 10)
}

func decodeTime(s string) (time.Time, error) {
	ms, err := parseCanonical(s, 63)
	if err != nil {
		return time.Time{}, err
	}
	if ms == 0 {
		return time.Time{}, nil
	}
	return time.UnixMilli(int64(ms)), nil
}

func knownKind(k runtime.ProgramBlockKind) bool {
	switch k {
	case runtime.ProgramBlockUnspecified, runtime.ProgramBlockPermission, runtime.ProgramBlockQuestion, runtime.ProgramBlockAuth:
		return true
	}
	return false
}

// truncateRunes returns the longest prefix of s of at most n bytes that ends
// on a rune boundary.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
