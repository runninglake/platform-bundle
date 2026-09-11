# rl-duckdb — DuckDB v1.5.5 for RunningLake's single-use sandbox pod.
#
# One pod, one statement, one Parquet result. The pod runs this image's ENTRYPOINT with
# no arguments as uid 65532 on a read-only root filesystem under gVisor, with no
# credential of any kind; the contract with the agent is README.md at the repository
# root. This image is the ADOPTED layer of the RunningLake Runtime (ADR 12): DuckDB and
# its extensions are upstream bytes, verified by sha256 and never patched, and only
# the runner is ours.
#
# Every input is pinned, and every pin is in PINS.md with the date it was verified:
# the two base images by index digest, the CLI archive and each extension by sha256.
# The build refuses to produce an image if any of them differs from the record.
#
# linux/amd64 ONLY. The sandbox node pool is amd64, the conformance corpus is run on
# amd64, and a vectorised engine has architecture-specific paths; shipping an arm64
# image nobody measured would make the capability table false on it.

ARG DEBIAN_DIGEST=sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171
ARG GOLANG_DIGEST=sha256:9fdc884aacc3bec89b20ffc69f4bb369c78210e3e4f600387b5128b12c199f81
ARG DISTROLESS_DIGEST=sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f
ARG DUCKDB_VERSION=1.5.5

# ---------------------------------------------------------------------------
# Engine: fetch the CLI, install the extensions, verify every byte
# ---------------------------------------------------------------------------
FROM public.ecr.aws/docker/library/debian:bookworm-slim@${DEBIAN_DIGEST} AS engine

ARG DUCKDB_VERSION
ARG DUCKDB_CLI_SHA256=08c0ca117111fcede14239d0093792352befdc174218c344d232c13279643d05
ARG EXT_HTTPFS_SHA256=887c392b1e49128d11667c81e3698d8b00dfdeb456771acf66d05a0f74f7b7d8
ARG EXT_AVRO_SHA256=f9c59d9735863896fb10c78f2d6e23132cc9ede4a4fb8c10a6f523b6d0793c77
ARG EXT_ICEBERG_SHA256=ebc4993903de0d71d42fe3027b6691b8d5e5936ed366e98bdb9eab5aeedeb422
# DuckDB's default repository is plain http; the same files are served over TLS.
ARG EXT_REPOSITORY=https://extensions.duckdb.org

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl unzip \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# The glibc build. It is dynamically linked against libstdc++, libm, libgcc_s, libdl
# and libpthread, which is why the runtime base is distroless/cc and not /static. The
# -musl build is not static either (it needs the musl loader), so it buys nothing.
RUN curl -fsSL --retry 3 -o duckdb_cli.zip \
      "https://github.com/duckdb/duckdb/releases/download/v${DUCKDB_VERSION}/duckdb_cli-linux-amd64.zip" \
 && echo "${DUCKDB_CLI_SHA256}  duckdb_cli.zip" | sha256sum -c - \
 && unzip -q duckdb_cli.zip duckdb \
 && chmod 0755 duckdb \
 && ./duckdb --version | grep -qE "^v${DUCKDB_VERSION}( |$)"

# Installed the way the BOM records them (SET extension_directory; INSTALL <name>), so
# the file DuckDB lays down is the file the sha256 was taken from. json is not
# installed: it is statically linked into the CLI, and LOAD json never opens a file
# even when one is present (PINS.md records the proof).
RUN mkdir -p /opt/duckdb/extensions \
 && ./duckdb -batch -bail -no-init -csv -noheader <<EOF
SET extension_directory='/opt/duckdb/extensions';
INSTALL httpfs FROM '${EXT_REPOSITORY}';
INSTALL avro FROM '${EXT_REPOSITORY}';
INSTALL iceberg FROM '${EXT_REPOSITORY}';
EOF

