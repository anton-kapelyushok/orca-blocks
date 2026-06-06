#!/usr/bin/env bash
set -euo pipefail

CONTROL="${CONTROL_URL:-http://localhost:18080}"
OFFSET=10
VOLUME_ID="demo-$(date +%s)"
DATA="ORCA-BLOCKS-REMOTE-EXECUTION-DATA-$VOLUME_ID"

json_get() {
  python3 -c 'import json,sys; obj=json.load(sys.stdin); print(obj[sys.argv[1]])' "$1"
}

wait_for() {
  local url="$1"
  for _ in $(seq 1 60); do
    if curl -fsS "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "timed out waiting for $url" >&2
  return 1
}

start_session() {
  local node="$1"
  curl -fsS -X POST "$CONTROL/sessions/start" \
    -H 'content-type: application/json' \
    -d "{\"volume_id\":\"$VOLUME_ID\",\"force_node\":\"$node\"}"
}

wait_for "$CONTROL/healthz"

create_json=$(curl -fsS -X POST "$CONTROL/volumes/create" \
  -H 'content-type: application/json' \
  -d "{\"volume_id\":\"$VOLUME_ID\",\"size_bytes\":1024,\"chunk_size\":16}")
volume_id=$(printf '%s' "$create_json" | json_get volume_id)
echo "volume created: $volume_id"

start_json=$(start_session node-1)
session_id=$(printf '%s' "$start_json" | json_get session_id)
node_url=$(printf '%s' "$start_json" | json_get node_url)
echo "session started on node-1: $session_id"

printf '%s' "$DATA" | curl -fsS -X PUT "$node_url/sessions/$session_id/write?offset=$OFFSET" --data-binary @- >/dev/null
echo "write completed"

commit_json=$(curl -fsS -X POST "$node_url/sessions/$session_id/commit")
snapshot_id=$(printf '%s' "$commit_json" | json_get snapshot_id)
echo "snapshot committed: $snapshot_id"

resume1_json=$(start_session node-1)
session1=$(printf '%s' "$resume1_json" | json_get session_id)
node1_url=$(printf '%s' "$resume1_json" | json_get node_url)
read1=$(curl -fsS "$node1_url/sessions/$session1/read?offset=$OFFSET&length=${#DATA}")
stats1=$(curl -fsS "$node1_url/sessions/$session1/stats")
hits=$(printf '%s' "$stats1" | json_get cache_hits)
echo "resume on node-1: cache hit count > 0 ($hits)"

resume2_json=$(start_session node-2)
session2=$(printf '%s' "$resume2_json" | json_get session_id)
node2_url=$(printf '%s' "$resume2_json" | json_get node_url)
read2=$(curl -fsS "$node2_url/sessions/$session2/read?offset=$OFFSET&length=${#DATA}")
stats2=$(curl -fsS "$node2_url/sessions/$session2/stats")
misses=$(printf '%s' "$stats2" | json_get cache_misses)
remote=$(printf '%s' "$stats2" | json_get remote_fetches)
echo "resume on node-2: cache miss count > 0 ($misses) and remote fetch count > 0 ($remote)"

if [[ "$read1" != "$DATA" || "$read2" != "$DATA" ]]; then
  echo "final read data mismatch" >&2
  exit 1
fi
if (( hits <= 0 || misses <= 0 || remote <= 0 )); then
  echo "cache validation failed: hits=$hits misses=$misses remote=$remote" >&2
  exit 1
fi

echo "final read data equals originally written data"
