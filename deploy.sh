#!/bin/bash
# deploy.sh: build, install here (controller + agent as systemd user units), push binary + kinds +
# config + token to each remote agent host and (re)start its agent. Safe while jobs run: steps are
# detached from the agent.
#
# Machine-specific settings go in ./deploy.local (untracked), e.g.:
#   DATA=/srv/gaffer               # controller db + logs, agent job dirs (default ~/.local/share/gaffer)
#   SLOTS='{ cpu = 4, gpu = 1 }'   # this host's slots
#   NTFY=http://ntfy.example:8093/gaffer
#   REMOTES="boxb boxc"            # ssh-reachable agent hosts; they only need to reach us over HTTP
#   REMOTE_DATA=/srv/gaffer        # data dir on the remotes (default: same as DATA)
#   REMOTE_SLOTS='{ cpu = 4 }'
set -euo pipefail
cd "$(dirname "$(readlink -f "$0")")"
[ -f deploy.local ] && . ./deploy.local
DATA=${DATA:-$HOME/.local/share/gaffer}; SLOTS=${SLOTS:-'{ cpu = 4 }'}; NTFY=${NTFY:-}
REMOTES=${REMOTES:-}; REMOTE_DATA=${REMOTE_DATA:-$DATA}; REMOTE_SLOTS=${REMOTE_SLOTS:-'{ cpu = 4 }'}
HOST=$(hostname -s | tr '[:upper:]' '[:lower:]')

CGO_ENABLED=0 go build -o gaffer .
IP=$(tailscale ip -4 2>/dev/null | head -1 || true); IP=${IP:-127.0.0.1}
mkdir -p ~/.local/bin ~/.config/gaffer ~/.config/systemd/user
\cp -f gaffer ~/.local/bin/gaffer.new && mv -f ~/.local/bin/gaffer.new ~/.local/bin/gaffer   # rename: running shims keep their inode
[ -f ~/.config/gaffer/config.toml ] || cat > ~/.config/gaffer/config.toml <<EOT
controller = "http://$IP:8097"
listen = "auto:8097"
data = "$DATA"
ntfy = "$NTFY"
kinds = "$PWD/kinds"
[agent]
host = "$HOST"
dir = "$DATA/agent"
slots = $SLOTS
EOT
\cp -f deploy/gaffer-*.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable gaffer-controller.service gaffer-agent.service >/dev/null 2>&1
systemctl --user restart gaffer-controller.service
for _ in $(seq 50); do [ -s ~/.config/gaffer/token ] && break; sleep 0.1; done
systemctl --user restart gaffer-agent.service

# Remotes need no root and no linger: the agent dials out over HTTP. A cron line keeps it up across
# reboots, since the agent refuses to run twice: `* * * * * $REMOTE_DATA/bin/gaffer agent >>$REMOTE_DATA/agent.log 2>&1`
for R in $REMOTES; do
  D=$REMOTE_DATA
  ssh "$R" "mkdir -p $D/bin ~/.config/gaffer"
  rsync -a gaffer "$R:$D/bin/gaffer.new" && ssh "$R" "mv -f $D/bin/gaffer.new $D/bin/gaffer"
  rsync -a --delete kinds/ "$R:$D/kinds/"
  ssh "$R" "cat > ~/.config/gaffer/config.toml" <<EOT
controller = "http://$IP:8097"
data = "$D"
kinds = "$D/kinds"
[agent]
host = "$R"
dir = "$D/agent"
slots = $REMOTE_SLOTS
EOT
  ssh "$R" "umask 077; cat > ~/.config/gaffer/token" < ~/.config/gaffer/token
  ssh "$R" "pid=\$(cat $D/agent.pid 2>/dev/null) && kill \$pid 2>/dev/null; sleep 1; cd $D && setsid nohup $D/bin/gaffer agent >> $D/agent.log 2>&1 < /dev/null & echo \$! > $D/agent.pid"
done
echo "deployed $(git rev-parse --short HEAD 2>/dev/null || echo uncommitted): controller http://$IP:8097, agents $HOST $REMOTES"
