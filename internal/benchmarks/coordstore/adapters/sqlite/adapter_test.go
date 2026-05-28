package sqlite

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
)

func TestOpenRecoversGeneratedIDSequence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	first := New()
	if err := first.Open(ctx, coordstore.Config{DataDir: dir}); err != nil {
		t.Fatalf("first open: %v", err)
	}
	created, err := first.Create(ctx, coordstore.Record{Title: "first", Status: "open", Type: "task"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if created.ID != "sq-1" {
		t.Fatalf("first generated ID = %q, want sq-1", created.ID)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}

	second := New()
	if err := second.Open(ctx, coordstore.Config{DataDir: dir}); err != nil {
		t.Fatalf("second open: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("second close: %v", err)
		}
	})
	next, err := second.Create(ctx, coordstore.Record{Title: "second", Status: "open", Type: "task"})
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if next.ID != "sq-2" {
		t.Fatalf("next generated ID = %q, want sq-2", next.ID)
	}
}
