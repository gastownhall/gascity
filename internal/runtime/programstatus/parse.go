package programstatus

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Spec rev 0.4 limits. A report that breaks any of them is discarded whole.
const (
	MaxSequence     = 4096 // whole sequence, introducer and terminator included
	MaxKey          = 16
	MaxApp          = 32
	MaxID           = 128
	MaxIDSegment    = 32
	MaxIDDepth      = 8
	MaxMsgEncoded   = 2732
	MaxMsgDecoded   = 2048
	MaxTitleEncoded = 256
	MaxTitleDecoded = 192
)

// oscPrefix7501 introduces every OSC 7501 sequence.
const oscPrefix7501 = "\x1b]7501;"

// stateClear is the report word that removes records; it is not a state.
const stateClear = "clear"

var (
	// ErrProbe is returned for the support probe's body ("?"); it is not a
	// report.
	ErrProbe = errors.New("programstatus: support probe")
	// ErrDropped wraps every reason a report is discarded whole.
	ErrDropped = errors.New("programstatus: report dropped")
)

// Report is one validated OSC 7501 report.
type Report struct {
	State    runtime.ProgramState     // "" when Clear
	Clear    bool                     // state=clear: remove ID and its descendants (everything when ID is "")
	ID       string                   // "" addresses the root record
	Kind     runtime.ProgramBlockKind // only with blocked; unknown kinds read as unspecified
	Progress int                      // 0-100 with working or blocked; -1 when absent
	App      string                   // own app; "" when absent or invalid
	Title    string                   // decoded, bidi/invisible characters stripped
	Msg      string                   // decoded, bidi/invisible characters stripped
}

// ParseBody validates the body of an OSC 7501 sequence (the bytes between
// "ESC ] 7501 ;" and the terminator) per spec rev 0.4. It returns [ErrProbe]
// for the probe, and an error wrapping [ErrDropped] when the report must be
// discarded: missing or unknown state, an invalid id, a broken limit, invalid
// base64, or text that is not UTF-8 or holds a C0, DEL or C1 control.
// Malformed pairs are skipped, unknown keys ignored, and the last of a
// repeated key wins.
func ParseBody(body string) (Report, error) {
	if body == "?" {
		return Report{}, ErrProbe
	}
	if len(oscPrefix7501)+len(body)+1 > MaxSequence {
		return Report{}, fmt.Errorf("%w: sequence over %d bytes", ErrDropped, MaxSequence)
	}
	pairs := map[string]string{}
	for _, pair := range strings.Split(body, ":") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !isKey(k) || !isValue(v) {
			continue
		}
		if len(k) > MaxKey {
			return Report{}, fmt.Errorf("%w: key %q over %d bytes", ErrDropped, k, MaxKey)
		}
		pairs[k] = v
	}

	r := Report{Progress: -1}
	state, ok := pairs["state"]
	switch {
	case !ok:
		return Report{}, fmt.Errorf("%w: no state", ErrDropped)
	case state == stateClear:
		r.Clear = true
	case knownState(runtime.ProgramState(state)):
		r.State = runtime.ProgramState(state)
	default:
		return Report{}, fmt.Errorf("%w: unknown state %q", ErrDropped, state)
	}
	if id, ok := pairs["id"]; ok {
		if !validID(id) {
			return Report{}, fmt.Errorf("%w: invalid id %q", ErrDropped, id)
		}
		r.ID = id
	}
	if app, ok := pairs["app"]; ok {
		if len(app) > MaxApp {
			return Report{}, fmt.Errorf("%w: app over %d bytes", ErrDropped, MaxApp)
		}
		if validApp(app) {
			r.App = app
		}
	}
	busy := r.State == runtime.ProgramWorking || r.State == runtime.ProgramBlocked
	if r.State == runtime.ProgramBlocked {
		switch k := runtime.ProgramBlockKind(pairs["kind"]); k {
		case runtime.ProgramBlockPermission, runtime.ProgramBlockQuestion, runtime.ProgramBlockAuth:
			r.Kind = k
		}
	}
	if p, ok := pairs["progress"]; ok && busy {
		r.Progress = parseProgress(p)
	}
	var err error
	if r.Title, err = decodeText("title", pairs["title"], MaxTitleEncoded, MaxTitleDecoded); err != nil {
		return Report{}, err
	}
	if r.Msg, err = decodeText("msg", pairs["msg"], MaxMsgEncoded, MaxMsgDecoded); err != nil {
		return Report{}, err
	}
	return r, nil
}

