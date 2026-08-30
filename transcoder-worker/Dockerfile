# syntax=docker/dockerfile:1
# =====================================================
# transcoder-worker - multi-stage build
#
# Build stage: golang:1.25.5-alpine - matches the Go
# version pinned in go.mod. The repo's single Go module
# is rooted at the repo root, so the build context is
# "." (set in docker-compose.yml) and we build the
# ./transcoder-worker subpackage.
#
# Runtime stage: ubuntu:24.04 with ffmpeg + ffprobe
# from the official Ubuntu archive (FFmpeg 6.x). ubuntu
# was chosen over Alpine because:
#   - FFmpeg from jrottenberg on Alpine ships static
#     binaries with fewer native deps but no glibc, and
#     any future CGO dependency in the worker would
#     clash with musl.
#   - Ubuntu LTS is what production will likely run, so
#     dev / prod parity is higher.
#
# Hardening (SEC-02):
#   - Non-root user UID 10001.
#   - /tmp is tmpfs mounted by docker-compose (512M,
#     mode 1777, owned by 10001) - the worker uses
#     /tmp/transcode/<videoID>-<random> for raw download
#     + intermediate HLS segments.
#   - cap_drop ALL + no-new-privileges. pids_limit
#     enforced at compose level (100).
#   - read_only root filesystem (compose level). The
#     only writable surface is /tmp.
# =====================================================

# ---------------- Build stage ----------------
FROM golang:1.25.5-alpine AS build

WORKDIR /src

# Copy module manifests first so layer cache survives
# source-only changes.
COPY go.mod go.sum ./
RUN go mod download

# Now copy the source. The build context is the repo
# root, so this single COPY carries the whole module.
COPY . .

# Build the worker binary. CGO is disabled so the
# binary is statically linked and runs against glibc
# from the Ubuntu runtime without surprises.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" \
    -o /out/transcoder-worker \
    ./transcoder-worker

# ---------------- Runtime stage ----------------
FROM ubuntu:24.04

# ffmpeg from Ubuntu archive ships the libav* libraries
# the worker needs (libx264, libx265, libvpx, etc.) so
# transcode to h264/hevc/vp9 just works without us
# rebuilding FFmpeg. ca-certificates is included so the
# binary can reach the R2 endpoint over HTTPS without
# cert errors.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ffmpeg \
        ca-certificates \
        tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && ln -sf /usr/share/zoneinfo/Etc/UTC /etc/localtime

# Non-root user matching the docker-compose directive
# (UID 10001). Ubuntu's useradd creates the user with
# no password and a disabled login.
RUN useradd -r -u 10001 -g users app \
    && mkdir -p /app \
    && chown app:users /app

# Copy the statically-linked binary from the build stage.
COPY --from=build /out/transcoder-worker /usr/local/bin/transcoder-worker
# Ensure it's executable for the runtime user.
RUN chmod 0555 /usr/local/bin/transcoder-worker

USER app
WORKDIR /app

# No HEALTHCHECK - the worker doesn't expose an HTTP
# port, so a TCP probe is meaningless. Container-level
# health is monitored by Docker / the orchestrator via
# process liveness.
#
# No EXPOSE - worker has no network surface.
ENTRYPOINT ["/usr/local/bin/transcoder-worker"]