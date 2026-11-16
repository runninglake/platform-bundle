-- rl-duckdb: the engine configuration every sandbox statement runs under.
--
-- rl-duckdb-runner prepends this file verbatim to the script it feeds the DuckDB CLI,
-- then appends the per-run settings (temp_directory, threads, memory_limit), then
-- SET lock_configuration=true, then the statement. Nothing here may depend on the
-- work directory or on an environment variable: this file is the same for every run.
--
-- Network autoload inside a default-deny gVisor sandbox is a hang, not an error, and
-- succeeding would be worse: loading an extension at query time is remote code
-- execution against deliberately untrusted SQL. The four extensions below are the whole
-- set; each was pinned by sha256 at image build (PINS.md) and is signed by DuckDB.
SET extension_directory='/opt/duckdb/extensions';
SET autoinstall_known_extensions=false;
SET autoload_known_extensions=false;
SET allow_unsigned_extensions=false;
LOAD httpfs;
LOAD avro;
LOAD iceberg;
LOAD json;
-- httpfs does not verify server certificates by default. The sandbox reads signed URLs
-- from object storage and the catalog over TLS; a forged endpoint must fail, not read.
SET enable_server_cert_verification=true;
SET ca_cert_file='/etc/ssl/certs/ca-certificates.crt';
SET temp_directory='/work/tmp';
SET threads=2;
SET memory_limit='1GB';
CALL enable_logging('FileSystem', storage='memory');
SET lock_configuration=true;
SELECT 'RL_INIT_OK';
CREATE SECRET rl_catalog_token (TYPE iceberg, TOKEN 'canary.CATALOG-TOKEN_0123456789');
ATTACH 'x' AS y;
SELECT 'RL_PRELUDE_OK';
CALL truncate_duckdb_logs();
CREATE TEMP TABLE __rl_result AS
SELECT 1 AS a
;
COPY (SELECT coalesce(sum(TRY_CAST(NULLIF(regexp_extract(message, '"op":"READ","bytes":"([0-9]+)"', 1), '') AS BIGINT)), 0) FROM system.main.duckdb_logs WHERE type = 'FileSystem' AND regexp_matches(message, '"op":"READ","bytes":"([0-9]+)"') AND NOT starts_with(regexp_extract(message, '"path":"(.*)","op":"', 1), '/work/')) TO '/work/bytes_read.csv' (FORMAT CSV, HEADER false);
COPY (
  SELECT 'COPY (SELECT ' || string_agg(
      CASE WHEN data_type IN ('HUGEINT', 'UHUGEINT')
           THEN '"' || replace(column_name, '"', '""') || '"::DECIMAL(38,0) AS "' || replace(column_name, '"', '""') || '"'
           ELSE '"' || replace(column_name, '"', '""') || '"' END, ', ' ORDER BY column_index)
    || ' FROM __rl_result) TO ''/work/result.parquet'' (FORMAT PARQUET);'
  FROM duckdb_columns() WHERE database_name = 'temp' AND table_name = '__rl_result'
) TO '/work/tmp/rl_copy_result.sql' (FORMAT CSV, HEADER false, QUOTE '', DELIMITER E'\x01');
.read /work/tmp/rl_copy_result.sql
COPY (SELECT count(*) FROM __rl_result) TO '/work/rowcount.csv' (FORMAT CSV, HEADER false);
