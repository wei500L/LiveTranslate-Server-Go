# LiveTranslate Server (Go) — single-binary build.
#
# The binary carries the API (serve), the admin UI (admin), migrations
# (migrate) and the admin CLI (create-admin / enable-totp); one image,
# different commands (see compose.yml).

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/livetranslate-server ./cmd/livetranslate-server

FROM alpine:3.20
RUN adduser -D -u 900 appuser
COPY --from=build /out/livetranslate-server /usr/local/bin/livetranslate-server
USER appuser
EXPOSE 8000 8081

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8000/health || exit 1

# serve applies migrations at boot (idempotent), then listens.
CMD ["livetranslate-server", "serve"]
