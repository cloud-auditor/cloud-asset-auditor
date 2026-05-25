# syntax=docker/dockerfile:1.7
#
# Multi-stage build for cloud-asset-auditor.
#
# Stage 1 (`build`)  - compiles a static, stripped binary with version
#                      metadata injected via -ldflags.
# Stage 2 (`runtime`) - distroless static image, runs as the non-root user
#                      that distroless ships with.
#
# Build args (mirror just docker / CI):
#   VERSION = git describe or semver tag                    (default "dev")
#   COMMIT  = git short SHA                                 (default "none")
#   DATE    = RFC3339 build timestamp                       (default "unknown")
#
# Image runs both modes (`audit` and `serve`) from the same binary; the
# default CMD prints help so `docker run <image>` is harmless.

ARG GO_VERSION=1.26

###############################################################################
# build
###############################################################################
FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src

# Module-download layer cached separately from source — re-runs only when
# go.mod / go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Source.
COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
ARG TARGETOS=linux
ARG TARGETARCH=amd64

ENV CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH}

RUN go build -trimpath \
    -ldflags="-s -w \
        -X github.com/cloud-auditor/cloud-asset-auditor/internal/version.Version=${VERSION} \
        -X github.com/cloud-auditor/cloud-asset-auditor/internal/version.Commit=${COMMIT}  \
        -X github.com/cloud-auditor/cloud-asset-auditor/internal/version.Date=${DATE}" \
    -o /out/auditor ./cmd/auditor

###############################################################################
# runtime
###############################################################################
# distroless/static: glibc-less, no shell, no package manager, ~2 MB base.
# :nonroot variant ships UID/GID 65532 by default — host volume mounts must
# be readable by that UID or the binary cannot read config/kubeconfig.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/auditor /auditor

# Web UI default port (`auditor serve --addr :8080`). No effect for `audit`.
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/auditor"]
CMD ["audit", "--help"]
