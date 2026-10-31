#!/usr/bin/env bash
# Builds the gce2e-base Incus image: Ubuntu 24.04 with a sudo user "dev",
# Homebrew, a global git identity, and Claude Code. No Gas City, no Dolt
# config, no Claude login (run-variant.sh installs the credential per run).
#
# Run on an Incus host: ./provision-image.sh
set -euo pipefail

c=gce2e-golden
setup=$(mktemp)
trap 'rm -f "$setup"' EXIT
cat > "$setup" <<'DEV'
set -euo pipefail
NONINTERACTIVE=1 /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)" >/dev/null
for f in ~/.bashrc ~/.profile; do
  echo 'eval "$(/home/linuxbrew/.linuxbrew/bin/brew shellenv)"' >> "$f"
  echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$f"
done
curl -fsSL https://claude.ai/install.sh | bash >/dev/null
git config --global user.name "Test Dev"
git config --global user.email dev@example.com
echo '{"hasCompletedOnboarding":true}' > ~/.claude.json
DEV

echo "[1/4] launch"
incus launch images:ubuntu/24.04 "$c"
for _ in $(seq 1 60); do incus exec "$c" -- systemctl is-system-running --wait >/dev/null 2>&1 && break; sleep 2; done
echo "[2/4] packages + user"
incus exec "$c" -- bash -c 'export DEBIAN_FRONTEND=noninteractive; apt-get update -q >/dev/null && apt-get install -yq build-essential ca-certificates curl file git procps lsof sudo locales xz-utils >/dev/null && useradd -m -s /bin/bash dev && echo "dev ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/dev && loginctl enable-linger dev'
echo "[3/4] homebrew + claude code + git identity"
incus file push "$setup" "$c/tmp/dev-setup.sh"
incus exec "$c" -- chmod 755 /tmp/dev-setup.sh
incus exec "$c" -- su - dev -c 'bash /tmp/dev-setup.sh'
incus exec "$c" -- su - dev -c 'command -v brew claude git'
echo "[4/4] publish image"
incus stop "$c"
incus publish "$c" --alias gce2e-base
incus delete "$c"
echo "done"
