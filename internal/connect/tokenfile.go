package connect

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// MaxTokenFileBytes is bd serve's cap on an auth token file (DES-CN F5). A
// render over it is refused, never truncated.
const MaxTokenFileBytes = 1 << 20

// ErrTokenFileTooLarge is wrapped by every refusal of content over
// MaxTokenFileBytes.
var ErrTokenFileTooLarge = errors.New("token file exceeds bd serve's 1 MiB cap")

// ErrTokenFileNoTokens is wrapped by the refusal of an operator source that
// holds no token line, which bd-enterprise refuses to load (DES-CN F5): it is
// what a non-atomic writer leaves after truncating.
var ErrTokenFileNoTokens = errors.New("token file holds no token")

// ErrTokenFileUnterminated is wrapped by the refusal of an operator source
// that does not end in a newline, which bd-enterprise refuses to load (DES-CN
// F5): its last line cannot be told from a token caught mid-write.
var ErrTokenFileUnterminated = errors.New("token file does not end in a newline")

// ErrInvalidOperatorLine is wrapped by RenderTokenFile's refusal of an
// operator line ParseOperatorLines would never return.
var ErrInvalidOperatorLine = errors.New("invalid operator line")

// TokenRow is one session incarnation that earns a bearer line: the session
// id and its generation, as MintBearer takes them.
type TokenRow struct {
	SessionID  string
	Generation int
}

// CompareTokenRows orders rows by session id, then generation: the order
// RenderTokenFile writes bearer lines in.
func CompareTokenRows(a, b TokenRow) int {
	if c := strings.Compare(a.SessionID, b.SessionID); c != 0 {
		return c
	}
	return cmp.Compare(a.Generation, b.Generation)
}

// gcLineRe matches the versioned prefix of every line gc renders into a
// token file: gcb<n>. for bearers and gcs<n>. for sentinels. bd serve's token
// file has no comment syntax (DES-CN F22), so the prefix is the only format
// marker.
var gcLineRe = regexp.MustCompile(`^gc[bs]([0-9]+)\.`)

// IsGCLine reports whether a trimmed token-file line carries a gc prefix
// (gcb<digits>. or gcs<digits>.), whatever its version.
func IsGCLine(line string) bool {
	return gcLineRe.MatchString(line)
}

