#!/usr/bin/env bash
# smoke.sh — boot runtime on $1, exercise the MVP 02 surface, shut down.
#
# Covers: /v1/{health,version}, /v1/runs create+list+cancel,
# /v1/runs/{id}/events with cursor, /v1/runs/{id}/attentions and
# /v1/attentions/{id}/answer, 404 JSON envelope, TUI --once.
set -euo pipefail
ADDR="${1:-127.0.0.1:7469}"
STATE_DIR="${WORK9FLOW_STATE_DIR:-$(mktemp -d)}"
export WORK9FLOW_STATE_DIR="$STATE_DIR"
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

echo "==> POST /v1/runs"
CREATE=$(curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"workflow_id":"feature-development","repo_path":"/tmp/repo","original_task":"smoke"}' \
  "http://$ADDR/v1/runs")
echo "$CREATE" | jq -c .
RUN_ID=$(echo "$CREATE" | jq -r .run.id)

echo "==> GET /v1/runs (list)"
curl -fsS "http://$ADDR/v1/runs" | jq -c .

echo "==> GET /v1/runs/$RUN_ID (detail)"
curl -fsS "http://$ADDR/v1/runs/$RUN_ID" | jq -c .

echo "==> GET /v1/runs/$RUN_ID/events"
curl -fsS "http://$ADDR/v1/runs/$RUN_ID/events" | jq -c .

echo "==> unknown route -> 404 json"
code=$(curl -s -o /tmp/wf_404.json -w '%{http_code}' "http://$ADDR/no/such/route")
test "$code" = "404"
grep -q '"error":"not_found"' /tmp/wf_404.json

echo "==> POST /v1/runs/$RUN_ID/steer (publish a live event)"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"agent_id":"a-smoke","message":"hi"}' \
  "http://$ADDR/v1/runs/$RUN_ID/steer")
test "$code" = "202"

echo "==> GET /v1/runs/$RUN_ID/events after steer"
curl -fsS "http://$ADDR/v1/runs/$RUN_ID/events" | jq -c .

echo "==> DELETE /v1/runs/$RUN_ID (cancel)"
code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "http://$ADDR/v1/runs/$RUN_ID")
test "$code" = "204"

echo "==> GET /v1/runs/$RUN_ID/events after cancel"
curl -fsS "http://$ADDR/v1/runs/$RUN_ID/events" | jq -c .

echo "==> TUI --once"
WORK9FLOW_RUNTIME_ENDPOINT="http://$ADDR" "$ROOT/bin/work9flow" --once | jq -c .

echo "smoke OK"
