// Package reviewworkflows holds shared test-local workflow formulas for review
// coverage so compile and integration tests exercise the same definitions.
package reviewworkflows

// ExpansionReviewPR is the test-local review expansion workflow fixture.
const ExpansionReviewPR = `description = """
Test-local review expansion used by integration tests.
Exercises compose.expand, pooled reviewer fan-out, Gemini soft-fail retries,
and synthesis without depending on private production formulas.
"""
formula = "expansion-review-pr"
version = 2
contract = "graph.v2"
type = "expansion"

[vars.skip_gemini]
description = "Skip Gemini reviewer"
default = "false"

[[template]]
id = "{target}.review-claude"
title = "Code review: Claude"
metadata = { "gc.run_target" = "polecat" }
description = "Claude review lane."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[template]]
id = "{target}.review-codex"
title = "Code review: Codex"
metadata = { "gc.run_target" = "polecat" }
description = "Codex review lane."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[template]]
id = "{target}.review-gemini"
title = "Code review: Gemini"
metadata = { "gc.run_target" = "polecat" }
condition = "!{{skip_gemini}}"
description = """
Optional Gemini lane. If unavailable or rate limited, close the attempt as a
transient failure with reason rate_limited so runtime can retry and
eventually soft-fail this logical step.
"""

[template.retry]
max_attempts = 3
on_exhausted = "soft_fail"

[[template]]
id = "{target}.synthesize"
title = "Synthesize review findings"
needs = ["{target}.review-claude", "{target}.review-codex", "{target}.review-gemini"]
metadata = { "gc.run_target" = "worker" }
description = "Merge available reviewer outputs."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"
`

// ExpansionDesignReview is the test-local design review expansion fixture.
const ExpansionDesignReview = `description = """
Test-local design review expansion used by integration tests.
Exercises a second compose.expand path, pooled persona generation/review fan-out,
Gemini soft-fail retries, and final synthesis without depending on private
production formulas.
"""
formula = "expansion-design-review"
version = 2
contract = "graph.v2"
type = "expansion"

[vars.skip_gemini]
description = "Skip Gemini reviewer"
default = "false"

[[template]]
id = "{target}.persona-gen-claude"
title = "Generate personas: Claude"
metadata = { "gc.run_target" = "polecat" }
description = "Claude persona generation lane."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[template]]
id = "{target}.persona-gen-codex"
title = "Generate personas: Codex"
metadata = { "gc.run_target" = "polecat" }
description = "Codex persona generation lane."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[template]]
id = "{target}.persona-gen-gemini"
title = "Generate personas: Gemini"
metadata = { "gc.run_target" = "polecat" }
condition = "!{{skip_gemini}}"
description = "Optional Gemini persona generation lane."

[template.retry]
max_attempts = 3
on_exhausted = "soft_fail"

[[template]]
id = "{target}.persona-synthesis"
title = "Synthesize personas"
needs = ["{target}.persona-gen-claude", "{target}.persona-gen-codex", "{target}.persona-gen-gemini"]
metadata = { "gc.run_target" = "worker" }
description = "Merge persona suggestions."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[template]]
id = "{target}.persona-reviews-claude"
title = "Persona reviews: Claude"
needs = ["{target}.persona-synthesis"]
metadata = { "gc.run_target" = "polecat" }
description = "Claude persona review batch."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[template]]
id = "{target}.persona-reviews-codex"
title = "Persona reviews: Codex"
needs = ["{target}.persona-synthesis"]
metadata = { "gc.run_target" = "polecat" }
description = "Codex persona review batch."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[template]]
id = "{target}.persona-reviews-gemini"
title = "Persona reviews: Gemini"
needs = ["{target}.persona-synthesis"]
metadata = { "gc.run_target" = "polecat" }
condition = "!{{skip_gemini}}"
description = "Optional Gemini persona review batch."

[template.retry]
max_attempts = 3
on_exhausted = "soft_fail"

[[template]]
id = "{target}.review-synthesis"
title = "Synthesize design review"
needs = ["{target}.persona-reviews-claude", "{target}.persona-reviews-codex", "{target}.persona-reviews-gemini"]
metadata = { "gc.run_target" = "worker" }
description = "Merge design review findings."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"
`

