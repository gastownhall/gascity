package scripts_test

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A GitHub Actions expression evaluator for workflow policy tests, ported
// from gastownhall/beads scripts/ci_blacksmith_runner_test.go (evalGHExpr).
// It covers the subset the workflows here use in the expressions it is fed:
// identifiers (dotted context paths, looked up in the caller's map; an
// absent key is null, as an absent event field is on GitHub), string and
// number literals, true/false/null, ==, !=, !, &&, || and parentheses. &&
// and || return an operand (GitHub's short-circuit semantics), which is
// what makes `cond && 'a' || 'b'` a string. Function calls (always(), startsWith()...)
// are not supported; ghStatusFuncs replaces the status functions first.

type ghToken struct {
	kind string // "ident", "string", "op", "lparen", "rparen", "bang"
	val  string
}

func ghTokenize(s string) ([]ghToken, error) {
	var toks []ghToken
	i := 0
	isIdentByte := func(b byte) bool {
		return b == '.' || b == '_' || b == '-' ||
			(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
	}
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			toks = append(toks, ghToken{"lparen", "("})
			i++
		case c == ')':
			toks = append(toks, ghToken{"rparen", ")"})
			i++
		case c == '\'':
			j := i + 1
			for j < len(s) && s[j] != '\'' {
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated string literal at byte %d in %q", i, s)
			}
			toks = append(toks, ghToken{"string", s[i+1 : j]})
			i = j + 1
		case strings.HasPrefix(s[i:], "=="):
			toks = append(toks, ghToken{"op", "=="})
			i += 2
		case strings.HasPrefix(s[i:], "!="):
			toks = append(toks, ghToken{"op", "!="})
			i += 2
		case strings.HasPrefix(s[i:], "&&"):
			toks = append(toks, ghToken{"op", "&&"})
			i += 2
		case strings.HasPrefix(s[i:], "||"):
			toks = append(toks, ghToken{"op", "||"})
			i += 2
		case c == '!':
			toks = append(toks, ghToken{"bang", "!"})
			i++
		default:
			j := i
			for j < len(s) && isIdentByte(s[j]) {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("unexpected character %q at byte %d in %q", c, i, s)
			}
			toks = append(toks, ghToken{"ident", s[i:j]})
			i = j
		}
	}
	return toks, nil
}

type ghParser struct {
	toks []ghToken
	pos  int
	ctx  map[string]any
}

func (p *ghParser) peek() (ghToken, bool) {
	if p.pos >= len(p.toks) {
		return ghToken{}, false
	}
	return p.toks[p.pos], true
}

func (p *ghParser) next() (ghToken, bool) {
	tok, ok := p.peek()
	if ok {
		p.pos++
	}
	return tok, ok
}

func (p *ghParser) parseExpr() (any, error) { return p.parseOr() }

func (p *ghParser) parseOr() (any, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for {
		tok, ok := p.peek()
		if !ok || tok.kind != "op" || tok.val != "||" {
			return left, nil
		}
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		if !ghTruthy(left) {
			left = right
		}
	}
}

func (p *ghParser) parseAnd() (any, error) {
	left, err := p.parseEquality()
	if err != nil {
		return nil, err
	}
	for {
		tok, ok := p.peek()
		if !ok || tok.kind != "op" || tok.val != "&&" {
			return left, nil
		}
		p.next()
		right, err := p.parseEquality()
		if err != nil {
			return nil, err
		}
		if ghTruthy(left) {
			left = right
		}
	}
}

func (p *ghParser) parseEquality() (any, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		tok, ok := p.peek()
		if !ok || tok.kind != "op" || (tok.val != "==" && tok.val != "!=") {
			return left, nil
		}
		p.next()
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		eq := ghEquals(left, right)
		if tok.val == "!=" {
			left = !eq
		} else {
			left = eq
		}
	}
}

func (p *ghParser) parseUnary() (any, error) {
	if tok, ok := p.peek(); ok && tok.kind == "bang" {
		p.next()
		v, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return !ghTruthy(v), nil
	}
	return p.parsePrimary()
}

func (p *ghParser) parsePrimary() (any, error) {
	tok, ok := p.next()
	if !ok {
		return nil, fmt.Errorf("unexpected end of expression")
	}
	switch tok.kind {
	case "lparen":
		v, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		closing, ok := p.next()
		if !ok || closing.kind != "rparen" {
			return nil, fmt.Errorf("expected ) in expression")
		}
		return v, nil
	case "string":
		return tok.val, nil
	case "ident":
		switch tok.val {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		}
		if f, err := strconv.ParseFloat(tok.val, 64); err == nil {
			return f, nil // a number literal
		}
		// An identifier the context does not mention (any
		// github.event.pull_request.* field on merge_group) is null.
		return p.ctx[tok.val], nil
	default:
		return nil, fmt.Errorf("unexpected token %+v", tok)
	}
}

func ghTruthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x != ""
	case nil:
		return false
	case float64:
		return x != 0 && !math.IsNaN(x)
	default:
		return true
	}
}

// ghEquals: GitHub's loose equality. Two strings compare case-insensitively;
// two booleans, or two nulls, directly; otherwise both sides are coerced to
// numbers (null and ” to 0, true to 1, false to 0, any other string to its
// numeric value or NaN) and NaN equals nothing.
func ghEquals(a, b any) bool {
	if sa, ok := a.(string); ok {
		if sb, ok := b.(string); ok {
			return strings.EqualFold(sa, sb)
		}
	}
	if ba, ok := a.(bool); ok {
		if bb, ok := b.(bool); ok {
			return ba == bb
		}
	}
	if a == nil && b == nil {
		return true
	}
	na, nb := ghNumber(a), ghNumber(b)
	return !math.IsNaN(na) && !math.IsNaN(nb) && na == nb
}

// ghNumber: GitHub's coercion of an operand to a number.
func ghNumber(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		t := strings.TrimSpace(x)
		if t == "" {
			return 0
		}
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return math.NaN()
		}
		return f
	case float64:
		return x
	default:
		return math.NaN()
	}
}

// ghStatusFuncs replaces the status-check functions with the literal a
// passing run of the step's earlier steps yields, so an `if` such as
// `always() && github.event_name == 'push'` can be evaluated.
var ghStatusFuncs = strings.NewReplacer(
	"!cancelled()", "true", //nolint:misspell // GitHub Actions status function
	"cancelled()", "false", //nolint:misspell // GitHub Actions status function
	"always()", "true",
	"success()", "true",
	"failure()", "false",
)

// evalGHExpr evaluates a GitHub Actions expression (the ${{ }} wrapper is
// optional) against ctx, a map from dotted identifier to its string value.
func evalGHExpr(expr string, ctx map[string]string) (any, error) {
	typed := make(map[string]any, len(ctx))
	for k, v := range ctx {
		typed[k] = v
	}
	expr = strings.TrimSpace(expr)
	expr = strings.TrimPrefix(expr, "${{")
	expr = strings.TrimSuffix(expr, "}}")
	expr = ghStatusFuncs.Replace(strings.TrimSpace(expr))
	toks, err := ghTokenize(expr)
	if err != nil {
		return nil, err
	}
	p := &ghParser{toks: toks, ctx: typed}
	v, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("trailing tokens after expression %q: %v", expr, p.toks[p.pos:])
	}
	return v, nil
}

// ghString renders an expression value as GitHub interpolates it.
func ghString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

var ghInterpolation = regexp.MustCompile(`\$\{\{(.*?)\}\}`)

// interpolateGH replaces every ${{ ... }} in s with its value under ctx.
func interpolateGH(t *testing.T, s string, ctx map[string]string) string {
	t.Helper()
	return ghInterpolation.ReplaceAllStringFunc(s, func(m string) string {
		v, err := evalGHExpr(m, ctx)
		if err != nil {
			t.Fatalf("evalGHExpr(%q): %v", m, err)
		}
		return ghString(v)
	})
}

// evalGHIf evaluates a job or step `if` (bare or wrapped) to a bool.
func evalGHIf(t *testing.T, cond string, ctx map[string]string) bool {
	t.Helper()
	if strings.TrimSpace(cond) == "" {
		return true
	}
	v, err := evalGHExpr(cond, ctx)
	if err != nil {
		t.Fatalf("evalGHExpr(%q): %v", cond, err)
	}
	return ghTruthy(v)
}

func TestGHExprEvaluator(t *testing.T) {
	ctx := map[string]string{"github.event_name": "merge_group", "github.ref": "refs/heads/q", "github.run_id": "9"}
	for expr, want := range map[string]string{
		"${{ github.event_name == 'pull_request' && github.event.pull_request.number || github.run_id }}": "9",
		"github.event_name == 'merge_group' && github.ref || github.run_id":                               "refs/heads/q",
		"${{ github.event.pull_request.head.repo.fork == true }}":                                         "false",
		"${{ github.event.pull_request.number || 0 }}":                                                    "0",
		"always() && github.event_name == 'push'":                                                         "false",
		"!cancelled() && (github.event_name == 'MERGE_GROUP')":                                            "true", //nolint:misspell // GitHub Actions status function
		"${{ true == 'true' }}":                                                                           "false",
		"${{ github.event.merge_group.base_sha }}":                                                        "",
	} {
		v, err := evalGHExpr(expr, ctx)
		if err != nil {
			t.Errorf("evalGHExpr(%q): %v", expr, err)
			continue
		}
		if got := ghString(v); got != want {
			t.Errorf("evalGHExpr(%q) = %q, want %q", expr, got, want)
		}
	}
	if _, err := evalGHExpr("startsWith(github.ref, 'x')", ctx); err == nil {
		t.Error("evalGHExpr accepted a function call it cannot evaluate")
	}
}
