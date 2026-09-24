# rl-trino — Trino 483 for RunningLake, with exactly five plugins.
#
# A SERVER, not a single-use sandbox: rl-duckdb runs one statement per pod and exits,
# while Trino runs as a long-lived coordinator (and, later, workers) that clients reach
# over its HTTP protocol. The plan's two roles, adhoc and batch, are configurations of
# this one image. This is the ADOPTED layer of the RunningLake Runtime (ADR 12): the
# server and the JRE are upstream bytes, verified by sha256 and never patched; the only
# RunningLake content is the default configuration under etc/ and the plugin set.
#
# FIVE PLUGINS, AND THE STRIPPING IS ENFORCEMENT RATHER THAN TIDINESS. Upstream ships 55.
# runtimes/2026.11/bom.yaml keeps iceberg, memory, jmx, opa and tpcds. A connector that is
# not installed cannot be configured, so "Trino cannot write Hudi" is true on the
# filesystem and not only in the router's rules: a catalog naming `hudi` fails the
# server's start (the smoke proves it). The build refuses to finish if the plugin
# directory holds anything else.
#
# Every input is pinned and every pin is in PINS.md with the date and the second source
# it was checked against. linux/amd64 ONLY, as rl-duckdb and for the same reason: the
# conformance corpus runs on amd64, and an arm64 image nobody measured would make the
# capability table false on it.

ARG UBI_MINIMAL_DIGEST=sha256:8ebe2ad8fdf3cab3e5a53c1edc69194c98209cfadab24b884f4ad9ebcf7bbbfc
# The same pinned Debian rl-duckdb fetches with, so the fetch stage adds no new pin.
ARG DEBIAN_DIGEST=sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171
ARG TRINO_VERSION=483

# ---------------------------------------------------------------------------
# Fetch: the server and the JRE, verified; the plugin set cut to five
# ---------------------------------------------------------------------------
FROM public.ecr.aws/docker/library/debian:bookworm-slim@${DEBIAN_DIGEST} AS fetch

ARG TRINO_VERSION
ARG TRINO_SERVER_SHA256=4f3978428f26f36398c94b85a3e03b5301394919c8a4271b497b0fcd1698d0cb
ARG JRE_VERSION=25.0.1+8
ARG JRE_SHA256=126c7f59b91df97b2e930b0a456422ce29af7f2385117e1c04297eba962b0b1c
ARG PLUGINS_KEPT="iceberg jmx memory opa tpcds"

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# Trino stopped publishing trino-server to Maven Central after 476; 483 is on GitHub
# Releases, whose server-computed asset digest is the second source PINS.md records.
RUN curl -fsSL --retry 3 -o trino-server.tar.gz \
      "https://github.com/trinodb/trino/releases/download/${TRINO_VERSION}/trino-server-${TRINO_VERSION}.tar.gz" \
 && echo "${TRINO_SERVER_SHA256}  trino-server.tar.gz" | sha256sum -c - \
 && tar -xzf trino-server.tar.gz \
 && mv "trino-server-${TRINO_VERSION}" trino \
 && rm trino-server.tar.gz

# Keep exactly the BOM's five and PROVE nothing else survived. The comparison is on the
# sorted directory listing, so a plugin added upstream under a new name cannot slip
# through by not being on a deny list: this is an allow list.
RUN cd trino/plugin \
 && for p in *; do case " ${PLUGINS_KEPT} " in *" ${p} "*) ;; *) rm -rf "${p}";; esac; done \
 && test "$(ls | tr '\n' ' ' | sed 's/ $//')" = "${PLUGINS_KEPT}" \
 && echo "plugins kept: $(ls | tr '\n' ' ')"

# The launcher is a static Go binary (bin/linux-amd64/launcher); the bash wrapper beside
# it only picks the platform. Every other platform's build is dead weight in an amd64
# image, and two of them (linux-arm64, linux-ppc64le) carried twenty of the thirty-three
# CVEs the release scan found — each a copy of the amd64 launcher's Go 1.26.4 findings.
# Deleted rather than excused; the survivors are allow-listed so a new platform upstream
# adds cannot ride in.
RUN cd trino/bin \
 && for d in */; do [ "$d" = "linux-amd64/" ] || rm -rf "$d"; done \
 && test "$(ls | tr '\n' ' ' | sed 's/ $//')" = "launcher launcher.properties linux-amd64" \
 && test -x linux-amd64/launcher

RUN JRE_URL_VERSION="$(printf '%s' "${JRE_VERSION}" | sed 's/+/%2B/')" \
 && JRE_FILE_VERSION="$(printf '%s' "${JRE_VERSION}" | tr '+' '_')" \
 && curl -fsSL --retry 3 -o jre.tar.gz \
      "https://github.com/adoptium/temurin25-binaries/releases/download/jdk-${JRE_URL_VERSION}/OpenJDK25U-jre_x64_linux_hotspot_${JRE_FILE_VERSION}.tar.gz" \
 && echo "${JRE_SHA256}  jre.tar.gz" | sha256sum -c - \
 && mkdir java \
 && tar -xzf jre.tar.gz -C java --strip-components=1 \
 && rm jre.tar.gz \
 && ./java/bin/java -version 2>&1 | grep -q "\"${JRE_VERSION%%+*}\""

# ---------------------------------------------------------------------------
# Runtime
# ---------------------------------------------------------------------------
# UBI9-minimal, as the runtime plan decided for the JVM images: glibc and the libraries a
# JRE links against, a package manager nobody runs at runtime, and Red Hat's patch cadence.
FROM registry.access.redhat.com/ubi9/ubi-minimal@${UBI_MINIMAL_DIGEST}

ARG VERSION=dev
ARG REVISION=unknown
ARG TRINO_VERSION
ARG UBI_MINIMAL_DIGEST
LABEL org.opencontainers.image.title="rl-trino" \
      org.opencontainers.image.description="Based on Trino ${TRINO_VERSION} with 0 RunningLake patches and five plugins kept (iceberg, jmx, memory, opa, tpcds). linux/amd64 only." \
      org.opencontainers.image.source="https://github.com/runninglake/platform-bundle" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="Apache-2.0 AND GPL-2.0-with-classpath-exception" \
      org.opencontainers.image.base.name="registry.access.redhat.com/ubi9/ubi-minimal" \
      org.opencontainers.image.base.digest="${UBI_MINIMAL_DIGEST}"

COPY --from=fetch /build/java /opt/java
COPY --from=fetch /build/trino /usr/lib/trino
COPY etc/ /etc/trino/
COPY NOTICE.runninglake /NOTICE.runninglake

# Everything the server writes goes under /work: the pod mounts it as the only writable
# path, and the root filesystem is read-only. HOME points there too, because the JVM
# resolves user.home and some libraries write under it.
ENV JAVA_HOME=/opt/java \
    PATH=/opt/java/bin:/usr/bin:/bin \
    HOME=/work

USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/lib/trino/bin/linux-amd64/launcher", "run", "--etc-dir=/etc/trino", "--data-dir=/work/data"]
