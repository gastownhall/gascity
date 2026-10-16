package tmux

import (
	"slices"
	"testing"
)

// literalTextArgs is what send-keys -l needs for tmux to type text byte for
// byte (E5); literalTextFromArgs reads it back as tmux does.
func TestLiteralTextArgs(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		for _, tc := range []struct {
			text string
			want []string
		}{
			{"", []string{""}},
			{"x", []string{"x"}},
			{";", []string{`\;`}},
			{"x;", []string{`x\;`}},
			{`x\;`, []string{`x\\;`}},
			{"x;;", []string{`x;\;`}},
			{"a;b", []string{"a;b"}},
			{`\`, []string{`\`}},
			{"x;\n", []string{"x;\n"}},
			{"-", []string{"--", "-"}},
			{"-x", []string{"--", "-x"}},
			{"-l", []string{"--", "-l"}},
			{"--", []string{"--", "--"}},
			{"-x;", []string{"--", `-x\;`}},
			{"x-", []string{"x-"}},
		} {
			got := literalTextArgs(tc.text)
			if !slices.Equal(got, tc.want) {
				t.Errorf("literalTextArgs(%q) = %q, want %q", tc.text, got, tc.want)
				continue
			}
			back, err := literalTextFromArgs(got)
			if err != nil || back != tc.text {
				t.Errorf("literalTextFromArgs(%q) = %q, %v, want %q", got, back, err, tc.text)
			}
		}
	})
	// tmux reads an unescaped trailing ";" as a command separator, not text.
	t.Run("unescaped trailing semicolon is a separator", func(t *testing.T) {
		for arg, want := range map[string]string{"x;": "x", ";": "", `x\;;`: `x\;`} {
			if got, err := literalTextFromArgs([]string{arg}); err != nil || got != want {
				t.Errorf("literalTextFromArgs(%q) = %q, %v, want %q", arg, got, err, want)
			}
		}
	})
	// Anything but one text argument, optionally after "--", is refused.
	t.Run("other shapes refused", func(t *testing.T) {
		for _, args := range [][]string{nil, {"--"}, {"a", "b"}, {"--", "a", "b"}} {
			if got, err := literalTextFromArgs(args); err == nil {
				t.Errorf("literalTextFromArgs(%q) = %q, want an error", args, got)
			}
		}
	})
}
