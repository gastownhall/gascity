package connect

import (
	"bytes"
	"cmp"
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

// tokenLines splits content the way bd serve does: on "\n", each line
// trimmed of surrounding whitespace, empty lines skipped.
func tokenLines(content []byte) []string {
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
// copied one would be a bearer no render could revoke (D5). Content over
// MaxTokenFileBytes is refused. The lines are never echoed in an error.
func ParseOperatorLines(content []byte) (lines []string, gcLike int, err error) {
	if len(content) > MaxTokenFileBytes {
		return nil, 0, fmt.Errorf("operator token source is %d bytes: %w", len(content), ErrTokenFileTooLarge)
	}
	for _, tok := range tokenLines(content) {
		if IsGCLine(tok) {
			gcLike++
			continue
		}
		lines = append(lines, tok)
	}
	return lines, gcLike, nil
}

// RenderTokenFile renders a bd serve token file (DES-CN §3.4): the operator
// lines in order, then the city's sentinel, then one bearer per distinct row
// sorted by (session id, generation), every line newline-terminated. It is a
// pure function of its arguments, so two renderers with the same inputs write
// identical bytes. Nil or empty rows render the seed.
//
// Operator lines must be as ParseOperatorLines returns them: non-empty,
// trimmed, without a line break or a gc prefix. Every row must be one
// MintBearer accepts. A render over MaxTokenFileBytes is refused with
// ErrTokenFileTooLarge. Errors never echo an operator line or the key.
func RenderTokenFile(operator []string, key []byte, city string, rows []TokenRow) ([]byte, error) {
	sentinel, err := Sentinel(key, city)
	if err != nil {
		return nil, fmt.Errorf("rendering token file: %w", err)
	}
	var buf bytes.Buffer
	for i, line := range operator {
		switch {
		case line == "":
			return nil, fmt.Errorf("rendering token file: operator line %d is empty", i)
		case strings.TrimSpace(line) != line:
			return nil, fmt.Errorf("rendering token file: operator line %d has surrounding whitespace", i)
		case strings.ContainsAny(line, "\r\n"):
			return nil, fmt.Errorf("rendering token file: operator line %d holds a line break", i)
		case IsGCLine(line):
			return nil, fmt.Errorf("rendering token file: operator line %d carries a gc prefix", i)
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
// (DES-CN D5, D7).
type TokenFileView struct {
	// GCLines counts every line carrying a gc prefix, of any version,
	// well-formed or not.
	GCLines int
	// Sentinels lists the gcs1 lines in file order.
	Sentinels []string
	// Bearers lists the well-formed gcb1 lines' rows in file order.
	Bearers []TokenRow
	// NewerFormat is set when a gc line's version is above 1: a newer
	// renderer owns the file.
	NewerFormat bool
	// Unterminated is set when non-empty content does not end in a newline,
	// which bd-enterprise refuses to load.
	Unterminated bool
}

// InspectTokenFile classifies content as bd serve would parse it.
func InspectTokenFile(content []byte) TokenFileView {
	var v TokenFileView
	v.Unterminated = len(content) > 0 && content[len(content)-1] != '\n'
	for _, tok := range tokenLines(content) {
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
			v.Sentinels = append(v.Sentinels, tok)
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
	return slices.Contains(tokenLines(content), line)
}
