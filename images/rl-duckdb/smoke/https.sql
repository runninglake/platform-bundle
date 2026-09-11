-- A public Parquet file over HTTPS, read through the baked httpfs with certificate
-- verification on and no credential of any kind.
SELECT count(*) AS n FROM read_parquet('https://blobs.duckdb.org/data/tpch-sf1/lineitem.parquet')
