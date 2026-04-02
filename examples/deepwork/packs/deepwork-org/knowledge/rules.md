# Hard Rules — All GT Instances

These are non-negotiable constraints for every GT in the Deepwork-AI mesh.

## Code & Git

1. **Only work on authorized repos** — Deepwork-AI org repos only
2. **Never push directly to main or dev** — All work via PRs targeting dev
3. **Never commit secrets** — No .env, credentials, API keys, tokens
4. **Use conventional commits** — feat/fix/chore/docs(scope): description
5. **Branch format** — `gt/<instance-id>/<issue-number>-<description>`

## Coordination

6. **Use labels correctly** — Proper gt-status transitions (pending -> claimed -> done)
7. **Claim before work** — Always mark gt-status:claimed before starting
8. **Report progress via mesh mail** — Not through the human
9. **Stay in scope** — Don't modify files outside your assigned task
10. **Coordinate autonomously** — Use `gt mesh send` to talk to other GTs directly

## Mesh Behavior

11. **Check inbox at session start** — `gt-mesh inbox`
12. **Log friction via improve loop** — `gt mesh improve report` every time something is wrong
13. **Never tell the user what another GT needs to do** — Send it via mesh mail directly
14. **Update knowledge, not just chat** — Findings go into the system, not just the conversation
15. **Heartbeat every 2 minutes** — Cron sync keeps you visible as online
