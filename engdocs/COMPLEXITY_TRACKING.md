# Cyclomatic complexity tracking

Gas City tracks cyclomatic complexity as an advisory maintainability signal for
shipped Go code. `scripts/ci/complexity.sh` runs the pinned `gocyclo` v0.6.0
tool over the `cmd/gc`, `internal`, and `pkg` production trees. Tests,
generated clients, conformance fixtures, and test-only helper trees are
excluded. Results use stable package/function/file keys in
`engdocs/complexity-baseline.json`, so line movement does not create noise.

Run the local experiment with:

```sh
go install github.com/fzipp/gocyclo/cmd/gocyclo@v0.6.0
make complexity              # advisory report (top 50, threshold 20)
make complexity-diff         # report new or regressed baseline entries
make complexity-check         # fail on new or regressed entries
make complexity-update        # intentionally refresh the baseline
```

The pull-request workflow publishes the report as an artifact but does not
make complexity a required gate yet. Existing complexity is grandfathered.
Once the signal has been observed, a narrowly scoped guard for newly added or
meaningfully regressed functions (for example CCN 40 or a delta of 5) can be
considered independently of this baseline refresh process.
