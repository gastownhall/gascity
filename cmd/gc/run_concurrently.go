package main

import (
	"fmt"
	"runtime/debug"
	"sync"
)

// workerPanic carries a panic raised on a runConcurrently worker back to the
// caller together with the worker's own stack: once a panic is re-raised on a
// different goroutine, its value alone no longer says where it came from.
type workerPanic struct {
	value any
	stack []byte
}

// Error implements error, so a recovered workerPanic prints the original panic
// value and the worker's stack wherever the caller logs it.
func (p *workerPanic) Error() string {
	return fmt.Sprintf("panic in concurrent worker: %v\n\n%s", p.value, p.stack)
}

// runConcurrently calls fn(0) through fn(n-1), each on its own goroutine, and
// returns once every call has finished. Each call must write only to its own
// index of any slice it shares with the others, so no lock is needed.
//
// A panic in any call is re-raised on the CALLER's goroutine, as a *workerPanic,
// after every call has finished. A bare `go` would take the whole process down
// instead, bypassing the recover the controller wraps its reconcile ticks in
// (CityRuntime.safeTick): the sequential loops this replaces let a panic reach
// that recover, and moving them onto goroutines must not change that. When
// several calls panic, the lowest index wins.
func runConcurrently(n int, fn func(i int)) {
	panics := make([]*workerPanic, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics[i] = &workerPanic{value: r, stack: debug.Stack()}
				}
			}()
			fn(i)
		}()
	}
	wg.Wait()
	for _, p := range panics {
		if p != nil {
			panic(p)
		}
	}
}
