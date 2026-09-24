# rl-trino pins

Every input to `images/rl-trino/Dockerfile`, with the value it was verified to have, the
date, and the SECOND SOURCE it was checked against. The Dockerfile carries the same values
as `ARG` defaults and refuses to build if a download differs. A pin checked against only
the bytes you happened to download is a pin of your download, not of the release.

**Trino version:** 483 (train `2026.11`, `runtimes/2026.11/bom.yaml` in the private
repository; the newest release, published 2026-07-18). **Platform:** `linux/amd64` only.
**verified_on:** 2026-09-24.

## Server and JRE

| Artifact | sha256 | Source | Second source |
|---|---|---|---|
| `trino-server-483.tar.gz` (851,844,304 bytes) | `4f3978428f26f36398c94b85a3e03b5301394919c8a4271b497b0fcd1698d0cb` | https://github.com/trinodb/trino/releases/download/483/trino-server-483.tar.gz | GitHub's server-computed asset digest for that release, identical |
| `OpenJDK25U-jre_x64_linux_hotspot_25.0.1_8.tar.gz` (Temurin 25.0.1+8) | `126c7f59b91df97b2e930b0a456422ce29af7f2385117e1c04297eba962b0b1c` | https://github.com/adoptium/temurin25-binaries/releases/download/jdk-25.0.1%2B8/ | Adoptium's published `.sha256.txt` and its API, identical |

**Trino no longer publishes `trino-server` to Maven Central.** The last version there is
476 (`maven-metadata.xml` last updated 2025-06-06); the Maven URL for 483 redirects to the
GitHub release. That is the source recorded above.

**Java 25.0.1 is the BOM's value and Trino 483's floor:** Trino 483 requires 64-bit Java
25, minimum 25.0.1; 21 and 24 do not work and 26+ is unsupported. Newer 25.x patch releases
exist (25.0.4.1 at the time of writing) and are inside the BOM's `>=25.0.1, <26` constraint;
moving to one is a BOM change, not an image change.

## Base images

| Image | Digest | Role |
|---|---|---|
| `registry.access.redhat.com/ubi9/ubi-minimal` | `sha256:8ebe2ad8fdf3cab3e5a53c1edc69194c98209cfadab24b884f4ad9ebcf7bbbfc` | Runtime. The `latest` index on 2026-09-24; the build reports **Red Hat Enterprise Linux release 9.8 (Plow)**. `:9.6` resolves to a different digest, so the minor is recorded from the build rather than from a tag. |
| `public.ecr.aws/docker/library/debian:bookworm-slim` | `sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171` | Fetch stage only (curl). The same pin rl-duckdb uses, so this image adds none. |

## The plugin set

Kept, from `runtimes/2026.11/bom.yaml` `plugins_kept`: **iceberg, jmx, memory, opa, tpcds.**
The build deletes every other directory under `plugin/` and then asserts the listing is
exactly those five — an allow list, so a plugin upstream adds under a new name cannot ride
in. Removed (50): ai-functions, bigquery, blackhole, cassandra, clickhouse, datasketches,
delta-lake, druid, duckdb, elasticsearch, exasol, exchange-filesystem, exchange-hdfs, faker,
functions-python, geospatial, google-sheets, hive, http-event-listener, hudi, ignite, kafka,
kafka-event-listener, lakehouse, ldap-group-provider, loki, mariadb, ml, mongodb, mysql,
mysql-event-listener, openlineage, opensearch, oracle, password-authenticators, pinot,
postgresql, prometheus, ranger, redis, redshift, resource-group-managers,
session-property-managers, singlestore, snowflake, spooling-filesystem, sqlserver,
teradata-functions, thrift, tpch.

`secrets-plugin/keystore-secrets-plugin` is kept: it is not a connector and nothing routes
to it; it resolves secret references in configuration.

**Note for the Trino cluster manifests (tracker 6.13.5):** fault-tolerant execution with an
S3 exchange manager needs the `exchange-filesystem` plugin, which the BOM's five do not
include. Adding it is a BOM change and should be decided there, not by editing this image.

## The launcher

`bin/linux-amd64/launcher` is upstream's static Go binary (Go 1.26.4). The linux-arm64,
linux-ppc64le and darwin builds are deleted: dead weight in an amd64 image, and the two
linux ones carried twenty of the release scan's thirty-three findings. The entrypoint calls
the amd64 binary directly, so the image needs no bash and no Python.

## Scan exceptions

Thirteen CRITICAL/HIGH findings are unreachable here and are excepted in `.trivyignore`,
each with its reason, its fixed version and an expiry of 2026-12-31. Measured 2026-09-24:
without that file the scan reports exactly those thirteen and nothing else.
