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
docker run -d --name featuretable -p 8080:8080 -v featuretable-data:/data --restart unless-stopped featuretable
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
| GET    | `/healthz`       | – (also pings the DB; used by the Docker HEALTHCHECK) |

## Configuration

| Env var   | Default (in container)   |
|-----------|--------------------------|
| `PORT`    | `8080`                   |
| `DB_PATH` | `/data/featuretable.db`  |

Data is kept in the `featuretable-data` volume, so it survives container rebuilds.
`docker compose down -v` deletes it.

## Files

- `main.go`: HTTP server, routes, graceful shutdown, `-healthcheck` flag
- `store.go`: SQLite access for the `features` table
- `static/index.html`: the single-page UI
- `Dockerfile`: multi-stage build to a distroless, non-root image
- `compose.yaml`: port 8080, data volume, auto-restart
