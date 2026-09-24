#!/usr/bin/env bash
# tq <base-url> <sql> — run one statement over Trino's HTTP client protocol and print each
# row as a JSON array, one per line. Exits 1 with the server's error if the query FAILED.
# Follows nextUri the way a client must; retries a 503 on a nextUri, as the protocol asks.
set -euo pipefail
base="$1"; sql="$2"
resp="$(curl -sS --fail-with-body -X POST "$base/v1/statement" \
  -H 'X-Trino-User: rl-smoke' -H 'X-Trino-Source: rl-trino-smoke' --data-binary "$sql")"
while :; do
  jq -c '.data[]?' <<<"$resp"
  err="$(jq -r '.error.message // empty' <<<"$resp")"
  if [[ -n "$err" ]]; then echo "QUERY FAILED: $(jq -r '.error.errorName' <<<"$resp"): $err" >&2; exit 1; fi
  next="$(jq -r '.nextUri // empty' <<<"$resp")"
  [[ -z "$next" ]] && break
  for try in 1 2 3 4 5; do
    code="$(curl -sS -o /tmp/tq.$$ -w '%{http_code}' "$next")"
    [[ "$code" == 200 ]] && break
    [[ "$code" == 503 ]] && { sleep 1; continue; }
    echo "HTTP $code on nextUri" >&2; exit 1
  done
  resp="$(cat /tmp/tq.$$)"; rm -f /tmp/tq.$$
done
