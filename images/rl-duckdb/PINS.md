# rl-duckdb pins

Every input to `images/rl-duckdb/Dockerfile`, with the value it was verified to have and
the date it was verified. The Dockerfile carries the same values as `ARG` defaults and
refuses to build if a download differs. A pin from memory is fiction with a version
number on it; every row below was checked against the upstream artifact on the date
shown, on x86_64 Linux.

**DuckDB version:** 1.5.5 (train `2026.11`, `runtimes/2026.11/bom.yaml` in the private
repository). **Platform:** `linux_amd64` only. **verified_on:** 2026-09-11.

## Engine

| Artifact | sha256 | Source |
|---|---|---|
| `duckdb_cli-linux-amd64.zip` v1.5.5 | `08c0ca117111fcede14239d0093792352befdc174218c344d232c13279643d05` | https://github.com/duckdb/duckdb/releases/download/v1.5.5/duckdb_cli-linux-amd64.zip |

The archive holds one file, `duckdb`, 61,936,648 bytes, dynamically linked against glibc,
libstdc++, libm, libgcc_s, libdl and libpthread. It is shipped byte-for-byte as released,
not stripped, so the binary in the image is the binary upstream signed off on.

## Extensions

Installed by the pinned CLI itself (`SET extension_directory='…'; INSTALL <name> FROM
'https://extensions.duckdb.org'`) into `v1.5.5/linux_amd64/`, then hashed. Each file is
signed by DuckDB; a corrupted or unsigned file is refused at LOAD with
`allow_unsigned_extensions=false`, which is the image's setting.

| Extension | sha256 of `<name>.duckdb_extension` | Bytes | Baked |
|---|---|---|---|
| `httpfs` | `887c392b1e49128d11667c81e3698d8b00dfdeb456771acf66d05a0f74f7b7d8` | 21,570,542 | yes |
| `avro` | `f9c59d9735863896fb10c78f2d6e23132cc9ede4a4fb8c10a6f523b6d0793c77` | 12,075,206 | yes |
| `iceberg` | `ebc4993903de0d71d42fe3027b6691b8d5e5936ed366e98bdb9eab5aeedeb422` | 50,785,246 | yes |
| `json` | `20921f71a8dd71c5518ab99649ea285b47182c693dd752bb89013c664e89bc32` | 33,608,126 | **no: built in** |

`avro` is not in the BOM's extension list; `LOAD iceberg` fails without it on 1.5.x, so
it is pinned here and the BOM points at this file.

**Why `json` is not baked.** The 1.5.5 CLI statically links `json`, `icu`, `parquet`,
`core_functions`, `autocomplete` and `shell` (`SELECT extension_name, install_path FROM
duckdb_extensions() WHERE installed` shows them as `(BUILT-IN)`). `LOAD json` therefore
never opens a file: with `extension_directory` pointing at a directory holding 100 KB of
`/dev/urandom` named `json.duckdb_extension`, `LOAD json` succeeded and
`json_extract('{"a":1}', '$.a')` returned `1`; the same directory's corrupted `httpfs`
was refused with "The file is not a DuckDB extension". Baking the loadable `json` would
add 33.6 MB the engine cannot use to an image with a hard 200 MB budget. The sha256 is
recorded so the BOM's row stays verifiable; `LOAD json` stays in `init.sql` so the
contract's load list is honoured and a future CLI that stops linking it fails the
build's offline-load check rather than a customer's query.

## Base images

Pinned by **index** digest (the multi-platform manifest list); buildx resolves the
`linux/amd64` manifest from it. A tag is a pointer somebody else can move.

| Stage | Image | Index digest |
|---|---|---|
| engine (build only) | `public.ecr.aws/docker/library/debian:bookworm-slim` | `sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171` |
| runner (build only) | `public.ecr.aws/docker/library/golang:1.26-bookworm` (Go 1.26.1) | `sha256:9fdc884aacc3bec89b20ffc69f4bb369c78210e3e4f600387b5128b12c199f81` |
| runtime | `gcr.io/distroless/cc-debian12:nonroot` | `sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f` |

The runtime index's `linux/amd64` manifest on the verification date was
`sha256:777e96cf322c46bc32aca926c263624c4dc8d7cf37e2fa65ba2c7e697318ebbb`.

## How to re-verify

Bases (no credentials needed; `docker manifest inspect <ref>` on a Docker host says the
same):

```sh
TOK=$(curl -s "https://public.ecr.aws/token/?scope=repository:docker/library/debian:pull" | jq -r .token)
curl -sI -H "Authorization: Bearer $TOK" \
  -H "Accept: application/vnd.oci.image.index.v1+json" \
  https://public.ecr.aws/v2/docker/library/debian/manifests/bookworm-slim | grep -i docker-content-digest
TOK=$(curl -s "https://public.ecr.aws/token/?scope=repository:docker/library/golang:pull" | jq -r .token)
curl -sI -H "Authorization: Bearer $TOK" \
  -H "Accept: application/vnd.oci.image.index.v1+json" \
  https://public.ecr.aws/v2/docker/library/golang/manifests/1.26-bookworm | grep -i docker-content-digest
TOK=$(curl -s "https://gcr.io/v2/token?scope=repository:distroless/cc-debian12:pull" | jq -r .token)
curl -sI -H "Authorization: Bearer $TOK" \
  -H "Accept: application/vnd.oci.image.index.v1+json" \
  https://gcr.io/v2/distroless/cc-debian12/manifests/nonroot | grep -i docker-content-digest
```

Engine and extensions, on any x86_64 Linux host:

```sh
curl -fsSLO https://github.com/duckdb/duckdb/releases/download/v1.5.5/duckdb_cli-linux-amd64.zip
sha256sum duckdb_cli-linux-amd64.zip
unzip duckdb_cli-linux-amd64.zip
./duckdb -batch -bail -no-init -c "SET extension_directory='$PWD/ext'; \
  INSTALL httpfs FROM 'https://extensions.duckdb.org'; \
  INSTALL avro FROM 'https://extensions.duckdb.org'; \
  INSTALL iceberg FROM 'https://extensions.duckdb.org'; \
  INSTALL json FROM 'https://extensions.duckdb.org';"
sha256sum ext/v1.5.5/linux_amd64/*.duckdb_extension
```

## Measured on the first build (2026-09-11, x86_64 QA host, Docker 29.8.0)

Flattened root filesystem (`docker export | wc -c`): **173,862,912 bytes (165.8 MB)**
against the 200 MB budget CI asserts. Compressed (`docker save | gzip -6`): about 60 MB.
`docker image inspect .Size` reported 239 MB on the same image: under the containerd
image store that figure adds the compressed blobs to the unpacked layers, which is why
the workflow measures the export and not the inspect.

## Bumping

A DuckDB bump is result-affecting (`bom.yaml`: it can change a query's answer) and so is
a new train family, never a serial. A base-image bump for a CVE is a serial. Either way:
change the value here and in the Dockerfile's `ARG` default in the same commit, record
the new `verified_on`, and let the build's own checks prove the values agree.
