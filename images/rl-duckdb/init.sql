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
