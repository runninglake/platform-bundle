#!/usr/bin/env bash
# Smoke-tests a built rl-duckdb image the way the pod will run it: uid 65532, read-only
# root filesystem, all capabilities dropped, /work the only writable path, the query
# mounted read-only, no credential. Runs in CI after the push and on the QA VM before
# it. gVisor is not part of this; the runtime class is the plane's.
#
# Usage: smoke/run.sh <image ref>      (DOCKER=... to use e.g. "sudo docker")
set -euo pipefail

IMAGE="${1:?image ref}"
DOCKER="${DOCKER:-docker}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH" 2>/dev/null || true' EXIT
FAILED=0

# run_case <name> <query file> <expected status> <expected error_class or -> <expected exit> [docker args...]
run_case() {
  local name="$1" query="$2" want_status="$3" want_class="$4" want_exit="$5"; shift 5
  local errfile="$SCRATCH/$name.stderr" out rc
  set +e
  out="$($DOCKER run --rm \
      --user 65532:65532 --read-only --cap-drop ALL --security-opt no-new-privileges \
      --memory 1g \
      -v "$HERE/$query:/run/rl/query.sql:ro" \
      -e RL_QUERY_FILE=/run/rl/query.sql -e RL_WORK_DIR=/work -e RL_RESULT_FILE=/work/result.parquet \
      -e RL_THREADS=2 -e RL_MEMORY_LIMIT=512MB -e HOME=/work \
      "$@" "$IMAGE" 2>"$errfile")"
  rc=$?
  set -e
  local lines; lines="$(printf '%s\n' "$out" | grep -c . || true)"
  local ok=1
  [[ "$lines" == 1 ]] || { echo "  stdout has $lines non-empty lines, want 1"; ok=0; }
  [[ "$out" == "RL_METRICS {"* ]] || { echo "  stdout does not start with RL_METRICS: $out"; ok=0; }
  [[ ! -s "$errfile" ]] || { echo "  stderr is not empty:"; sed 's/^/    /' "$errfile"; ok=0; }
  if grep -q rl_canary <<<"$out" || grep -q rl_canary "$errfile" 2>/dev/null; then
    echo "  SQL text leaked to the pod's stdout or stderr"; ok=0
  fi
  local json="${out#RL_METRICS }"
  local status class
  status="$(jq -r .status <<<"$json")"; class="$(jq -r '.error_class // "-"' <<<"$json")"
  [[ "$status" == "$want_status" ]] || { echo "  status=$status, want $want_status"; ok=0; }
  [[ "$class" == "$want_class" ]] || { echo "  error_class=$class, want $want_class"; ok=0; }
  [[ "$rc" == "$want_exit" ]] || { echo "  exit=$rc, want $want_exit"; ok=0; }
  LAST_JSON="$json"
  if [[ $ok == 1 ]]; then echo "ok   $name  $json"; else echo "FAIL $name  $json"; FAILED=1; fi
}

echo "image: $IMAGE"

run_case count count.sql succeeded - 0 --network none --tmpfs /work:rw,size=1g,mode=1777 -e RL_TIMEOUT_SECONDS=120
[[ "$(jq -r .rows_out <<<"$LAST_JSON")" == 1 ]] || { echo "  rows_out != 1"; FAILED=1; }
[[ "$(jq -r .result_bytes <<<"$LAST_JSON")" -gt 0 ]] || { echo "  result_bytes == 0"; FAILED=1; }
COUNT_JSON="$LAST_JSON"

run_case statement statement.sql succeeded - 0 --network none --tmpfs /work:rw,size=1g,mode=1777 -e RL_TIMEOUT_SECONDS=120
[[ "$(jq -r .rows_out <<<"$LAST_JSON")" == 0 ]] || { echo "  rows_out != 0 for a non-query"; FAILED=1; }

# The classes the runner reports for an engine refusal, one case each. The runner used
# to fold every engine error into sql_error; since #2 it reads the engine's own prefix,
# and the smoke has to say which one it means — a case named syntax_error that queried a
# missing TABLE was a catalog error by the engine's own words, and the first build after
# #2 failed exactly there. Merges here build nothing; only a tag runs this.
run_case catalog_error catalog_error.sql failed catalog_error 1 --network none --tmpfs /work:rw,size=1g,mode=1777 -e RL_TIMEOUT_SECONDS=120
run_case syntax_error syntax_error.sql failed syntax_error 1 --network none --tmpfs /work:rw,size=1g,mode=1777 -e RL_TIMEOUT_SECONDS=120

run_case timeout timeout.sql failed query_timeout 2 --network none --tmpfs /work:rw,size=1g,mode=1777 -e RL_TIMEOUT_SECONDS=2

run_case https https.sql succeeded - 0 --tmpfs /work:rw,size=1g,mode=1777 -e RL_TIMEOUT_SECONDS=300
[[ "$(jq -r .rows_out <<<"$LAST_JSON")" == 1 ]] || { echo "  rows_out != 1"; FAILED=1; }

# The result file: written by one run into a bind-mounted /work, read back by a second
# run of the same image, which errors unless the values are right.
WORK="$SCRATCH/work"; mkdir -p "$WORK"; chmod 0777 "$WORK"
run_case count_to_disk count.sql succeeded - 0 --network none -v "$WORK:/work" -e RL_TIMEOUT_SECONDS=120
[[ -s "$WORK/result.parquet" ]] || { echo "  /work/result.parquet was not written"; FAILED=1; }
[[ -s "$WORK/rowcount.csv" && -f "$WORK/error.log" && -d "$WORK/tmp" ]] || { echo "  expected work files missing: $(ls "$WORK")"; FAILED=1; }
run_case verify_result verify_result.sql succeeded - 0 --network none -v "$WORK:/work" -e RL_RESULT_FILE=/work/verify.parquet -e RL_TIMEOUT_SECONDS=120

# A wide integer keeps its digits. hugeint.sql writes /work/result.parquet with a sum
# past 2^53; verify_hugeint.sql reads it back and error()s unless it is exact, which is
# an sql_error and so a failed case. The pair has to run in this order against the same
# /work, like count/verify_result above.
run_case hugeint hugeint.sql succeeded - 0 --network none -v "$WORK:/work" -e RL_TIMEOUT_SECONDS=120
run_case verify_hugeint verify_hugeint.sql succeeded - 0 --network none -v "$WORK:/work" -e RL_RESULT_FILE=/work/verify_hugeint.parquet -e RL_TIMEOUT_SECONDS=120

if [[ $FAILED == 0 ]]; then
  echo "smoke: all cases passed"
  echo "SMOKE_METRICS=$COUNT_JSON"
else
  echo "smoke: FAILED"; exit 1
fi
