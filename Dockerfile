# stream-analyzer with an ffmpeg that passes its startup self-test.
#
#   docker build -t stream-analyzer .                          # this host's architecture
#   docker build --platform linux/amd64 -t stream-analyzer .   # or linux/arm64
#
# The config is mounted at /config/channels.yaml and the data folder at
# /data; nothing private is built in. See "Run in a container" in the
# README.

# The build runs on the builder's own platform and cross-compiles, so a
# second architecture needs no emulation here. The Go version is go.mod's.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.1-trixie@sha256:8f58fd67ea075142d947a60e0caa4317746a55118d312f027793d382c7741734 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/stream-analyzer .

# Debian 13's ffmpeg is 7.1 with ffmpeg's own H.264 and HEVC decoders,
# which black detection needs (see "Run on Linux" in the README).
FROM docker.io/library/debian:13.7-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ffmpeg ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
# Stop the build if a Debian update ever brings an ffmpeg older than 7.0 or
# without its own H.264 and HEVC decoders; the monitor would refuse it.
RUN v="$(ffmpeg -hide_banner -version | sed -n '1s/^ffmpeg version n\{0,1\}\([0-9]*\)\..*/\1/p')" \
 && test "${v:-0}" -ge 7 \
 && ffmpeg -hide_banner -decoders | awk '$2 == "h264" {h = 1} $2 == "hevc" {e = 1} END {exit !(h && e)}'
RUN groupadd --gid 10001 stream-analyzer \
 && useradd --uid 10001 --gid 10001 --no-create-home \
      --home-dir /nonexistent --shell /usr/sbin/nologin stream-analyzer \
 && install -d -o 10001 -g 10001 /data \
 && install -d /config
COPY --from=build /out/stream-analyzer /usr/local/bin/stream-analyzer
USER 10001:10001
WORKDIR /data
ENTRYPOINT ["/usr/local/bin/stream-analyzer"]
CMD ["-config", "/config/channels.yaml", "-data", "/data"]
