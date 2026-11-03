# Proxied scope readiness: coverage delivery plan

`ga-qip9htv` preserves the readiness behavior already present on main while
closing the direct health/probe coverage gap left by `ga-pi1135`. This is
one test scope on the existing carrier, returned from validator intake for
acceptance criteria. It needs no duplicate implementation bead.

Planning base: `b916251bcfa24beed7be2a61f385bd012915ff2a`.
Architecture source: `ga-xiojrx`; current-code reconciliation and prior
tutorial evidence: `ga-pi1135`. The parent closed as a no-op, not as a new
fix shipment. Its older implementation prescription is not renewed here.

## Acceptance criteria

1. Invoke the repository's actual `gc-beads-bd.sh` for **both** `health`
   and `probe` against isolated, provider-owned proxied-scope fixtures.
   Each successful operation invokes the fixture's `bd ping` exactly once
   in the requested scope, returns success, and needs no real listener.
2. For each operation, a distinctive nonzero `bd ping` exit is propagated
   without being masked or followed by another readiness attempt. Record
   the command and scope as well as the observed exit status.
3. Both proxied operations avoid the GC-managed TCP, SQL, managed-process
   and allocator-port readiness paths. Use observable trap/call evidence
   as well as an unavailable GC-managed endpoint; an exit code alone does
   not prove that a port probe was skipped. Neither operation starts,
   stops or recovers a real service, or touches a shared city.
4. Controls pin the current boundaries for both operations: provider-owned
   direct transport also delegates readiness to `bd ping` and propagates
   success/failure; an unowned proxied legacy invocation retains its
   unsupported-operation exit 2 without a `bd ping` or GC-managed probe;
   an unowned non-proxied legacy invocation retains its existing readiness
   checks and unavailable-endpoint results. The direct provider-owned
   control must not be mistaken for the old GC-managed shell path.
5. These are hermetic Linux script tests using fake executables and
   temporary scope directories. They run in the ordinary cmd/gc Bazel
   target without `skipSlowCmdGCTest`, real bd/Dolt, a controller, tmux,
   network access or a real listening port. Reuse the existing script
   fixture conventions and respect TESTING.md's resource ratchets.
6. Provide real named PASS evidence on the unmodified behavior. Coverage
   verification must also show relevant tests fail when health dispatch,
   probe dispatch or failure propagation is deliberately broken, and pass
   again after restoration. Mutations stay local and uncommitted; the
   delivered change contains tests and necessary build inputs only.
7. Run the owning `//cmd/gc:gc_test` target with a verbose filter that
   includes every new case, then the full owning target and required
   `make check` gates. Preserve existing slow lifecycle coverage. If files
   or imports are added, regenerate Bazel BUILD inputs. Complexity has
   zero growth against the pinned target merge base; never refresh
   `engdocs/complexity-baseline.json` for this work. Report any unrelated
   gate failure using its existing tracker and the applicable evidence.

## One carrier through validator and builder

Set `coverage_only` on `ga-qip9htv`: source inspection and the parent
record show correct existing behavior, so there is no honest defect RED
to observe. The existing `mol-tdd-build` coverage-only contract explicitly
allows the spec author to set this flag and requires mutation failure plus
restored-code success instead of a production change.

Route this same carrier to validator with `needs-tests` and `--no-formula`.
The ordinary `mol-acceptance-tests` suite step requires a pre-existing RED
and does not implement coverage-only mode. The validator authors the
characterization cases, records their actual base-PASS evidence and test
paths, and hands the same carrier to builder as `ready-to-build`, retaining
`coverage_only`. It must not claim the tests fail on unchanged main.

Build: carry those tests through the existing coverage-only TDD workflow,
prove their assertions with temporary mutations, restore all production
inputs, verify the real suite and submit the test-only change through the
normal review/deploy path. A real uncovered behavior defect goes back to
intake with its reproduction; do not expand this scope to implement one.

The discovered-from edge to `ga-pi1135` remains. No live prerequisite or
second test/build carrier is required. Recovery cooldowns, Go-native
readiness migration and tutorial rewrites remain outside this scope.
