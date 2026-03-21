package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestValidateExplicitName(t *testing.T) {
	longName := strings.Repeat("a", explicitSessionNameMaxLen+1)
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{name: "empty allowed", input: "", want: ""},
		{name: "trimmed", input: "  sky  ", want: "sky"},
		{name: "single char", input: "x", want: "x"},
		{name: "bad syntax", input: "sky.chat", wantErr: ErrInvalidSessionName},
		{name: "reserved prefix", input: "s-gc-123", wantErr: ErrInvalidSessionName},
		{name: "too long", input: longName, wantErr: ErrInvalidSessionName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateExplicitName(tt.input)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error %v, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr.Error()) {
					t.Fatalf("error = %v, want contains %q", err, tt.wantErr.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnsureSessionNameAvailable_RejectsOpenIdentifierCollisions(t *testing.T) {
	store := beads.NewMemStore()
	open, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"template": "myrig/worker",
		},
	})
	if err != nil {
		t.Fatalf("Create(open): %v", err)
	}

	if err := ensureSessionNameAvailable(store, "worker"); !errors.Is(err, ErrSessionNameExists) {
		t.Fatalf("ensureSessionNameAvailable(open collision) error = %v, want %v", err, ErrSessionNameExists)
	}

	if err := store.Close(open.ID); err != nil {
		t.Fatalf("Close(open): %v", err)
	}
	if err := ensureSessionNameAvailable(store, "worker"); err != nil {
		t.Fatalf("ensureSessionNameAvailable(closed collision) = %v, want nil", err)
	}
}

func TestWithCitySessionNameLock_EmptyCityPathFallsBackWithoutLockFile(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir(tmp): %v", err)
	}
	defer func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()

	called := false
	if err := WithCitySessionNameLock("", "sky", func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("WithCitySessionNameLock: %v", err)
	}
	if !called {
		t.Fatal("lock function did not execute")
	}
	if _, err := os.Stat(filepath.Join(tmp, ".gc")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".gc should not be created for empty cityPath, got err=%v", err)
	}
}
