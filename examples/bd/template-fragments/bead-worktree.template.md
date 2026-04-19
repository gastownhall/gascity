{{- define "bead-worktree" -}}
## Worktree recovery

When you create a git worktree (via `git worktree add` or the EnterWorktree tool), save its path to your task bead so the orchestrator starts you there on restart:

1. Find your assigned bead:
   ```
   gc bd list --json --assignee="{{.AgentName}}" --status=in-progress
   ```
2. Update the bead with the absolute worktree path:
   ```
   gc bd update <bead_id> --set-metadata work_dir=<absolute_worktree_path>
   ```

Replace `<bead_id>` with the `id` field from step 1 and `<absolute_worktree_path>` with the full path to the worktree directory.
{{- end -}}
