#!/usr/bin/env bash
# Runs a 4-node resume mesh on this machine, all on loopback.
# Pages: http://127.0.0.1:8081 .. :8084   Stop: kill $(cat .mesh/*.pid)
set -euo pipefail
go build -o .mesh/site ./cmd/site
regions=(Mumbai Bengaluru Singapore Frankfurt)
for i in 1 2 3 4; do
  peers=""
  for j in 1 2 3 4; do
    [ $i = $j ] && continue
    peers+="node-$j@${regions[$((j-1))]}=127.0.0.1:908$j,"
  done
  ( setsid nohup .mesh/site -id node-$i -region "${regions[$((i-1))]}" \
      -addr 127.0.0.1:808$i -raft-addr 127.0.0.1:908$i -peers "${peers%,}" \
      > .mesh/node-$i.log 2>&1 < /dev/null & echo $! > .mesh/node-$i.pid )
done
echo "mesh up: http://127.0.0.1:8081 (also 8082-8084)"
