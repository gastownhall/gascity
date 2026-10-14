package connect

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const goldenCity = "maintainer-city"

func goldenRows() []TokenRow {
	// Deliberately unsorted, with a duplicate: the render sorts by (session
	// id, generation), numerically on the generation, and drops duplicates.
	return []TokenRow{
		{SessionID: "mc.abc.12", Generation: 130},
		{SessionID: "mc-abc12", Generation: 10},
		{SessionID: "mc-zz9", Generation: 1},
		{SessionID: "mc-abc12", Generation: 7},
		{SessionID: "mc-zz9", Generation: 1},
	}
}

func mustRender(t *testing.T, operator []string, rows []TokenRow) []byte {
	t.Helper()
	out, err := RenderTokenFile(operator, testKey(), goldenCity, rows)
	if err != nil {
		t.Fatalf("RenderTokenFile: %v", err)
	}
	return out
}

func TestRenderTokenFileGolden(t *testing.T) {
	got := mustRender(t, []string{"operator-token-one", "operator-token-two"}, goldenRows())
	want := readTestdata(t, "tokenfile_v1.golden")
	if !bytes.Equal(got, want) {
		t.Fatalf("render drifted from testdata/tokenfile_v1.golden:\n%s", got)
	}
}

func TestRenderTokenFileIsPure(t *testing.T) {
	operator := []string{"operator-token-one", "operator-token-two"}
	want := mustRender(t, operator, goldenRows())
	rows := goldenRows()
	for i := range 10 {
		shuffled := slices.Clone(rows)
		// A deterministic rotation and reversal per pass.
		shuffled = append(shuffled[i%len(shuffled):], shuffled[:i%len(shuffled)]...)
		if i%2 == 1 {
			slices.Reverse(shuffled)
		}
		if got := mustRender(t, operator, shuffled); !bytes.Equal(got, want) {
			t.Fatalf("pass %d: row order changed the bytes:\n%s", i, got)
		}
	}
	if !slices.Equal(rows, goldenRows()) {
		t.Fatal("RenderTokenFile mutated its rows argument")
	}
}

func TestRenderTokenFileSeed(t *testing.T) {
	seed := mustRender(t, []string{"op"}, nil)
	if want := "op\ngcs1.x5f4iebvW3fmymjWGE5g9uG-H6FDn7W7LdrsnfWCTGU\n"; string(seed) != want {
		t.Fatalf("seed = %q, want %q", seed, want)
	}
	if empty := mustRender(t, nil, []TokenRow{}); string(empty) != "gcs1.x5f4iebvW3fmymjWGE5g9uG-H6FDn7W7LdrsnfWCTGU\n" {
		t.Fatalf("operator-less seed = %q", empty)
	}
}

func TestRenderTokenFileRefusals(t *testing.T) {
	cases := []struct {
		name     string
		operator []string
		key      []byte
		city     string
		rows     []TokenRow
	}{
		{"operator line holding a newline", []string{"a\nb"}, testKey(), goldenCity, nil},
		{"operator line holding a carriage return", []string{"a\rb"}, testKey(), goldenCity, nil},
		{"empty operator line", []string{""}, testKey(), goldenCity, nil},
		{"untrimmed operator line", []string{" a"}, testKey(), goldenCity, nil},
		{"gc bearer as operator line", []string{"gcb1.x.1.y"}, testKey(), goldenCity, nil},
		{"gc sentinel as operator line", []string{"gcs1.x"}, testKey(), goldenCity, nil},
		{"newer gc line as operator line", []string{"gcb2.x"}, testKey(), goldenCity, nil},
		{"short key", nil, testKey()[:MinKeyLen-1], goldenCity, nil},
		{"empty city", nil, testKey(), "", nil},
		{"invalid session id", nil, testKey(), goldenCity, []TokenRow{{SessionID: "a b", Generation: 1}}},
		{"zero generation", nil, testKey(), goldenCity, []TokenRow{{SessionID: "s", Generation: 0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if out, err := RenderTokenFile(tc.operator, tc.key, tc.city, tc.rows); err == nil {
				t.Fatalf("RenderTokenFile accepted; rendered %q", out)
			}
		})
	}
}

func TestRenderTokenFileSizeCap(t *testing.T) {
	rows := make([]TokenRow, 5000)
	for i := range rows {
		rows[i] = TokenRow{SessionID: fmt.Sprintf("mc-%06d", i), Generation: 1 + i%50}
	}
	out := mustRender(t, nil, rows)
	if len(out) > 400_000 {
		t.Fatalf("5,000 rows rendered %d bytes; DES-CN D6 budgets about 350 KB", len(out))
	}
	big := strings.Repeat("x", MaxTokenFileBytes/2)
	_, err := RenderTokenFile([]string{big, big + "y"}, testKey(), goldenCity, nil)
	if !errors.Is(err, ErrTokenFileTooLarge) {
		t.Fatalf("over-cap render error = %v, want ErrTokenFileTooLarge", err)
	}
}

func TestParseOperatorLines(t *testing.T) {
	content := []byte("  tok-a \n\n\r\ngcb1.s.1.mac\ntok-b\r\ngcs1.sentinel\n\tgcb7.future\ngcbx.not-gc\ntok-a\nlast-unterminated")
	lines, gcLike, err := ParseOperatorLines(content)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tok-a", "tok-b", "gcbx.not-gc", "tok-a", "last-unterminated"}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines = %q, want %q", lines, want)
	}
	if gcLike != 3 {
		t.Fatalf("gcLike = %d, want 3", gcLike)
	}
	if lines, gcLike, err := ParseOperatorLines(nil); err != nil || len(lines) != 0 || gcLike != 0 {
		t.Fatalf("empty source = %q, %d, %v", lines, gcLike, err)
	}
	if _, _, err := ParseOperatorLines(make([]byte, MaxTokenFileBytes+1)); !errors.Is(err, ErrTokenFileTooLarge) {
		t.Fatalf("oversized source error = %v, want ErrTokenFileTooLarge", err)
	}
	// Whatever the source holds, its parsed lines always render.
	if _, err := RenderTokenFile(lines, testKey(), goldenCity, nil); err != nil {
		t.Fatalf("parsed operator lines do not render: %v", err)
	}
}

