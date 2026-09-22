# platform-bundle

The **engine images** of the RunningLake Runtime: the adopted, version-locked, sha256-verified
query engines a RunningLake data plane runs. Today that is one image, `rl-duckdb`.

This repository is public on purpose, and its name is older than its job. RunningLake's
ADR 12 splits the estate by one rule: *the artifact a linter guards is built in the
repository that runs the linter; everything else is built where the minutes are.* The
platform bundle (the Kustomize manifests a plane's Flux applies) is linted by Go tests in
the private repository and is built and signed there. Engine images are adopted software:
they need no private source, they are large, and a public repository has the Actions
minutes and the GHCR storage that private tiers do not. So the repository called
`platform-bundle` does not build the bundle; it builds the engines.

## What is here

```
images/rl-duckdb/
  Dockerfile           three stages: fetch+verify DuckDB, build the runner, distroless runtime
  PINS.md              every sha256 and digest the build accepts, and the date each was verified
  init.sql             the engine configuration every sandbox statement runs under
  NOTICE.runninglake   what is upstream, what is ours, under which licence
  .trivyignore         dated, justified scanner exceptions (none today)
  runner/              rl-duckdb-runner: Go, stdlib only, static; the image's ENTRYPOINT
  smoke/               the pod-shaped smoke tests CI runs against the pushed image
.github/workflows/image-release.yaml
```

## How an image reaches a plane

The workflow pushes `ghcr.io/runninglake/rl-duckdb:<version>` and prints the image
**digest** in its step summary. The digest is the hand-off. Per ADR 9, engine digests
are not shipped inside the platform bundle: they live in `runtimes/index.yaml` in the
private repository, keyed by train serial, and are served to each plane's agent over
the outbound stream that already exists. A CVE rebuild of an engine base layer is then
one index entry and no bundle release. Nothing consumes the tag.

Every image is signed keyless with cosign under this workflow's OIDC identity and
scanned with Trivy before it is signed. The signature is on the digest, never the tag.

## `rl-duckdb`

DuckDB v1.5.5 with the `httpfs`, `avro` and `iceberg` extensions baked in and verified
(`json`, `icu` and `parquet` are statically linked into the CLI upstream), on
`gcr.io/distroless/cc-debian12:nonroot`, with a small Go runner as the ENTRYPOINT. It is
the engine of the free tier: one single-use gVisor sandbox pod per statement, no shared
process, no credential of any kind in the pod, reads through signed URLs or not at all.

**linux/amd64 only.** The sandbox node pool is amd64 and the conformance corpus is
measured on amd64. A vectorised engine has architecture-specific paths; an arm64 image
nobody measured would make the capability table false on it.

**Hard 200 MB budget**, asserted by CI on the uncompressed image.

### The contract with the agent

The pod runs the image's ENTRYPOINT with no arguments, as uid 65532, with a read-only
root filesystem, `automountServiceAccountToken: false`, `runtimeClassName: rl-sandbox`,
and this environment:

| Variable | Meaning |
|---|---|
| `RL_QUERY_FILE` | the SQL file, one statement, mounted read-only (default `/run/rl/query.sql`) |
| `RL_WORK_DIR` | the only writable path, an emptyDir (default `/work`) |
| `RL_RESULT_FILE` | where the Parquet result goes (default `/work/result.parquet`) |
| `RL_THREADS` | DuckDB `threads` |
| `RL_MEMORY_LIMIT` | DuckDB `memory_limit`, e.g. `512MB` |
| `RL_TIMEOUT_SECONDS` | wall-clock limit; the engine is killed at it. Unset or `0`: no runner-side limit, the pod's `activeDeadlineSeconds` is the only one |
| `RL_RESULT_UPLOAD_URL` | a presigned `PUT` the result is sent to. Absent: the result stays in the emptyDir and dies with the pod |
| `RL_RESULT_UPLOAD_HEADERS` | a JSON object of headers the presigned signature covers, sent verbatim. Required with the URL; a URL without it is treated as no upload |
| `HOME` | `/work` |

No variable may start with `AWS_`. Egress is DNS and TCP 443 only.

**The upload carries no credential, and that is the point.** This pod runs with
`automountServiceAccountToken: false` and the admission policy refuses it otherwise, so it
can authenticate to nothing. A presigned URL is not an identity: it is a signed statement
that one method may be used on one key until one deadline, and the headers are part of
what was signed — so dropping one does not write an unencrypted object, it fails the
signature. That is what lets a result leave a pod which is deliberately unable to
authenticate.

**A failed upload is a successful query without a link.** The query ran and its numbers
are real, so the report says `result_uploaded: false` and the run still exits 0. Reporting
a failure would tell a user their query failed when it did not, and would lose the metrics
the cost path settles from. The agent emits a result handle only when `result_uploaded` is
true, because a handle for an object that was never written sends a browser to the storage
endpoint for an XML error quoting the customer's bucket.

**The URL never reaches `error.log`.** Its query string is its authorisation, and
`net/http` wraps transport failures in a `*url.Error` that embeds the whole URL — so an
unredacted DNS or TLS failure would write a working credential for the customer's own
object into a file anybody who can read the pod can read.

The runner:

1. Reads the SQL file. Refuses one over 1 MiB, one holding no statement, one holding
   more than one statement (a `;` outside a string, quoted identifier, dollar-quoted
   string or comment, followed by anything but whitespace, comments or more `;`), one
   containing a NUL byte, and one whose first significant character is `.` or `#`
   (the DuckDB CLI would take that line as a command rather than SQL). Leading
   whitespace and comments and a trailing `;` are stripped.
2. Feeds the DuckDB CLI (`-batch -bail -no-init -csv -noheader`) one script on stdin:
   `init.sql` (extension directory, `autoinstall_known_extensions=false`,
   `autoload_known_extensions=false`, `allow_unsigned_extensions=false`, `LOAD httpfs`,
   `LOAD avro`, `LOAD iceberg`, `LOAD json`, TLS certificate verification on), then
   `SET temp_directory='<RL_WORK_DIR>/tmp'` (created by the runner), `SET threads`,
   `SET memory_limit`, then `SET lock_configuration=true` so the statement cannot
   change any setting, then the statement:
   - first token `SELECT`, `WITH`, `FROM`, `VALUES`, `PIVOT`, `UNPIVOT` (case-insensitive),
     or a leading `(`: `CREATE TEMP TABLE __rl_result AS <statement>`, then
     a COPY the script GENERATES from the result's own schema — `HUGEINT` and `UHUGEINT`
     columns cast to `DECIMAL(38,0)`, because Parquet has no 128-bit integer and the
     writer's own mapping is `DOUBLE`, which silently rounds a sum past 2^53 — then `count(*)` of the
     table written to `<RL_WORK_DIR>/rowcount.csv`. No row is ever printed.
   - `DESCRIBE`, `SHOW`, `SUMMARIZE`: the same, materialised through
     `CREATE TEMP TABLE __rl_result AS SELECT * FROM (<statement>)`, because DuckDB does
     not accept them directly after `AS`.
   - anything else: executed as-is; `rows_out` is 0 and no result file is written.
3. Prints **exactly one line** to stdout and nothing to stderr:
   ```
   RL_METRICS {"status":"succeeded","rows_out":1,"bytes_scanned":0,"cpu_core_seconds":0.41,"peak_memory_bytes":97218560,"wall_seconds":0.52,"result_bytes":631,"result_uploaded":true}
   ```
   `error_class` is present only on failure. When DuckDB names the kind of error, the
   runner says which: `catalog_error` (a table or schema the engine could not find),
   `binder_error` (a column or function it could not resolve), `syntax_error`,
   `io_error`, `http_error` and `permission_denied`. `sql_error` remains the answer for
   any DuckDB error whose kind is not one of those — guessing a kind is worse than
   declining to, because a wrong class sends a person to look in the wrong place.

   That list is not new to the platform: `internal/agent/duckdb`'s `RunnerClasses` has
   admitted all of them from the start and `cmd/rl` carries a sentence for each. What was
   new is a runner that says one. Before this every engine error was `sql_error`, so a
   person querying a table their project had DECLARED was told "the engine reported an
   error in the statement" — pointing at their SQL, which was fine; the sandbox simply has
   no catalog to find the table in.

   The rest: `query_timeout`, `out_of_memory` (DuckDB's
   "Out of Memory Error", or the engine killed by SIGKILL outside the runner's own
   timeout), `runner_io_error` (the runner could not do its own work: work directory,
   result or count file, the engine binary, or the engine failed before the statement
   ran, e.g. an extension did not load), `invalid_query_file` (any refusal in step 1, or
   a missing file). `cpu_core_seconds` is user+system CPU of the engine process,
   `peak_memory_bytes` its maximum resident set (`ru_maxrss`), `wall_seconds` the runner's
   own wall clock, `result_bytes` the size of the Parquet file. **`bytes_scanned` is
   always 0 in this version**: DuckDB exposes no per-query byte counter the runner can
   read without a profiling mode that the configuration lock forbids the statement from
   enabling; it is a placeholder for a later serial.
4. Exits 0 on `succeeded`, 1 on `failed`, 2 on `query_timeout`.

Everything the engine writes to stderr goes to `<RL_WORK_DIR>/error.log`, never to the
pod's log stream, because a DuckDB error quotes the SQL and can quote row values. The
engine's stdout goes to `<RL_WORK_DIR>/duckdb.out`. Runner diagnostics are appended to
`error.log` prefixed `rl-duckdb-runner:` and never contain statement text. The engine
runs with a fresh environment of `HOME` and `TMPDIR` only, so an `AWS_*` variable that
reached the pod by mistake would still not reach DuckDB.

Things the runner deliberately does **not** do: it is not the policy boundary. A
non-query statement such as `INSTALL x` (fails: network install is disabled and port 80
is not reachable), `LOAD x` (fails unless `x` is baked and signed), `ATTACH` or `COPY`
to a path under `/work` will run. The catalog and the AST rewrite before the SQL
arrives are the boundary; the runner is one of the seven layers, not the seventh.

### Building and testing locally

```sh
cd images/rl-duckdb/runner && go test ./...          # stdlib only; the test binary is the fake engine
docker build -t rl-duckdb:local images/rl-duckdb    # needs network: fetches DuckDB and the extensions
images/rl-duckdb/smoke/run.sh rl-duckdb:local       # runs it the way the pod does
```

`docker build` fails if any download's sha256 differs from `PINS.md`. To bump anything,
change `PINS.md` and the matching `ARG` default in the Dockerfile in the same commit.

## Licence

The code RunningLake wrote in this repository (the runner, the Dockerfiles, the
workflows) is under the Apache License 2.0 (`LICENSE`). The private RunningLake
repositories carry no licence; this one does because it is public and because adopted
software sits beside our own here. DuckDB and its extensions are MIT-licensed by the
DuckDB Foundation and are shipped unmodified; each image carries a
`/NOTICE.runninglake` naming every upstream component, its version and its licence.
DuckDB is a trademark of the DuckDB Foundation; `rl-duckdb` is "based on DuckDB", not a
DuckDB distribution.
