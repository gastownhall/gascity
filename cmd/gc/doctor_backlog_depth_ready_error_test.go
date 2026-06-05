package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
)

type backlogDepthReadyErrorStore struct {
	beads.Store
}

func (s backlogDepthReadyErrorStore) ListOpen(...string) ([]beads.Bead, error) {
	return nil, nil
}

func (s backlogDepthReadyErrorStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	return nil, fmt.Errorf("ready unavailable")
}

func TestBacklogDepthCheckReadyErrorIsGraceful(t *testing.T) {
	check := newBacklogDepthCheck("/city", func(string) (beads.Store, error) {
		return backlogDepthReadyErrorStore{}, nil
	})

	res := check.Run(&doctor.CheckContext{})

	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning for Ready failure: %#v", res.Status, res)
	}
	if res.Status == doctor.StatusError {
		t.Fatalf("Ready failure should not be a blocking StatusError: %#v", res)
	}
	if !strings.Contains(res.Message, "backlog depth unknown: listing ready beads: ready unavailable") {
		t.Fatalf("Message = %q, want Ready error context", res.Message)
	}
	if check.CanFix() {
		t.Fatal("CanFix() = true, want false for read-only observability check")
	}
}