// knownState reports whether st is one of rev 0.4's five states.
func knownState(st runtime.ProgramState) bool {
	switch st {
	case runtime.ProgramIdle, runtime.ProgramWorking, runtime.ProgramDone, runtime.ProgramBlocked, runtime.ProgramError:
		return true
	}
	return false
}

// isKey reports whether k matches [a-z]+.
func isKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 'a' || k[i] > 'z' {
			return false
		}
	}
	return true
}

// isValue reports whether v matches [A-Za-z0-9_.,+/=-]*.
func isValue(v string) bool {
	for i := 0; i < len(v); i++ {
		if !isAlnum(v[i]) && !strings.ContainsRune("_.,+/=-", rune(v[i])) {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// isNameByte reports whether c is in [A-Za-z0-9_.+-], the charset of app and
// of id segments.
func isNameByte(c byte) bool {
	return isAlnum(c) || c == '_' || c == '.' || c == '+' || c == '-'
}

// ValidApp reports whether app is a well-formed OSC 7501 app name,
// [A-Za-z0-9_.+-]{1,32}.
func ValidApp(app string) bool { return validApp(app) }

// validApp reports whether app matches [A-Za-z0-9_.+-]{1,32}.
func validApp(app string) bool {
	if app == "" || len(app) > MaxApp {
		return false
	}
	for i := 0; i < len(app); i++ {
		if !isNameByte(app[i]) {
			return false
		}
	}
	return true
}

// validID reports whether id matches segment ("/" segment)* within the
// spec's total, segment and depth limits.
func validID(id string) bool {
	if id == "" || len(id) > MaxID {
		return false
	}
	segs := strings.Split(id, "/")
	if len(segs) > MaxIDDepth {
		return false
	}
	for _, seg := range segs {
		if seg == "" || len(seg) > MaxIDSegment {
			return false
		}
		for i := 0; i < len(seg); i++ {
			if !isNameByte(seg[i]) {
				return false
			}
		}
	}
	return true
}

// parseProgress returns p as 0-100, or -1 when it is not a canonical decimal
// in range.
func parseProgress(p string) int {
	if p == "" || len(p) > 3 || (len(p) > 1 && p[0] == '0') {
		return -1
	}
	n := 0
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return -1
		}
		n = n*10 + int(p[i]-'0')
	}
	if n > 100 {
		return -1
	}
	return n
}

// decodeText decodes a base64 msg or title (padding optional), enforcing the
// encoded limit before decoding and the decoded limit after, rejecting text
// that is not UTF-8 or holds a control character, and stripping bidi and
// invisible formatting characters.
func decodeText(key, enc string, maxEncoded, maxDecoded int) (string, error) {
	if enc == "" {
		return "", nil
	}
	if len(enc) > maxEncoded {
		return "", fmt.Errorf("%w: %s over %d encoded bytes", ErrDropped, key, maxEncoded)
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(enc, "="))
	if err != nil {
		return "", fmt.Errorf("%w: %s: invalid base64: %w", ErrDropped, key, err)
	}
	if len(raw) > maxDecoded {
		return "", fmt.Errorf("%w: %s over %d decoded bytes", ErrDropped, key, maxDecoded)
	}
	text := string(raw)
	if err := checkText(text); err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrDropped, key, err)
	}
	return stripInvisible(text), nil
}

// checkText rejects text that is not UTF-8 or that holds a C0 control, DEL
// or a C1 control.
func checkText(text string) error {
	if !utf8.ValidString(text) {
		return errors.New("not UTF-8")
	}
	for _, r := range text {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return fmt.Errorf("control character %U", r)
		}
	}
	return nil
}

// invisible reports whether r is a bidi control or invisible formatting
// character stripped for display.
func invisible(r rune) bool {
	return r == 0x061c || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) ||
		(r >= 0x2060 && r <= 0x2069) || r == 0xfeff
}

func stripInvisible(text string) string {
	if !strings.ContainsFunc(text, invisible) {
		return text
	}
	return strings.Map(func(r rune) rune {
		if invisible(r) {
			return -1
		}
		return r
	}, text)
}
