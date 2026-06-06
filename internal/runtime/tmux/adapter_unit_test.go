package tmux

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestProviderAttachRefusesDeadPane(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{"", "1"},
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	err := p.Attach("runner")
	if err == nil {
		t.Fatal("Attach = nil, want dead pane error")
	}
	if !strings.Contains(err.Error(), "dead pane") {
		t.Fatalf("Attach error = %v, want dead pane context", err)
	}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "attach-session") {
			t.Fatalf("Attach attempted tmux attach-session for dead pane: %v", fe.calls)
		}
	}
}

func TestProviderAttachMissingSessionWrapsRuntimeSentinel(t *testing.T) {
	fe := &fakeExecutor{
		err: ErrSessionNotFound,
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	err := p.Attach("runner")
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Attach error = %v, want runtime.ErrSessionNotFound", err)
	}
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Attach error = %v, want tmux ErrSessionNotFound", err)
	}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "attach-session") {
			t.Fatalf("Attach attempted tmux attach-session for missing session: %v", fe.calls)
		}
	}
}

func TestProviderAttachReportsHasSessionError(t *testing.T) {
	fe := &fakeExecutor{
		err: errors.New("tmux unavailable"),
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	err := p.Attach("runner")
	if err == nil {
		t.Fatal("Attach = nil, want has-session error")
	}
	if !strings.Contains(err.Error(), "checking tmux session before attach") {
		t.Fatalf("Attach error = %v, want checking context", err)
	}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "attach-session") {
			t.Fatalf("Attach attempted tmux attach-session after has-session error: %v", fe.calls)
		}
	}
}
