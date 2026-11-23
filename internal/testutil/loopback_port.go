package testutil

import "github.com/gastownhall/gascity/internal/loopbackport"

// ReserveLoopbackPort returns a 127.0.0.1 port for a server a test starts
// later in a child process. On Linux it is leased for the life of the test
// process, so no other caller in the network namespace is handed it
// (loopbackport.Reserve).
func ReserveLoopbackPort() (int, error) {
	return loopbackport.Reserve()
}
