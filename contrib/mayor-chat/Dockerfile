# Mayor Chat — Ink terminal UI for containerized Claude Code.
#
# Build:
#   docker build -t mayor-chat -f contrib/mayor-chat/Dockerfile contrib/mayor-chat/
#
# Run via gc-session-docker (recommended):
#   Configure in city.toml with GC_DOCKER_HOME_MOUNT=true to provide
#   credentials via the standard Gas City HOME mount pattern.
#
# Run standalone:
#   docker run -d --name mayor-chat --entrypoint sleep mayor-chat infinity
#   docker exec -it mayor-chat node /app/chat.mjs
#
# Credentials: The Agent SDK discovers credentials from $HOME/.claude/
# at runtime. Use GC_DOCKER_HOME_MOUNT or your own volume mounts to
# provide them. Never bake credentials into the image.

FROM node:20-slim@sha256:2cf067cfed83d5ea958367df9f966191a942351a2df77d6f0193e162b5febfc0

ARG CLAUDE_CODE_VERSION=2.1.101

# tmux is a hard requirement of scripts/gc-session-docker (it execs
# `tmux new-session` inside the container and dies if tmux is absent).
RUN apt-get update \
    && apt-get install -y --no-install-recommends tmux \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Install Claude Code globally, for anything that runs `claude` directly
# (e.g. a shell in the container). The Agent SDK does not use it: it runs
# its own native Claude Code binary, whose version is the SDK's
# `claudeCodeVersion` rather than CLAUDE_CODE_VERSION (see the check below).
RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}"

# Install project dependencies exactly as locked in package-lock.json.
COPY package.json package-lock.json ./
RUN npm ci --omit=dev

# The Agent SDK ships that binary only in optional per-platform packages,
# and npm still succeeds when an optional package fails to download or
# has no build for this platform. Fail the build here, not on the first
# query() at runtime.
RUN test -x "node_modules/@anthropic-ai/claude-agent-sdk-linux-$(node -p process.arch)/claude"

# Copy app files.
COPY chat.mjs transport.mjs ./

# CMD, not ENTRYPOINT: gc-session-docker starts images as
# `<image> sleep infinity` and manages the agent via tmux exec. A fixed
# ENTRYPOINT would swallow that command and launch the TTY-only Ink UI.
CMD ["node", "chat.mjs"]
