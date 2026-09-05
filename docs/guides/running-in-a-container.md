---
title: Run a city in a container
description: Run Gas City and its agent clients in one Docker container around a bind-mounted workspace.
---

`contrib/docker` packages `gc`, `bd`, Dolt, Claude Code, and Codex into one
image. `tini` starts `gc supervisor run`; Gas City owns cities, Dolt servers,
and agent sessions. Docker owns mounts, port publication, and restarts.
There is no container-specific supervisor or startup script.

## Build

Use Docker Engine on Linux (including WSL2) with BuildKit and Compose 2.20.0
or newer. From a checkout of this repository:

```bash
make docker-city
```

The image is built for your host uid and gid, with `make build`'s version
metadata. It reuses Dolt from the Kubernetes base image, so the first build
also builds that base and its tools. Only the Dolt binary is copied into the
final city image; the Kubernetes runtime and sudo configuration are not.

## Configure

```bash
cd contrib/docker
cp .env.example .env
```

Edit `.env`:

| Variable | Meaning |
|----------|---------|
| `GASCITY_WORKSPACE` | Existing absolute host directory that holds your cities. Mounted at the identical path inside. |
| `GASCITY_AUTHOR_NAME`, `GASCITY_AUTHOR_EMAIL` | Git and Dolt identity for commits the city makes. |
| `GASCITY_PORT` | Host loopback port. Default `8372`. |

In `compose.yaml`, keep the client-state binds you want to share:

- Claude Code uses both `~/.claude` and `~/.claude.json`.
- Codex uses `~/.codex`.
- **Remove the bind entries for clients you do not use** (both entries for
  Claude). Unused client directories can remain in the container's home.

Every retained source must exist with the correct file/directory type and
be accessible to your uid/gid. `GASCITY_CLAUDE_DIR`, `GASCITY_CLAUDE_JSON`,
and `GASCITY_CODEX_DIR` select alternative host sources; they do not create
or authenticate them. See [Harness Recipes](/guides/harness-recipes) for
client setup. No login state is baked into the image.

Initialize the identity once, using Git and Dolt's native configuration
commands rather than interpolating names into configuration syntax:

```bash
docker compose run --rm gascity sh -ec '
    git config --global user.name "$GASCITY_AUTHOR_NAME"
    git config --global user.email "$GASCITY_AUTHOR_EMAIL"
    git config --global beads.role maintainer
    dolt config --global --add user.name "$GASCITY_AUTHOR_NAME"
    dolt config --global --add user.email "$GASCITY_AUTHOR_EMAIL"
    dolt config --global --add metrics.disabled true
    dolt config --global --add versioncheck.disabled true
'
```

This runs as the normal container user and persists settings in the `home`
volume. Repeat it after changing the author identity or creating a fresh
home volume. Identity initialization does not run automatically at container startup.

## Run

```bash
docker compose up -d --wait
docker compose exec gascity gc init my-city --template minimal --default-provider claude
```

The service's working directory is your workspace. `.env` variables belong
to Compose, so the command uses a relative city path rather than a host-shell
variable. Open `http://127.0.0.1:<GASCITY_PORT>/`, using the port from `.env`
(default `http://127.0.0.1:8372/`), for the dashboard and API.

For city-local commands, select that city's working directory, for example:

```bash
docker compose exec -w /absolute/workspace/my-city gascity gc status
```

The workspace must include any Git common directory referenced by its linked
worktrees; preserving a worktree path cannot expose metadata outside the mount.

```bash
docker compose stop
docker compose start
```

The supervisor handles `SIGTERM` and reads its registered cities from the
same `home` volume after restart. `docker compose down` preserves that volume;
`down --volumes` deletes it. Rebuilding for a different uid/gid does not migrate
existing volume ownership.

## Upgrade

From `contrib/docker`, update your checkout as appropriate, then:

```bash
make -C ../.. docker-city
docker compose up -d --wait
```

The image seeds `/home/gascity/.gc/supervisor.toml` only when the `home`
volume is first created. After that it is operator-owned configuration:
rebuilding does not overwrite it or reset choices such as `allow_mutations`
and `write_auth_allow_unverified`. Review release notes for configuration
changes, explicitly edit the persisted file if needed, and restart the
container. Do not delete the home volume merely to refresh defaults; that
also discards the city registry and other state.

Claude Code and Codex versions are build arguments tracked by Renovate.
`bd` follows the source revision in `deps.env`; Dolt follows the Kubernetes
base build. Inspect image scan results independently—sharing build inputs
is not a guarantee of identical binaries or a passing vulnerability policy.

## Trust boundary

The supervisor listens on all container interfaces and accepts unverified
writes. Compose publishes the port only on host loopback. Keep the container
network trusted too; loopback publication is not authentication for peers
that can reach the container directly. For remote access use the existing
[Remote hardened city](/runbooks/remote-hardened-city) configuration.

The workspace and client profiles are **writable host mounts**. Agents can
modify or delete their contents, including settings used by later host-side
client sessions. Supply dedicated client-state paths if you do not want to
share your usual profiles. Other host paths are not mounted, there is no
Docker socket, and the runtime is non-root. Containers share the host kernel;
this is not VM-equivalent isolation. See [Trust boundaries](/reference/trust-boundaries).