func TestIsGCLine(t *testing.T) {
	for _, line := range []string{"gcb1.x", "gcs1.x", "gcb2.x", "gcs12.x", "gcb0.x"} {
		if !IsGCLine(line) {
			t.Errorf("IsGCLine(%q) = false", line)
		}
	}
	for _, line := range []string{"gcb.x", "gcbx.x", "gcb1", "GCB1.x", "xgcb1.x", "gcq1.x", ""} {
		if IsGCLine(line) {
			t.Errorf("IsGCLine(%q) = true", line)
		}
	}
}

func TestInspectTokenFile(t *testing.T) {
	for _, content := range []string{"", "op-a\nop-b\n", "  \n\n"} {
		if v := InspectTokenFile([]byte(content)); v.GCLines != 0 || v.NewerFormat || len(v.Sentinels) != 0 || len(v.Bearers) != 0 {
			t.Errorf("InspectTokenFile(%q) = %+v, want no gc line", content, v)
		}
	}
	if v := InspectTokenFile(nil); v.Unterminated {
		t.Error("absent content reported unterminated")
	}

	golden := readTestdata(t, "tokenfile_v1.golden")
	v := InspectTokenFile(golden)
	if v.GCLines != 5 || v.NewerFormat || v.Unterminated {
		t.Fatalf("golden view = %+v", v)
	}
	if !slices.Equal(v.Sentinels, []string{"gcs1.x5f4iebvW3fmymjWGE5g9uG-H6FDn7W7LdrsnfWCTGU"}) {
		t.Fatalf("sentinels = %q", v.Sentinels)
	}
	wantRows := []TokenRow{{"mc-abc12", 7}, {"mc-abc12", 10}, {"mc-zz9", 1}, {"mc.abc.12", 130}}
	if !slices.Equal(v.Bearers, wantRows) {
		t.Fatalf("bearers = %+v, want %+v", v.Bearers, wantRows)
	}

	for _, line := range []string{"gcb2.s.1.mac", "gcs2.mac", "gcb10.x"} {
		if v := InspectTokenFile([]byte("op\n" + line + "\n")); !v.NewerFormat || v.GCLines != 1 {
			t.Errorf("InspectTokenFile(%q) = %+v, want NewerFormat", line, v)
		}
	}
	if v := InspectTokenFile([]byte("op\ngcs1.x")); !v.Unterminated || v.GCLines != 1 {
		t.Errorf("unterminated view = %+v", v)
	}
	// A malformed gcb1 line still counts as a gc line, so it is never
	// mistaken for an operator-only file and seeded over.
	if v := InspectTokenFile([]byte("gcb1.not-a-bearer\n")); v.GCLines != 1 || len(v.Bearers) != 0 {
		t.Errorf("malformed bearer view = %+v", v)
	}
}

func TestTokenFileHoldsLine(t *testing.T) {
	golden := readTestdata(t, "tokenfile_v1.golden")
	if !TokenFileHoldsLine(golden, "gcb1.mc-zz9.1.gKpYN8tMk3M21PGeraHrtRjKEiixa21wXKyAmNroRCs") {
		t.Fatal("bearer line not found")
	}
	if TokenFileHoldsLine(golden, "gcb1.mc-zz9.1") {
		t.Fatal("a prefix of a line matched")
	}
	if TokenFileHoldsLine(golden, "") {
		t.Fatal("the empty line matched")
	}
}
