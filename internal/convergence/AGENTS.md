# Gate subprocess lifecycle

A canceled gate must stop its owned writers before a retry can reuse their
worktree. Preserve cooperative cancellation through execgrace so a script can
clean up descendants that own another process session. Keep cleanup bounded
separately from the condition deadline; cancellation never becomes a pass.

An explicit exit 78 during deadline cleanup remains a configuration hold.
Parent cancellation remains GateError and does not retry an already canceled
operation. Validate this boundary with the shipped validator helper and real
processes; retain partial diagnostics and preserve unrelated processes.
