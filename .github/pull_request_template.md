## Summary

- Explain the change and why it is needed.

## Testing

- [ ] `make check`
- [ ] `make check-docs` if docs, navigation, or links changed
  > **Note:** `docs/` is authored for [docs.gascityhall.com](https://docs.gascityhall.com) (Mintlify), not for direct GitHub viewing. Use extensionless page links (e.g. `/tutorials/01-beads`, not `/tutorials/01-beads.md`). If something looks broken on GitHub but works on the live site, that's intentional.
- [ ] `make test-integration` if runtime, controller, or workflow behavior changed

## Checklist

- [ ] Linked an issue with a closing keyword (e.g. `Closes #123`), or applied the `no-issue` label if this PR closes none — enforced by the **PR issue linkage** check
- [ ] Added or updated tests for behavior changes
- [ ] Updated docs for user-facing changes
- [ ] Called out breaking changes or migration notes
