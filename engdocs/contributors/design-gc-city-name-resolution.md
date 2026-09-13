# Design: accept a bare city NAME (not just a path) across gc city-targeting commands

- **Status:** Implemented (shipped in PR #3625; the code and tests cited below are canonical where the draft prose disagrees)
- **Issue:** ga-m3ev9r
- **Follows:** PR #3623 (gc unregister fails loudly on unknown name/path)
- **Produced by:** multi-agent design workflow (explore → 3 proposals → adversarial review → synthesis)

> **Implementation note (shipped via PR #3625):** This design is implemented;
> the shipped helper names differ from the draft prose below, and the code in
> `cmd/gc/city_arg_resolve.go`, its tests in `cmd/gc/city_arg_resolve_test.go`,
> and `internal/supervisor/registry.go` are canonical when they disagree with
> this document. Draft → shipped renames: `classifyCityArg` →
> `classifyCityRef`; `cityArgKind`/`cityArgEmpty`/`cityArgPath`/`cityArgName` →
> `cityRefKind`/`cityRefEmpty`/`cityRefPath`/`cityRefName`; `cityArgOpts` →
> `cityRefOpts`; `resolveCityArg` → `resolveCityRef`; `resolveCityContextArg` →
> `resolveCityNameContext` (plus the path-returning `resolveCityNameRef` and the
> shared fact-gatherer `lookupCityNameFacts`); `cityArgNotFoundErr` →
> `cityRefNotFoundErr`. Per Decision 1, the single genuine ambiguity is a **loud
> failure** (`cityRefAmbiguousErr`, and `cityRefRigVsRegisteredErr` when a local
> rig directory shadows a different registered city), not the draft §3
> "path-wins + breadcrumb". `resolveCityNameContext` also runs a rig-path probe
> so a slashless local rig directory (e.g. `frontend`) resolves to its owning
> city across the positional `stop`/`start`/`restart`/`reload`/`suspend`/
> `resume`/`status` seams; the `--city <name>` flag is resolved by
> `resolveCityFlagValue`.

> **Critical insight from adversarial review (do not skip):**
> All three proposals share a fatal flaw the break-testers exposed: they frame name resolution as run-the-path-resolver-first then fall-back-to-registry-on-error. But every existing path resolver ends in findCity(cwd/token), which WALKS UP the directory tree (verified in city_discovery.go:19-49: from inside any city, findCity returns the ambient ancestor city with err=nil). So a bare name run from inside a city resolves to the AMBIENT city, the path resolver succeeds, the name fallback never fires, and stop/suspend/reload/restart silently mis-target; under --json this reports ok:true for the wrong city. The fix is to classify by arg SHAPE up front and ROUTE accordingly: a name-shaped token that is NOT an existing directory in cwd must skip the path resolver entirely and go straight to Registry.LookupCityByName. This is the only synthesis that resolves every blocking issue. I drop proposal 3's --as-name/--as-path escape-hatch flags (YAGNI; the single ambiguous case is handled deterministically with a stderr breadcrumb).

**Chosen approach:**
Hybrid: shape-classify-then-route (proposal 3 shape gate) plus per-command class awareness so register opts out of name fallback (proposal 2), MINUS the escape-hatch flags. The decisive correction over all three: a name-shaped token that does not name an existing *city* directory is routed directly to the registry and NEVER passed to the walk-up path resolver, eliminating the silent ambient-city mis-target. A single shared resolver plus Registry.LookupCityByName and IsValidCityName; each command injects its existing path resolver as a closure used only for the path-shaped and city-dir-exists branches.

---

## Decisions (resolved 2026-06-20 by maintainer)

These answers to the open questions are authoritative and **supersede** the
conflicting notes in §3, §5, and Appendix A below.

1. **No escape-hatch flags; ambiguity is a loud failure.** v1 ships no
   `--as-name`/`--as-path`. The only genuine ambiguity — a name-shaped token
   that is BOTH an existing local **city** directory AND a registered name
   pointing at a **different** path — is a **loud failure** (exit 1): the
   command refuses and tells the user to disambiguate (`./<dir>` for the local
   path, or `cd` elsewhere to use the registered name). This replaces the
   workflow's "path-wins + breadcrumb" (old RULE 2/RULE 4). **Refinement that
   closes a walk-up sub-hole:** a name-shaped token only takes the PATH branch
   when `cwd/<token>` is an actual city (`citylayout.HasCityConfig`), not
   merely an existing directory — a non-city dir of the same name must NOT be
   fed to `findCity`'s upward walk. Non-ambiguous cases are unchanged: a
   path-shaped arg always means path; a registered name with no local city dir
   resolves via the registry; a genuine miss fails loudly (preserves PR #3623).

2. **`--city <name>` and `GC_CITY` are IN SCOPE from the start** (was deferred
   in §5). The shared resolver is reframed as a single "city reference"
   resolver applied to (a) the positional arg, (b) the `--city` persistent
   flag, and (c) the `GC_CITY` env var. `GC_CITY_PATH` / `GC_CITY_ROOT` stay
   path-only (their names denote paths). This brings the central resolution
   sites into wiring scope: `resolveContext` steps 1–2 (main.go ~466,475),
   `resolveExplicitCityPathEnv` (city_context.go), and `resolveStartDir`'s
   direct `filepath.Abs(cityFlag)` (cmd_start.go ~571). Because these feed
   nearly every command, this is the highest-risk wiring and gets its own
   phase with dedicated precedence/regression tests.
   **Refinement (precedence for GC_CITY):** the positional arg and `--city`
   flag are explicit user input and use the loud-ambiguity policy of
   decision 1. `GC_CITY` is ambient and deliberately set, so it uses
   **path-first / local-wins**: a same-named local city directory wins over a
   different registration silently (no loud ambiguity), matching the env's
   "this is my working city" intent. Per-command `--city` flags that bypass
   the central chain (e.g. `gc bd --city`) remain path-only and are tracked as
   a separate follow-up.

3. **`gc restart` resolves the reference once at the top** and threads the
   resolved **path** (never the raw name) into both the stop and start legs
   (as already specified in §5).

4. **`gc register` completion shows registered city names as hints** (with
   file completion still enabled), to help users avoid double-registering.

---

## 1. Context and goal

gc city-targeting commands (register, unregister, start, stop, restart, reload, suspend, resume, status) today treat their optional positional argument strictly as a path. Passing the name shown by `gc cities` (e.g. `gc unregister chris-city`) silently resolves to cwd/chris-city, which usually is not a city, and the command fails or, far worse, walks up to the ambient city and acts on the wrong target. PR #3623 added a fail-loudly-on-no-match guard to unregister (writeUnregisterNotRegistered, cmd_register.go:196-206). This design is the follow-up that makes the intuitive name form work, while preserving PR #3623 fail-loudly for genuine misses. Goal: every city-targeting command accepts either a directory PATH (today behavior, byte-for-byte unchanged) or a bare registered city NAME resolved against the supervisor registry, with deterministic disambiguation and zero regression for path callers.

The load-bearing constraint the naive design gets wrong: validateCityPath (main.go:594-603), resolveContextFromPath (main.go:565-591), and resolveStopCityPath (cmd_stop.go:154-177) all end in findCity(abs) (city_discovery.go:19-49), which walks upward looking for an ancestor city.toml/.gc (bounded only by HOME and TMPDIR ceilings). From cwd /x/mycity/sub, findCity(/x/mycity/sub/chris-city) returns /x/mycity with err nil. Therefore try-path-first-then-registry-on-error is unsafe: from inside any city a bare name never errors, it walks up to the ambient city, and under --json the command reports ok:true for it. This design classifies by shape before any resolution and only invokes the walk-up path resolver for tokens that actually denote a path or an existing directory.

## 2. Resolution algorithm

A registered city name can never contain a slash: validCityName requires a leading alphanumeric and allows only alphanumerics, dots, underscores, hyphens (registry.go:24). name-shaped means supervisor.IsValidCityName(TrimSpace(arg)); path-shaped means not name-shaped (contains a separator, leading dot or tilde, is absolute, empty, or has any out-of-class character).

RULE 0 no arg (unchanged): keep each command current no-arg behavior (resolveCommandCity(nil) / resolveContext / cwd). Name resolution applies only to an explicitly supplied positional.

RULE 1 path-shaped means PATH, no registry IO: run the command existing path resolver verbatim and return its result including its error. Byte-for-byte unchanged for ., ./x, ../x, ~/x, /abs, rel/dir.

RULE 2 name-shaped AND an existing directory in cwd means PATH wins: if cwd/token exists as a directory, run the existing path resolver against it. Path wins the single genuine collision (RULE 4 adds a breadcrumb when that directory shadows a same-named registration elsewhere). Existence is a single os.Stat on filepath.Join(cwd, token); it does NOT walk up.

RULE 3 name-shaped AND NOT an existing directory means REGISTRY ONLY: the fix for the walk-up footgun. The token is NOT fed to the path resolver. Instead call Registry.LookupCityByName(token): hit returns entry.Path (absolute; flows through normal downstream validation such as requireBootstrappedCity and registeredCityEntry); miss returns a combined error (RULE 5). The os.Stat in RULE 2/3 replaces the implicit cwd/token walk-up with an exact non-ascending existence check, so a name-shaped token never resolves to an ancestor city.

RULE 4 collision breadcrumb (non-fatal, stderr only): when RULE 2 fired AND LookupCityByName(token) also hits AND the resolved local dir abs path differs from entry.Path, print to stderr a note that token is also a registered city at entry.Path, using local directory absDir, cd elsewhere or pass an absolute path to target the registered city. Suppressed under --json (stderr only; stdout JSONL untouched). Same path means no note. Cost: one extra LookupCityByName only on the rare name-shaped plus dir-exists branch.

RULE 5 combined miss (fail loudly, preserves PR #3623): if neither a path nor a registered name resolves, return a structured error naming both attempts; exit 1 with empty stdout so run() central handler (main.go:173-186) emits ok:false under --json. No fabricated success.

RULE 6 register opts out of name fallback: register creates a new registration for a path, so a name match is meaningless. It skips RULE 3 registry lookup; a name-shaped non-directory yields the existing not-a-city-directory error plus a clarifying note if the token already names a registration.

## 3. Ambiguity policy and escape hatches

The shape gate collapses ambiguity to one case: a bare name-shaped token that is simultaneously an existing relative directory under cwd and a registered city name pointing at a different absolute path. Policy: PATH WINS, with a visible breadcrumb (RULE 4). Name-first would silently retarget gc stop chris-city away from a real ./chris-city in cwd, the more dangerous surprise and a regression of today path semantics. Path-wins guarantees zero regression for existing path callers; the breadcrumb keeps it non-silent. No escape-hatch flags in v1: an earlier proposal added --as-name/--as-path; rejected as YAGNI since the single collision is rare, deterministic and breadcrumbed, and threading two mutually-exclusive flags through nine commands plus the fixed-signature resolveContextFromPath roughly doubles the surface. The escape hatch that already exists is trivial: pass an absolute or ./-prefixed path to force PATH, or cd out of the colliding directory to force NAME. Name comparison is exact and case-sensitive on the stored EffectiveName() snapshot, matching Register dedup (registry.go:144) and LookupRigByName (registry.go:509-524). The registry is the source of registration identity: a post-registration rename in city.toml/site.toml does not change the registered-name lookup; this matches what gc cities shows (gascity#602) and is documented in help text.

## 4. Shared helper API

Registry layer, internal/supervisor/registry.go, mirroring LookupRigByName exactly: func (r *Registry) LookupCityByName(name string) (CityEntry, bool) takes RLock, loads entries, returns the first entry whose EffectiveName() equals name with true, else zero plus false (and false on load error). Also func IsValidCityName(s string) bool returns validCityName.MatchString(TrimSpace(s)) so the CLI can classify without importing the regexp. No new locking or load paths.

CLI layer, new file cmd/gc/city_arg_resolve.go. A cityArgKind enum (cityArgEmpty, cityArgPath, cityArgName) and classifyCityArg(raw) string-only: empty if TrimSpace is empty; cityArgName if supervisor.IsValidCityName(raw); else cityArgPath. A cityArgOpts struct carries cmd (diagnostic name), allowNameFallback (false for register), and warn io.Writer (breadcrumb sink, nil to suppress). The core resolver func resolveCityArg(raw string, opts cityArgOpts, pathResolve func(string) (string, error)) (string, error) where pathResolve is the command EXISTING path resolver injected as a closure. Behavior: cityArgEmpty and cityArgPath return pathResolve(raw) directly (RULE 0/1). For cityArgName: compute dir as filepath.Join(cwd, raw); if isExistingDir(dir) call pathResolve(raw) and on success call maybeWarnCityNameCollision (RULE 2/4); otherwise if opts.allowNameFallback do reg.LookupCityByName(raw) and return entry.Path on hit (RULE 3); else return cityArgNotFoundErr (RULE 5/6). CRITICAL: pathResolve is invoked ONLY for path-shaped or directory-exists tokens, so the findCity walk-up cannot silently resolve a bare name to an ambient ancestor city. Helpers isExistingDir, maybeWarnCityNameCollision (does the extra LookupCityByName plus samePath compare, prints RULE 4 to opts.warn), and cityArgNotFoundErr live in the same file. A second wrapper func resolveCityContextArg(args []string, opts cityArgOpts) (resolvedContext, error) returns a resolvedContext for family-A commands that need RigName; on a name hit it builds a city-only context (no rig). Where it hooks in: resolveCityArg and resolveCityContextArg are the single shared seam; each command keeps its own pathResolve closure so the path branch is identical to today. It is intentionally NOT wired into resolveContextFromPath (main.go:565) because that has a fixed (path string) signature, is also called by requireBootstrappedCity and resolveContext, and embeds the walk-up; wrapping it would either change its signature everywhere (merge-hostile) or re-introduce the walk-up footgun.

## 5. Per-command scope

unregister [path|name] (registry-required): PRIMARY. Replace the filepath.Abs plus normalize branch (cmd_register.go:144-148) with resolveCityArg(args[0], opts allowNameFallback true warn stderr, pathFn) where pathFn is filepath.Abs then normalizePathForCompare. Keep registeredCityEntry fail-loud (:156-173); rewrite writeUnregisterNotRegistered. Headline fix.

stop [path|name] (create-capable): wrap the len(args)>0 body of resolveStopCityPath (cmd_stop.go:154-177) as pathFn; call resolveCityArg with allowNameFallback true. Standalone/unregistered-dir stops preserved.

start [path|name] (create-capable): in resolveStartDir (cmd_start.go:566-575), route the len(args)>0 case through resolveCityArg BEFORE filepath.Abs(args[0]). Name fallback fires only when cwd/token is not a dir; requireBootstrappedCity still validates .gc and errors clearly for a stale registration. New/standalone path dirs never regress.

restart [path|name] (create-capable): resolve NAME to PATH ONCE at the top of cmdRestartJSON and thread the resolved PATH (not the raw name) into both cmdStop and doStartWithNameOverride so the three legs cannot re-resolve a bare name to different targets. restartRegistrationName uses resolveCityArg for the JSON city_name.

reload [path|name] (registry-friendly): cmdReload calls resolveCityContextArg(args, opts) instead of resolveCommandCity(args) (cmd_reload.go:127). Name selects which controller socket.

suspend / resume [path|name] (registry-friendly): resolveSuspendDir (cmd_suspend.go:112-114) calls resolveCityContextArg. Standalone cities still work via the dir-exists branch.

status [path|name] top-level newStatusCmd (cmd_citystatus.go): cmdCityStatus (cmd_citystatus.go:147) calls resolveCityContextArg. This is TOP-LEVEL city status, distinct from gc rig status. Existing --json resolve-failure path (:149-151) carries the combined error.

register [path] (create-only): path-first, NO name fallback (allowNameFallback false). Keep validateCityPath(args[0]). A name-shaped non-dir yields not-a-city-directory plus a takes-a-path-not-a-name note, plus a note if the token already names a registration.

supervisor start/stop/status/reload/run/logs/install/uninstall: OUT OF SCOPE, all cobra.NoArgs (cmd_supervisor.go:74,108,127,927), operate on the machine-wide supervisor, take no city positional.

--city <path> flag and GC_CITY/GC_CITY_PATH/GC_CITY_ROOT env: OUT OF SCOPE for v1; they flow through validateCityPath (main.go:466,475,659) and resolveStartDir direct filepath.Abs(cityFlag) (cmd_start.go:571). Deferred; positional name support covers the headline cases.

gc rig status/suspend/resume/restart [name]: OUT OF SCOPE; they resolve a RIG name against cfg.Rigs, a different mechanism (config, not registry).

Prior-art correction: the brief cited gc status [name] and gc rig suspend/resume [name] as reusable name-to-target resolvers. Verified false: top-level gc status resolves via resolveCommandCity then resolveContextFromPath with NO registry name lookup; gc rig commands resolve a rig name from cfg.Rigs. The only genuine precedent is LookupRigByName; LookupCityByName is built fresh.

## 6. UX: success, error, help text

Success (unchanged shape): gc unregister chris-city prints City unregistered.; --json success stays ok:true via writeLifecycleActionJSONOrExit with CityName equal to entry.EffectiveName(). Errors: combined miss (gc stop bogus) prints that bogus is not a city directory (no city.toml or .gc at cwd/bogus) and is not a registered city name, run gc cities to see registered cities. unregister genuine miss rewrites writeUnregisterNotRegistered dropping the now-false treated-as-a-path-not-a-name line, to a message that chris-city is not a registered city name and cwd/chris-city is not a registered city, run gc cities then gc unregister name-or-path. register name-shaped non-dir prints not a city directory cwd/chris-city no city.toml found, gc register takes a path to a city directory not a registration name, plus note chris-city is already a registered city when applicable. start name resolves but .gc missing uses the existing message via the resolved path: city runtime not bootstrapped at path, run gc init path first. Breadcrumb (stderr, action proceeds, suppressed under --json): note that chris-city is also a registered city at /srv/chris-city, using local directory cwd/chris-city, cd elsewhere or pass an absolute path to target the registered city. Help text: for register, unregister, start, stop, restart, reload, suspend, resume, status change Use from <cmd> [path] to <cmd> [path|name] and add to Long that it accepts either a path to a city directory or a registered city name (as shown by gc cities), a name is resolved against the supervisor registry, and an existing local directory of the same name takes precedence. For register, Long states it takes a path only.

## 7. Shell completion

Add completeCityNames and cityNameCandidates to cmd/gc/completion.go mirroring completeRigNames/rigNameCandidates (completion.go:47-120). completeCityNames early-exits (nil, ShellCompDirectiveDefault) when len(args)>0; otherwise returns cityNameCandidates(toComplete) with ShellCompDirectiveDefault. cityNameCandidates wraps work in quietDefaultLogger (mandatory, keeps log noise off the completion line), constructs supervisor.NewRegistry(supervisor.RegistryPath()), calls reg.List(), and for each entry whose EffectiveName has the toComplete prefix appends EffectiveName tab Path. Directive is ShellCompDirectiveDefault (NOT NoFileComp) because city args accept name-or-path, so the shell should offer registered names AND directories; reg.List needs no resolution context so completion works from any cwd. Wire as ValidArgsFunction on the constructors that have none today: newUnregisterCmd, newStopCmd, newStartCmd, newRestartCmd (city), newReloadCmd, newSuspendCmd, newResumeCmd, newStatusCmd (top-level), and newRegisterCmd (names as hints; file completion stays on). newCitiesCmd is NoArgs, skip. --city flag completion is deferred with the flag name support.

## 8. Backward compatibility and migration

Do existing absolute/relative PATH invocations behave identically? YES. Path-shaped args (., ./x, ../x, ~/x, /abs, rel/dir, anything with a separator) are classified cityArgPath and routed straight to the existing pathResolve with no registry IO: identical code path and error text. Preserves TestResolveCommandContextPathValidatesExactCityRootAtHomeBoundary and the unregister ghost-absolute-path tests (cmd_register_test.go:494-578). A name-shaped token that names an existing dir is cityArgName plus dir-exists, routed to pathResolve (path wins): preserves TestRigAnywhere_ResolveCommandContext arg-inside/arg-outside cases and standalone-dir start/stop. A name-shaped token with no local dir that is registered now resolves instead of failing (or, critically, instead of silently walking up to the ambient city): strictly additive. Walk-up safety: RULE 3 routes name-shaped non-dir tokens to the registry and never to findCity, so gc stop other-name from inside city A no longer silently stops A; behavior changes ONLY for previously-broken cases. JSON contract unchanged: success ok:true; miss exits 1 with empty stdout so run() emits ok:false (main.go:173-186). No registry write-path change. No --city/GC_CITY change. The one deliberate string change: writeUnregisterNotRegistered treated-as-a-path-not-a-name line is removed (now false); its comments at cmd_register_test.go:489-493,510-511 become false and must be updated. Audit confirms no test asserts that exact substring; tests assert no-registered-city and gc-cities which remain present.

## 9. Test plan

Registry, internal/supervisor/registry_test.go: TestLookupCityByName registers two cities, each name returns the right entry plus true, unknown returns zero plus false (mirrors TestRigLookupByName registry_test.go:559-583); TestIsValidCityName table accepts chris-city, a.b, a_b, 1city and rejects a/b, ./x, ../x, ~/x, /abs, empty, has-space.

Classifier/resolver, new cmd/gc/city_arg_resolve_test.go: TestClassifyCityArg table; TestResolveCityArg_PathShapedSkipsRegistry (foo/bar miss returns pathFn error verbatim; registry NOT consulted via a spy that fails the test if called); TestResolveCityArg_NameNoDirHitsRegistry (name-shaped, no cwd/token dir, registered returns entry.Path; pathFn NOT called, proving no walk-up); TestResolveCityArg_NameNoDirMissCombinedError (both not-a-city-directory and registered-city-name substrings); TestResolveCityArg_DirExistsPathWins (create cwd/token as a city dir AND register a same-name city elsewhere returns local dir, breadcrumb written to warn); TestResolveCityArg_DirExistsSamePathNoBreadcrumb; TestResolveCityArg_RegisterNoNameFallback (allowNameFallback false means registry never consulted on name-shaped non-dir); and the core-bug guard TestResolveCityArg_FromInsideCityDoesNotWalkUp (cwd is a real city subdir, register a DIFFERENT city by name, resolveCityArg(other-name) returns the registered other-city path, NOT the ambient city).

Per-command (each *_test.go, isolated GC_HOME via t.Setenv plus supervisor.NewRegistry(RegistryPath()).Register, cf. cmd_register_test.go:505-508): cmd_register_test.go gets TestDoUnregisterByName (success plus ok:true), TestDoUnregisterByName_PathWinsOverName (local ./name registered elsewhere means local dir unregistered plus breadcrumb), updates the false comments at :489-493,510-511, keeps ghost-absolute-path tests behaviorally green, and adds a bare-unknown-NAME miss producing loud failure plus ok:false; cmd_stop_test.go gets TestStopByRegisteredName, TestStopStandalonePathStillWorks, TestStopByNameFromInsideAnotherCity (inside A, gc stop B-name targets B); cmd_start_test.go gets TestStartByRegisteredName, TestStartNewUnregisteredPathStillWorks; cmd_restart_test.go gets TestRestartByRegisteredName, TestRestartByName_SingleTargetAcrossLegs; cmd_reload_test.go, cmd_suspend_test.go (suspend plus resume), cmd_citystatus_test.go get by-name resolves plus one path-arg case re-asserted. Regression guard: keep TestRigAnywhere_ResolveCommandContext and TestResolveCommandContextPathValidatesExactCityRootAtHomeBoundary (command_context_test.go:83-146) green unchanged. Completion: TestCompleteCityNames_EarlyExitOnExtraArgs and TestCityNameCandidates_LoadsAndFilters (name-tab-path shape, prefix filter), seeding via Register under isolated GC_HOME.

## 10. Risks and open questions

Two resolver families (family A reload/suspend/resume/status via resolveCommandCity vs bespoke stop/start/restart/unregister): mitigated by the single shared seam every entrypoint adopts; a grep checklist of all nine entrypoints is in the test plan. restart fan-out: cmdRestartJSON threads raw args into three legs; resolve once at the top and thread the PATH down (covered by TestRestartByName_SingleTargetAcrossLegs). Stale registry: LookupCityByName may return a path whose city.toml/.gc was deleted; name resolution returns only the path and downstream validation errors clearly referencing the resolved path so the user can gc unregister it (no worse than today). Symlink normalization: returned registry paths must flow through the same normalization the path branch uses; for unregister the resolved value re-enters registeredCityEntry then normalizeRegisteredCityPath (EvalSymlinks), verify in tests. EffectiveName snapshot vs live name: lookup matches the registered name not a post-registration rename (correct, gascity#602, documented). --city/GC_CITY: name support SHIPPED (the persistent --city flag and GC_CITY both accept a name; GC_CITY uses local-wins precedence per decision 2). Per-command --city flags that bypass the central chain (gc bd/import/analyze) remain path-only — tracked as a separate follow-up. Open questions: should register completion show names at all or files only (resolved: hints).

## 11. Phased implementation plan (each phase 5 files or fewer)

Phase 1 primitives (2 files): add Registry.LookupCityByName plus IsValidCityName to internal/supervisor/registry.go; add TestLookupCityByName plus TestIsValidCityName to internal/supervisor/registry_test.go; run package tests. Phase 2 shared CLI resolver (2 files): new cmd/gc/city_arg_resolve.go (classifyCityArg, resolveCityArg, resolveCityContextArg, breadcrumb plus error helpers); new cmd/gc/city_arg_resolve_test.go (full matrix incl. the walk-up regression guard); run. Phase 3 primary command plus diagnostic rewrite (2 files): wire unregister in cmd/gc/cmd_register.go incl. the writeUnregisterNotRegistered rewrite; update cmd/gc/cmd_register_test.go; run. Phase 4 create-capable commands (3 files): wire stop (cmd/gc/cmd_stop.go), start (cmd/gc/cmd_start.go), restart (cmd/gc/cmd_restart.go, single-resolution threading); verify standalone-path regression and from-inside-another-city tests. Phase 5 registry-friendly commands plus completion plus help (5 files): wire reload (cmd/gc/cmd_reload.go), suspend/resume (cmd/gc/cmd_suspend.go), status (cmd/gc/cmd_citystatus.go) through resolveCityContextArg; add completeCityNames plus ValidArgsFunction wiring plus Use/Long help-text edits (cmd/gc/completion.go plus constructors); add completion tests (cmd/gc/completion_test.go); run full cmd/gc suite plus go vet.

## Appendix A — Open questions

- Should gc register tab-completion offer registered city names as hints (to help users avoid double-registering) or stay file-only? Proposed: show names as informational hints with file completion still on (ShellCompDirectiveDefault).
- Is --city name (persistent flag) name support worth scheduling now, or deferred until a real user asks? It touches several validateCityPath call sites (main.go:466,475,659) plus resolveStartDir direct filepath.Abs(cityFlag) at cmd_start.go:571. Proposed: defer to a clearly-scoped follow-up.
- For the collision breadcrumb wording, is cd-elsewhere-or-pass-an-absolute-path the right guidance given there are no escape-hatch flags in v1, or should v1 ship a minimal --as-name on stop/unregister only?
- restart threads raw args into three legs; preferred mechanism to pass the single resolved PATH down: rewrite the args slice to one resolved path, or add path-accepting internal entrypoints for cmdStop/doStartWithNameOverride? The slice-rewrite is smaller but relies on every leg treating an absolute path identically.

## Appendix B — Phased implementation plan

1. Phase 1 - Registry primitives (2 files): add Registry.LookupCityByName plus IsValidCityName to internal/supervisor/registry.go (mirroring LookupRigByName, exact case-sensitive match on EffectiveName); add TestLookupCityByName plus TestIsValidCityName to internal/supervisor/registry_test.go. Verify package tests pass.
2. Phase 2 - Shared CLI resolver (2 files): create cmd/gc/city_arg_resolve.go with classifyCityArg, resolveCityArg (shape-classify-then-route: path-shaped and dir-exists go to injected pathResolve; name-shaped-no-dir goes straight to registry, never to the findCity walk-up), resolveCityContextArg, plus breadcrumb (RULE 4) and combined-error (RULE 5) helpers; create cmd/gc/city_arg_resolve_test.go covering the full matrix including TestResolveCityArg_FromInsideCityDoesNotWalkUp. Verify.
3. Phase 3 - Primary command plus diagnostic rewrite (2 files): wire gc unregister through resolveCityArg in cmd/gc/cmd_register.go and rewrite writeUnregisterNotRegistered to drop the now-false not-a-name line; update cmd/gc/cmd_register_test.go (add TestDoUnregisterByName, path-wins-over-name, bare-unknown-name miss; fix the false comments at :489-493,510-511). Verify.
4. Phase 4 - Create-capable commands (3 files): wire gc stop (cmd/gc/cmd_stop.go via resolveStopCityPath body as pathFn), gc start (cmd/gc/cmd_start.go: route resolveStartDir arg case through resolveCityArg before filepath.Abs), gc restart (cmd/gc/cmd_restart.go: resolve name-to-path once, thread the PATH into both stop and start legs). Verify, including standalone-path regression and from-inside-another-city tests.
5. Phase 5 - Registry-friendly commands plus completion plus help (5 files): wire gc reload (cmd/gc/cmd_reload.go), suspend/resume (cmd/gc/cmd_suspend.go), status (cmd/gc/cmd_citystatus.go) through resolveCityContextArg; add completeCityNames plus cityNameCandidates and ValidArgsFunction wiring plus Use/Long help-text edits in cmd/gc/completion.go and the command constructors; add completion tests in cmd/gc/completion_test.go. Run full cmd/gc suite plus go vet.

## Appendix C — Alternatives considered (review digest)

Three proposals were generated and each was scored by a maintainer reviewer and an adversarial break-tester:

- path-first-name-fallback
- class-aware-name-or-path
- shape-classified city name-or-path resolution with escape hatches

The break-tester rejected/down-scored the path-first approaches for the walk-up footgun above; the synthesis adopts the shape-classify-then-route gate that eliminates it.
