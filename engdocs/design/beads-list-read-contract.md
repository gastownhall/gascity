# Local bead-list observation contract

The ordinary routed list remains a best-effort human projection. An admission
caller needs to distinguish a complete observation from a missing scope before
interpreting absence. `gc beads list --fresh --strict --all --format=json`
provides that local read through the existing native store interfaces.

| Requirement | Behavior | Focused verification |
| --- | --- | --- |
| CLI-BEADS-READ-001 | `--fresh` bypasses local API discovery/cache and reads native stores; remote selectors fail without local fallback. | `TestBeadsListFreshAndStrictBypassAPI; TestBeadsListStrictAcceptsNativePackContext; TestBeadsListFreshAndStrictRefuseRemoteSelectors` |
| CLI-BEADS-READ-002 | `--strict` implies fresh and refuses any configured work scope that fails to open and undeclared or ambiguous site scopes; ordinary convoy/open-list behavior is unchanged. | `TestOpenConvoyStoresStrictRejectsAnyMissingScope; TestBeadsListStrictRefusesIndeterminateConfigScopesWithoutJSON; TestStrictBeadsScopeBindingsRefuseConflictingPaths` |
| CLI-BEADS-READ-003 | An unavailable class binding, partial list, or failed merge emits a nonzero result and no successful JSON observation. | `TestBeadsListStrictRefusesUnavailableClassBindingWithoutJSON`; `TestBeadsListStrictCollectionIsUnboundedAndRejectsPartialRows` |
| CLI-BEADS-READ-004 | The native list has no page limit, includes closed rows with `--all`, and merges work/class bindings without deduplicating unrelated stores by bare ID. Only proven migrated copies are superseded. | `TestBeadsListStrictReadsClassRowsAndKeepsIndependentScopeIDCollision`; `TestBeadsListStrictCollectionIsUnboundedAndRejectsPartialRows` |
| CLI-BEADS-READ-005 | No-flag list routing/cache/output and all other convoy readers retain their existing contracts. | Existing routed read matrix and legacy-opener control in `TestOpenConvoyStoresStrictRejectsAnyMissingScope` |
| CLI-BEADS-READ-006 | A work-scope directory must exist before the ordinary factory runs. BD-contract scopes require existing valid backend metadata; a declared existing file scope may legitimately be empty before its first ledger write. Physical database availability remains the native open/List contract. | TestStrictBeadsWorkScopeRefusesMissingDirectoryWithoutCreation; TestBeadsListStrictAcceptsExistingEmptyFileScope; TestBeadsListStrictRefusesUninitializedBDMetadataWithoutOpening; TestStrictBeadsWorkScopeOpensExistingBDIdentity |

A strict success is the complete set returned by those live store reads for the
requested filters. It is not a simultaneous transaction across stores; concurrent
admission remains fenced by the native mutation. No store migration, alternate
runtime, auth, provider policy or generic filesystem/API endpoint is introduced.
