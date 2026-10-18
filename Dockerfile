# syntax=docker/dockerfile:1
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/core/ ./internal/core/
COPY internal/sandbox/supervisor/ ./internal/sandbox/supervisor/
COPY internal/agent/codexrunner/ ./internal/agent/codexrunner/
COPY cmd/sandboxd/ ./cmd/sandboxd/
COPY cmd/codex-runner/ ./cmd/codex-runner/
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /out/sandboxd ./cmd/sandboxd \
    && CGO_ENABLED=0 go build -trimpath -o /out/codex-runner ./cmd/codex-runner

FROM python:3.13-slim-bookworm@sha256:a1165e272e578941b84abc79e4ab38a0305cd12803a5c4247979ac7655f4d641 AS environment
RUN groupadd --gid 1000 agent \
    && useradd --uid 1000 --gid 1000 --create-home agent \
    && mkdir -p /workspace /var/lib/makez \
    && chown agent:agent /workspace \
    && chmod 700 /var/lib/makez
WORKDIR /workspace
EXPOSE 8099
ENTRYPOINT ["/usr/local/bin/sandboxd"]

FROM environment AS sandbox
COPY --from=build /out/sandboxd /usr/local/bin/sandboxd

FROM environment AS codex
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git nodejs npm \
    && npm install --global @openai/codex@0.163.0 \
    && npm cache clean --force \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/codex-runner /usr/local/bin/codex-runner
COPY --from=build /out/sandboxd /usr/local/bin/sandboxd
