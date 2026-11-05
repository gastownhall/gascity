// Package tokenrender is the I/O half of the connect token-file renderer
// (DES-CN D5 to D7, D15): the one engine that renders a bd serve
// --auth-token-file for a connect-enabled city, whichever host runs it. The
// controller runs it in process for the files on its own host; `gc connect
// render-tokens --watch` runs it as a sidecar beside a remote bd serve.
//
// Each render takes an exclusive flock on every output's parent directory
// (no lock file is created), each directory once by (device, inode) and in
// that order, and holds the locks for the whole render: it removes its own
// stale temp files, reads each output's operator source, inspects the file's
// current bytes, seeds a file that holds no gc line, reads the eligible
// session rows once, then renders with connect.RenderTokenFile and replaces
// each file atomically, keeping its mode and group. A busy lock is retried
// until the caller's context ends, and a directory that cannot be locked
// fails only its own outputs. The bytes are a pure function of (operator
// lines, key, city, rows), so renderers with the same inputs converge and a
// restarted renderer rewrites nothing.
//
// A render never overwrites a file that holds gc lines without this key's
// sentinel, or a gc line of a newer format, and it never writes an output
// whose operator source cannot be read, is not a regular file, or is not
// whole (no token line, or no final newline: the states a non-atomic writer
// passes through), or whose rows cannot be listed. A seed never drops
// content: a file over 1 MiB, or one holding token lines its operator source
// does not, is left as it is. Each such cause is a typed *Error (render_*),
// logged once per change. Reports, views and renderers print no key,
// sentinel, bearer or operator token.
//
// The package has no wiring of its own: the controller's host, the sidecar
// command and the [connect] config are CN-3a2's.
package tokenrender
