package runtime

import (
	"slices"
	"testing"
)

// TmuxLiteralArgs is what send-keys -l needs for tmux to type text byte for
// byte (E5); TmuxLiteralText reads it back as tmux does and accepts nothing
// else.
func TestTmuxLiteralArgs(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		for _, tc := range []struct {
			text string
			want []string
		}{
			{"", []string{""}},
			{"x", []string{"x"}},
			{";", []string{`\;`}},
			{";;", []string{`;\;`}},
			{"x;", []string{`x\;`}},
			{`x\;`, []string{`x\\;`}},
			{"x;;", []string{`x;\;`}},
			{`x\;;`, []string{`x\;\;`}},
			{"a;b", []string{"a;b"}},
			{`\`, []string{`\`}},
			{"x;\n", []string{"x;\n"}},
			{"-", []string{"--", "-"}},
			{"-x", []string{"--", "-x"}},
			{"- item", []string{"--", "- item"}},
			{"-l", []string{"--", "-l"}},
			{"--", []string{"--", "--"}},
			{"-x;", []string{"--", `-x\;`}},
			{`-\;`, []string{"--", `-\\;`}},
			{"x-", []string{"x-"}},
		} {
			got := TmuxLiteralArgs(tc.text)
			if !slices.Equal(got, tc.want) {
				t.Errorf("TmuxLiteralArgs(%q) = %q, want %q", tc.text, got, tc.want)
				continue
			}
			back, err := TmuxLiteralText(got)
			if err != nil || back != tc.text {
				t.Errorf("TmuxLiteralText(%q) = %q, %v, want %q", got, back, err, tc.text)
			}
		}
	})
	// Arguments TmuxLiteralArgs never produces are refused, so a raw
	// send-keys -l that skips the encoder fails loudly instead of losing a
	// byte (an unescaped trailing ";" is a tmux separator; a leading "-"
	// without "--" is read as flags).
	t.Run("non-canonical arguments refused", func(t *testing.T) {
		for _, args := range [][]string{
			nil,
			{"--"},
			{"a", "b"},
			{"--", "a", "b"},
			{"x;"},
			{";"},
			{`x\;;`},
			{"-x"},
			{"-"},
			{"--", "x"},
			{"--", ""},
		} {
			if got, err := TmuxLiteralText(args); err == nil {
				t.Errorf("TmuxLiteralText(%q) = %q, want an error", args, got)
			}
		}
	})
}
