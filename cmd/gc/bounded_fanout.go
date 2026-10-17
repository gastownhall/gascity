package main

import (
	"fmt"
	"runtime/debug"
	"sync"
)

// boundedFanOut calls fn(0) through fn(n-1) with at most limit calls running at
// once, and returns when every call has returned. Each call must write only to
// its own index of anything it shares with the others, so the caller needs no
// lock to read the results afterwards.
//
// A limit of 1 or less, or a single call, is the plain sequential loop on the
// caller's goroutine: index order, one at a time, and a panic reaches the
// caller's own recover with its original value. That is what lets a caller
// whose loop runs under a recover — the controller's rig-store opens run under
// CityRuntime.safeTick — share this helper at limit 1 without its failure
// behavior moving.
//
// Above that, calls run on worker goroutines. A panic in one is raised again
// on the CALLER's goroutine once every call has finished, as a *fanOutPanic
// carrying the original value and the worker's stack: a bare panic on a worker
// would take the whole process down past any recover the caller has, which the
// sequential loop never did. When several calls panic, the lowest index wins.
func boundedFanOut(n, limit int, fn func(i int)) {
	if n <= 0 {
		return
	}
	if limit <= 1 || n == 1 {
		for i := range n {
			fn(i)
		}
		return
	}
	panics := make([]*fanOutPanic, n)
	slots := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range n {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			defer func() {
				if r := recover(); r != nil {
					panics[i] = &fanOutPanic{value: r, stack: debug.Stack()}
				}
			}()
			fn(i)
		})
	}
	wg.Wait()
	for _, p := range panics {
		if p != nil {
			panic(p)
		}
	}
}

// fanOutPanic is a panic raised on a boundedFanOut worker, carried back to the
// caller together with the worker's stack: once a panic is raised again on a
// different goroutine, its value alone no longer says where it came from.
type fanOutPanic struct {
	value any
	stack []byte
}

// Error implements error, so a recovered fanOutPanic prints the original panic
// value and the worker's stack wherever the caller logs it.
func (p *fanOutPanic) Error() string {
	return fmt.Sprintf("panic in concurrent worker: %v\n\n%s", p.value, p.stack)
}
