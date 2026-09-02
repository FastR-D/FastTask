# Multi-stage build for FastTask production deployment.
FROM node:20-alpine AS web
WORKDIR /src
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web ./
RUN npm run build

FROM golang:1.24-alpine AS go
RUN apk add --no-cache build-base sqlite-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ENV CGO_ENABLED=1
RUN go build -trimpath -ldflags="-s -w" -o /out/fasttask ./cmd/fasttask

FROM alpine:3.19
RUN apk add --no-cache ca-certificates sqlite-libs su-exec
ENV FASTTASK_LISTEN=0.0.0.0     FASTTASK_PORT=10000     FASTTASK_WEB_DIST=/opt/fasttask/web/dist     FASTTASK_AUDIO_DIR=/var/lib/fasttask/audio     FASTTASK_DATABASE=/var/lib/fasttask/fasttask.db     FASTTASK_ENV=production
RUN addgroup -S fasttask && adduser -S -G fasttask fasttask &&     mkdir -p /opt/fasttask/web/dist /var/lib/fasttask /var/backups/fasttask &&     chown -R fasttask:fasttask /var/lib/fasttask /var/backups/fasttask
COPY --from=go /out/fasttask /usr/local/bin/fasttask
COPY --from=web /src/web/dist /opt/fasttask/web/dist
COPY deployments/docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
EXPOSE 10000
USER fasttask
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["serve", "--with-worker", "--with-scheduler"]

