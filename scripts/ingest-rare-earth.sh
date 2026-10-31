#!/usr/bin/env bash
# Upload the rare-earth-business list to Watchman and show it in /v2/listinfo.
#
#   scripts/ingest-rare-earth.sh [host] [csv]
#   scripts/ingest-rare-earth.sh http://localhost:8084 configs/ingest/rare-earth-entities.csv
#
# Re-run after every change to the list. The upload replaces the whole source.
set -euo pipefail

host="${1:-http://localhost:8084}"
csv="${2:-configs/ingest/rare-earth-entities.csv}"
source_name="rare-earth-business"

if [ ! -f "$csv" ]; then
  echo "csv not found: $csv" >&2
  exit 1
fi

echo "POST ${host}/v2/ingest/${source_name} <- ${csv}"
curl -sS --fail -X POST --data-binary "@${csv}" \
  -H 'Content-Type: text/csv' \
  "${host}/v2/ingest/${source_name}" \
  | jq -r '"fileType=\(.fileType) entities=\(.entities | length)"'

echo "GET ${host}/v2/listinfo"
curl -sS --fail "${host}/v2/listinfo" \
  | jq -r --arg s "$source_name" '"rows=\(.lists[$s]) hash=\(.listHashes[$s])"'
