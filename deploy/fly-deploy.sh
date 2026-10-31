#!/usr/bin/env bash
# Deploys the 4-node mesh as four Fly apps in one organisation, so they share
# the private 6PN network. Raft traffic uses <app>.internal:9080 on that
# network; only port 8080 (the public site) is exposed through Fly's proxy.
# Needs: flyctl, `fly auth login`, and PREFIX set to a unique app prefix.
set -euo pipefail
: "${PREFIX:?set PREFIX, e.g. aravindh-resume}"
regions=(bom sin fra ams); labels=(Mumbai Singapore Frankfurt Amsterdam)
for i in 0 1 2 3; do
  peers=""
  for j in 0 1 2 3; do
    [ $i = $j ] && continue
    peers+="node-$((j+1))@${labels[$j]}=${PREFIX}-${regions[$j]}.internal:9080,"
  done
  export APP="${PREFIX}-${regions[$i]}" REGION="${regions[$i]}" NODE_ID="node-$((i+1))" LABEL="${labels[$i]}" PEERS="${peers%,}"
  envsubst < deploy/fly.toml.tmpl > "deploy/fly.${regions[$i]}.toml"
  fly apps create "$APP" 2>/dev/null || true
  fly deploy --config "deploy/fly.${regions[$i]}.toml" --remote-only --ha=false
done
echo "public: https://${PREFIX}-bom.fly.dev (and -sin, -fra, -ams)"