# Verify, then drop the .info sidecars (origin URL and ETag; PINS.md is the record) and
# assert nothing else arrived.
RUN cd "/opt/duckdb/extensions/v${DUCKDB_VERSION}/linux_amd64" \
 && printf '%s  httpfs.duckdb_extension\n%s  avro.duckdb_extension\n%s  iceberg.duckdb_extension\n' \
      "${EXT_HTTPFS_SHA256}" "${EXT_AVRO_SHA256}" "${EXT_ICEBERG_SHA256}" | sha256sum -c - \
 && rm -f ./*.duckdb_extension.info \
 && test "$(ls | wc -l)" = 3 \
 && chmod 0644 ./*.duckdb_extension

# Prove the runtime configuration loads everything OFFLINE, exactly as the runner will
# run it, before anything is copied into the final image.
COPY init.sql /opt/duckdb/init.sql
RUN { cat /opt/duckdb/init.sql; \
      echo "SELECT extension_name FROM duckdb_extensions() WHERE loaded AND extension_name IN ('httpfs','avro','iceberg','json') ORDER BY 1;"; } \
    | ./duckdb -batch -bail -no-init -csv -noheader > /tmp/loaded.txt \
 && printf 'avro\nhttpfs\niceberg\njson\n' | diff - /tmp/loaded.txt

# ---------------------------------------------------------------------------
# Runner: our code, stdlib only, static
# ---------------------------------------------------------------------------
FROM public.ecr.aws/docker/library/golang:1.26-bookworm@${GOLANG_DIGEST} AS runner

WORKDIR /src
COPY runner/ ./

ARG VERSION=dev
# The tests run here as well as in CI, so the image cannot be built from a runner that
# fails them. CGO_ENABLED=0 so the binary needs nothing from the base; -trimpath so the
# builder's paths do not ship.
RUN gofmt -l . | tee /dev/stderr | test -z "$(cat)" \
 && go vet ./... \
 && go test -count=1 ./... \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/rl-duckdb-runner .

# ---------------------------------------------------------------------------
# Runtime
# ---------------------------------------------------------------------------
# distroless/cc-debian12:nonroot: glibc, libstdc++, libgcc, CA certificates, tzdata.
# No shell, no package manager. :nonroot so the image runs as 65532 even where a pod
# security context is missing; the admission policy requires it, and an image that only
# complies when the policy is applied will one day run where it is not.
FROM gcr.io/distroless/cc-debian12:nonroot@${DISTROLESS_DIGEST}

ARG VERSION=dev
ARG REVISION=unknown
ARG DUCKDB_VERSION
ARG DISTROLESS_DIGEST
LABEL org.opencontainers.image.title="rl-duckdb" \
      org.opencontainers.image.description="Based on DuckDB v${DUCKDB_VERSION} with 0 RunningLake patches. Single-use sandbox engine for the RunningLake free tier: one pod, one statement, one Parquet result. linux/amd64 only." \
      org.opencontainers.image.source="https://github.com/runninglake/platform-bundle" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="MIT AND Apache-2.0" \
      org.opencontainers.image.base.name="gcr.io/distroless/cc-debian12:nonroot" \
      org.opencontainers.image.base.digest="${DISTROLESS_DIGEST}"

COPY --from=engine /build/duckdb /usr/local/bin/duckdb
COPY --from=engine /opt/duckdb/extensions /opt/duckdb/extensions
COPY --from=engine /opt/duckdb/init.sql /opt/duckdb/init.sql
COPY --from=runner /out/rl-duckdb-runner /usr/local/bin/rl-duckdb-runner
COPY NOTICE.runninglake /NOTICE.runninglake

# The contract's defaults, so a bare `docker run` with a query mounted at
# /run/rl/query.sql behaves like the pod. The pod sets all of them explicitly anyway.
# None starts with AWS_.
ENV HOME=/work \
    RL_QUERY_FILE=/run/rl/query.sql \
    RL_WORK_DIR=/work \
    RL_RESULT_FILE=/work/result.parquet

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/rl-duckdb-runner"]
