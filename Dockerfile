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

FROM node:22.22.2-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
COPY sidecar/ /src/sidecar/
RUN npm run build

FROM golang:1.26.8-alpine AS go
RUN apk add --no-cache build-base sqlite-dev
WORKDIR /src
COPY --from=fastcas-sdk / /FastCAS/sdk/go/
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
RUN apk add --no-cache ca-certificates sqlite-libs su-exec tzdata
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
RUN addgroup -S fasttask && adduser -S -G fasttask fasttask && \
    mkdir -p /opt/fasttask/web/dist /var/lib/fasttask /var/backups/fasttask && \
    chown -R fasttask:fasttask /var/lib/fasttask /var/backups/fasttask
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
