# Feature Table

A single Go executable that serves a one-page web app for viewing, inserting,
editing, and deleting rows in a SQLite table with four columns: **ID, Date, Name, Feature**.

- The web page is compiled into the binary (`go:embed`), so there is one file to ship.
- SQLite runs through `modernc.org/sqlite`, a pure-Go driver. The binary is static (no CGO).
- The Docker image starts the server when the container starts (`ENTRYPOINT`), and
  `restart: unless-stopped` in compose brings the container back up when Docker starts.

## Run it

```powershell
powershell -ExecutionPolicy Bypass -File .\start.ps1
```
or by hand:
```powershell
docker compose up -d --build
start http://localhost:8080
```

Without compose:
```powershell
docker build -t featuretable .
docker run -d --name featuretable -p 8080:8080 -v ${PWD}/db:/db -v ${PWD}/logs:/logs -e TZ=Asia/Jerusalem --restart unless-stopped featuretable
```

Check it from outside the container:
```powershell
curl.exe -i http://localhost:8080/          # the web page
curl.exe http://localhost:8080/api/rows     # the table as JSON
```

## API

| Method | Path             | Body                                              |
|--------|------------------|---------------------------------------------------|
| GET    | `/api/rows`      | –                                                 |
| POST   | `/api/rows`      | `{"date":"2026-11-08","name":"…","feature":"…"}`  |
| PUT    | `/api/rows/{id}` | same as POST                                      |
| DELETE | `/api/rows/{id}` | –                                                 |
| GET    | `/healthz`       | – 204 when healthy, 503 during shutdown or if the DB is unreachable |

## Logging, tracing, health check, shutdown

These follow the patterns in [kelche.co: Building an HTTP server in Go](https://www.kelche.co/blog/go/http-server/), using only the standard library.

- **Logging**: `log.New(os.Stdout, "http: ", log.LstdFlags)`. A logging middleware writes one line per request after the handler finishes:
  `http: 2026/11/09 15:00:00 id=3f9c0a1b2c3d4e5f GET /api/rows status=200 bytes=152 dur=412µs remote=172.18.0.1:53210 ua="Mozilla/5.0 …"`
  View it with `docker logs -f featuretable`.
- **Tracing**: a tracing middleware reads `X-Request-Id` from the request. If it is missing or unsafe, it generates a random 16-hex ID. The ID goes into the request context, appears on every log line, and is returned in the `X-Request-Id` response header.
- **Health check**: an atomic `healthy` flag is set to 1 once the server starts and to 0 as soon as shutdown begins. `/healthz` returns 204 or 503. The Docker `HEALTHCHECK` runs the binary with `-healthcheck`, because the distroless image has no curl.
- **Graceful shutdown**: on SIGINT or SIGTERM (`docker stop`), the app marks itself unhealthy, disables keep-alives, and calls `server.Shutdown` with a 30s deadline. That lets in-flight requests finish before the database closes. Server timeouts: read 5s, read-header 5s, write 10s, idle 15s. Compose sets `stop_grace_period: 35s`, so Docker doesn't kill the app partway through.

## Log files and rotation

Besides stdout (`docker logs`), the server writes its log to a file per day, which appears in `.\logs` on the host (bind-mounted at `/logs`):

```
logs\http-2026-11-09.log
logs\http-2026-11-10.log
...
```

- **Rotation**: the first log line after midnight opens the new day's file. Midnight is in the container's `TZ`, which compose sets to `Asia/Jerusalem`. Docker's health check logs a request every 30 seconds, so the switch happens within a minute of midnight.
- **Retention**: when the server starts and at every rotation, it deletes `http-YYYY-MM-DD.log` files dated more than `LOG_RETENTION_DAYS` days ago (default 4). On the 9th, the files for the 5th through the 9th are kept and the 4th and earlier are deleted. Other files in the folder are never touched.
- Each rotation writes a `logrotate:` line at the top of the new file, naming any files it deleted.
- If `LOG_DIR` is unset, the server logs to stdout only.

## Tests

`main_test.go` covers the tracing, logging, health, probe, timeout and graceful-shutdown behaviour, plus CRUD through the full handler stack against a temporary SQLite database. The Docker build runs `go vet` and `go test`, so the image is not built if a test fails.

`deploy.ps1` stops the running container, rebuilds the image, starts the new container and runs `verify.ps1`. Those checks run from the host and include stopping the container to confirm a clean shutdown. Results are written to `deploy.log` and `verify.log`.

## Configuration

| Env var   | Default (in container)   |
|-----------|--------------------------|
| `PORT`    | `8080`                   |
| `DB_PATH` | `/db/featuretable.db`    |
| `DB_JOURNAL_MODE` | `DELETE` (`WAL` or `TRUNCATE` also accepted; the app's default outside Docker is `WAL`) |
| `SHUTDOWN_TIMEOUT` | `30s`           |
| `LOG_DIR` | `/logs` (unset = stdout only) |
| `LOG_RETENTION_DAYS` | `4`           |
| `TZ` | `Asia/Jerusalem` (set in compose; decides when midnight is) |

The SQLite database is the file `.\db\featuretable.db` in the project folder, mapped into the container at `/db`. It survives container restarts, rebuilds and `docker compose down -v`. To back it up, stop the container (`docker compose stop`) and copy the file. Because the folder is mapped from Windows, SQLite runs with `DB_JOURNAL_MODE=DELETE`: WAL mode depends on shared memory, which isn't reliable on a Windows folder mapped into a Linux container.

## Files

- `main.go`: HTTP server, routes, middleware (tracing, logging), health check, graceful shutdown, `-healthcheck` flag
- `rotate.go`: daily log file writer with retention purge
- `main_test.go`, `rotate_test.go`: unit and integration tests (run during the Docker build)
- `store.go`: SQLite access for the `features` table
- `static/index.html`: the single-page UI
- `Dockerfile`: multi-stage build to a distroless, non-root image
- `compose.yaml`: port 8080, `.\db` and `.\logs` folders, time zone, auto-restart
