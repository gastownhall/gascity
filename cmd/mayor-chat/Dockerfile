# Mayor Chat — Ink terminal UI for containerized Claude Code.
#
# Build:
#   docker build -t mayor-chat -f cmd/mayor-chat/Dockerfile cmd/mayor-chat/
#
# Run (interactive):
#   docker run -d --name mayor-chat --entrypoint sleep \
#     -v ~/.claude/.credentials.json:/root/.claude/.credentials.json:ro \
#     -v ~/.claude/settings.json:/root/.claude/settings.json:ro \
#     -v ~/.claude.json:/root/.claude.json:ro \
#     mayor-chat infinity
#   docker exec -it mayor-chat node /app/chat.mjs
#
# From local tmux pane:
#   ssh namurim "docker exec -it mayor-chat node /app/chat.mjs"

FROM node:20-slim

WORKDIR /app

# Install Claude Code globally (used as subprocess by Agent SDK).
RUN npm install -g @anthropic-ai/claude-code 2>/dev/null

# Install project dependencies.
COPY package.json .
RUN npm install --omit=dev 2>/dev/null

# Copy app files.
COPY chat.mjs transport.mjs ./

ENTRYPOINT ["node", "chat.mjs"]
