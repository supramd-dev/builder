# syntax=docker/dockerfile:1
#
# md-builder: the Vite frontend and the Go server in one image. Build with
# Docker or Podman from the repository root:
#
#   docker build -t md-builder:local .
#   podman build -t md-builder:local .
#
# To stamp the source revision into the binary (it shows up next to the
# footer's health link), pass the same value the Makefile uses:
#
#   docker build --build-arg VERSION="$(git describe --always --dirty)" -t md-builder:local .
#
# The server is built with CGO_ENABLED=0 — SQLite is the pure-Go driver,
# git is go-git, SSH is x/crypto/ssh — so the runtime stage needs no build
# toolchain and nothing beyond the Alpine base. Object storage is mandatory,
# so the container must be given MD_BUILDER_S3_* values (docker-compose.yml
# does this for you); see md-builder-server.example.yaml for the file-based
# form.

ARG NODE_VERSION=24
ARG GO_VERSION=1.27
ARG ALPINE_VERSION=3.21

# --- frontend -------------------------------------------------------------
FROM node:${NODE_VERSION}-alpine AS frontend
WORKDIR /src/frontend
# Dependencies first: package-lock.json changes far less often than the
# sources, so the install layer survives most edits.
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# --- server ---------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS backend
# Point at a module mirror when the default one is unreachable (a build
# behind a firewall, an offline mirror):
#   docker build --build-arg GOPROXY=https://goproxy.cn,direct .
ARG GOPROXY=https://proxy.golang.org,direct
ARG VERSION=dev
ENV GOPROXY=${GOPROXY} \
    CGO_ENABLED=0 \
    GOOS=linux
WORKDIR /src/server
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/md-builder .

# --- runtime --------------------------------------------------------------
FROM alpine:${ALPINE_VERSION}
# ca-certificates: the server speaks HTTPS to the code repository and to a
# TLS object store. tzdata: the binary embeds a copy of the zone database
# (time/tzdata), this just keeps the image's own tools consistent with it.
# The account owns /data only — /app stays root-owned and read-only to it.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 10001 -h /app md-builder \
 && mkdir -p /data \
 && chown md-builder:md-builder /data

WORKDIR /app
COPY --from=backend /out/md-builder /app/md-builder
# resolveDistDir looks for frontend/dist relative to the working directory.
COPY --from=frontend /src/frontend/dist /app/frontend/dist

# The database is the only state that must survive the container, and it is
# not in a layer: MD_BUILDER_DSN points at the volume. Clones and YAML
# fetches go to a temp dir under /tmp.
ENV MD_BUILDER_DSN=/data/md-builder.db
# A named volume inherits this directory's ownership when it is created, so
# the server (uid 10001) can write it. A bind mount does not: point one at a
# host directory owned by the uid the container runs as — docker-compose.yml
# sets that from MD_BUILDER_UID, defaulting to 10001.
VOLUME /data

EXPOSE 8080
USER md-builder

# Liveness only: /api/health answers unauthenticated as long as the HTTP
# server is up. The dependency probes live behind /api/health/deep (they
# need a session) and would make the container flap whenever the object
# store is briefly slow. The port is read the same way the server reads it
# — MD_BUILDER_PORT wins, then the port inside MD_BUILDER_ADDR, else 8080 —
# so a container started with -port is still probed correctly. busybox
# provides wget.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD p="${MD_BUILDER_PORT:-}"; [ -n "$p" ] || p="${MD_BUILDER_ADDR##*:}"; case "$p" in ''|*[!0-9]*) p=8080;; esac; wget -q -O /dev/null "http://127.0.0.1:$p/api/health"

ENTRYPOINT ["/app/md-builder"]
