# AOE binaries plus a pinned Claude Code CLI (docs/04 §7.1).
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/ ./cmd/...

FROM node:22-bookworm-slim
ARG CLAUDE_CODE_VERSION=2.1.306
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
 && npm cache clean --force \
 && useradd --create-home --uid 10001 aoe \
 && mkdir -p /app/data && chown aoe /app/data
COPY --from=build /out/ /usr/local/bin/
COPY config /app/config
WORKDIR /app
USER aoe
ENV AOE_CONFIG=/app/config/aoe.yaml
CMD ["worker"]
