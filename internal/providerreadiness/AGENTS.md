# Provider runtime readiness

Inspect the provider's resolved family, executable and launch environment.
Provider names are user aliases; infer protocols from explicit inheritance.
Use only local status commands and bounded, cancellable subprocesses. No probe
starts a model conversation or changes credentials. Return a missing credential
verdict only when the native CLI says so. Unknown protocols or wrapper/config
arguments remain explicitly unverified, with a real-task verification hint.

Local configuration is distinct from successful remote inference. Accept the
native CLI's supported login modes equally; subscription-specific onboarding
policy belongs to its own projection. Never return or log native credential
output, environment values or tokens in diagnostic details.

Resolve bare executable names with the effective worker PATH. An explicitly
empty PATH must not fall back to the supervisor process environment.

Hook copy destinations must match the effective session runtime, including
per-agent overrides: local consumers resolve them from the session workdir;
container consumers retain their city-root projection. Keep managed hook
content fingerprints and unrelated user files intact.
