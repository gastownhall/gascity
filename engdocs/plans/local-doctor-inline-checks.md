# Local Doctor Inline Checks Plan

Source bead: `ga-5frpc`
Architecture parent: `ga-9nzvf`
Priority: P2

## Goal

Let operators declare city-local doctor scripts directly in `city.toml`
with `[[doctor.check]]`, avoiding pack ceremony while reusing the existing
doctor script execution path.

## Work Packages

1. `ga-5frpc.1` — Tests: inline local doctor checks cover config, execution, and containment
   - Route: `gascity/validator`
   - Label: `needs-tests`
   - Acceptance: cover TOML parsing, `local:<name>` execution, path
     containment rejection, fix-script containment, and pack-check
     regression behavior.

2. `ga-5frpc.2` — As an operator, I can declare local doctor checks in city.toml
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-5frpc.1`
   - Acceptance: add `LocalDoctorCheck` and `DoctorConfig.Checks` with
     the singular `toml:"check,omitempty"` key while preserving existing
     doctor config behavior.

3. `ga-5frpc.3` — As an operator, local doctor checks run through gc doctor
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-5frpc.2`
   - Acceptance: register `cfg.Doctor.Checks` after pack checks, enforce
     city-root containment for script and fix paths, use `local:` names,
     and surface invalid paths as named doctor errors.

4. `ga-5frpc.4` — Docs: document `[[doctor.check]]` local doctor hooks
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-5frpc.3`
   - Acceptance: document city-local check syntax, `local:` prefix
     behavior, relative path containment, optional fix scripts, and the
     reused exit-code protocol.

## Dependency Graph

`ga-5frpc.1` blocks `ga-5frpc.2`, which blocks `ga-5frpc.3`, which
blocks `ga-5frpc.4`.

## Guardrails

- Reuse `doctor.PackScriptCheck`; do not duplicate the execution engine.
- Keep registration inside the `cfgErr == nil` path.
- Keep `PackName` empty for local checks so no pack state dir is injected.
- Do not add a warmup field in this slice.
- Do not let configured paths escape the city root.
