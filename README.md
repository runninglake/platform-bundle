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
| `RL_PROBE_FILE` | a JSON file that turns the run into a **probe** (below): `{"name_key_hex":"<16 to 32 bytes as hex>","inputs":["inp_<8 to 64 letters and digits>", ...]}`. Absent: a plain query, which is every run before the mode existed |
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

**Probe mode (`RL_PROBE_FILE`): a dry run of one statement on a sample, which writes nothing.**
**A tag cut after this commit contains the probe mode, off unless `RL_PROBE_FILE` is set; the plain-query path is unchanged and tested.** Merging it is not a release: every `rl-duckdb/v*` tag is admin-only and is cut only on the owner's explicit yes.
The control plane's change-set gates ask a plane to run an author's statement against a small
sample of its inputs and say whether it ran, how many rows it made, how many of each output
column were null and a digest of the output, without a row, a value or a name leaving the pod.
With the variable set the runner reads the spec, runs the statement as above, and then does NOT
write the result Parquet, does NOT upload (the agent mints no URL, and the runner makes no
request even if one is given), and reads four small files the engine writes under `RL_WORK_DIR`
instead. It prints **two** lines, the facts first and the metrics last (the agent reads the last
`RL_METRICS` line, so the facts can never be mistaken for the metrics; a probe that failed has
no facts line):
```
RL_PROBE {"inputs":[{"alias":"inp_3f9c01ab","rows":10000}],"columns":[{"name_hash":"<64 hex>","type_family":"string","nulls":3}],"digest_algorithm":"bag_digest_v1","digest":"<64 hex>"}
RL_METRICS {"status":"succeeded","rows_out":1200,...}
```
- The prelude (the control plane's) materialises each sampled input as a **TEMP TABLE named by its
  alias**; the runner counts each. A view over a `LIMIT` would be cut again at every read.
- A column's **name never leaves the pod**: `name_hash` is HMAC-SHA-256 of the lower-cased name under
  the key the control plane minted for this one probe, so the control plane, which holds the
  statement and the key, can put the author's names back and nothing else can.
- `type_family` is one word of a closed set: `boolean`, `integer` (TINYINT, SMALLINT, INTEGER),
  `long` (BIGINT), `float`, `double`, `decimal`, `string` (VARCHAR), `date`, `time`, `timestamp`,
  `timestamptz`, `binary` (BLOB), `uuid`, and `other` for anything else (unsigned and 128-bit integers,
  nested types including a list of decimals, intervals): an exotic column is a count and a hash, never a
  refusal. A result of thousands of columns or millions of rows probes as the plain query runs.
- `digest` is `bag_digest_v1`, **keyed, order-independent and constant-memory**. A row is its cells in
  order, each cell `N` for a null or `V`, the byte length of the cell's text, a colon and the text
  (DuckDB's own `CAST(cell AS VARCHAR)`), so no value can imitate another encoding. Each row hashes to
  SHA-256 of the key's hex, a colon and the row; the four 64-bit words of those hashes are each summed
  over the rows exactly (in 128 bits, so the sum does not depend on order) and the digest is the SHA-256
  of the text `count:w0:w1:w2:w3`. Permuting the rows changes nothing; a different bag, a duplicated
  row, a null moved to another column all change it; no row is ever held at once, so a result the
  plain query streams to a file is as cheap to probe. **The key is the spec's**: an unkeyed digest of a
  small or low-entropy result (`SELECT 'alice'`) is a guess-and-check oracle for its values, and a keyed
  one is that only for whoever holds the key, the control plane, which holds the statement anyway. Two
  digests are therefore comparable only under the same key (the control plane gives every engine of one
  comparison the same key) and the same algorithm name. It is DuckDB's text form, so it compares one
  DuckDB run with another and **not yet one engine with another**: a cross-engine digest is a later
  algorithm name.
- **What a name hash does and does not hide.** The name is hashed in the pod, so the control plane never
  sees a name it did not already hold. A column whose name is DATA (a `PIVOT ... ON <column>` names its
  columns from the values of one) is therefore known to whoever holds the key by dictionary attack, and
  the probe does not flag it. Two further things are the engine's, not the runner's: DuckDB renames a
  duplicate output name (`SELECT 1 AS a, 2 AS a` becomes `a` and `a_1`), and the DuckDB CLI reads its
  script with line endings normalised, so a CR LF inside an alias reaches the engine as LF. The hash is of
  the name the engine has.
- A spec the runner cannot hold (not JSON, a short or non-hex key, an alias that is not an opaque id
  or is repeated, more than 16 inputs) and a statement whose first token is not a query keyword
  (`CREATE`, `INSERT`, `EXPLAIN`, `PRAGMA`, ...: the runner's own classification) are
  `invalid_query_file` before the engine starts. A runner built before this mode ignores the
  variable, runs the statement as a plain query and prints no `RL_PROBE` line; the agent then
  answers that this runner cannot probe, and the dry run reads "not run", never a pass.

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
3. Prints **exactly one line** to stdout and nothing to stderr (a probe prints a second line, its
   facts, before this one; see above):
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

**Task mode (`RL_TASK_FILE`): one write of a pipeline task, composed here from a spec file, and a claim of what it did.**
**A tag cut after this commit contains the task mode, off unless `RL_TASK_FILE` is set; the plain-query path is unchanged and tested.** Merging it is not a release: every `rl-duckdb/v*` tag is admin-only and is cut only on the owner's explicit yes. **Nothing here has run against an Iceberg catalog.** The tests run the real pinned CLI against a native DuckDB database standing in for the catalog, so the composition, the quoting, the checks, the sentinels and the `.read` chain are exercised; what DuckDB's iceberg extension does with the same statements rests on `docs/measurements/2026-09-29-duckdb-1.5.5-iceberg-writes.md` in the main repository, from which each statement is taken.

- **The files.** `RL_QUERY_FILE` is still exactly one `SELECT` (a task whose statement is anything else is `invalid_query_file`). `RL_TASK_FILE` names the task's structured intent, version 2, which carries no SQL: `attempt`, `mode` (`append`, `overwrite`, `partition_overwrite`, `merge`), `target` (`warehouse`, `namespace` levels, `name`), `unique_key`, `partition` (`column` and typed `start`/`end`) and `base_snapshot_id`. A spec the runner cannot hold (not JSON of that shape, an unknown field, an unknown version, a mode or kind it does not know, a name with a control character, a level holding a dot, a bound that is not of its kind) is `invalid_query_file` before the engine starts, and prints no claim. The error never quotes the spec.
- **The write.** The `SELECT` is materialised into `__rl_result` as for any query. The runner then writes it to `rl_catalog."<levels joined with dots>"."<name>"` (the alias is the agent's `ATTACH`, which must say `SUPPORT_NESTED_NAMESPACES true`; the quoting is one identifier for all the levels, doubled quotes): `append` is one `INSERT INTO t SELECT *` (position-based, as measured, which is why the columns are checked first, below); `overwrite` is `BEGIN; DELETE FROM t; INSERT ...; COMMIT`; `partition_overwrite` is the same with `DELETE ... WHERE` the partition's bounds (`[start, end)` for a date, instant or integer, equality for the static one; an instant is compared at the instant, the session being UTC); `merge` is `MERGE INTO t USING (...) AS s USING (<key>) WHEN MATCHED THEN UPDATE WHEN NOT MATCHED THEN INSERT`. **An append or a merge of an empty result runs no statement** and claims `no_change`; an overwrite of an empty result still deletes. **Only single-level namespaces are proven for writes**: a write into a nested namespace through the dotted form was not measured.
- **Checks before anything is written**, because the engine enforces none of them: **the result's columns are the target's, by name (case-insensitively) and in the target's order** (the writes match by position, and the engine raises no error when two swapped columns have the same type; the target's columns are read through the catalog with a `LIMIT 0` read into a temp table, so a missing, extra, renamed or reordered column is refused; `BY NAME` forms were not used because they are unmeasured on Iceberg); a merge's source holds each key once and none is `NULL` (the measured engine refuses a duplicate only for a key already in the table and lands a duplicate new key twice), and a partition overwrite's result holds no row outside the partition it replaces, `NULL` included. A refusal is an ordinary engine error before the first write statement: `not_committed`. The claims depend on the CLI's stdout being flushed at exit; if the work volume is full and the flush fails, a `not_committed` or `unknown` claim can be lost, which is safe (the agent rolls back any head past the base that no `committed` claim covers), and the `committed` claim is printed only after the commit returned.
- **The claim.** Directly before the `RL_METRICS` line the runner prints one `RL_TASK {"v":1,"attempt":"...","outcome":"...","base_snapshot_id":"...","committed_snapshot_id":"0","files_written":0,"bytes_written":0}`. The outcome is how far the script got, from sentinels it prints: `committed` (the `COMMIT`, or the one statement, returned), `no_change` (nothing was to be written), `not_committed` (the engine failed before any write statement, or inside a transaction before `COMMIT` was sent) or `unknown` (it failed after a statement that may have committed, or after `COMMIT` was sent, or the engine was killed or timed out, which may lose the sentinels). **The runner does not name the snapshot it committed and does not count files or bytes**: DuckDB gives it neither in a form it can trust, and the agent reads the table after the Job ends and decides from the catalog. `"0"` is none. The base is the spec's, echoed. **The runner does not compare the table's snapshot with the base before it writes**; the agent refuses a moved table at acceptance and proves the outcome after the Job. The agent must accept a `committed` claim with snapshot id `"0"` (its own change, owed).
- **The catalog token.** When `RL_CATALOG_TOKEN_FILE` names a file, the runner makes `CREATE SECRET rl_catalog_token (TYPE iceberg, TOKEN '<token>')` itself, after `SET lock_configuration=true` and the init sentinel and before the prelude, whose `ATTACH` finds the one iceberg secret by default (measured to work after the lock, so no new tag; a *named* secret with no `SECRET` clause in the `ATTACH` is the form `m64` measured, an explicit `SECRET` clause is unmeasured). The token is checked against `[A-Za-z0-9._~+/=-]`, 8 to 8192 bytes, and quoted; a file that does not hold one is `runner_io_error` and the engine does not run. The token is on the engine's stdin and in no file, no environment variable, no log line and no output. The split was agreed with E06-BE on 2026-10-22: the control plane's prelude carries no secret and no `ATTACH`, the agent prepends the `ATTACH`, the runner makes the secret.
- **Not done.** No upload and no result file (a task's result is the table); `bytes_scanned` stays 0; a task and a probe together are refused.

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

## rl-trino

A Trino 483 **server** image — unlike rl-duckdb, which runs one statement per pod and
exits, this runs as a long-lived coordinator (and later workers) that clients reach over
Trino's HTTP protocol on port 8080. Pins, the plugin set and the scan exceptions are in
`images/rl-trino/PINS.md`; the smoke is `images/rl-trino/smoke/run.sh`.

**What a deployment must provide:**

- **`/work` writable AND executable.** It is the node's data directory and the JVM's
  `java.io.tmpdir`, and native libraries (zstd, brotli) are extracted there and mapped. A
  `noexec` `/work` breaks them — the smoke measures this, and Kubernetes' default
  `emptyDir` is fine. The root filesystem can and should be read-only.
- **uid 65532.** The image sets it; the smoke runs as it with all capabilities dropped.
- **Configuration under `/etc/trino`.** The image bakes a single-node default and three
  catalogs (tpcds, memory, jmx). A deployment mounts its own `config.properties`,
  `jvm.config` and `catalog/` — the Iceberg catalog is REST (Lakekeeper) and is never
  baked, because it carries an endpoint and credentials.

**Five plugins, as enforcement.** A catalog naming a connector this image does not ship
stops the server's start (`No factory for connector 'hudi'`); the smoke asserts it, and
asserts it fails when the Hudi plugin is put back.
