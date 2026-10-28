//go:build windows

package herdr

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestListRunningServerNotRunningIsServerAbsentPartial(t *testing.T) {
	bin, _ := registryFake(t, nil, "")
	p := New("gctest-list-absent", t.TempDir(), t.TempDir(), time.Second, 0)
	p.c.bin = bin

	names, err := p.ListRunning("")
	if err == nil {
		t.Fatal("ListRunning returned nil error for server_not_running")
	}
	if !runtime.IsPartialListError(err) {
		t.Fatalf("ListRunning err = %v, want PartialListError", err)
	}
	if !runtime.IsRuntimeServerAbsent(err) {
		t.Fatalf("ListRunning err = %v, want ServerAbsent partial list", err)
	}
	if got := herdrCodeAnyShape(err); got != "server_not_running" {
		t.Fatalf("herdr code = %q, want server_not_running (err: %v)", got, err)
	}
	if len(names) != 0 {
		t.Fatalf("ListRunning names = %v, want none", names)
	}
}
