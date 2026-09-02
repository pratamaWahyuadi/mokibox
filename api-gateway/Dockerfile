# syntax=docker/dockerfile:1
# =====================================================
# api-gateway - production multi-stage build
#
# Build context for this Dockerfile MUST be the repo
# root (NOT ./api-gateway) because go.mod / go.sum
# live at the root and the binary imports shared/.
# See docker-compose.yml `build.context: .` and the
# pitfall section in mokibox-go-shared SKILL.md
# "Docker build context untuk multi-package Go module".
#
# Stage 1 (builder): golang:1.25.5-alpine to compile
# a static binary. CGO_ENABLED=0 so the result has no
# libc dependency, allowing the distroless runtime image
# below to run it without a shell or package manager.
#
# Stage 2 (runtime): gcr.io/distroless/static-debian12:nonroot.
# Static binaries only (no glibc, no dlopen). The
# :nonroot tag ships a non-root user with UID 65532
# so the container does NOT run as root - matches the
# hardening posture documented for the worker in
# docker-compose.yml (read_only root FS, cap_drop ALL,
# no-new-privileges, etc.).
#
# EXPOSE 8080 - the default listen address for the
# api-gateway. API_GATEWAY_ADDR env var can override
# it but the docker-compose service does not need
# to remap.
#
# ENTRYPOINT runs the binary directly; there is no
# shell, so SIGTERM goes straight to the api-gateway
# process and the 30s graceful shutdown wired in
# main.go fires correctly.
# =====================================================

# ---- Stage 1: builder ----
FROM golang:1.25.5-alpine AS builder

WORKDIR /src

# Copy module files first so dependency resolution
# is cached when only the source changes. go.mod
# + go.sum must come from the build context (repo
# root), NOT from ./api-gateway.
COPY go.mod go.sum ./
RUN go mod download

# Copy the source tree. Note: the api-gateway
# subdirectory is the package being built; shared/
# is imported via the module path.
COPY . .

# Build the api-gateway binary. CGO disabled so the
# binary is fully static; -ldflags="-s -w" strips
# debug info for a smaller image. -trimpath removes
# absolute paths from the binary for reproducibility.
RUN CGO_ENABLED=0 GOOS=linux \
    go build \
        -ldflags="-s -w" \
        -trimpath \
        -o /out/api-gateway \
        ./api-gateway

# ---- Stage 2: runtime ----
FROM gcr.io/distroless/static-debian12:nonroot

# Static binary, copied once. The :nonroot tag
# provides the non-root user; COPY --chown keeps
# the file readable by that user (uid 65532).
COPY --from=builder --chown=nonroot:nonroot /out/api-gateway /usr/local/bin/api-gateway

# The default user from :nonroot is already
# 65532:65532 (nonroot:nonroot). Documenting here
# so a future reader does not assume the container
# is running as root.
USER nonroot:nonroot

# Listen address is set via API_GATEWAY_ADDR env
# var (default :8080). The compose service does not
# publish this port to the host - Nginx is the only
# public entry point.
EXPOSE 8080

# ENTRYPOINT (not CMD) so the binary is PID 1 and
# receives SIGTERM directly when docker compose
# stops the service. With a shell wrapper the
# signal would go to the shell, which would not
# forward it cleanly.
ENTRYPOINT ["/usr/local/bin/api-gateway"]