// AdoptPR is the test-local adopt-pr workflow fixture.
const AdoptPR = `description = """
Test-local adopt-pr workflow used by integration tests.
Exercises a body scope, setup retries, a Check loop, compose.expand fan-out,
Gemini soft-fail retries, finalize, and teardown.
"""
formula = "mol-adopt-pr-v2"
version = 2
contract = "graph.v2"

[vars]
[vars.issue]
required = true

[vars.base_branch]
default = "main"

[vars.pr_ref]
required = true

[vars.skip_gemini]
default = "false"

[[steps]]
id = "body"
title = "Adopt PR body"
needs = ["preflight", "rebase-check", "review-loop", "finalize"]
description = "Terminal latch for the workflow body."
metadata = { "gc.kind" = "scope", "gc.scope_name" = "adopt-pr", "gc.scope_role" = "body" }

[[steps]]
id = "preflight"
title = "Preflight"
description = "Read the source bead and prime the city."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "setup", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "rebase-check"
title = "Prepare worktree"
needs = ["preflight"]
description = "Prepare worktree metadata for the review loop."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "setup", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "review-loop"
title = "Review loop"
needs = ["rebase-check"]
description = "Check loop for iterative review and fixes."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.check]
max_attempts = 5

[steps.check.check]
mode = "exec"
path = ".gc/scripts/checks/adopt-pr-review-approved.sh"
timeout = "10m"

[[steps.children]]
id = "review-pipeline"
title = "Review pipeline"
description = "Expanded via compose.expand."

[[steps.children]]
id = "apply-fixes"
title = "Apply fixes"
needs = ["review-pipeline"]
description = "Apply review feedback and mark the Check verdict."

[steps.children.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[compose]
[[compose.expand]]
target = "review-pipeline"
with = "expansion-review-pr"
vars = { skip_gemini = "{skip_gemini}" }

[[steps]]
id = "finalize"
title = "Finalize"
needs = ["review-loop"]
description = "Finalize the review workflow."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "cleanup-worktree"
title = "Cleanup worktree"
needs = ["body"]
description = "Teardown after the body reaches terminal state."
metadata = { "gc.kind" = "cleanup", "gc.scope_ref" = "body", "gc.scope_role" = "teardown" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"
`

// PersonalWork is the test-local personal-work workflow fixture.
const PersonalWork = `description = """
Test-local personal-work workflow used by integration tests.
Exercises two Check loops, two compose.expand sites, pooled fan-out,
Gemini soft-fail retries, and body teardown without depending on private
production formulas.
"""
formula = "mol-personal-work-v2"
version = 2
contract = "graph.v2"

[vars]
[vars.issue]
required = true

[vars.base_branch]
default = "main"

[vars.skip_gemini]
default = "false"

[vars.setup_command]
default = ""

[vars.test_command]
default = ""

[[steps]]
id = "body"
title = "Personal work body"
needs = ["load-context", "workspace-setup", "design-review-loop", "implement", "code-review-loop", "submit"]
description = "Terminal latch for the workflow body."
metadata = { "gc.kind" = "scope", "gc.scope_name" = "work", "gc.scope_role" = "body" }

[[steps]]
id = "load-context"
title = "Load context"
description = "Inspect the assigned work bead."

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "workspace-setup"
title = "Prepare worktree"
needs = ["load-context"]
description = "Prepare worktree metadata for the workflow."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "setup", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "design-review-loop"
title = "Design review loop"
needs = ["workspace-setup"]
description = "Check loop for iterative design review."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.check]
max_attempts = 5

[steps.check.check]
mode = "exec"
path = ".gc/scripts/checks/design-review-approved.sh"
timeout = "10m"

[[steps.children]]
id = "design-review-pipeline"
title = "Design review pipeline"
description = "Expanded via compose.expand."

[[steps.children]]
id = "apply-design-changes"
title = "Apply design changes"
needs = ["design-review-pipeline"]
description = "Apply design review feedback and mark the Check verdict."

[steps.children.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "implement"
title = "Implement"
needs = ["design-review-loop"]
description = "Perform the main work."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "code-review-loop"
title = "Code review loop"
needs = ["implement"]
description = "Check loop for iterative code review."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.check]
max_attempts = 5

[steps.check.check]
mode = "exec"
path = ".gc/scripts/checks/code-review-approved.sh"
timeout = "10m"

[[steps.children]]
id = "review-pipeline"
title = "Code review pipeline"
description = "Expanded via compose.expand."

[[steps.children]]
id = "apply-code-fixes"
title = "Apply code fixes"
needs = ["review-pipeline"]
description = "Apply code review feedback and mark the Check verdict."

[steps.children.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[compose]
[[compose.expand]]
target = "design-review-pipeline"
with = "expansion-design-review"
vars = { skip_gemini = "{skip_gemini}" }

[[compose.expand]]
target = "review-pipeline"
with = "expansion-review-pr"
vars = { skip_gemini = "{skip_gemini}" }

[[steps]]
id = "submit"
title = "Submit"
needs = ["code-review-loop"]
description = "Finalize the work item."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "cleanup-worktree"
title = "Cleanup worktree"
needs = ["body"]
description = "Teardown after the body reaches terminal state."
metadata = { "gc.kind" = "cleanup", "gc.scope_ref" = "body", "gc.scope_role" = "teardown" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"
`
