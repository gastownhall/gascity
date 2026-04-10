# Tutorial issues

Issues found while validating tutorial examples against the current `gc` build. Filed inline first; promote to GitHub during a filing pass.

## 05-formulas.md

### formula-show-step-count-off-by-one

`gc formula show <name>` reports a step count that includes the implicit root wrapper. A formula with five user-defined steps is rendered as `Steps (6):` followed by only five lines. The count and the listing should agree.

Repro:

```shell
$ gc formula show pancakes
Formula: pancakes
Description: Make pancakes from scratch

Steps (6):
  ├── pancakes.dry: Mix dry ingredients
  ├── pancakes.wet: Mix wet ingredients
  ├── pancakes.combine: Combine wet and dry [needs: pancakes.dry, pancakes.wet]
  ├── pancakes.cook: Cook the pancakes [needs: pancakes.combine]
  └── pancakes.serve: Serve [needs: pancakes.cook]
```

Five steps shown, header says six.

### cook-ignores-step-conditions

`gc formula cook` materializes steps regardless of their `condition` field. Only `gc formula show` evaluates conditions. Cook should evaluate them too — otherwise conditional steps end up as live beads in the store.

Repro with this formula:

```toml
formula = "deploy-flow"

[vars]
env = "dev"

[[steps]]
id = "build"
title = "Build"

[[steps]]
id = "deploy"
title = "Deploy to staging"
condition = "{{env}} == staging"
```

```shell
$ gc formula cook deploy-flow
Root: mc-yzc
Created: 3
deploy-flow -> mc-yzc
deploy-flow.build -> mc-yzc.1
deploy-flow.deploy -> mc-yzc.2          # should not be created — env=dev
```

`gc formula show deploy-flow --var env=dev` correctly omits the deploy step, so condition evaluation works in one path but not the other.

Additionally, `gc formula show deploy-flow` (no `--var`) does not apply `[vars]` defaults to conditions — the deploy step appears unless `--var env=dev` is passed explicitly. Defaults should flow into condition evaluation in `show` the same way they do for variable substitution.
