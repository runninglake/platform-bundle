#!/usr/bin/env bash
# Smoke-tests a built rl-trino image the way a plane will run it: uid 65532, read-only root
# filesystem, all capabilities dropped, /work the only writable path, a memory cap. Runs in
# the release job BEFORE the push, and on the QA host by hand. Needs docker, curl and jq.
#
# Usage: smoke/run.sh <image ref>      (DOCKER=... to use e.g. "sudo docker")
set -euo pipefail

IMAGE="${1:?image ref}"
DOCKER="${DOCKER:-docker}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TQ="$HERE/tq.sh"
PORT="${RL_SMOKE_PORT:-18080}"
BASE="http://127.0.0.1:${PORT}"
NAME="rl-trino-smoke-$$"
FAILED=0
trap '$DOCKER rm -f "$NAME" >/dev/null 2>&1 || true' EXIT

ok()   { echo "ok   $1"; }
fail() { echo "FAIL $1"; FAILED=1; }

# start <tmpfs options> [extra docker args...] — returns 0 once SERVER STARTED, 1 if the
# container exited first, 2 on timeout.
start() {
  local tmpfs="$1"; shift
  $DOCKER rm -f "$NAME" >/dev/null 2>&1 || true
  $DOCKER run -d --name "$NAME" \
    --user 65532:65532 --read-only --cap-drop ALL --security-opt no-new-privileges \
    --tmpfs "/work:${tmpfs}" --memory 3g -p "127.0.0.1:${PORT}:8080" \
    "$@" "$IMAGE" >/dev/null
  for _ in $(seq 1 60); do
    if $DOCKER logs "$NAME" 2>&1 | grep -q "SERVER STARTED"; then return 0; fi
    if [[ "$($DOCKER inspect -f '{{.State.Running}}' "$NAME")" != true ]]; then return 1; fi
    sleep 2
  done
  return 2
}
q() { "$TQ" "$BASE" "$1"; }

echo "image: $IMAGE"

# CASE 1. The plugin set, read from the image itself rather than from the Dockerfile's claim.
plugins="$($DOCKER run --rm --entrypoint ls "$IMAGE" /usr/lib/trino/plugin | tr '\n' ' ' | sed 's/ $//')"
[[ "$plugins" == "iceberg jmx memory opa tpcds" ]] && ok "exactly five plugins: $plugins" || fail "plugins are '$plugins'"

# CASE 2. It starts under the pod's posture, with the smoke's Iceberg catalog, and logs no ERROR.
if start "rw,exec,size=2g,mode=1777" -v "$HERE/catalogs/iceberg.properties:/etc/trino/catalog/iceberg.properties:ro"; then
  errs="$($DOCKER logs "$NAME" 2>&1 | grep -c "	ERROR	" || true)"
  [[ "$errs" == 0 ]] && ok "started as 65532 on a read-only root, 0 ERROR lines" || { fail "started with $errs ERROR lines"; $DOCKER logs "$NAME" 2>&1 | grep -A3 "	ERROR	" | head -12; }
else
  fail "did not start"; $DOCKER logs "$NAME" 2>&1 | tail -30; exit 1
fi

# CASE 3. The server says which release it is. The conformance Runner cross-checks exactly this.
v="$(curl -sS "$BASE/v1/info" | jq -r '.nodeVersion.version + " " + (.starting|tostring)')"
[[ "$v" == "483 false" ]] && ok "/v1/info: version 483, not starting" || fail "/v1/info says '$v'"

# CASE 4. Catalogs: the baked three, the smoke's iceberg, and system. Nothing a stripped plugin
#    could have provided.
cats="$(q "SHOW CATALOGS" | jq -r '.[0]' | tr '\n' ' ' | sed 's/ $//')"
[[ "$cats" == "iceberg jmx memory system tpcds" ]] && ok "catalogs: $cats" || fail "catalogs are '$cats'"

# CASE 5. Known answers from the generator connector.
[[ "$(q 'SELECT count(*) FROM tpcds.tiny.customer')" == "[1000]" ]] && ok "tpcds.tiny.customer = 1000" || fail "tpcds.tiny.customer count"

