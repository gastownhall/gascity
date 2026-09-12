# Retiring the beads `pinned` boolean — phased plan

Written 2026-08-02. The ruling this implements is recorded on `ga-ktn9pe.4.14`; this file is the
work order.

## The decision in one paragraph

The `pinned` boolean column on beads issues and wisps is removed. `status='pinned'` becomes the
sole pin mechanism. Surfaces that spell the word — `bd list --pinned`, the `pinned:` query
predicate, the `pinned_issues` stat — survive as sugar over the status; the boolean beneath them
does not. The load-bearing fact: all three destructive commands pre-filter their candidate sets
to `Status=closed` before consulting the pin (`gc.go:84-90`, `purge.go:114-118`, `cleanup.go`),
so a status-pinned bead is never a deletion candidate in the first place. The boolean's only
unique capability is protecting a bead that is closed **and** pinned — a state the status cannot
express, that is conceptually incoherent, and of which a fleet census found exactly zero
instances.

## Why this is safe to do now

Two rows carry `pinned=1` fleet-wide: `mc-5pt81` and `mc-yl5fo` in maintainer-city, both
`bd:memory` beads, both carrying `status='pinned'` **and** the boolean, both set at create time.
They need no data migration — the status protects them identically. The only writer is an
unmerged prototype whose every boolean read is strictly conjunctive with the status, and whose
own violation message calls it "the pinned compatibility marker".

**This is the last cheap moment.** Once memory beads land and rows accumulate, removal needs a
data migration and a conversation with whoever owns them.


## Point of no return

Phase 9 (the guarded DROP COLUMN migration pair). Every phase before it is a clean git revert
because the column and DB rows are untouched. Once the migration runs on any shared database,
reversal is fix-forward only: a new migration re-ADDing the column with DEFAULT 0, with the
(redundant) pinned=1 values of mc-5pt81/mc-yl5fo unrecoverable and historical values unreadable
via dolt_history_issues. Phase 9 therefore ships alone, re-numbered at land time, and only after
Phases 7 and 8 are deployed fleet-wide including every bd-enterprise deployment sharing a hosted
DB.

## Phases

### Phase 0 — Gates (NOT yet done — both block Phase 1)

**Changes.** GATE A — hosted census. The local fleet census could not reach the hosted gateway. Run a READ-
ONLY aggregation against every issues/wisps table the hosted gateway serves, using the normal
hosted-read path (EIA credential via BEADS_DOLT_CREDENTIAL_COMMAND, per the beads-1045
convention — never a direct dolt server touch): `SELECT status, COUNT(*) FROM issues WHERE
pinned = 1 GROUP BY status;` and the same for `wisps`, per database. PASS iff every returned row
has status='pinned' (in particular: zero rows with status='closed' and zero with any open
status). Any closed+pinned or open+pinned row is exactly the state only the boolean protects —
it invalidates the 'migration is free' premise and goes back to the owner before Phase 1. GATE B
— shared-agent-memory owner ack. Get written confirmation from the owner of beads
feature/shared-agent-memory-prototype / the mc-hpshy field trial that status='pinned' alone
carries the memory-bead contract, and record it on ga-ktn9pe.4.14. The code answer is already
yes (every boolean read is conjunctive with status — projection.go:150-154,
memory_activate.go:378-379, issueops/generation_cas.go:81,525), but the branch's own comments
call it a designed 'compatibility marker', so the supersession must be explicit, not inferred.

**Proven by.** Gate A: the SQL result set pasted onto the bead (statuses all 'pinned'). Gate B: owner ack
recorded on the bead.

**Depends on.** Nothing.

**Reversal.** N/A — read-only.

### Phase 1 — beads: close the last origination path (graph plans)

**Changes.** cmd/bd/graph_apply.go: delete GraphApplyNode.Pinned (~:64) and the `issue.Pinned = node.Pinned`
assignment (~:870); knownGraphNodeFields self-updates (built by reflection from json tags,
:152). Add graphFieldHints entry: `"pinned": "use \"status\": \"pinned\" — the pinned status is
the single pin mechanism"`. Update cmd/bd/help_supplements/create_graph_plan.md (drop the pinned
example, :32). Adjudicated behavior (verified this session, origin/main db966433eb): plans
carrying "pinned" hit warnUnknownGraphFields (:264-286, invoked :342-343) — stderr warning
naming the field + hint, field dropped, exit 0. Warn-with-hint, not reject; the auditor claim of
hard rejection was wrong. ~3 files + tests.

