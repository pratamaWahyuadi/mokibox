# syntax=docker/dockerfile:1
# =====================================================
# api-gateway - placeholder Dockerfile
# The real multi-stage build (Go + Alpine) is wired in
# Phase 9 (Fase 9). This stub exists so `docker compose
# up` in Phase 0 can resolve the service even though the
# Go binary has not been written yet.
# =====================================================
FROM alpine:3.19
RUN adduser -D -u 10001 app
USER app
WORKDIR /app
# Real image will copy the compiled binary and run it on :8080.
CMD ["sh", "-c", "echo 'api-gateway binary not yet built (Phase 9)' && sleep 3600"]
