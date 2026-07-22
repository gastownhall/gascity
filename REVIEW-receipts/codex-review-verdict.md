[SEV: major] internal/bootstrap/packs/core/assets/scripts/notify-on-human-gate-creation.sh:179 — failed sends are neither loud nor guaranteed to retry — line 183 only prints before the script exits 0, while the controller persists the event cursor before execution and logs output only on nonzero exit (`cmd/gc/order_dispatch.go:1344`, `cmd/gc/order_dispatch.go:1373`); a lone failed creation notification is consumed until the 1h stale sweep — persist pending gates, return nonzero after writing state, and make the cooldown sweep retry pending gates immediately.

[SEV: major] internal/bootstrap/packs/core/assets/scripts/notify-on-human-gate-creation.sh:179 — `--notify` nudge failures are deduplicated as success — `gc mail send` prints a nudge error but still returns 0 (`cmd/gc/cmd_mail.go:1847`, `cmd/gc/cmd_mail.go:1858`), so this line and renudge line 209 stamp success after a transient runtime-nudge failure — expose/check a strict typed notify result and track mail/nudge phases separately.

[SEV: major] internal/bootstrap/packs/core/assets/scripts/renudge-stale-human-gates.sh:103 — GNU-only `date -d` disables the sweep on macOS — BSD `date` rejects `-d`, `iso_to_epoch` returns empty, and every candidate is skipped at line 160 — parse with jq or add the GNU/BSD fallback used by `wisp-compact.sh:68`.

[SEV: minor] internal/bootstrap/packs/core/assets/scripts/notify-on-human-gate-creation.sh:116 — event parsing assumes only the API envelope — `gc events` local fallback preserves raw bus payload (`cmd/gc/cmd_events.go:654`, `cmd/gc/cmd_events.go:766`), making fields `.payload.issue_type/.id`, so fallback output silently yields no gates — normalize with `(.payload.bead // .payload)`.

[SEV: minor] internal/bootstrap/packs/core/pack_orders_test.go:178 — substring-only tests cannot detect the runtime defects above — scripts containing the expected words pass without exercising exit status, state mutation, BSD timestamps, or partial notify failure (`internal/bootstrap/packs/core/pack_orders_test.go:195`, `internal/bootstrap/packs/core/pack_orders_test.go:295`) — add hermetic script tests with fake CLI responses.

CLEAN: CLI command/flag surface and API-mode event envelope (`cmd/gc/cmd_events.go:218`, `cmd/gc/cmd_bd.go:88`, `cmd/gc/cmd_rig.go:217`, `cmd/gc/cmd_mail.go:1464`, `internal/api/event_payloads.go:259`).

CLEAN: Nested loop state scope and conditional rig-argument expansion (`internal/bootstrap/packs/core/assets/scripts/notify-on-human-gate-creation.sh:128`, `internal/bootstrap/packs/core/assets/scripts/notify-on-human-gate-creation.sh:185`, `internal/bootstrap/packs/core/assets/scripts/renudge-stale-human-gates.sh:132`, `internal/bootstrap/packs/core/assets/scripts/renudge-stale-human-gates.sh:215`).

CLEAN: jq addressee precedence and valid-state pruning (`internal/bootstrap/packs/core/assets/scripts/notify-on-human-gate-creation.sh:158`, `internal/bootstrap/packs/core/assets/scripts/notify-on-human-gate-creation.sh:191`, `internal/bootstrap/packs/core/assets/scripts/renudge-stale-human-gates.sh:188`, `internal/bootstrap/packs/core/assets/scripts/renudge-stale-human-gates.sh:222`).

CLEAN: Sibling-order convention and registration; flat TOMLs are discovered and `orders/` is embedded without a manifest edit (`internal/orders/order.go:2`, `internal/bootstrap/packs/core/embed.go:12`).

VERDICT: HOLD — creation failures can disappear silently, nudge failures are falsely deduplicated, and the stale sweep is inert on macOS.
