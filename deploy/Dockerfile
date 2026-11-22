# Multi-target build:
#   docker build -f deploy/Dockerfile --target server -t unseen-backup/server .
#   docker build -f deploy/Dockerfile --target agent  -t unseen-backup/agent .
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN set -e; \
    LDFLAGS="-s -w -X centralbackup/internal/agent.Version=${VERSION}"; \
    CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o /out/backup-server ./cmd/server; \
    CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags "$LDFLAGS" -o /out/agents/backup-agent-linux-amd64 ./cmd/agent; \
    CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -ldflags "$LDFLAGS" -o /out/agents/backup-agent-linux-arm64 ./cmd/agent; \
    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$LDFLAGS" -o /out/agents/backup-agent-windows-amd64.exe ./cmd/agent; \
    CGO_ENABLED=0 GOOS=windows GOARCH=386   go build -ldflags "$LDFLAGS" -o /out/agents/backup-agent-windows-386.exe ./cmd/agent

# Legacy 32-bit agent for Windows 7 SP1 / Embedded POSReady 7 (NT 6.1), which
# the standard Go 1.25 binaries cannot run on. It is built out-of-band with
# the Go 1.20 toolchain (scripts/build-legacy-agent.sh) and committed under
# deploy/prebuilt, so the image build just copies it in — no toolchain
# download at image-build time, which is unreliable in many CI/build
# environments. Regenerate and re-commit it when the agent code changes.
COPY deploy/prebuilt/backup-agent-windows-386-legacy.exe /out/agents/backup-agent-windows-386-legacy.exe

# ---- Kopia (backup engine) ----
# Built from source through the Go module proxy, so every dependency is
# verified against Go's checksum database. The server uses the native binary;
# agents download theirs from the server (/api/agent/kopia/binary), which
# checks the SHA-256. Set KOPIA_VERSION= (empty) to skip it entirely.
FROM golang:1.25-alpine AS kopia
ARG KOPIA_VERSION=v0.23.1
RUN set -e; mkdir -p /out/agents /out/bin; \
    if [ -n "$KOPIA_VERSION" ]; then \
      CGO_ENABLED=0 GOBIN=/out/bin go install github.com/kopia/kopia@${KOPIA_VERSION}; \
      for t in linux/amd64 linux/arm64 windows/amd64; do \
        os=${t%/*}; arch=${t#*/}; ext=""; [ "$os" = windows ] && ext=".exe"; \
        CGO_ENABLED=0 GOOS=$os GOARCH=$arch go install github.com/kopia/kopia@${KOPIA_VERSION}; \
        src="$(go env GOPATH)/bin/${os}_${arch}/kopia$ext"; \
        [ -f "$src" ] || src="$(go env GOPATH)/bin/kopia$ext"; \
        cp "$src" "/out/agents/kopia-$os-$arch$ext"; \
      done; \
    fi

# ---- central server image ----
FROM alpine:3.21 AS server
# rclone: OneDrive / Google Drive as the Kopia off-site copy (docs/kopia.md).
RUN apk add --no-cache ca-certificates tzdata rclone
WORKDIR /app
COPY --from=build /out/backup-server /app/backup-server
# Prebuilt agent binaries, served at /dl/ for the client install scripts.
COPY --from=build /out/agents /app/agents
COPY --from=kopia /out/agents/ /app/agents/
COPY --from=kopia /out/bin/ /usr/local/bin/
ENV CB_DATA_DIR=/data \
    CB_LISTEN=:8443 \
    CB_AGENT_BIN_DIR=/app/agents
VOLUME /data
# 8443: agents (and the GUI unless CB_ADMIN_LISTEN is set); 8444: the
# optional separate admin listener; 51515: the Kopia repository server
# (only when CB_KOPIA_PASSWORD is set).
EXPOSE 8443 8444 51515
ENTRYPOINT ["/app/backup-server"]

# ---- containerized agent image (for Docker hosts) ----
FROM alpine:3.21 AS agent
RUN apk add --no-cache ca-certificates tzdata docker-cli
COPY --from=build /out/agents/backup-agent-linux-amd64 /usr/local/bin/backup-agent
COPY deploy/agent-entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
VOLUME /var/lib/backup-agent
ENTRYPOINT ["/entrypoint.sh"]
