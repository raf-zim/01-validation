package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

var webpBytes = append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), make([]byte, 24)...)

// mp4Bytes is an ftyp box (size 24) followed by a few bytes of payload.
func mp4Bytes(brand string) []byte {
	b := []byte{0, 0, 0, 24}
	b = append(b, "ftyp"+brand+"\x00\x00\x02\x00"+brand+"iso2"...)
	return append(b, []byte("\x00\x00\x00\x08free")...)
}

var pdfBytes = []byte("%PDF-1.4\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n")

func upload(t *testing.T, h http.Handler, id, field, name string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile(field, name)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(data)
	mw.Close()
	req := httptest.NewRequest("PUT", "/api/rows/"+id+"/file", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func newRowHandler(t *testing.T) (http.Handler, *Store) {
	t.Helper()
	s := newTestStore(t)
	h := newHandler(s, discardLogger(), newRequestID)
	if rec := do(t, h, "POST", "/api/rows", `{"date":"2026-11-09","name":"Ada","feature":"Files"}`); rec.Code != 201 {
		t.Fatalf("create row: %d", rec.Code)
	}
	return h, s
}

// ---- pure helpers ----

func TestContentTypeFor(t *testing.T) {
	png := pngBytes(t)
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"a.png", png, "image/png"},
		{"A.PNG", png, "image/png"},
		{"a.jpg", jpegBytes(t), "image/jpeg"},
		{"a.jpeg", jpegBytes(t), "image/jpeg"},
		{"a.webp", webpBytes, "image/webp"},
		{"clip.mp4", mp4Bytes("isom"), "video/mp4"},
		{"clip.MP4", mp4Bytes("mp42"), "video/mp4"},
		{"doc.pdf", pdfBytes, "application/pdf"},
		{"fake.mp4", []byte("<html>not a video at all</html>"), "application/octet-stream"},
		{"fake.pdf", []byte("<html>not a pdf</html>"), "application/octet-stream"},
		{"mp4-named.pdf", mp4Bytes("isom"), "application/octet-stream"},
		{"notes.txt", []byte("hello\nworld"), "text/plain; charset=utf-8"},
		{"fake.png", []byte("<html><script>alert(1)</script></html>"), "application/octet-stream"},
		{"jpeg-named.png", jpegBytes(t), "application/octet-stream"},
		{"binary.txt", []byte{0, 1, 2, 0xff}, "application/octet-stream"},
		{"page.html", []byte("<html></html>"), "application/octet-stream"},
		{"report.docx", []byte("PK\x03\x04"), "application/octet-stream"},
		{"noext", png, "application/octet-stream"},
	}
	for _, c := range cases {
		if got := contentTypeFor(c.name, c.data); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCleanFileName(t *testing.T) {
	cases := map[string]string{
		"photo.png":                       "photo.png",
		`C:\Users\me\photo.png`:           "photo.png",
		"../../etc/passwd":                "passwd",
		"bad\r\nname\".txt":               "badname.txt",
		"  spaced.txt ":                   "spaced.txt",
		"..":                              "",
		"":                                "",
		strings.Repeat("a", 300) + ".txt": strings.Repeat("a", 196) + ".txt",
	}
	for in, want := range cases {
		if got := cleanFileName(in); got != want {
			t.Errorf("cleanFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- migration ----

func TestMigrationAddsFileColumnsToOldTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old.Exec(`CREATE TABLE features (id INTEGER PRIMARY KEY AUTOINCREMENT, date TEXT NOT NULL, name TEXT NOT NULL, feature TEXT NOT NULL)`)
	old.Exec(`INSERT INTO features (date, name, feature) VALUES ('2026-01-01', 'old', 'row')`)
	old.Close()

	s, err := OpenStore(path, "DELETE")
	if err != nil {
		t.Fatalf("open old db: %v", err)
	}
	defer s.Close()
	rows, err := s.List()
	if err != nil || len(rows) != 1 || rows[0].Name != "old" || rows[0].File != nil {
		t.Fatalf("rows after migration: %+v err=%v", rows, err)
	}
	if _, err := s.SetFile(1, "a.txt", []byte("hi")); err != nil {
		t.Errorf("SetFile on migrated table: %v", err)
	}
	// Opening again must not try to add the columns twice.
	s.Close()
	s2, err := OpenStore(path, "DELETE")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s2.Close()
}

// ---- HTTP: upload, list, download, view, remove ----

func TestUploadDownloadAndViewImage(t *testing.T) {
	h, _ := newRowHandler(t)
	data := pngBytes(t)

	rec := upload(t, h, "1", "file", "photo.png", data)
	if rec.Code != 200 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	var row Row
	json.Unmarshal(rec.Body.Bytes(), &row)
	if row.File == nil || row.File.Name != "photo.png" || row.File.Type != "image/png" ||
		row.File.Size != int64(len(data)) || !row.File.Viewable {
		t.Fatalf("row.file = %+v", row.File)
	}

	// The list carries metadata only, never the bytes.
	list := get(h, "/api/rows")
	if !strings.Contains(list.Body.String(), `"name":"photo.png"`) || len(list.Body.String()) > 500 {
		t.Errorf("list body unexpected: %s", list.Body.String())
	}

	dl := get(h, "/api/rows/1/file")
	if dl.Code != 200 || !bytes.Equal(dl.Body.Bytes(), data) {
		t.Fatalf("download: %d, %d bytes", dl.Code, dl.Body.Len())
	}
	disp, params, _ := mime.ParseMediaType(dl.Header().Get("Content-Disposition"))
	if disp != "attachment" || params["filename"] != "photo.png" {
		t.Errorf("Content-Disposition = %q", dl.Header().Get("Content-Disposition"))
	}
	for k, want := range map[string]string{
		"Content-Type":           "image/png",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := dl.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if csp := dl.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q", csp)
	}

	view := get(h, "/api/rows/1/file?view=1")
	if view.Code != 200 || !strings.HasPrefix(view.Header().Get("Content-Disposition"), "inline") || !bytes.Equal(view.Body.Bytes(), data) {
		t.Errorf("view: %d %q", view.Code, view.Header().Get("Content-Disposition"))
	}
}

func TestViewTextAndOtherTypes(t *testing.T) {
	h, _ := newRowHandler(t)

	if rec := upload(t, h, "1", "file", "notes.txt", []byte("hello\n")); rec.Code != 200 {
		t.Fatalf("upload txt: %d", rec.Code)
	}
	v := get(h, "/api/rows/1/file?view=1")
	if v.Code != 200 || v.Header().Get("Content-Type") != "text/plain; charset=utf-8" || v.Body.String() != "hello\n" {
		t.Errorf("txt view: %d %q %q", v.Code, v.Header().Get("Content-Type"), v.Body.String())
	}

	for _, n := range []string{"page.html", "fake.png", "fake.pdf", "fake.mp4"} {
		body := []byte("<html><script>alert(1)</script></html>")
		rec := upload(t, h, "1", "file", n, body)
		var row Row
		json.Unmarshal(rec.Body.Bytes(), &row)
		if rec.Code != 200 || row.File.Viewable || row.File.Type != "application/octet-stream" {
			t.Errorf("%s: code %d file %+v", n, rec.Code, row.File)
		}
		if v := get(h, "/api/rows/1/file?view=1"); v.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s: view status %d, want 415", n, v.Code)
		}
		dl := get(h, "/api/rows/1/file")
		if dl.Code != 200 || dl.Header().Get("Content-Type") != "application/octet-stream" ||
			!strings.HasPrefix(dl.Header().Get("Content-Disposition"), "attachment") {
			t.Errorf("%s: download %d %q %q", n, dl.Code, dl.Header().Get("Content-Type"), dl.Header().Get("Content-Disposition"))
		}
	}
}

func TestNonASCIIFileName(t *testing.T) {
	h, _ := newRowHandler(t)
	upload(t, h, "1", "file", "résumé ü.txt", []byte("x"))
	dl := get(h, "/api/rows/1/file")
	_, params, err := mime.ParseMediaType(dl.Header().Get("Content-Disposition"))
	if err != nil || params["filename"] != "résumé ü.txt" {
		t.Errorf("Content-Disposition %q -> %q (%v)", dl.Header().Get("Content-Disposition"), params["filename"], err)
	}
}

func TestUpdateRowKeepsFileAndDeleteRemovesIt(t *testing.T) {
	h, _ := newRowHandler(t)
	upload(t, h, "1", "file", "a.txt", []byte("keep me"))

	rec := do(t, h, "PUT", "/api/rows/1", `{"date":"2026-11-10","name":"Ada","feature":"Edited"}`)
	var row Row
	json.Unmarshal(rec.Body.Bytes(), &row)
	if rec.Code != 200 || row.File == nil || row.File.Name != "a.txt" {
		t.Fatalf("after update: %d %+v", rec.Code, row.File)
	}
	if dl := get(h, "/api/rows/1/file"); dl.Body.String() != "keep me" {
		t.Errorf("file changed by row update: %q", dl.Body.String())
	}

	if rec := do(t, h, "DELETE", "/api/rows/1/file", ""); rec.Code != 204 {
		t.Errorf("remove file: %d", rec.Code)
	}
	if dl := get(h, "/api/rows/1/file"); dl.Code != 404 {
		t.Errorf("download after remove: %d, want 404", dl.Code)
	}
	var rows []Row
	json.Unmarshal(get(h, "/api/rows").Body.Bytes(), &rows)
	if len(rows) != 1 || rows[0].File != nil {
		t.Errorf("rows after remove: %+v", rows)
	}

	upload(t, h, "1", "file", "b.txt", []byte("bye"))
	do(t, h, "DELETE", "/api/rows/1", "")
	if dl := get(h, "/api/rows/1/file"); dl.Code != 404 {
		t.Errorf("file of deleted row: %d", dl.Code)
	}
}

func TestUploadErrors(t *testing.T) {
	h, _ := newRowHandler(t)

	if rec := do(t, h, "PUT", "/api/rows/1/file", `{"not":"multipart"}`); rec.Code != 400 {
		t.Errorf("non-multipart: %d", rec.Code)
	}
	if rec := upload(t, h, "1", "other", "a.txt", []byte("x")); rec.Code != 400 {
		t.Errorf("missing file field: %d", rec.Code)
	}
	if rec := upload(t, h, "1", "file", "empty.txt", nil); rec.Code != 400 {
		t.Errorf("empty file: %d", rec.Code)
	}
	if rec := upload(t, h, "99", "file", "a.txt", []byte("x")); rec.Code != 404 {
		t.Errorf("unknown row: %d", rec.Code)
	}
	if rec := upload(t, h, "abc", "file", "a.txt", []byte("x")); rec.Code != 400 {
		t.Errorf("bad id: %d", rec.Code)
	}
	if rec := get(h, "/api/rows/1/file"); rec.Code != 404 {
		t.Errorf("no file yet: %d", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/api/rows/99/file", ""); rec.Code != 404 {
		t.Errorf("remove on unknown row: %d", rec.Code)
	}
}

func TestUploadTooLarge(t *testing.T) {
	h, _ := newRowHandler(t)
	saved := maxUploadBytes
	maxUploadBytes = 1 << 10
	defer func() { maxUploadBytes = saved }()

	if rec := upload(t, h, "1", "file", "ok.txt", bytes.Repeat([]byte("a"), 1<<10)); rec.Code != 200 {
		t.Errorf("exactly the limit: %d %s", rec.Code, rec.Body.String())
	}
	rec := upload(t, h, "1", "file", "big.txt", bytes.Repeat([]byte("a"), 1<<10+1))
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "too large") {
		t.Errorf("over the limit: %d %s", rec.Code, rec.Body.String())
	}
	// The earlier file must still be there.
	if dl := get(h, "/api/rows/1/file"); dl.Body.Len() != 1<<10 {
		t.Errorf("file after rejected upload: %d bytes", dl.Body.Len())
	}
}

func TestViewVideoAndPDF(t *testing.T) {
	h, _ := newRowHandler(t)
	video := mp4Bytes("isom")

	rec := upload(t, h, "1", "file", "clip.mp4", video)
	var row Row
	json.Unmarshal(rec.Body.Bytes(), &row)
	if rec.Code != 200 || row.File.Type != "video/mp4" || !row.File.Viewable {
		t.Fatalf("mp4 upload: %d %+v", rec.Code, row.File)
	}
	v := get(h, "/api/rows/1/file?view=1")
	if v.Code != 200 || v.Header().Get("Content-Type") != "video/mp4" ||
		!strings.HasPrefix(v.Header().Get("Content-Disposition"), "inline") {
		t.Errorf("mp4 view: %d %q %q", v.Code, v.Header().Get("Content-Type"), v.Header().Get("Content-Disposition"))
	}
	if csp := v.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "media-src 'self'") || !strings.Contains(csp, "sandbox") {
		t.Errorf("mp4 CSP = %q", csp)
	}
	// Range requests (used by the video player to seek) return 206.
	req := httptest.NewRequest("GET", "/api/rows/1/file?view=1", nil)
	req.Header.Set("Range", "bytes=4-7")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusPartialContent || rr.Body.String() != "ftyp" {
		t.Errorf("range: %d %q", rr.Code, rr.Body.String())
	}

	rec = upload(t, h, "1", "file", "doc.pdf", pdfBytes)
	json.Unmarshal(rec.Body.Bytes(), &row)
	if rec.Code != 200 || row.File.Type != "application/pdf" || !row.File.Viewable {
		t.Fatalf("pdf upload: %d %+v", rec.Code, row.File)
	}
	v = get(h, "/api/rows/1/file?view=1")
	if v.Code != 200 || v.Header().Get("Content-Type") != "application/pdf" || !bytes.Equal(v.Body.Bytes(), pdfBytes) {
		t.Errorf("pdf view: %d %q", v.Code, v.Header().Get("Content-Type"))
	}
	if csp := v.Header().Get("Content-Security-Policy"); csp != "" {
		t.Errorf("pdf must not be sandboxed (breaks browser PDF viewers), CSP = %q", csp)
	}
	if v.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("pdf missing nosniff")
	}
}

func TestCSPFor(t *testing.T) {
	for ctype, want := range map[string]string{
		"image/png":                 "img-src 'self'",
		"text/plain; charset=utf-8": "sandbox",
		"video/mp4":                 "media-src 'self'",
		"application/octet-stream":  "default-src 'none'",
	} {
		if got := cspFor(ctype); !strings.Contains(got, want) || !strings.Contains(got, "sandbox") {
			t.Errorf("cspFor(%q) = %q, want it to contain %q and sandbox", ctype, got, want)
		}
	}
	if cspFor("application/pdf") != "" {
		t.Error("pdf should get no CSP")
	}
}
