# Plan: Tutorial 01 Published-User Workaround and gc 1.0.1 Patch

**Source:** mayor mail `gm-wisp-v42x9` on 2026-05-22
**Root context:** `ga-6a84` / GitHub issue #1670
**Priority:** P1 - published `gc 1.0.0` users can still hit the clean-install Tutorial 01 failure

## Decision

Mayor approved both tracks, sequenced:

1. Ship the Quickstart/Tutorial 01 docs workaround now for published `gc 1.0.0` users.
2. In parallel, prepare a focused `gc 1.0.1` patch from the `1.0.x` release line by cherry-picking only the Tutorial 01 fix, validating that branch, then publishing Homebrew from the validated artifact.

Do not release from current main while main is red across broader gates.

## Work Tree

| Bead | Title | Route | Blocks |
|------|-------|-------|--------|
| `ga-6a84.1` | Docs: publish Tutorial 01 workaround for gc 1.0.0 users | `gascity/builder` | `ga-6a84.4` |
| `ga-6a84.2` | Release: prepare focused gc 1.0.1 Tutorial 01 patch branch | `gascity/builder` | `ga-6a84.3` |
| `ga-6a84.3` | Validate: gc 1.0.1 Tutorial 01 patch branch is green | `gascity/validator` | `ga-6a84.4` |
| `ga-6a84.4` | Release: publish gc 1.0.1 and Homebrew after validation | `gascity/builder` | - |

## Dependency Graph

```text
ga-6a84.2 -> ga-6a84.3 -> ga-6a84.4
ga-6a84.1 -------------> ga-6a84.4
```

Docs are independent and should move immediately. Publishing waits for both the docs workaround and a green validated patch branch.

## Acceptance Summary

- `ga-6a84.1`: public docs include the `gc doctor --fix` workaround for affected `gc 1.0.0` clean installs and link or mention GitHub issue #1670.
- `ga-6a84.2`: patch branch is based on the `1.0.x` release line and contains only the Tutorial 01 issue-prefix fix.
- `ga-6a84.3`: clean-install Tutorial 01 regression coverage and required release-branch gates pass on the patch branch.
- `ga-6a84.4`: `gc 1.0.1` and Homebrew are published only after validation, with release notes and GitHub issue #1670 updated.

## Risks

- Cherry-picking from main can accidentally pull unrelated red-main changes. Mitigation: builder must list exact cherry-picked commit(s), and validator gates the focused branch before publish.
- Docs workaround wording can look permanent. Mitigation: label it clearly as `gc 1.0.0` published-user guidance while `1.0.1` is being prepared.
