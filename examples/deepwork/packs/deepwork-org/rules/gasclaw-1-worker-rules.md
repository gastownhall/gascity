# gasclaw-1 Worker Rules
# Shared rules for worker GTs in Deepwork-AI mesh
# Location: deepwork-base/rules/gasclaw-1-worker-rules.md

## Rule 1: Execute Without Asking
- Parent (gt-local) assigned work = DO IT immediately
- No permission asking
- Claim + execute + report

## Rule 2: Ask for Work When Idle
- When no work available, SEND MESH MAIL to gt-local
- Subject: "Work Request: Available for Next Tasks"
- List completed work
- Ask what to prioritize
- NEVER STAY IDLE

## Rule 3: Report Completion
- After finishing work, report to pratham
- Ask parent for more work via mesh mail

## Rule 4: PR Review Notification
- AFTER creating PR, mail parent for review
- Include PR link and summary
- Always notify gt-local when PR is ready

## Rule 5: Stay Silent When No Work
- Don't spam with "no work found" messages
- Only announce when work is found or completed
- Use delivery: "none" for polling cron jobs

## Implementation Notes
- Check mesh inbox every 2 minutes
- Check GitHub repos: OfficeWorld, ai-planogram, alc-ai-villa, gt-mesh
- Auto-claim issues with gt-to:gasclaw-1,gt-status:pending
- Create branch format: gt/gasclaw-1/<issue>-<desc>
- PR target: dev branch
- PR label: gt-from:gasclaw-1

## Repositories Monitored
- Deepwork-AI/OfficeWorld
- Deepwork-AI/ai-planogram
- Deepwork-AI/alc-ai-villa
- Deepwork-AI/gt-mesh

---
**Synced from:** gasclaw-1 mesh.yaml  
**Date:** 2026-03-07  
**Author:** gasclaw-1
