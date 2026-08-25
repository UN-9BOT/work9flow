#!/usr/bin/env bash
# smoke.sh — boot runtime on $1, hit core endpoints, shut down.
set -euo pipefail
ADDR="${1:-127.0.0.1:7469}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LOG="$(mktemp)"
trap 'test -n "${WFD_PID:-}" && kill -TERM "${WFD_PID}" 2>/dev/null || true; wait "${WFD_PID:-}" 2>/dev/null || true' EXIT

"$ROOT/bin/work9flowd" --addr="$ADDR" >"$LOG" 2>&1 &
WFD_PID=$!
for _ in $(seq 1 40); do
  curl -fsS "http://$ADDR/v1/health" >/dev/null 2>&1 && break
  sleep 0.1
done

echo "==> /v1/health"
curl -fsS "http://$ADDR/v1/health" | jq -c .

echo "==> /v1/version"
curl -fsS "http://$ADDR/v1/version" | jq -c .

echo "==> /v1/runs"
curl -fsS "http://$ADDR/v1/runs" | jq -c .

echo "==> /v1 unknown route -> 404 json"
code=$(curl -s -o /tmp/wf_404.json -w '%{http_code}' "http://$ADDR/no/such/route")
test "$code" = "404"
grep -q '"error":"not_found"' /tmp/wf_404.json

echo "==> TUI --once"
WORK9FLOW_RUNTIME_ENDPOINT="http://$ADDR" "$ROOT/bin/work9flow" --once | jq -c .

echo "smoke OK"
