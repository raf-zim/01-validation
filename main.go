package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	_ "time/tzdata" // zone database baked in: the distroless image has none, and TZ decides when logs rotate
)

//go:embed static
var staticFiles embed.FS

// Context key for the request ID (unexported type so no other package can collide).
type key int

const requestIDKey key = 0

// healthy is 1 while the server accepts traffic and 0 once shutdown has begun,
// so /healthz starts failing before connections are drained.
var healthy int32

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the running server and exit 0 if healthy")
	flag.Parse()

	port := env("PORT", "8080")

	// Used by the Docker HEALTHCHECK: the runtime image has no shell or curl.
	if *healthcheck {
		os.Exit(probe("http://127.0.0.1:" + port + "/healthz"))
	}

	// Log to stdout (docker logs) and, if LOG_DIR is set, to a daily file
	// <LOG_DIR>/http-YYYY-MM-DD.log, keeping LOG_RETENTION_DAYS days.
	var logOut io.Writer = os.Stdout
	var logFile *DailyFile
	if dir := os.Getenv("LOG_DIR"); dir != "" {
		keep, err := strconv.Atoi(env("LOG_RETENTION_DAYS", "4"))
		if err != nil {
			log.Fatalf("invalid LOG_RETENTION_DAYS: %v", err)
		}
		logFile, err = OpenDailyFile(dir, "http", keep, time.Now)
		if err != nil {
			log.Fatalf("open log dir %s: %v", dir, err)
		}
		logOut = io.MultiWriter(os.Stdout, logFile)
	}
	logger := log.New(logOut, "http: ", log.LstdFlags)
	if logFile != nil {
		logger.Printf("logging to %s (daily rotation, %s days kept, tz %s)",
			logFile.Path(), env("LOG_RETENTION_DAYS", "4"), time.Now().Location())
	}

	if mb, err := strconv.Atoi(env("MAX_UPLOAD_MB", "10")); err != nil || mb < 1 {
		logger.Fatalf("invalid MAX_UPLOAD_MB: %q", os.Getenv("MAX_UPLOAD_MB"))
	} else {
		maxUploadBytes = int64(mb) << 20
	}

	shutdownTimeout, err := time.ParseDuration(env("SHUTDOWN_TIMEOUT", "30s"))
	if err != nil {
		logger.Fatalf("invalid SHUTDOWN_TIMEOUT: %v", err)
	}

	dbPath := env("DB_PATH", "db/featuretable.db")
	store, err := OpenStore(dbPath, env("DB_JOURNAL_MODE", "WAL"))
	if err != nil {
		if strings.Contains(err.Error(), "disk I/O error") {
			logger.Printf("hint: a database file created in WAL mode cannot be opened on a host-mounted folder; " +
				"convert it once with `sqlite3 file.db 'PRAGMA journal_mode=DELETE'` on a local disk, then copy it back")
		}
		logger.Fatalf("open sqlite db %s: %v", dbPath, err)
	}
	if mode, err := store.JournalMode(); err == nil {
		logger.Printf("sqlite database %s (journal mode %s)", dbPath, mode)
	}

	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		logger.Fatalf("listen on :%s: %v", port, err)
	}

	server := newServer(newHandler(store, logger, newRequestID), logger)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	logger.Printf("server is ready to handle requests at %s (sqlite: %s, shutdown timeout: %s)",
		ln.Addr(), dbPath, shutdownTimeout)

	if err := serve(server, ln, quit, logger, shutdownTimeout); err != nil {
		logger.Fatalf("server error: %v", err)
	}
	if err := store.Close(); err != nil {
		logger.Printf("closing database: %v", err)
	}
	logger.Println("server stopped")
	if logFile != nil {
		logFile.Close()
	}
}

// newServer builds the http.Server with the timeouts from the article,
// plus ReadHeaderTimeout to bound slow-header (Slowloris) clients.
func newServer(h http.Handler, logger *log.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ErrorLog:          logger,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       15 * time.Second,
	}
}

