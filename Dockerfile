# syntax=docker/dockerfile:1

# ---- Build stage: compile a static Go binary ----
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY static ./static
# go mod tidy resolves the SQLite driver (modernc.org/sqlite, pure Go, no CGO)
# and writes go.sum inside the build, so no host Go toolchain is needed.
# go vet + go test run here: a failing test fails the image build.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod tidy \
 && go vet ./... \
 && go test -count=1 ./... \
 && CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/server . \
 && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/featuretable.exe . \
 && cp go.mod go.sum /out/ \
 && mkdir -p /out/data /out/logs

# ---- Export stage: copies the executables out to the host ----
#   docker build --target export --output type=local,dest=bin .
FROM scratch AS export
COPY --from=build /out/featuretable.exe /featuretable.exe
COPY --from=build /out/server /featuretable-linux
COPY --from=build /out/go.mod /out/go.sum /

# ---- Runtime stage: minimal image, runs as non-root ----
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/server /app/server
COPY --from=build --chown=nonroot:nonroot /out/data /data
COPY --from=build --chown=nonroot:nonroot /out/logs /logs

ENV PORT=8080 \
    DB_PATH=/data/featuretable.db \
    SHUTDOWN_TIMEOUT=30s \
    LOG_DIR=/logs \
    LOG_RETENTION_DAYS=4
EXPOSE 8080
VOLUME ["/data", "/logs"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --start-interval=2s --retries=3 \
  CMD ["/app/server", "-healthcheck"]

# docker stop sends SIGTERM; the app drains in-flight requests before exiting.
STOPSIGNAL SIGTERM

# The Go app starts automatically whenever the container starts.
ENTRYPOINT ["/app/server"]
