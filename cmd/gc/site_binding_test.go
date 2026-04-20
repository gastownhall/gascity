package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func mustLoadSiteBinding(t *testing.T, fs fsys.FS, cityPath string) *config.SiteBinding {
	t.Helper()
	binding, err := config.LoadSiteBinding(fs, cityPath)
	if err != nil {
		t.Fatalf("LoadSiteBinding(%q): %v", cityPath, err)
	}
	return binding
}
