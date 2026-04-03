#!/bin/sh
set -eu

STATE_FILE="node_modules/.package-hash"

compute_hash() {
  if [ -f package-lock.json ]; then
    cat package.json package-lock.json | sha256sum | awk '{print $1}'
    return
  fi

  sha256sum package.json | awk '{print $1}'
}

mkdir -p node_modules
CURRENT_HASH="$(compute_hash)"
INSTALLED_HASH=""

if [ -f "$STATE_FILE" ]; then
  INSTALLED_HASH="$(cat "$STATE_FILE")"
fi

if [ ! -f node_modules/.bin/vite ] || [ "$CURRENT_HASH" != "$INSTALLED_HASH" ]; then
  npm install --no-fund --no-audit --package-lock=false
  CURRENT_HASH="$(compute_hash)"
  printf '%s' "$CURRENT_HASH" > "$STATE_FILE"
fi

exec "$@"