**Proven by.** graph_apply tests: new assertion that a plan with "pinned": true warns with the corrective hint
and creates the bead un-pinned; a plan with "status": "pinned" still pins
(validateGraphApplyNodeFields:657 accepts it). scripts/check-cli-docs-drift.sh.

**Depends on.** Phase 0.

**Reversal.** git revert — field and column still exist everywhere else.

### Phase 2 — beads: close guard goes status-only

**Changes.** internal/validation/issue.go: drop `issue.Pinned ||` from NotPinned (:66) and rewrite the doc
comment (:51-58) to state status-only semantics. cmd/bd/close.go: keep the ga-ktn9pe.4.8 skip-
validation branch (closed-re-close idempotence) but rewrite its boolean-pinned rationale comment
(:126-140). Rewrite internal/validation/issue_test.go and cmd/bd/show_unit_helpers_test.go
boolean legs. 4 files.

**Proven by.** internal/validation/issue_test.go; cmd/bd/show_unit_helpers_test.go; oracle-a
enumerated.json:405-420 (update --status pinned then close) stays green as the regression
sentinel — invisible change for all real rows since the guard was already status-aware.

**Depends on.** Phase 0. Independent of Phase 1.

**Reversal.** git revert.

### Phase 3 — beads: query-language predicate becomes status sugar

**Changes.** internal/query/parser.go:338 + evaluator.go (:191-192, :477-478, :722-723): alias `pinned:true`
→ status='pinned' and `pinned:false` → status != 'pinned' (ExcludeStatus machinery exists,
evaluator.go:220,595); in-memory predicate re-keys `i.Pinned` → `i.Status == StatusPinned`.
Update docs/cli-reference/query.md:46 to describe it as a status alias. RULED: alias, not
removal — outright removal is the one hard script break (documented predicate → parse error). ~4
files.

**Proven by.** internal/query/query_test.go with new alias cases, including: status-pinned row now MATCHES
pinned:true (the divergence is the point of the ruling; zero live rows diverge today per
census). Docs-drift check.

**Depends on.** Phase 0. Independent of 1-2.

**Reversal.** git revert.

### Phase 4 — beads: bd list --pinned/--no-pinned re-key to status

**Changes.** internal/workapi/list.go (:308-314): --pinned → Status=pinned, --no-pinned → ExcludeStatus
(already the default at :205-206); the `bd list --status hooked` boolean-suppression special
case (:311) dissolves. cmd/bd/list.go / list_input.go flag wiring; regenerate
internal/workapi/testdata/list_filter_golden.json; docs/cli-reference/list.md. RULED: keep both
flags as status sugar — help text ('Show only pinned issues') stays literally true, and the re-
key FIXES a latent bug: a status-only pin is invisible to `bd list --pinned` today. Do NOT yet
delete IssueFilter.Pinned (compile-coupled into Phase 7); just stop populating it. ~5 files.

**Proven by.** list_embedded_test.go, list_helpers_test.go, regenerated golden (currently pins Pinned:false
into nearly every case — forgetting it fails every list test); new test: status-pinned bead
appears under --pinned.

**Depends on.** Phase 0. Independent of 1-3.

**Reversal.** git revert.

### Phase 5 — beads: stats + display re-key

**Changes.** internal/storage/issueops/statistics.go (:11,22,30): SUM(pinned=1) → COUNT(status='pinned');
RULED: KEEP the `pinned_issues` JSON key (types.go:1690, no omitempty — always present; MCP
models.py:286 tolerant either way but keep it). cmd/bd/list_format.go:23-25,
show_format.go:287-288, format/format.go (:63,72,94,104): delete the boolean 📌/'Pinned: yes'
indicators — format.go:163 already renders 📌 for the STATUS, so re-keying would duplicate. ~5
files.

