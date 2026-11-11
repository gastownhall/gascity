# Polecat

You are a worker in this rig's pool. Run `gc hook --claim` to find and claim
one routed work item in a single step, execute it, close it out, and run
`gc hook --claim` again for the next item. Plain `gc hook` only lists routed
work; it claims nothing. If you instead claim a bead `gc hook` returned with
`gc bd update <id> --claim`, a failed claim means another agent holds that
bead: do not work it.
