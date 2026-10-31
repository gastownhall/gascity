//go:build !windows

package herdr

import (
	"errors"
	"net"
	goruntime "runtime"
)

// goroutineCount counts the goroutines a leak check should see.
func goroutineCount() int { return goruntime.NumGoroutine() }

// listenHerdrEndpoint and acceptAndClose serve herdr's Windows pipe. Shared
// tests reach them only behind a Windows check; herdr serves a unix socket
// here, which those tests listen on directly.
func listenHerdrEndpoint(string) (net.Listener, error) {
	return nil, errors.New("listenHerdrEndpoint serves herdr's Windows pipe; it is not used on this platform")
}

func acceptAndClose(net.Listener) {
	panic("acceptAndClose serves herdr's Windows pipe; it is not used on this platform")
}
