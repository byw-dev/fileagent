# syntax=docker/dockerfile:1
#
# Multi-stage build for the single-binary Control Plane (Web UI embedded).
#
# Build context MUST be the repository root (the Go workspace spans several
# modules: the root module provides api/v1, plus ./agent and ./pkg/trollsift via
# go.work). Build from the repo root:
#
#   docker build -f controlplane/Dockerfile -t fileagent/controlplane .
#
# The produced image is fully self-contained: DB migrations are embedded in the
# binary (D-023) and the Web UI is embedded via the `webui` build tag (D-022),
# so no external migrations/ directory or static site needs to be shipped.

# ── Stage 1: build the Web UI (webui/dist) ──────────────────────────────────
FROM node:24-alpine AS webui-builder
WORKDIR /webui
# corepack ships with Node 24 and pins pnpm from webui/package.json packageManager.
RUN corepack enable
# Install deps first (better layer caching) then build.
COPY webui/package.json webui/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY webui/ ./
RUN pnpm build

# ── Stage 2: build the Go binary with the Web UI embedded ───────────────────
FROM golang:1.24 AS go-builder
WORKDIR /src
# Copy the whole workspace (see .dockerignore for exclusions). The build needs
# the root module (api/v1), controlplane, agent and pkg/trollsift for go.work.
COPY . .
# Drop in the freshly built Web UI so //go:embed all:dist has something to embed.
COPY --from=webui-builder /webui/dist ./controlplane/internal/webui/dist
# CGO is not required (Postgres via lib/pq, everything else pure Go) → a fully
# static binary that runs on a minimal runtime image.
RUN cd controlplane && \
    CGO_ENABLED=0 GOFLAGS=-buildvcs=false \
    go build -tags webui -ldflags "-s -w" -o /out/controlplane ./cmd/server

# ── Stage 3: minimal runtime ────────────────────────────────────────────────
FROM alpine:3.20
# ca-certificates for outbound TLS (MinIO over SSL, webhook targets, OIDC).
RUN apk add --no-cache ca-certificates wget && \
    adduser -D -u 10001 fileagent && \
    mkdir -p /data && chown fileagent:fileagent /data
COPY --from=go-builder /out/controlplane /usr/local/bin/controlplane
USER fileagent
# /data holds the bootstrap admin credentials file (mount a volume here).
WORKDIR /data
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/controlplane"]
