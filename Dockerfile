# Multi-stage build for FastTask production deployment (doc/tech.md §21.5).
#
# Three stages, one deployment artifact: the web bundle is built by Node, linked into the Go binary's
# served directory, and only the binary plus that bundle reach the runtime image. The build context is
# filtered by .dockerignore so a stale host `web/dist`, a development database or a `.env` cannot be baked
# into a layer.
#
# This image contains no Node, therefore it cannot host the optional sidecar: agent runs use the in-browser
# WASM host (doc/harness.md §16, ADR-0005). libfx's native addons are not built for musl, so "just add
# node" is not the answer; a sidecar deployment uses the systemd units in deployments/systemd/ instead.

# The Alpine package mirror is an argument for the same reason the Go module proxy is: the default
# CDN is slow enough from some networks that a build looks hung rather than downloading. Measured on
# the deployment host: 19s vs 1s for one APKINDEX. Overridable, never hardcoded to one country.
ARG APK_MIRROR=dl-cdn.alpinelinux.org

FROM node:22.22.2-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
COPY sidecar/ /src/sidecar/
RUN npm run build

# go.mod replaces github.com/FastR-D/FastCAS/sdk/go with ../FastCAS/sdk/go — a sibling checkout that
# exists on a developer's machine and not in a build context, so `COPY . .` can never bring it in. This
# stage fetches it and puts it exactly where that relative path resolves to from /src.
#
# It downloads the pinned tarball rather than cloning: one small HTTPS GET with retries instead of a git
# negotiation, which is what a build network is most likely to interrupt. The ref is pinned because a
# floating branch would let an upstream push change what "this commit of FastTask" builds into.
#
# A build that must not reach the network overrides the stage instead of editing it, with a directory
# whose root holds FastCAS/sdk/go: `docker build --build-context fastcas-sdk=/path/to/parent .`
FROM alpine:3.22 AS fastcas-sdk
ARG FASTCAS_REF=676d49b18f700c009f1f491280a93de7da294836
ARG FASTCAS_TARBALL=https://codeload.github.com/FastR-D/FastCAS/tar.gz/${FASTCAS_REF}
RUN set -eu; \
    for attempt in 1 2 3; do \
        wget -qO /sdk.tar.gz "$FASTCAS_TARBALL" && break; \
        echo "fastcas sdk download failed (attempt $attempt of 3)" >&2; \
        sleep 5; \
    done; \
    mkdir -p /FastCAS; \
    tar -xzf /sdk.tar.gz -C /FastCAS --strip-components=1; \
    test -f /FastCAS/sdk/go/go.mod

FROM golang:1.26.8-alpine AS go
# The module proxy is an argument because the default one is not reachable from every network this
# is built on; a build that cannot reach it fails at `go mod download` with a dial timeout that
# looks like a dependency problem and is not. Override it, do not edit the image.
ARG GOPROXY=https://proxy.golang.org,direct
ARG APK_MIRROR
ENV GOPROXY=$GOPROXY
RUN sed -i "s|dl-cdn.alpinelinux.org|${APK_MIRROR}|g" /etc/apk/repositories \
 && apk add --no-cache build-base sqlite-dev
WORKDIR /src
# The module, not the whole upstream repository: go.mod's replace target has to be a directory whose
# go.mod declares github.com/FastR-D/FastCAS/sdk/go.
COPY --from=fastcas-sdk /FastCAS/sdk/go/ /FastCAS/sdk/go/
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ENV CGO_ENABLED=1
RUN go build -trimpath -ldflags="-s -w" -o /out/fasttask ./cmd/fasttask

FROM alpine:3.22
# tzdata is not optional: the server loads Asia/Shanghai for daily planning and `doctor` fails without it
# (internal/bootstrap/commands.go), and alpine ships no zoneinfo by default. su-exec is what lets the
# entrypoint start as root to repair a bind mount's ownership and then drop to `fasttask`.
ARG APK_MIRROR
RUN sed -i "s|dl-cdn.alpinelinux.org|${APK_MIRROR}|g" /etc/apk/repositories \
 && apk add --no-cache ca-certificates sqlite-libs su-exec tzdata
ENV FASTTASK_LISTEN=0.0.0.0 \
    FASTTASK_PORT=10000 \
    FASTTASK_WEB_DIST=/opt/fasttask/web/dist \
    FASTTASK_AUDIO_DIR=/var/lib/fasttask/audio \
    FASTTASK_ATTACHMENT_DIR=/var/lib/fasttask/attachments \
    FASTTASK_DATABASE=/var/lib/fasttask/fasttask.db \
    FASTTASK_ENV=production
# Relative defaults (`data/...`) would resolve against the image's working directory, so every writable
# path is pinned above and the working directory is the state directory the volumes are mounted on.
WORKDIR /var/lib/fasttask
# No /var/backups: the working directory is the state directory, so `fasttask backup`'s relative
# `backups/` default lands inside the bind mount, next to the database it snapshots and already
# owned by the right user. A second, unused backups directory in the image would only suggest
# otherwise.
RUN addgroup -S fasttask && adduser -S -G fasttask fasttask && \
    mkdir -p /opt/fasttask/web/dist /var/lib/fasttask && \
    chown -R fasttask:fasttask /var/lib/fasttask
COPY --from=go /out/fasttask /usr/local/bin/fasttask
COPY --from=web /src/web/dist /opt/fasttask/web/dist
COPY deployments/docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/entrypoint.sh
EXPOSE 10000
# Runs as the unprivileged user by default; `docker run --user 0` is for repairing a bind mount's ownership
# only, and the entrypoint still drops to `fasttask` before starting the server.
USER fasttask
# Readiness, not liveness: a 503 here means migrations are pending or a dependency check failed, and
# restarting the container would not fix either. The start period covers the first migration run.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${FASTTASK_PORT}/health/ready" >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/entrypoint.sh", "/usr/local/bin/fasttask"]
CMD ["serve", "--with-worker", "--with-scheduler"]
