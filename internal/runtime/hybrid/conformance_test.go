package hybrid

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

func TestHybridConformance(t *testing.T) {
	var counter int64

	runtimetest.RunProviderTests(t, func(_ *testing.T) (runtime.Provider, runtime.Config, string) {
		return New(runtime.NewFake(), runtime.NewFake(), func(string) bool { return false }), runtime.Config{}, fmt.Sprintf("hybrid-conform-%d", atomic.AddInt64(&counter, 1))
	})
}
