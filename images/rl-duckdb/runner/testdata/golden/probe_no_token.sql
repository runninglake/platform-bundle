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
SET lock_configuration=true;
SELECT 'RL_INIT_OK';
CREATE TEMP TABLE inp_abcdefgh12 AS SELECT 1;
SELECT 'RL_PRELUDE_OK';
CREATE TEMP TABLE __rl_result AS
SELECT 1 AS a
;
COPY (SELECT column_index, hex(column_name), data_type FROM duckdb_columns() WHERE database_name = 'temp' AND table_name = '__rl_result' ORDER BY column_index) TO '/work/probe_cols.csv' (FORMAT CSV, HEADER false);
COPY (
  SELECT 'COPY (SELECT count(*)' || string_agg(', count(*) - count("' || replace(column_name, '"', '""') || '")', '' ORDER BY column_index)
    || ' FROM __rl_result) TO ''/work/probe_counts.csv'' (FORMAT CSV, HEADER false);'
  FROM duckdb_columns() WHERE database_name = 'temp' AND table_name = '__rl_result'
) TO '/work/tmp/rl_probe_counts.sql' (FORMAT CSV, HEADER false, QUOTE '', DELIMITER E'\x01');
.read /work/tmp/rl_probe_counts.sql
COPY (
  SELECT 'COPY (SELECT sha256(count(*)::VARCHAR || '':'' || coalesce(sum((''0x'' || substr(h, 1, 16))::UBIGINT::HUGEINT), 0)::VARCHAR || '':'' || coalesce(sum((''0x'' || substr(h, 17, 16))::UBIGINT::HUGEINT), 0)::VARCHAR || '':'' || coalesce(sum((''0x'' || substr(h, 33, 16))::UBIGINT::HUGEINT), 0)::VARCHAR || '':'' || coalesce(sum((''0x'' || substr(h, 49, 16))::UBIGINT::HUGEINT), 0)::VARCHAR) FROM (SELECT sha256(''00112233445566778899aabbccddeeff:'' || concat(' || string_agg(
      '(CASE WHEN "' || replace(column_name, '"', '""') || '" IS NULL THEN ''N'' ELSE ''V'' || strlen(CAST("' || replace(column_name, '"', '""')
      || '" AS VARCHAR)) || '':'' || CAST("' || replace(column_name, '"', '""') || '" AS VARCHAR) END)', ', ' ORDER BY column_index)
    || ')) AS h FROM __rl_result)) TO ''/work/probe_digest.csv'' (FORMAT CSV, HEADER false);'
  FROM duckdb_columns() WHERE database_name = 'temp' AND table_name = '__rl_result'
) TO '/work/tmp/rl_probe_digest.sql' (FORMAT CSV, HEADER false, QUOTE '', DELIMITER E'\x01');
.read /work/tmp/rl_probe_digest.sql
COPY (SELECT count(*) FROM "inp_abcdefgh12") TO '/work/probe_in_0.csv' (FORMAT CSV, HEADER false);
COPY (SELECT count(*) FROM "inp_zyxwvuts98") TO '/work/probe_in_1.csv' (FORMAT CSV, HEADER false);
