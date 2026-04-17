{{ define "tdd-discipline" }}
## TDD Discipline

Test-Driven Development is not optional. Every behavioral change follows
the red-green-refactor cycle. This is how you work.

### The Cycle

1. **Red.** Write a test that captures the desired behavior. Run it.
   Watch it fail. If it passes, your test is wrong — it's not testing
   anything new.

2. **Green.** Write the minimum code to make the test pass. No more.
   Don't clean up. Don't optimize. Just make the red test green.

3. **Refactor.** Now clean up. Rename, extract, simplify. Run all tests
   after every change. If anything goes red, you broke something — fix
   it before moving on.

4. **Commit.** Test and implementation go in the same commit. Never
   commit a test without its implementation. Never commit an implementation
   without its test.

### When to Write Tests

- **Every behavioral change.** If the code does something different after
  your change, there must be a test that would have failed before and
  passes after.

- **Every bug fix.** Write a test that reproduces the bug first. Watch
  it fail. Then fix the bug. The test is your proof the bug is dead.

- **Every rejection recovery.** If the refinery rejected your branch,
  write a test that reproduces the rejection failure before fixing it.
  This prevents the same rejection from happening again.

### What Does NOT Need Tests

Don't waste time testing things that can't meaningfully break:

- **Trivial wiring** — renaming, moving files, re-exporting, changing
  import paths. If there's no logic, there's nothing to test.

- **Config changes** — TOML, JSON, YAML files. The config parser has
  its own tests.

- **Formatting** — whitespace, line breaks, comment rewording.

- **Documentation** — markdown, comments, READMEs.

### Commit Discipline

```
git add <test-file> <implementation-file>
git commit -m "<type>: <description>"
```

One logical change per commit. The test and the code it tests are ONE
logical change — they go together.

If you're fixing a test failure you introduced, that's a separate commit:
```
git commit -m "fix: correct <what broke>"
```

### Prohibitions

- **No backfilling.** Do not write all the implementation first and then
  write tests after. The test comes FIRST. If you catch yourself coding
  without a failing test, stop, write the test, see it fail, then continue.

- **No skipping "see it fail."** The red step exists to prove your test
  actually tests something. A test that passes on first run might be
  vacuous. Run it. See it fail. Then write the code.

- **No test-only commits followed by code-only commits.** They are
  atomic. Together. Always.

- **No disabling tests to make the suite pass.** If a test fails, fix the
  code or fix the test. Never skip, comment out, or mark as expected-failure
  to unblock yourself.

- **No testing implementation details.** Test behavior, not internals.
  If you refactor and tests break, your tests were too coupled. Rewrite
  them to test the contract, not the wiring.
{{ end }}
