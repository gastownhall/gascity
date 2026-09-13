package config

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestDiscoverPackSkills_PropagatesStatErrors(t *testing.T) {
	t.Parallel()

	fake := fsys.NewFake()
	packDir := "/packs/helper"
	skillsDir := filepath.Join(packDir, "skills")
	wantErr := errors.New("boom")
	fake.Errors[skillsDir] = wantErr

	_, err := DiscoverPackSkills(fake, packDir, "helper")
	if !errors.Is(err, wantErr) {
		t.Fatalf("DiscoverPackSkills() error = %v, want %v", err, wantErr)
	}
}
