package testutil

import "github.com/gastownhall/gascity/internal/loopbackport"

// ReserveLoopbackPort returns a 127.0.0.1 port for a server a test starts
// later in a child process, leased host-wide for the life of the test
// process (loopbackport.Reserve).
func ReserveLoopbackPort() (int, error) {
	return loopbackport.Reserve()
}
