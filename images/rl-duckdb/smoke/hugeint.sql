-- A sum past 2^53, which is where a HUGEINT written as a DOUBLE starts lying.
--
-- DuckDB widens SUM over any integer to HUGEINT; Parquet has no 128-bit integer, so the
-- writer used to map it to DOUBLE and 9007199254740993 came back as ...992. The runner
-- now casts HUGEINT and UHUGEINT result columns to DECIMAL(38,0). `small` and `label`
-- are here to prove the cast is not applied to everything.
SELECT SUM(x) AS units, 3::INTEGER AS small, 'keep me' AS label
  FROM (SELECT 9007199254740993::BIGINT AS x) t
