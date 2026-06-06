package beads_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

func TestNativeDoltStoreConformance(t *testing.T) {
	beadstest.RunStoreTests(t, beads.NewNativeDoltStoreForConformance)
}
