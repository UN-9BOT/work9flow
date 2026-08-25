#!/usr/bin/env bash
# smoke-full.sh — boot work9flowd with the inline DSH backed by a
# scripted fake provider, then drive a feature-development run all
# the way to DONE through the HTTP surface.
#
# This is the shell-level equivalent of the Go e2e test in
# cmd/work9flowd/inline_e2e_test.go. To exercise a real provider
# (e.g. minim), set WORK9FLOW_PROVIDERS_FILE=providers.toml + the
# matching *_API_KEY env and replace the fake below.
set -euo pipefail
ADDR="${1:-127.0.0.1:7469}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Pick a free port for the fake provider.
FAKE_PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
FAKE_URL="http://127.0.0.1:${FAKE_PORT}"
FAKE_PY="$ROOT/.fake_provider.py"

cat >"$FAKE_PY" <<PYEOF
from http.server import BaseHTTPRequestHandler, HTTPServer
import json

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        _ = self.rfile.read(n)
        body = json.dumps({
            "choices": [{"message": {"role": "assistant", "content": "ok\noutcome: advance"}}]
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a, **kw): pass

HTTPServer(("127.0.0.1", ${FAKE_PORT}), H).serve_forever()
PYEOF

# State dir + providers + work9flow.yaml.
STATE="$ROOT/.smoke-full-state"
rm -rf "$STATE"
mkdir -p "$STATE"
PROV="$ROOT/.providers.toml"
WFY="$ROOT/.work9flow.yaml"

cat >"$PROV" <<TOMLEOF
[fake]
display_name = "Fake"
protocol     = "openai"
base_url     = "${FAKE_URL}"
api_key_env  = "FAKE_KEY"
default_model = "fake/test"
[[fake.models]]
id = "test"
TOMLEOF

cat >"$WFY" <<YAMLEOF
state_dir: ${STATE}
runtime_endpoint: http://${ADDR}
providers_file: ${PROV}
iteration_limits:
  default: 5
  implementing: 3
YAMLEOF

export FAKE_KEY="test-key"

# Boot fake provider.
python3 "$FAKE_PY" >/dev/null 2>&1 &
FAKE_PID=$!
# Boot work9flowd.
"$ROOT/bin/work9flowd" --config="$WFY" >"$ROOT/.smoke-full.log" 2>&1 &
WFD_PID=$!

cleanup() {
  kill -TERM "$WFD_PID" 2>/dev/null || true
  kill -TERM "$FAKE_PID" 2>/dev/null || true
  wait "$WFD_PID" 2>/dev/null || true
  wait "$FAKE_PID" 2>/dev/null || true
  rm -f "$FAKE_PY" "$PROV" "$WFY" "$ROOT/.smoke-full.log"
  rm -rf "$STATE"
}
trap cleanup EXIT

# Wait for runtime to come up.
for _ in $(seq 1 50); do
  if curl -fsS "http://$ADDR/v1/health" >/dev/null 2>&1; then break; fi
  sleep 0.1
done

echo "==> /v1/health"
curl -fsS "http://$ADDR/v1/health" | jq -c .

echo "==> POST /v1/runs"
CREATE=$(curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"workflow_id":"feature-development","repo_path":"/tmp/repo","original_task":"smoke-full"}' \
  "http://$ADDR/v1/runs")
RUN_ID=$(echo "$CREATE" | jq -r .run.id)
echo "$CREATE" | jq -c .

echo "==> poll until terminal"
deadline=$(( $(date +%s) + 30 ))
STATE=""
while [ "$(date +%s)" -lt "$deadline" ]; do
  STATE=$(curl -fsS "http://$ADDR/v1/runs/$RUN_ID" | jq -r .run.state)
  case "$STATE" in
    DONE|FAILED|CANCELED) break ;;
  esac
  sleep 0.5
done
echo "final state: $STATE"

if [ "$STATE" != "DONE" ]; then
  echo "==> runtime stderr (tail)"
  tail -40 "$ROOT/.smoke-full.log" 2>/dev/null || true
  exit 1
fi

echo "==> events (kind+seq)"
curl -fsS "http://$ADDR/v1/runs/$RUN_ID/events" | jq -c '.events | map({seq, kind})'

echo "smoke-full OK"