// TokenLines splits content the way bd serve does: on "\n", each line
// trimmed of surrounding whitespace, empty lines skipped.
func TokenLines(content []byte) []string {
	var out []string
	for line := range strings.SplitSeq(string(content), "\n") {
		if tok := strings.TrimSpace(line); tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

// ParseOperatorLines parses an operator token source exactly as bd serve
// parses a token file (DES-CN F22) and returns its lines in source order.
// Lines that look like gc lines are dropped and counted, never returned: a
// copied one would be a bearer no render could revoke (D5).
//
// A source bd-enterprise would refuse to load is refused here too, whole:
// content over MaxTokenFileBytes (ErrTokenFileTooLarge), content with no
// token line (ErrTokenFileNoTokens) and content that does not end in a
// newline (ErrTokenFileUnterminated). The last two are the states a
// non-atomic writer passes through; copying one would revoke the operator's
// tokens or serve a fragment of one. The lines are never echoed in an error.
func ParseOperatorLines(content []byte) (lines []string, gcLike int, err error) {
	if len(content) > MaxTokenFileBytes {
		return nil, 0, fmt.Errorf("operator token source is %d bytes: %w", len(content), ErrTokenFileTooLarge)
	}
	all := TokenLines(content)
	if len(all) == 0 {
		return nil, 0, fmt.Errorf("operator token source of %d bytes: %w", len(content), ErrTokenFileNoTokens)
	}
	if content[len(content)-1] != '\n' {
		return nil, 0, fmt.Errorf("operator token source of %d bytes: %w; replace it atomically (temp file, then rename)", len(content), ErrTokenFileUnterminated)
	}
	for _, tok := range all {
		if IsGCLine(tok) {
			gcLike++
			continue
		}
		lines = append(lines, tok)
	}
	return lines, gcLike, nil
}

// ForeignOperatorLines counts the token lines of content that are not gc
// lines and are not among operator: the lines a seed over content would drop
// (DES-CN D5's seed rule).
func ForeignOperatorLines(content []byte, operator []string) int {
	held := make(map[string]struct{}, len(operator))
	for _, line := range operator {
		held[line] = struct{}{}
	}
	n := 0
	for _, tok := range TokenLines(content) {
		if _, ok := held[tok]; !ok && !IsGCLine(tok) {
			n++
		}
	}
	return n
}

// RenderTokenFile renders a bd serve token file (DES-CN §3.4): the operator
// lines in order, then the city's sentinel, then one bearer per distinct row
// sorted by (session id, generation), every line newline-terminated. It is a
// pure function of its arguments, so two renderers with the same inputs write
// identical bytes. Nil or empty rows render the seed.
//
// Operator lines must be as ParseOperatorLines returns them: non-empty,
// trimmed, without a newline or a gc prefix (an interior carriage return is
// kept, as bd serve keeps it); a line that is not is refused with an error
// wrapping ErrInvalidOperatorLine. Every row must be one MintBearer accepts.
// A render over MaxTokenFileBytes is refused with ErrTokenFileTooLarge.
// Errors never echo an operator line or the key.
func RenderTokenFile(operator []string, key []byte, city string, rows []TokenRow) ([]byte, error) {
	sentinel, err := Sentinel(key, city)
	if err != nil {
		return nil, fmt.Errorf("rendering token file: %w", err)
	}
	var buf bytes.Buffer
	for i, line := range operator {
		switch {
		case line == "":
			return nil, fmt.Errorf("rendering token file: operator line %d is empty: %w", i, ErrInvalidOperatorLine)
		case strings.TrimSpace(line) != line:
			return nil, fmt.Errorf("rendering token file: operator line %d has surrounding whitespace: %w", i, ErrInvalidOperatorLine)
		case strings.Contains(line, "\n"):
			return nil, fmt.Errorf("rendering token file: operator line %d holds a newline: %w", i, ErrInvalidOperatorLine)
		case IsGCLine(line):
			return nil, fmt.Errorf("rendering token file: operator line %d carries a gc prefix: %w", i, ErrInvalidOperatorLine)
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	buf.WriteString(sentinel)
	buf.WriteByte('\n')

	sorted := slices.SortedFunc(slices.Values(rows), CompareTokenRows)
	sorted = slices.Compact(sorted)
	for _, row := range sorted {
		bearer, err := MintBearer(key, city, row.SessionID, row.Generation)
		if err != nil {
			return nil, fmt.Errorf("rendering token file: %w", err)
		}
		buf.WriteString(bearer)
		buf.WriteByte('\n')
		if buf.Len() > MaxTokenFileBytes {
			break
		}
	}
	if buf.Len() > MaxTokenFileBytes {
		return nil, fmt.Errorf("rendering token file for %d operator lines and %d rows: %w", len(operator), len(sorted), ErrTokenFileTooLarge)
	}
	return buf.Bytes(), nil
}

// TokenFileView classifies a token file's current bytes by line prefix only;
// it needs no key. A renderer uses it for the seed rule (no gc line), the
// foreign-sentinel refusal, the key-loss rule and the newer-format refusal
// (DES-CN D5, D7). It never holds a sentinel in clear text, so printing a
// view leaks no credential.
type TokenFileView struct {
	// GCLines counts every line carrying a gc prefix, of any version,
	// well-formed or not.
	GCLines int
	// Sentinels counts the gcs1 lines; HasSentinel tests one.
	Sentinels int
	// Bearers lists the well-formed gcb1 lines' rows in file order.
	Bearers []TokenRow
	// NewerFormat is set when a gc line's version is above 1: a newer
	// renderer owns the file.
	NewerFormat bool
	// Unterminated is set when non-empty content does not end in a newline,
	// which bd-enterprise refuses to load.
	Unterminated bool

	sentinelDigests [][sha256.Size]byte
}

// HasSentinel reports whether the file holds sentinel as one of its gcs1
// lines.
func (v TokenFileView) HasSentinel(sentinel string) bool {
	if sentinel == "" {
		return false
	}
	return slices.Contains(v.sentinelDigests, sha256.Sum256([]byte(sentinel)))
}

// InspectTokenFile classifies content as bd serve would parse it.
func InspectTokenFile(content []byte) TokenFileView {
	var v TokenFileView
	v.Unterminated = len(content) > 0 && content[len(content)-1] != '\n'
	for _, tok := range TokenLines(content) {
		m := gcLineRe.FindStringSubmatch(tok)
		if m == nil {
			continue
		}
		v.GCLines++
		version, err := strconv.Atoi(m[1])
		if err != nil || version > 1 {
			v.NewerFormat = true
			continue
		}
		switch {
		case strings.HasPrefix(tok, SentinelPrefix):
			v.Sentinels++
			v.sentinelDigests = append(v.sentinelDigests, sha256.Sum256([]byte(tok)))
		case strings.HasPrefix(tok, BearerPrefix):
			if id, gen, ok := ParseBearer(tok); ok {
				v.Bearers = append(v.Bearers, TokenRow{SessionID: id, Generation: gen})
			}
		}
	}
	return v
}

// TokenFileHoldsLine reports whether content, parsed as bd serve parses it,
// holds line as one whole token.
func TokenFileHoldsLine(content []byte, line string) bool {
	if line == "" {
		return false
	}
	return slices.Contains(TokenLines(content), line)
}
