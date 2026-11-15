package tap

import (
	"context"
	"slices"
	"strings"
	"testing"
)

const (
	testSocket = "/tmp/tmux-1000/gc-city"
	testStatus = "v1|r4nd.%3|1|working||1761912000000|0|1|0||0|0||gen"
	testText   = "v1|r4nd.%3||aGk|"
)

func TestTapWriteIsNonceGuarded(t *testing.T) {
	args, err := guardedArgs(testSocket, "%3", testNonce, Write{Status: testStatus, Text: testText})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-S", testSocket,
		"if-shell", "-F", "-t", "%3", "#{==:#{@gc_ps_tap},r4nd.%3}",
		"set-option -p -t %3 @gc_ps '" + testStatus + "' ; set-option -p -t %3 @gc_ps_text '" + testText + "'",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("args =\n%q\nwant\n%q", args, want)
	}

	args, err = guardedArgs(testSocket, "%3", testNonce, Write{Status: testStatus})
	if err != nil {
		t.Fatal(err)
	}
	if got := args[len(args)-1]; got != "set-option -p -t %3 @gc_ps '"+testStatus+"'" {
		t.Fatalf("status-only command = %q", got)
	}
}

func TestTapWriteRejectsUnquotableValues(t *testing.T) {
	for _, w := range []Write{
		{Status: "v1|it's"},
		{Status: testStatus, Text: "v1|x'y"},
		{Status: ""},
	} {
		if _, err := guardedArgs(testSocket, "%3", testNonce, w); err == nil {
			t.Errorf("guardedArgs(%+v) succeeded, want an error", w)
		}
	}
}

func TestTapIgnoresInheritedTMUXEnv(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin:/bin",
		"TMUX=/tmp/tmux-1000/default,4242,0", // the tmux server's global env names another server
		"TMUX_PANE=%9",
		"HOME=/home/u",
		"TMUXLIKE=kept",
	}
	p, err := NewTmuxPublisher("/usr/bin/tmux", testSocket, "%3", testNonce, environ)
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := p.command(context.Background(), Write{Status: testStatus})
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			t.Fatalf("publish inherits %q; every write must address the server by -S only", kv)
		}
	}
	for _, want := range []string{"PATH=/usr/bin:/bin", "HOME=/home/u", "TMUXLIKE=kept"} {
		if !slices.Contains(cmd.Env, want) {
			t.Fatalf("env %q lacks %q", cmd.Env, want)
		}
	}
	if cmd.Path != "/usr/bin/tmux" || !slices.Equal(cmd.Args[1:3], []string{"-S", testSocket}) {
		t.Fatalf("command %q %q, want the tmux binary addressed with -S %s", cmd.Path, cmd.Args, testSocket)
	}
}

func TestNewTmuxPublisherValidates(t *testing.T) {
	for _, tc := range []struct{ bin, socket, pane, nonce string }{
		{"", testSocket, "%3", testNonce},
		{"tmux", "", "%3", testNonce},
		{"tmux", testSocket, "3", testNonce},
		{"tmux", testSocket, "%3x", testNonce},
		{"tmux", testSocket, "%3", "r4nd.%4"}, // nonce bound to another pane
		{"tmux", testSocket, "%3", "bad'.%3"},
	} {
		if _, err := NewTmuxPublisher(tc.bin, tc.socket, tc.pane, tc.nonce, nil); err == nil {
			t.Errorf("NewTmuxPublisher(%+v) succeeded, want an error", tc)
		}
	}
}
