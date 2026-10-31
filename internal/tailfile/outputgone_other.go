//go:build !unix

package tailfile

import "io"

// outputGone reports nothing on this platform: a follow learns that its
// output's reader has gone only when its next write fails, as Git for
// Windows' tail -f does.
func outputGone(io.Writer) error {
	return nil
}
