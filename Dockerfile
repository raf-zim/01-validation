# ---- Build stage: compile a static Go binary ----
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY static ./static
# go mod tidy resolves the SQLite driver (modernc.org/sqlite, pure Go, no CGO)
# and writes go.sum inside the build, so no host Go toolchain is needed.
# go vet + go test run here: a failing test fails the image build.
# /out/db and /out/logs become the data directories, owned by the non-root user
# so the app can write there even when nothing is mounted over them.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod tidy \
 && go vet ./... \
 && go test -count=1 ./... \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server . \
 && mkdir -p /out/db /out/logs

# ---- Runtime stage: minimal image, runs as non-root ----
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/server /app/server
COPY --from=build --chown=nonroot:nonroot /out/db /db
COPY --from=build --chown=nonroot:nonroot /out/logs /logs

# Settings that differ from the app's built-in defaults. PORT (8080),
# SHUTDOWN_TIMEOUT (30s) and LOG_RETENTION_DAYS (4) use those defaults.
# DB_JOURNAL_MODE=DELETE because /db is normally a mounted host folder.
ENV DB_PATH=/db/featuretable.db \
    DB_JOURNAL_MODE=DELETE \
    LOG_DIR=/logs
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --start-interval=2s --retries=3 \
  CMD ["/app/server", "-healthcheck"]

# The Go app starts automatically whenever the container starts.
ENTRYPOINT ["/app/server"]
