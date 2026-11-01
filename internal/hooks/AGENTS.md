# Provider hooks

Managed commands must honor the launching binary through quoted
`"${GC_BIN:-gc}"`; append fallback install directories to the existing PATH.
The controller and its hook children must not silently use different builds.

Upgrade only exact known managed command shapes. Preserve user wrappers,
additional arguments, chained commands and custom hook events. Keep upgrades
idempotent and avoid duplicate hook execution. Legacy regression fixtures must
retain actual old bytes instead of deriving old commands from today's template.
