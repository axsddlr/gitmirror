# syntax=docker/dockerfile:1

# Base images are pinned by tag AND digest (the digest is the multi-arch index,
# so TARGETARCH builds still work). Verified on Docker Hub 2026-11-13:
#   golang:1.26.11-alpine3.24 -> sha256:f496f8c5...  (== golang:1.26.11-alpine)
#   alpine:3.24               -> sha256:0f49f331...
# Update both together (Dependabot/Renovate can bump tag+digest).
FROM --platform=$BUILDPLATFORM golang:1.26.11-alpine3.24@sha256:f496f8c537ed506626ee1a6913afb32c45068a48697163c088f604cc775a0c38 AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/gitmirror ./cmd/gitmirror

FROM alpine:3.24@sha256:0f49f331d9212aff11b932863be4eb01cdf962969aa86dab7d1ee21ac6f5f52b

RUN apk add --no-cache git git-lfs ca-certificates tini \
 && addgroup -g 10001 -S gitmirror \
 && adduser -u 10001 -S -G gitmirror -h /data -s /sbin/nologin gitmirror \
 && mkdir -p /data /config \
 && chown gitmirror:gitmirror /data

COPY --from=build /out/gitmirror /usr/local/bin/gitmirror

# HOME=/data: git and git-lfs write their config/cache under $HOME.
# The config file is expected at /config/config.yaml (mount it read-only).
# GITMIRROR_STATE_DIR overrides state_dir (the default ./data would be /data/data).
ENV HOME=/data \
    GITMIRROR_CONFIG=/config/config.yaml \
    GITMIRROR_STATE_DIR=/data

USER gitmirror:gitmirror
WORKDIR /data
VOLUME /data
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/sbin/tini", "--", "gitmirror"]
CMD ["serve"]