**Proven by.** cmd/bd/status_test.go, count_filter_test.go; conformance audit_search-counts-stats.go:261-294
rewritten to status-keyed counting (its comment explicitly isolates the column — that
distinction is what's being abolished). Output identical for the two dual-marked mc rows.

**Depends on.** Phase 0.

**Reversal.** git revert.

### Phase 6 — beads: delete redundant boolean checks on destructive + ready/wisp/doctor paths (two PRs)

**Changes.** PR 6a — destructive commands: cmd/bd/closed_delete_candidates.go (:10,30-31) drop the
issue.Pinned skip; candidates are pre-filtered to Status=closed (gc.go:84-90, purge.go:114-118)
so the check can never fire post-removal. RULED: KEEP the `pinned_skipped` JSON key and 'Pinned
(skipped)' wiring emitting a definitional 0 (purge.go, purge_proxied_server.go, cleanup.go) —
zero wire breakage. ~5 files. PR 6b — non-destructive protections:
internal/storage/sqlbuild/ready.go:109 drop `(pinned = 0 OR pinned IS NULL)`;
issueops/ready_work.go (:301-310 populate, :433 check); cmd/bd/wisp.go isProtectedWisp (:700-718
— StatusPinned already protected via CategoryFrozen, :661,677; the :709 comment claiming the
flag is 'independent of the status' is overruled by this ruling);
cmd/bd/doctor/maintenance.go:94-98 raw SQL + doctor/fix/maintenance.go:63-85; scripts/bench-
ready-indexes/main.go (:185,197). Rewrite conformance audit_dependencies_readiness.go:395-404
(testAuditReadyTypeAndPinnedExclusions creates Open+Pinned:true — the exact boolean-only
capability being removed) to Status=StatusPinned. ~5 files + conformance.

**Proven by.** 6a: closed_delete_candidates_test.go rewrite; purge/cleanup --json goldens show
pinned_skipped:0. 6b: rewritten conformance readiness audit green against every conforming
store; ready_embedded_test.go; doctor/maintenance_cgo_test.go; wisp tests.

**Depends on.** Phase 0. The raw-SQL deletions here MUST land before Phase 9 or those queries hard-fail post-
drop.

**Reversal.** git revert per PR.

### Phase 7 — beads: remove the wire field and storage plumbing (the one necessarily-large atomic PR)

**Changes.** Everything compile-coupled through types.Issue.Pinned, in ONE commit because splitting is a
build failure: types.go:137 delete Issue.Pinned; :1762-1763 delete IssueFilter.Pinned;
ComputeContentHash :198 — RULED: replace `w.flag(i.Pinned, "pinned")` with a literal
`w.h.Write([]byte{0}) // retired pinned-flag slot; keeps hashes stable` (flag always wrote the
separator, :260-265; naive deletion churns EVERY issue's hash and breaks the cross-clone
determinism contract at :174-176 — verified this session); sqlbuild.go:42 IssueSelectColumns +
ALL positional scans (issueops/scan.go, issueops/history.go, dolt/history.go —
domain/db/issue.go:35-37 aliases the column list, so both plumbings move together); INSERT lists
(issueops/helpers.go:115,139; domain/db/issue.go:687,730 — issueUpsertColumns already excludes
pinned, import-UPDATE leg untouched); WHERE builders (sqlbuild/filter.go:203-207,
dolt/transaction.go:540-545); update allowlists BOTH plumbings (issueops/update.go:25,
domain/db/issue.go:44) + auto-clear block (update.go:443-457) + matchesBool (:700-701);
public_create.go:125; openapi.v0.yaml 4 sites (:969,1071,1152,1277) + `make api-gen`; the ~28
test files (grep `Pinned: true|pinned = 1|--pinned`). Old JSONL with "pinned":true still imports
(unknown-field drop); optionally add a one-line import notice for the dropped key — the only
zero-feedback surface. cmd/bd/info.go changelog entries (:1155, :1201): leave — changelog
history is immutable, and the :1155 '--pinned works' line describes a flag that no longer exists
on main (verified: bd update registers no --pinned).

**Proven by.** Full suite. Specifically: write_verbs_parity_test.go / parity_direct_test.go / serve-reads
parity (both plumbings must move together or parity fails); the conformance suite; the OpenAPI
spec-sync check + httpapi/pinning.go compile assertions; dolt/schema_parity_test.go stays green
(column still exists in DB AND migrations — nothing compares code to schema); content-hash tests
unchanged (placeholder preserves the byte stream).

**Depends on.** Phases 1-6 (they exist to shrink this PR to the compile-coupled core). MUST precede Phase 9 by
at least one full release.

**Reversal.** git revert — safe as long as no drop migration has shipped, which is exactly why Phase 9 is
separate.

### Phase 8 — bd-enterprise: sync the fork before any migration ships

**Changes.** Merge upstream beads through Phase 7 into /data/projects/bd-enterprise. Low-conflict: the fork's
pinned surface is a strict SUBSET of beads main (its NotPinned at
internal/validation/issue.go:57 is already status-only; it lacks main's update.go matcher,
close.go boolean handling, and wisp boolean protection). Its enterprise-specific 'pinned' hits
are homonyms (DPoP jkt pinning, Dolt connection pinning, version pinning) — untouched.
Coordinate Phase 9's migration version numbers with the fork's sequence NOW to avoid a
duplicate-version collision on rebase. Deploy the synced binaries to every enterprise deployment
that shares a hosted Dolt DB with a beads-main binary — answering the open question of who owns
that sequencing is part of this phase's exit criteria.

**Proven by.** Fork CI green post-merge; `git grep -n 'issue\.Pinned\|\.Pinned\b' -- 'cmd/bd'
'internal/storage' 'internal/types' 'internal/validation'` returns only homonyms; all shared-DB
deployments confirmed on the synced binary.

**Depends on.** Phase 7. HARD-BLOCKS Phase 9.

**Reversal.** Revert the merge on the fork (standard).

### Phase 9 — beads: drop the column — POINT OF NO RETURN

**Changes.** One PR, two migration files, nothing else: (a) main-plane guarded fix-forward migration (next
free number at land time; auditors cited next-after-0062 against 2af843a601 — RE-NUMBER at land
time, main has moved): INFORMATION_SCHEMA-guarded `ALTER TABLE issues DROP COLUMN pinned` +
guarded wisps drop, exactly the 0060_add_storage_class idempotence pattern (DROP precedent:
ignored/0005); (b) the MANDATORY ignored-series twin (next after 0021) dropping wisps.pinned —
wisps is dolt_ignored (0019), fresh clones materialize it from the ignored series alone and
never run a main-plane wisps ALTER (hygiene check D; bd-hs7fa precedent). Guard both so double
application is a no-op. Also update migration_repairs.go:140 (repair DDL still lists the column)
in the same PR. Ship ONLY after the entire fleet — including every bd-enterprise deployment
sharing a hosted DB — runs Phase-7+ binaries. The two mc rows (mc-5pt81, mc-yl5fo) need NO data
migration: status='pinned' protects them identically; their pinned=1 is silently discarded, and
that discard is correct.

**Proven by.** scripts/check-migration-hygiene.sh (checks A-D); dolt/schema_parity_test.go post-migration;
fresh-clone wisps materialization; run-twice idempotence of both guards.

**Depends on.** Phases 7 AND 8, each fully deployed fleet-wide. This is the point of no return.

**Reversal.** Fix-forward ONLY (migration hygiene forbids editing shipped migrations): a NEW guarded migration
re-ADDing the column with DEFAULT 0. The two rows' pinned=1 values are NOT restored — acceptable
and ruled, since the value was redundant with their status. Historical pinned values at old dolt
commits become unreadable via dolt_history_issues regardless (history tables mirror current
schema).

### Phase 10 — downstream: Gas Town at its next SDK bump; Gas City nothing

**Changes.** Gas Town: delete internal/mail/store.go:251 (`Pinned: si.Pinned`) in the SAME commit as its next
beads SDK bump past Phase 7 — the value is already dead (ToMessage(), types.go:387-439, never
copies it) and it is the single compile-time dependency; Gas Town pins beads v1.0.0 so there is
no urgency. The inert `gt mail send --pinned` flag is a SEPARATE Gas Town-owner decision (see
doNotBundle). Gas City: zero changes in any phase — no boolean consumers, native store has no
such column (native_dolt_store.go:805,962,1985,2162 never touch it); bump its two lockstepped
bd-version knobs (go.mod + deps.env) on normal cadence. Prototype branches: if the shared-agent-
memory field trial is ever revived, it rebases onto post-removal main and drops
candidate.go:158,164 `Pinned: true`, projection.go:150-155 `!issue.Pinned`,
memory_activate.go:378-379 CAS precondition, generation_cas plumbing, memory_list.go:221 — all
loud compile errors, all capability-neutral.

**Proven by.** Gas Town builds at bump time (the field access is a compile error otherwise). Gas City: nothing
to prove.

**Depends on.** Phase 7 (for the SDK bump). No ordering constraint on the rest of the campaign.

**Reversal.** Trivial per-repo reverts.

## What this makes moot — and what survives

- ga-ktn9pe.4.9 (facade close has no pinned check): boolean half CLOSED-AS-MOOT — there is no
boolean to check. Status half SURVIVES and gains weight: under the single mechanism,
transitioning status away from 'pinned' via close is the ONLY way a pinned bead can lose
gc/purge/cleanup immunity, so a close path that skips the status-only validation.NotPinned guard
is a live protection hole, not a stale nit. Re-scope the bead to 'facade close must enforce
NotPinned (status)'; do not close it.
- ga-ktn9pe.4.17 (fourth close path): same disposition — boolean half CLOSED-AS-MOOT, status half
survives re-scoped to the status-only NotPinned guard on that path. Do not close.
- Ruling 10 / ga-z3vht, boolean half: CLOSED-AS-MOOT in full — every provision of that ruling that
concerned the `pinned` column/field is void. The status-based half of ruling 10 stands unchanged
and is unaffected by this removal.

## Do not bundle

- NEVER bundle the code removal (Phase 7) with the schema drop (Phase 9). Migrations run at new-
binary startup against shared dolt databases that older binaries may still serve; a pre-removal
binary against a post-drop DB hard-fails EVERY issue scan and insert (positional SELECT/INSERT
lists name `pinned` — Error 1054). Two releases, minimum.
- MUST bundle (the inverse rule): the main-plane drop migration and its ignored-series wisps twin
ship in the same PR (hygiene check D; bd-hs7fa precedent). And within Phase 7, the types.Issue
field, the openapi.v0.yaml edits (4 sites), `make api-gen`, and the SELECT/scan/INSERT edits
across BOTH plumbings are one atomic commit — spec-sync tests and compile coupling make any
split a build failure.
- Do NOT bundle the Gas Town `gt mail send --pinned` flag removal into this campaign. It is
already a no-op on the beads path (Message.Pinned never serialized — router.go:1126-1162,
buildLabels :235-255) but it is user-visible Gas Town CLI surface; it needs its own Gas Town-
owner ruling. Only the one-line `store.go:251` delete is forced, and only at SDK-bump time.
- Do NOT bundle any status-string sweep. The surviving mechanism IS `status='pinned'`:
internal/linear/mapping.go:450-451, ui/styles.go, statuses.go:24, every `status <> 'pinned'` SQL
in issueops/blocked*.go, dolt/queries.go:127, domain/db/*, workapi/list.go:206, and migrations
0017-0047 view SQL are all LEAVE ALONE. An over-eager grep-sweep here removes the mechanism the
ruling keeps.
- Do NOT bundle a 'cleanup' of the content-hash placeholder byte into any later refactor. Deleting
it churns every issue's content hash and breaks the cross-clone determinism contract
(types.go:174-176). It is ruled, permanent, and cheap.
- Do NOT let a homonym sweep touch: Gas City session pin_awake (cmd/gc/compute_awake_set.go:70,
cmd_session_pin.go:137), bd-enterprise DPoP jkt pinning (gwauth/store/store.go:22) and Dolt
connection pinning (uow/doltserver_tx.go), or formula/binary version pinning. A false 'Gas City
reads the boolean' claim from exactly this collision already reached a merged commit message
once.

## Residual risks

- Hosted census (Phase 0 Gate A) may surface a closed+pinned or open+pinned row the local census
could not see. That is the one finding that reopens the ruling — it is exactly why the gate
blocks Phase 1.
- Archived JSONL exports are files outside any census. A hypothetical old export containing a
boolean-only closed+pinned row would restore months later with no error, no warning
(import.go:264 plain Unmarshal), and no protection. Probability near zero (no such row ever
observed; the only known writer set both markers); the optional import notice in Phase 7 closes
it cheaply.
- Automation feeding graph plans with "pinned": true keeps exiting 0 while no longer pinning —
mitigated by the stderr warning + the new graphFieldHints entry naming the replacement, but
pipelines that discard stderr see nothing. Only known plan author is the unmerged prototype,
which also sets status.
- Content-hash placeholder byte: a future 'dead code' cleanup that deletes it churns every issue's
content hash. No comparer exists in-repo today (import stale-guard keys on updated_at), but the
determinism contract at types.go:174-176 means a future consumer of that promise would break
silently. The placeholder is ruled permanent.
- Mixed-fleet window: any pre-Phase-7 binary (notably un-synced bd-enterprise deployments —
domain/db/issue.go:612,655 in the fork lists the column in INSERTs) against a post-Phase-9 DB
hard-fails all issue reads and writes. Phase 8's exit criteria (who owns shared-DB sequencing)
is the open operational question and must be answered before Phase 9, not after.
- Stale-checkout traps for the implementer: /data/projects/beads sits on local/deploy-current-
integrated, which LACKS GraphApplyNode.Pinned and carries list-filter logic in a different file
than origin/main; /data/projects/gascity root is a detached hybrid. Work from origin/main via
worktree; one auditor made exactly this error before re-verifying. Also: origin/main moved from
the audited 2af843a601 to db966433eb during scoping — verified zero pinned-surface drift, but
line numbers are per-SHA and Phase 9's migration number must be picked at land time.
- beads main may grow new boolean consumers between now and Phase 7 (close.go and update.go grew
them recently). Each addition widens Phase 7 and the fork merge. Land Phases 1-6 promptly to
freeze the surface.
- The 'pinned' homonym field (session pin_awake, DPoP pinning, connection pinning, version
pinning) has already produced one false claim in a merged commit message. Every future reference
to consumers must cite file:line.
