package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// maxUploadBytes caps an uploaded file (MAX_UPLOAD_MB, default 10).
var maxUploadBytes int64 = 10 << 20

// fileTransferTimeout replaces the server's short read/write timeouts for
// a single upload or download, so large files on slow links still finish.
const fileTransferTimeout = 2 * time.Minute

// viewableTypes are the only types ever served inline (by extension).
var viewableTypes = map[string]string{
	".txt":  "text/plain; charset=utf-8",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
}

func isViewable(ctype string) bool {
	for _, t := range viewableTypes {
		if t == ctype {
			return true
		}
	}
	return false
}

// contentTypeFor picks the type a file is stored and served as. Only a
// viewable extension whose bytes match it gets a viewable type; everything
// else (including a .png that is really HTML) is application/octet-stream.
func contentTypeFor(name string, data []byte) string {
	want, ok := viewableTypes[strings.ToLower(path.Ext(name))]
	if !ok {
		return "application/octet-stream"
	}
	if strings.HasPrefix(want, "text/") {
		if utf8.Valid(data) && !bytes.ContainsRune(data, 0) {
			return want
		}
		return "application/octet-stream"
	}
	if http.DetectContentType(data) == want {
		return want
	}
	return "application/octet-stream"
}

// cleanFileName keeps only the base name, drops control characters and
// caps the length, so it is safe in headers, logs and the page.
func cleanFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(strings.TrimSpace(name))
	if name == "." || name == "/" || name == ".." {
		return ""
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' {
			return -1
		}
		return r
	}, name)
	if len(name) > 200 {
		ext := path.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		name = strings.ToValidUTF8(name[:200-len(ext)], "") + ext
	}
	return name
}

// extendDeadlines lifts the server-wide timeouts for one request. It is a
// no-op where unsupported (e.g. httptest.ResponseRecorder).
func extendDeadlines(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(fileTransferTimeout)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
}

func registerFileRoutes(mux *http.ServeMux, store *Store, logger *log.Logger) {
	// Upload or replace: multipart/form-data with a part named "file".
	mux.HandleFunc("PUT /api/rows/{id}/file", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		extendDeadlines(w)
		name, data, err := readUpload(w, r)
		if err != nil {
			var tooBig *http.MaxBytesError
			switch {
			case errors.As(err, &tooBig), errors.Is(err, errTooLarge):
				writeJSON(w, http.StatusRequestEntityTooLarge,
					map[string]string{"error": fmt.Sprintf("file too large (max %d MB)", maxUploadBytes>>20)})
			default:
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			}
			return
		}
		row, err := store.SetFile(id, name, data)
		if err != nil {
			writeErr(w, r, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, row)
	})

	// Download (attachment), or ?view=1 to show a viewable type inline.
	mux.HandleFunc("GET /api/rows/{id}/file", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		info, data, err := store.GetFile(id)
		if err != nil {
			writeErr(w, r, logger, err)
			return
		}
		view := r.URL.Query().Get("view") == "1"
		if view && !info.Viewable {
			writeJSON(w, http.StatusUnsupportedMediaType,
				map[string]string{"error": "only txt, webp, jpg, jpeg and png files can be viewed"})
			return
		}
		disposition := "attachment"
		if view {
			disposition = "inline"
		}
		h := w.Header()
		h.Set("Content-Type", info.Type)
		h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": info.Name}))
		h.Set("X-Content-Type-Options", "nosniff")
		// Uploaded content never runs as a page: no scripts, no plugins,
		// only its own image and inline styles (for the text view).
		h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; sandbox")
		h.Set("Cache-Control", "private, no-cache")
		extendDeadlines(w)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	})

	mux.HandleFunc("DELETE /api/rows/{id}/file", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if err := store.ClearFile(id); err != nil {
			writeErr(w, r, logger, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

var errTooLarge = errors.New("file too large")

// readUpload streams the "file" part of a multipart body into memory
// (no temp files), enforcing maxUploadBytes.
func readUpload(w http.ResponseWriter, r *http.Request) (string, []byte, error) {
	// Allow some room for multipart headers around the file itself.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+64<<10)
	mr, err := r.MultipartReader()
	if err != nil {
		return "", nil, errors.New("expected multipart/form-data with a \"file\" field")
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return "", nil, errors.New("no \"file\" field in upload")
		}
		if err != nil {
			return "", nil, err
		}
		if part.FormName() != "file" {
			part.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, maxUploadBytes+1))
		part.Close()
		if err != nil {
			return "", nil, err
		}
		if int64(len(data)) > maxUploadBytes {
			return "", nil, errTooLarge
		}
		return part.FileName(), data, nil
	}
}
