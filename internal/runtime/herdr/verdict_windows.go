//go:build windows

package herdr

// herdrVerdictCode returns the error code herdr answered an invocation with.
//
// herdr 0.9.1 on Windows reports every CLI error by exiting non-zero with one
// error envelope as the whole of stderr and nothing on stdout. That was measured
// for agent list, wait, prompt and start, and for agent_prompt_stalled from a
// live agent. So the typed envelope alone never carries a real herdr's verdict
// there. herdrAnswerCode reads both channels, and text this client passes to
// herdr can write neither.
func herdrVerdictCode(err error) string { return herdrAnswerCode(err) }
