package tmux

import (
	"reflect"
	"strings"
	"testing"
)

// Short unbracketed input can lose its prefix in Claude's TUI even when tmux
// delivers every byte. Claude must receive one bracketed paste at any length.
func TestSendLiteralTextUsesProviderInputTransport(t *testing.T) {
	message := "BEGIN\n" + strings.Repeat("context line\n", 90) + "END"
	for _, tc := range []struct {
		provider string
		paste    bool
	}{
		{provider: "claude", paste: true},
		{provider: "codex", paste: false},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			// GC_PROVIDER carries the resolved builtin ancestor, including for
			// custom providers inheriting Claude. The session name is arbitrary.
			fe := &fakeExecutor{out: "GC_PROVIDER=" + tc.provider}
			tm := NewTmux()
			tm.exec = fe
			if err := tm.sendLiteralText("custom-worker", message); err != nil {
				t.Fatal(err)
			}
			last := fe.calls[len(fe.calls)-1]
			if tc.paste {
				if len(last) != 8 || !reflect.DeepEqual(last[:5], []string{"-u", "paste-buffer", "-p", "-d", "-b"}) ||
					!reflect.DeepEqual(last[6:], []string{"-t", "custom-worker"}) {
					t.Fatalf("Claude input must use bracketed paste; got %v", last)
				}
			} else if want := []string{"-u", "send-keys", "-t", "custom-worker", "-l", message}; !reflect.DeepEqual(last, want) {
				t.Fatalf("other provider input = %v, want %v", last, want)
			}
		})
	}
}
