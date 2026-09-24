-- Reads the Parquet written by hugeint.sql and fails (an sql_error) unless the sum kept
-- every digit and the other columns were left alone.
--
-- COMPARED AS TEXT, AND THE TYPE ASSERTED, because the obvious comparison cannot fail.
-- `units = 9007199254740993` against a DOUBLE converts the literal to DOUBLE too, and both
-- sides round to ...992 — measured on 1.5.5: the first version of this file printed 'ok'
-- against the UNFIXED image's result, whose units read back as 9007199254740992.0. The
-- one check that runs against the shipped image was therefore unable to see the defect it
-- was written for. Text cannot round, and a DOUBLE is refused by type before its digits are
-- even compared.
SELECT CASE WHEN typeof(units) LIKE 'DECIMAL%'
             AND units::VARCHAR = '9007199254740993'
             AND small = 3 AND label = 'keep me'
            THEN 'ok'
            ELSE error('a wide integer lost digits in the result file: units is '
                       || typeof(units) || ' ' || units::VARCHAR) END AS verdict
FROM read_parquet('/work/result.parquet')
