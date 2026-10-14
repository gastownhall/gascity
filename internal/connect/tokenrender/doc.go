// Package tokenrender is the I/O half of the connect token-file renderer
// (DES-CN D5 to D7, D15): the one engine that renders a bd serve
// --auth-token-file for a connect-enabled city, whichever host runs it. The
// controller runs it in process for the files on its own host; `gc connect
// render-tokens --watch` runs it as a sidecar beside a remote bd serve.
//
// Each render takes an exclusive flock on every output's parent directory
// (no lock file is created) and holds it for the whole render: it reads each
// output's operator source, inspects the file's current bytes, seeds a file
// that holds no gc line, reads the eligible session rows once, then renders
// with connect.RenderTokenFile and replaces each file atomically. The bytes
// are a pure function of (operator lines, key, city, rows), so renderers with
// the same inputs converge and a restarted renderer rewrites nothing.
//
// A render never overwrites a file that holds gc lines without this key's
// sentinel, or a gc line of a newer format, and it never writes an output
// whose operator source cannot be read or whose rows cannot be listed. Each
// such cause is a typed *Error (render_*), logged once per change.
//
// The package has no wiring of its own: the controller's host, the sidecar
// command and the [connect] config are CN-3a2's.
package tokenrender