# CASE 6. Values that break a careless client or writer, through the memory connector: a bigint
#    past 2^53, a decimal whose trailing zero is part of its scale, a millisecond timestamp.
q "CREATE TABLE memory.default.v AS SELECT BIGINT '9007199254740993' AS big, DECIMAL '123.450' AS d, TIMESTAMP '2026-01-02 03:04:05.678' AS ts" >/dev/null
row="$(q 'SELECT CAST(big AS varchar), CAST(d AS varchar), CAST(ts AS varchar) FROM memory.default.v')"
[[ "$row" == '["9007199254740993","123.450","2026-01-02 03:04:05.678"]' ]] && ok "memory round trip exact: $row" || fail "memory round trip gave $row"

# CASE 7. Iceberg: write Parquet with ZSTD and read it back, digit for digit. This is the plugin
#    that reads the customer's lake, so it is the one that has to be proven, not assumed.
q "CREATE SCHEMA iceberg.smoke" >/dev/null
q "CREATE TABLE iceberg.smoke.t (big bigint, d decimal(10,3), ts timestamp(6)) WITH (format = 'PARQUET')" >/dev/null
q "INSERT INTO iceberg.smoke.t VALUES (BIGINT '9007199254740993', DECIMAL '123.450', TIMESTAMP '2026-01-02 03:04:05.678901')" >/dev/null
row="$(q 'SELECT CAST(big AS varchar), CAST(d AS varchar), CAST(ts AS varchar) FROM iceberg.smoke.t')"
[[ "$row" == '["9007199254740993","123.450","2026-01-02 03:04:05.678901"]' ]] && ok "iceberg parquet round trip exact: $row" || fail "iceberg round trip gave $row"
# CASE 8. The table's own metadata says what was written: one Parquet data file.
files="$(q 'SELECT count(*) FROM iceberg.smoke."t$files" WHERE file_format = '"'"'PARQUET'"'"'')"
[[ "$files" == "[1]" ]] && ok "iceberg wrote one PARQUET data file" || fail "iceberg \$files says $files"

# CASE 9. NEGATIVE: a connector this image does not ship cannot be configured. This is the claim
#    the plugin stripping exists for — "Trino cannot write Hudi" as a fact of the filesystem
#    — so it is asserted by trying, not by listing a directory.
if start "rw,exec,size=2g,mode=1777" -v "$HERE/catalogs/hudi.properties:/etc/trino/catalog/hudi.properties:ro"; then
  fail "the server STARTED with a hudi catalog; the stripped connector is reachable"
elif $DOCKER logs "$NAME" 2>&1 | grep -qiE "no factory for connector 'hudi'|connector 'hudi'"; then
  ok "a hudi catalog refuses the server's start: $($DOCKER logs "$NAME" 2>&1 | grep -oiE "no factory for connector 'hudi'[^\"]{0,40}" | head -1)"
else
  fail "the hudi catalog failed for a reason other than the missing connector"; $DOCKER logs "$NAME" 2>&1 | grep -iE "error" | head -5
fi

# CASE 10. THE /work REQUIREMENT, measured rather than stated: native libraries are extracted to
#    java.io.tmpdir (/work) and mapped from there, so /work must NOT be noexec. Kubernetes'
#    default emptyDir is exec; Docker's --tmpfs default is noexec. If this case ever starts
#    passing with 0 errors, the requirement has gone away and the README should say so.
if start "rw,noexec,size=2g,mode=1777"; then
  errs="$($DOCKER logs "$NAME" 2>&1 | grep -c "	ERROR	" || true)"
  [[ "$errs" -gt 0 ]] && ok "a noexec /work breaks native libraries ($errs ERROR lines) — the requirement is real" || fail "a noexec /work logged no ERROR; update the README's /work requirement"
else
  fail "did not start on a noexec /work"
fi

if [[ $FAILED == 0 ]]; then echo "smoke: all cases passed"; else echo "smoke: FAILED"; exit 1; fi