// serve runs the server until a signal arrives on quit, then shuts down
// gracefully: mark unhealthy, stop keep-alives, and let in-flight requests
// finish within timeout. It returns nil on a clean shutdown.
func serve(server *http.Server, ln net.Listener, quit <-chan os.Signal, logger *log.Logger, timeout time.Duration) error {
	done := make(chan error, 1)

	go func() {
		sig := <-quit
		logger.Printf("received %v, server is shutting down...", sig)
		atomic.StoreInt32(&healthy, 0)

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		server.SetKeepAlivesEnabled(false)
		if err := server.Shutdown(ctx); err != nil {
			logger.Printf("could not gracefully shut down the server: %v", err)
			done <- err
			return
		}
		done <- nil
	}()

	atomic.StoreInt32(&healthy, 1)
	if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return <-done
}

// newHandler wires middleware around the routes: tracing runs first so the
// logging middleware can include the request ID.
func newHandler(store *Store, logger *log.Logger, nextRequestID func() string) http.Handler {
	return tracing(nextRequestID)(logging(logger)(newMux(store, logger)))
}

// newRequestID returns a random 16-hex-char ID. The article uses
// time.Now().UnixNano(), which can collide under concurrent requests.
func newRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b)
}

// tracing propagates X-Request-Id: it keeps a caller-supplied ID (if it is
// safe to log), otherwise generates one, stores it in the request context
// and echoes it on the response.
func tracing(nextRequestID func() string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get("X-Request-Id")
			if !validRequestID(requestID) {
				requestID = nextRequestID()
			}
			ctx := context.WithValue(r.Context(), requestIDKey, requestID)
			w.Header().Set("X-Request-Id", requestID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// validRequestID accepts 1-128 chars of [A-Za-z0-9._-] so a client cannot
// inject newlines or junk into the logs.
func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

func requestIDFrom(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return "unknown"
}

// statusRecorder captures the status code and size for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the real connection (used to
// extend deadlines for file uploads and downloads).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// logging writes one access-log line per request after the handler returns.
// Same fields as the article, plus status, size and duration.
func logging(logger *log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			defer func() {
				status := rec.status
				if status == 0 {
					status = http.StatusOK
				}
				logger.Printf("id=%s %s %s status=%d bytes=%d dur=%s remote=%s ua=%q",
					requestIDFrom(r.Context()), r.Method, r.URL.Path, status, rec.bytes,
					time.Since(start).Round(time.Microsecond), r.RemoteAddr, r.UserAgent())
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

func newMux(store *Store, logger *log.Logger) *http.ServeMux {
	mux := http.NewServeMux()

	sub, _ := fs.Sub(staticFiles, "static")
	mux.Handle("GET /", http.FileServerFS(sub))

	// 204 while healthy, 503 once shutdown has started or the DB is unreachable.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&healthy) == 1 && store.Ping() == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	registerFileRoutes(mux, store, logger)

	mux.HandleFunc("GET /api/rows", func(w http.ResponseWriter, r *http.Request) {
		rows, err := store.List()
		if err != nil {
			writeErr(w, r, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	})

	mux.HandleFunc("POST /api/rows", func(w http.ResponseWriter, r *http.Request) {
		var in Row
		if !readJSON(w, r, &in) {
			return
		}
		out, err := store.Insert(in)
		if err != nil {
			writeErr(w, r, logger, err)
			return
		}
		writeJSON(w, http.StatusCreated, out)
	})

	mux.HandleFunc("PUT /api/rows/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in Row
		if !readJSON(w, r, &in) {
			return
		}
		out, err := store.Update(id, in)
		if err != nil {
			writeErr(w, r, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("DELETE /api/rows/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if err := store.Delete(id); err != nil {
			writeErr(w, r, logger, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

// probe is the -healthcheck mode: exit 0 only on 204 from /healthz.
func probe(url string) int {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return 1
	}
	return 0
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return 0, false
	}
	return id, true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return false
	}
	return true
}

func writeErr(w http.ResponseWriter, r *http.Request, logger *log.Logger, err error) {
	var ve ValidationError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": ve.Error()})
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNoFile):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	default:
		logger.Printf("id=%s error: %v", requestIDFrom(r.Context()), err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
