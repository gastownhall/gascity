# Pack configuration diagnostics

Explicit configuration validation reuses pack doctor scripts, with a fresh
versioned JSON input from the same loaded configuration. Formula input names
are opaque pack data. Include every configured rig, even without an applicable
agent, and leave selection, optional fields and fallback policy to the script.

Capture formula provenance from the Source reads used by the real compiler,
including inheritance and expansion contributors. Paths identify lookup
locations; hashes identify selected bytes. Mark incomplete capture unavailable
with a reason. This is point-in-time evidence, not an atomic filesystem snapshot
or proof about a later run or about unrelated instruction files.

`RunConfigValidation` always sets the explicit marker, even for empty stdin;
ordinary Run/Fix must strip it. Keep diagnostic exit and stdout semantics,
bound subprocess execution, and never invoke fixes from config validation.
