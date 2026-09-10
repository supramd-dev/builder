#!/usr/bin/env bash
# seed-demo.sh — populate the md-builder database with demo dashboard data:
# a demo user, three fake environments, five git pushes, regression/unit/
# build test-run reports and two finished task graphs with logs, so every
# dashboard tab (All / Build / Unit tests / Regression tests) and the
# dependency-graph view can be inspected without a real git host or SSH
# nodes.
#
# The heavy lifting is done by the server's `seed` subcommand (server/seed.go),
# which writes through the store layer; this wrapper only rebuilds the binary
# and forwards the DSN. It is idempotent: re-running keeps existing demo
# objects and replaces the run reports.
#
# Usage:
#   scripts/seed-demo.sh [--force]        # DSN = <project root>/md-builder.db
#   MD_BUILDER_DSN=... scripts/seed-demo.sh
#
# Options:
#   --force  drop demo task graphs first so they are rebuilt (run reports
#            are kept)
#
# After seeding, restart the server if it is running (SQLite is opened once)
# and log in as demo / demo-pass-123.

set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DSN="${MD_BUILDER_DSN:-$ROOT/md-builder.db}"

FORCE=0
[ "${1:-}" = "--force" ] && FORCE=1

BIN="$(mktemp)"
trap 'rm -f "$BIN"' EXIT

echo "seed-demo: building server binary"
(cd "$ROOT/server" && GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" go build -o "$BIN" .) || {
  echo "seed-demo: build failed" >&2
  exit 1
}

echo "seed-demo: seeding database ($DSN)"
if [ "$FORCE" = 1 ]; then
  "$BIN" seed -dsn "$DSN" -force
else
  "$BIN" seed -dsn "$DSN"
fi

echo
echo "seed-demo: log in as demo / demo-pass-123 (password also printed above)."
echo "seed-demo: If the server is running, restart it to pick up the new data."
