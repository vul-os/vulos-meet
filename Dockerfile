# Dockerfile — Vulos Meet deploy image (vulos-meet wrapper + pinned livekit-server)
#
# vulos-meet supervises a `livekit-server` child process, so the deploy image
# MUST contain BOTH binaries (see fly.toml [build] + CONTRIBUTING-FORK.md §1).
# The pinned livekit-server version lives in the LIVEKIT_VERSION arg below and
# must track the fork's supported matrix (CONTRIBUTING-FORK.md §1 / the v1.7.x
# "Current" row). Bump it there and here together.
#
# Build:   docker build -t vulos-meet .
# Run:     vulos-meet --config /etc/vulos-meet/config.yaml  (rendered by entrypoint)
# Fly:     referenced by fly.toml [build].dockerfile; deploys per-region.

# ── Stage 1: build the vulos-meet wrapper (pure-Go, static) ───────────────────
FROM golang:1.26-bookworm AS build
WORKDIR /src
# Cache deps first.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO disabled — vulos-meet is pure-Go (no sqlite/CGO here); produces a static
# binary that runs on the slim runtime image.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/vulos-meet ./cmd/vulos-meet

# ── Stage 2: fetch the pinned livekit-server release binary ───────────────────
FROM debian:bookworm-slim AS livekit
ARG LIVEKIT_VERSION=v1.7.2
ARG TARGETARCH=amd64
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl tar \
 && rm -rf /var/lib/apt/lists/*
# Release asset layout: livekit_<X.Y.Z>_linux_<arch>.tar.gz (strip the leading "v").
RUN set -eux; \
    ver="${LIVEKIT_VERSION#v}"; \
    url="https://github.com/livekit/livekit-server/releases/download/${LIVEKIT_VERSION}/livekit_${ver}_linux_${TARGETARCH}.tar.gz"; \
    curl -fsSL "$url" -o /tmp/livekit.tar.gz; \
    tar -xzf /tmp/livekit.tar.gz -C /usr/local/bin livekit-server; \
    chmod +x /usr/local/bin/livekit-server; \
    /usr/local/bin/livekit-server --version

# ── Stage 3: runtime — both binaries + entrypoint ─────────────────────────────
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --create-home --uid 10001 vulos
COPY --from=build    /out/vulos-meet           /usr/local/bin/vulos-meet
COPY --from=livekit  /usr/local/bin/livekit-server /usr/local/bin/livekit-server
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh \
 && mkdir -p /etc/vulos-meet /var/lib/vulos-meet \
 && chown -R vulos:vulos /etc/vulos-meet /var/lib/vulos-meet

USER vulos
ENV VULOS_MEET_LIVEKIT_BIN=/usr/local/bin/livekit-server

# Public signal-gate (HTTP/WS). Admin :7881 + metrics :7882 stay internal (not
# EXPOSEd as Fly services — see fly.toml). RTC media is UDP, served by
# livekit-server.
#
# Two transport shapes (pick ONE; keep consistent with config.yaml + fly.toml):
#   (a) NARROW RANGE (default): rtc_port_range_start/end 50000-50200.
#   (b) SINGLE UDP MUX PORT (500-participant tier): livekit.rtc_udp_port: 50000
#       — then EXPOSE the single port "50000/udp" instead of the range.
# EXPOSE is documentation-only for Fly (the [[services]] block in fly.toml is
# what actually opens the port), but we keep it accurate for plain `docker run`.
EXPOSE 7883
EXPOSE 50000-50200/udp
# Shape (b): replace the range line above with the single muxed port:
# EXPOSE 50000/udp

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
