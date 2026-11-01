#!/usr/bin/env bash
# Deploys the 4-node resume mesh as four Fly.io apps in your personal
# organisation, so they share Fly's private network (6PN). Raft traffic uses
# <app>.internal:9080 on that network; only the website (port 8080, behind
# Fly's HTTPS proxy) is public.
#
# Works in Git Bash on Windows, macOS and Linux. Needs: flyctl, `fly auth login`.
#
#   PREFIX=aravindh-resume ./deploy/fly-deploy.sh
set -euo pipefail
: "${PREFIX:?set PREFIX, e.g. PREFIX=aravindh-resume}"
cd "$(dirname "$0")/.."

regions=(bom sin fra ams)
labels=(Mumbai Singapore Frankfurt Amsterdam)

for i in 0 1 2 3; do
  app="${PREFIX}-${regions[$i]}"
  peers=""
  for j in 0 1 2 3; do
    [ "$i" = "$j" ] && continue
    peers+="node-$((j+1))@${labels[$j]}=${PREFIX}-${regions[$j]}.internal:9080,"
  done
  peers="${peers%,}"
  cfg="fly.${regions[$i]}.toml"
  cat > "$cfg" <<EOF
app = "${app}"
primary_region = "${regions[$i]}"

[build]
  dockerfile = "Dockerfile"

[env]
  NODE_ID = "node-$((i+1))"
  NODE_REGION = "${labels[$i]}"
  PUBLIC_ADDR = "0.0.0.0:8080"
  RAFT_ADDR = "fly-local-6pn:9080"
  PEERS = "${peers}"

[http_service]
  internal_port = 8080
  force_https = true
  auto_stop_machines = "off"
  auto_start_machines = true
  min_machines_running = 1

[[vm]]
  size = "shared-cpu-1x"
  memory = "256mb"
EOF
  echo "== ${app} (${labels[$i]})"
  if ! fly status --app "$app" >/dev/null 2>&1; then
    fly apps create "$app" --org personal
  fi
  fly deploy --config "$cfg" --remote-only --ha=false --yes
done

echo
echo "Done. Open https://${PREFIX}-bom.fly.dev (also -sin, -fra, -ams)."
echo "To stop all costs later: for r in ${regions[*]}; do fly apps destroy ${PREFIX}-\$r --yes; done"
