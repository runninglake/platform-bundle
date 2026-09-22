-- Reads the Parquet written by hugeint.sql and fails (an sql_error) unless the sum kept
-- every digit and the other columns were left alone. Before the DECIMAL(38,0) cast this
-- case fails with units = 9007199254740992.
SELECT CASE WHEN units = 9007199254740993 AND small = 3 AND label = 'keep me'
            THEN 'ok'
            ELSE error('a wide integer lost digits in the result file') END AS verdict
FROM read_parquet('/work/result.parquet')
