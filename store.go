package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGO), registers "sqlite"
)

// Row is one record in the table: Date, Name, Feature, ID, plus an
// optional attached file (stored in the file BLOB column).
type Row struct {
	ID      int64     `json:"id"`
	Date    string    `json:"date"` // YYYY-MM-DD
	Name    string    `json:"name"`
	Feature string    `json:"feature"`
	File    *FileInfo `json:"file"` // null when no file is attached
}

// FileInfo describes an attached file; the bytes are fetched separately.
type FileInfo struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // content type the server will serve it as
	Size     int64  `json:"size"`
	Viewable bool   `json:"viewable"` // txt/webp/jpg/jpeg/png: can be shown inline
}

var (
	ErrNotFound    = errors.New("row not found")
	ErrNoFile      = errors.New("row has no file")
	ErrEmptyFile   = ValidationError{"file is empty"}
	ErrBadFileName = ValidationError{"file name is required"}
)

const schema = `
CREATE TABLE IF NOT EXISTS features (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    date    TEXT NOT NULL,
    name    TEXT NOT NULL,
    feature TEXT NOT NULL
);`

// fileColumns are added to tables created before attachments existed.
var fileColumns = []struct{ name, decl string }{
	{"file", "BLOB"},
	{"file_name", "TEXT"},
	{"file_type", "TEXT"},
	{"file_size", "INTEGER"},
}

const rowColumns = `id, date, name, feature, file_name, file_type, file_size`

// Store wraps the SQLite database holding the "features" table.
type Store struct{ db *sql.DB }

// JournalModes are the SQLite journal modes OpenStore accepts.
//   - WAL: fastest, but needs shared memory (mmap), which is unreliable on
//     host folders bind-mounted into Docker Desktop on Windows/macOS.
//   - DELETE: SQLite's classic rollback journal; safe on any filesystem.
var JournalModes = map[string]bool{"WAL": true, "DELETE": true, "TRUNCATE": true}

func OpenStore(path, journalMode string) (*Store, error) {
	journalMode = strings.ToUpper(strings.TrimSpace(journalMode))
	if !JournalModes[journalMode] {
		return nil, fmt.Errorf("unsupported journal mode %q (use WAL, DELETE or TRUNCATE)", journalMode)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(" + journalMode + ")&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite allows one writer; serialise to avoid SQLITE_BUSY
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate adds any missing file columns (ALTER TABLE ... ADD COLUMN keeps
// existing rows; the new columns start out NULL).
func migrate(db *sql.DB) error {
	rs, err := db.Query(`PRAGMA table_info(features)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rs.Next() {
		var (
			cid       int
			name, typ string
			notnull   int
			dflt      sql.NullString
			pk        int
		)
		if err := rs.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rs.Close()
			return err
		}
		have[name] = true
	}
	rs.Close()
	for _, c := range fileColumns {
		if !have[c.name] {
			if _, err := db.Exec(`ALTER TABLE features ADD COLUMN ` + c.name + ` ` + c.decl); err != nil {
				return err
			}
		}
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanRow(sc scanner) (Row, error) {
	var (
		r     Row
		fname sql.NullString
		ftype sql.NullString
		fsize sql.NullInt64
	)
	if err := sc.Scan(&r.ID, &r.Date, &r.Name, &r.Feature, &fname, &ftype, &fsize); err != nil {
		return Row{}, err
	}
	if fname.Valid && fname.String != "" {
		r.File = &FileInfo{Name: fname.String, Type: ftype.String, Size: fsize.Int64, Viewable: isViewable(ftype.String)}
	}
	return r, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping() error { return s.db.Ping() }

// JournalMode reports the journal mode SQLite is actually using.
func (s *Store) JournalMode() (string, error) {
	var mode string
	err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	return strings.ToUpper(mode), err
}

func (s *Store) List() ([]Row, error) {
	// The BLOB itself is never selected here: only its metadata.
	rs, err := s.db.Query(`SELECT ` + rowColumns + ` FROM features ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []Row{}
	for rs.Next() {
		r, err := scanRow(rs)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

func (s *Store) Get(id int64) (Row, error) {
	r, err := scanRow(s.db.QueryRow(`SELECT `+rowColumns+` FROM features WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Row{}, ErrNotFound
	}
	return r, err
}

// SetFile attaches (or replaces) the row's file.
func (s *Store) SetFile(id int64, name string, data []byte) (Row, error) {
	name = cleanFileName(name)
	if name == "" {
		return Row{}, ErrBadFileName
	}
	if len(data) == 0 {
		return Row{}, ErrEmptyFile
	}
	ctype := contentTypeFor(name, data)
	res, err := s.db.Exec(`UPDATE features SET file = ?, file_name = ?, file_type = ?, file_size = ? WHERE id = ?`,
		data, name, ctype, len(data), id)
	if err != nil {
		return Row{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Row{}, ErrNotFound
	}
	return s.Get(id)
}

// GetFile returns the row's file metadata and bytes.
func (s *Store) GetFile(id int64) (FileInfo, []byte, error) {
	var (
		data  []byte
		fname sql.NullString
		ftype sql.NullString
	)
	err := s.db.QueryRow(`SELECT file, file_name, file_type FROM features WHERE id = ?`, id).Scan(&data, &fname, &ftype)
	if errors.Is(err, sql.ErrNoRows) {
		return FileInfo{}, nil, ErrNotFound
	}
	if err != nil {
		return FileInfo{}, nil, err
	}
	if !fname.Valid || fname.String == "" || data == nil {
		return FileInfo{}, nil, ErrNoFile
	}
	return FileInfo{Name: fname.String, Type: ftype.String, Size: int64(len(data)), Viewable: isViewable(ftype.String)}, data, nil
}

// ClearFile removes the row's file.
func (s *Store) ClearFile(id int64) error {
	res, err := s.db.Exec(`UPDATE features SET file = NULL, file_name = NULL, file_type = NULL, file_size = NULL WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Insert(r Row) (Row, error) {
	if err := validate(&r); err != nil {
		return Row{}, err
	}
	res, err := s.db.Exec(`INSERT INTO features (date, name, feature) VALUES (?, ?, ?)`, r.Date, r.Name, r.Feature)
	if err != nil {
		return Row{}, err
	}
	r.ID, err = res.LastInsertId()
	return r, err
}

func (s *Store) Update(id int64, r Row) (Row, error) {
	if err := validate(&r); err != nil {
		return Row{}, err
	}
	// Only the three text columns change; an attached file is kept.
	res, err := s.db.Exec(`UPDATE features SET date = ?, name = ?, feature = ? WHERE id = ?`, r.Date, r.Name, r.Feature, id)
	if err != nil {
		return Row{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Row{}, ErrNotFound
	}
	return s.Get(id)
}

func (s *Store) Delete(id int64) error {
	res, err := s.db.Exec(`DELETE FROM features WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type ValidationError struct{ msg string }

func (e ValidationError) Error() string { return e.msg }

func validate(r *Row) error {
	r.Date = strings.TrimSpace(r.Date)
	r.Name = strings.TrimSpace(r.Name)
	r.Feature = strings.TrimSpace(r.Feature)
	if _, err := time.Parse("2006-01-02", r.Date); err != nil {
		return ValidationError{"date must be YYYY-MM-DD"}
	}
	if r.Name == "" {
		return ValidationError{"name is required"}
	}
	if r.Feature == "" {
		return ValidationError{"feature is required"}
	}
	if len(r.Name) > 200 || len(r.Feature) > 1000 {
		return ValidationError{"name or feature is too long"}
	}
	return nil
}
