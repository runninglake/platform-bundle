-- Reads the Parquet written by count.sql and fails (an sql_error) unless the values are
-- exactly what 10,000,000 integers from 0 add up to.
SELECT CASE WHEN n = 10000000 AND s = 49999995000000 THEN 'ok'
            ELSE error('unexpected result values') END AS verdict
FROM read_parquet('/work/result.parquet')
