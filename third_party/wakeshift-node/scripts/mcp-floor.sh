#!/bin/sh
# Installs @modelcontextprotocol/server, client and core at the oldest 2.x
# the peer range allows, in place and unsaved, after npm ci: CI's "v2-floor"
# row then runs the unit tests and the run against a real core
# (scripts/e2e.sh, which serves events/poll and the wait task's tasks/get)
# on it. pack-check only typechecks and runs a server there, which is how
# 2.0 to 2.2 refusing tasks/get on protocol 2026-07-28 went unnoticed.
#
# @modelcontextprotocol/node is test glue here (apps install their own):
# NODE_FOR_FLOOR is the newest release whose peer range takes the floor.
set -eu
cd "$(dirname "$0")/.."
NODE_FOR_FLOOR=2.1.1
floor=$(node -p 'require("./package.json").peerDependencies["@modelcontextprotocol/server"].replace(/^\^/, "")')
npm install --no-save --ignore-scripts --no-fund --no-audit \
  "@modelcontextprotocol/server@$floor" "@modelcontextprotocol/client@$floor" "@modelcontextprotocol/core@$floor" \
  "@modelcontextprotocol/node@$NODE_FOR_FLOOR" >/dev/null
for p in server client core; do
  got=$(node -p "require('./node_modules/@modelcontextprotocol/$p/package.json').version")
  if [ "$got" != "$floor" ]; then
    echo "@modelcontextprotocol/$p is $got, not the floor $floor" >&2
    exit 1
  fi
done
echo "@modelcontextprotocol/server, client and core $floor (the floor), node $NODE_FOR_FLOOR"
