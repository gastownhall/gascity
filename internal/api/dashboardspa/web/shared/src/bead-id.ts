// Bead-id validator for dashboard code that reads or acts on a bead by id.
//
// Supervisor-issued bead ids come in mixed shapes (gc-123, co-ysv,
// agent-diagnostics-y84, td-7t24i6, etc.); the gc-CLI BEAD_ID_RE
// /^(td|th|jt)-[a-z0-9-]{3,32}$/ is too narrow for any prefix outside
// td/th/jt. The char class excludes whitespace and shell metacharacters, so a
// matching id is safe to pass as an argv value and safe to use as a route ref.

export const BEAD_ID_RE = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/;
