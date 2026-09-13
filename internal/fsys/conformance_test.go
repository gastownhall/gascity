package fsys_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/fsys/fsystest"
)

func TestOSFSConformance(t *testing.T) {
	fsystest.RunConformance(t, func() fsys.OSFS { return fsys.OSFS{} })
}

func TestFakeConformance(t *testing.T) {
	fsystest.RunConformance(t, fsys.NewFake)
}
