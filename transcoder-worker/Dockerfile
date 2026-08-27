# syntax=docker/dockerfile:1
# =====================================================
# transcoder-worker - placeholder Dockerfile
# The real multi-stage build (Go + FFmpeg) with the
# hardened runtime (non-root, read-only FS, cap_drop ALL,
# no-new-privileges, tmpfs /tmp, pids_limit) is wired in
# Phase 5. This stub exists so `docker compose up` in
# Phase 0 can resolve the service.
# =====================================================
FROM alpine:3.19
RUN adduser -D -u 10001 worker
USER worker
WORKDIR /app
# Real image will run the Go binary that consumes Asynq
# jobs from Redis. No HTTP port is ever exposed.
CMD ["sh", "-c", "echo 'transcoder-worker binary not yet built (Phase 5)' && sleep 3600"]
