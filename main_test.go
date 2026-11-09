package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "test.db"), "WAL")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func fixedID(id string) func() string { return func() string { return id } }

// ---- tracing ----

func TestTracingGeneratesRequestID(t *testing.T) {
	var seen string
	h := tracing(fixedID("gen-1"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = requestIDFrom(r.Context())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if seen != "gen-1" {
		t.Errorf("context request id = %q, want gen-1", seen)
	}
	if got := rec.Header().Get("X-Request-Id"); got != "gen-1" {
		t.Errorf("response X-Request-Id = %q, want gen-1", got)
	}
}

func TestTracingPropagatesIncomingRequestID(t *testing.T) {
	var seen string
	h := tracing(fixedID("should-not-be-used"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = requestIDFrom(r.Context())
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-Id", "client-abc_123.x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != "client-abc_123.x" || rec.Header().Get("X-Request-Id") != "client-abc_123.x" {
		t.Errorf("incoming id not propagated: ctx=%q header=%q", seen, rec.Header().Get("X-Request-Id"))
	}
}

func TestTracingReplacesUnsafeRequestID(t *testing.T) {
	for _, bad := range []string{"has space", "new\nline", strings.Repeat("a", 129), "quote\""} {
		h := tracing(fixedID("safe"))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Request-Id", bad)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("X-Request-Id"); got != "safe" {
			t.Errorf("id %q: got %q, want replacement 'safe'", bad, got)
		}
	}
}

func TestNewRequestIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := newRequestID()
		if !validRequestID(id) || seen[id] {
			t.Fatalf("bad or duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestRequestIDFromMissing(t *testing.T) {
	if got := requestIDFrom(context.Background()); got != "unknown" {
		t.Errorf("got %q, want unknown", got)
	}
}

// ---- logging ----

func TestLoggingWritesAccessLine(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "http: ", 0)
	h := tracing(fixedID("req-42"))(logging(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("hello"))
	})))
	req := httptest.NewRequest("POST", "/api/rows", nil)
	req.Header.Set("User-Agent", "unit-test")
	h.ServeHTTP(httptest.NewRecorder(), req)

	line := buf.String()
	for _, want := range []string{"http: ", "id=req-42", "POST /api/rows", "status=201", "bytes=5", `ua="unit-test"`, "dur="} {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q missing %q", line, want)
		}
	}
}

func TestLoggingDefaultsTo200(t *testing.T) {
	var buf bytes.Buffer
	h := logging(log.New(&buf, "", 0))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	if !strings.Contains(buf.String(), "status=200") || !strings.Contains(buf.String(), "id=unknown") {
		t.Errorf("unexpected log line %q", buf.String())
	}
}

// ---- health check ----

func TestHealthz(t *testing.T) {
	h := newHandler(newTestStore(t), discardLogger(), fixedID("h"))

	atomic.StoreInt32(&healthy, 1)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("healthy: status %d, want 204", rec.Code)
	}

	atomic.StoreInt32(&healthy, 0)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unhealthy: status %d, want 503", rec.Code)
	}
}

func TestHealthzFailsWhenDatabaseClosed(t *testing.T) {
	s := newTestStore(t)
	h := newHandler(s, discardLogger(), fixedID("h"))
	atomic.StoreInt32(&healthy, 1)
	s.Close()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("closed db: status %d, want 503", rec.Code)
	}
}

func TestProbe(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer bad.Close()
	if probe(ok.URL) != 0 || probe(bad.URL) != 1 || probe("http://127.0.0.1:1/") != 1 {
		t.Error("probe returned wrong exit codes")
	}
}

// ---- server config & graceful shutdown ----

func TestServerTimeouts(t *testing.T) {
	s := newServer(http.NotFoundHandler(), discardLogger())
	if s.ReadTimeout != 5*time.Second || s.WriteTimeout != 10*time.Second ||
		s.IdleTimeout != 15*time.Second || s.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("unexpected timeouts: %+v", s)
	}
}

func TestGracefulShutdownDrainsInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte("finished"))
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)
	srv := newServer(mux, logger)
	quit := make(chan os.Signal, 1)
	served := make(chan error, 1)
	go func() { served <- serve(srv, ln, quit, logger, 5*time.Second) }()

	type result struct {
		body string
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			resCh <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		resCh <- result{body: string(b)}
	}()

	<-started
	if atomic.LoadInt32(&healthy) != 1 {
		t.Error("server should be healthy while serving")
	}
	quit <- syscall.SIGTERM

	res := <-resCh
	if res.err != nil || res.body != "finished" {
		t.Errorf("in-flight request was not drained: body=%q err=%v", res.body, res.err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after shutdown")
	}
	if atomic.LoadInt32(&healthy) != 0 {
		t.Error("healthy flag should be 0 after shutdown")
	}
	if _, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		t.Error("listener still accepting connections after shutdown")
	}
	if !strings.Contains(logBuf.String(), "shutting down") {
		t.Errorf("shutdown not logged: %q", logBuf.String())
	}
}

func TestShutdownTimeoutReturnsError(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/stuck", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := newServer(mux, discardLogger())
	quit := make(chan os.Signal, 1)
	served := make(chan error, 1)
	go func() { served <- serve(srv, ln, quit, discardLogger(), 100*time.Millisecond) }()
	go http.Get("http://" + ln.Addr().String() + "/stuck")

	<-started
	quit <- syscall.SIGINT
	if err := <-served; err == nil {
		t.Error("expected shutdown deadline error, got nil")
	}
	close(release)
}

// ---- CRUD through the full handler stack ----

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCRUD(t *testing.T) {
	h := newHandler(newTestStore(t), discardLogger(), newRequestID)

	if rec := do(t, h, "GET", "/", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "<title>Feature Table</title>") {
		t.Fatalf("index page: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/rows", ""); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("empty list: %d %q", rec.Code, rec.Body.String())
	}

	rec := do(t, h, "POST", "/api/rows", `{"date":"2026-11-09","name":" Ada ","feature":"Search"}`)
	if rec.Code != 201 {
		t.Fatalf("insert: %d %s", rec.Code, rec.Body.String())
	}
	var row Row
	json.Unmarshal(rec.Body.Bytes(), &row)
	if row.ID != 1 || row.Name != "Ada" {
		t.Fatalf("inserted row = %+v", row)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("API response missing X-Request-Id")
	}

	if rec := do(t, h, "PUT", "/api/rows/1", `{"date":"2026-11-10","name":"Ada","feature":"Export"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Export") {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, "PUT", "/api/rows/99", `{"date":"2026-11-10","name":"x","feature":"y"}`); rec.Code != 404 {
		t.Errorf("update missing: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/api/rows", `{"date":"not-a-date","name":"x","feature":"y"}`); rec.Code != 400 {
		t.Errorf("bad date: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/api/rows", `{bad json`); rec.Code != 400 {
		t.Errorf("bad json: %d", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/api/rows/abc", ""); rec.Code != 400 {
		t.Errorf("bad id: %d", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/api/rows/1", ""); rec.Code != 204 {
		t.Errorf("delete: %d", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/api/rows/1", ""); rec.Code != 404 {
		t.Errorf("delete again: %d", rec.Code)
	}
}

// ---- journal mode ----

func TestJournalModes(t *testing.T) {
	for _, mode := range []string{"WAL", "DELETE", "truncate"} {
		path := filepath.Join(t.TempDir(), "j.db")
		s, err := OpenStore(path, mode)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if got, err := s.JournalMode(); err != nil || got != strings.ToUpper(mode) {
			t.Errorf("mode %s: got %q err %v", mode, got, err)
		}
		if _, err := s.Insert(Row{Date: "2026-11-09", Name: "a", Feature: "b"}); err != nil {
			t.Errorf("%s insert: %v", mode, err)
		}
		s.Close()
	}
	if _, err := OpenStore(filepath.Join(t.TempDir(), "x.db"), "MEMORY"); err == nil {
		t.Error("unsupported journal mode accepted")
	}
}

// A database created in WAL mode (the old Docker volume) must open in
// DELETE mode with all its rows: this is the migration path to ./db.
func TestWALDatabaseReopensInDeleteMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrate.db")
	s, _ := OpenStore(path, "WAL")
	for i := 0; i < 3; i++ {
		s.Insert(Row{Date: "2026-11-09", Name: fmt.Sprintf("n%d", i), Feature: "f"})
	}
	s.Close()

	s2, err := OpenStore(path, "DELETE")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	rows, _ := s2.List()
	mode, _ := s2.JournalMode()
	if len(rows) != 3 || mode != "DELETE" {
		t.Errorf("after reopen: %d rows, mode %s", len(rows), mode)
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		t.Error("leftover -wal file after switching to DELETE mode")
	}
}
