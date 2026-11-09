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

// Row is one record in the table: Date, Name, Feature, ID.
type Row struct {
	ID      int64  `json:"id"`
	Date    string `json:"date"` // YYYY-MM-DD
	Name    string `json:"name"`
	Feature string `json:"feature"`
}

var ErrNotFound = errors.New("row not found")

const schema = `
CREATE TABLE IF NOT EXISTS features (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    date    TEXT NOT NULL,
    name    TEXT NOT NULL,
    feature TEXT NOT NULL
);`

// Store wraps the SQLite database holding the "features" table.
type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite allows one writer; serialise to avoid SQLITE_BUSY
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping() error { return s.db.Ping() }

func (s *Store) List() ([]Row, error) {
	rs, err := s.db.Query(`SELECT id, date, name, feature FROM features ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []Row{}
	for rs.Next() {
		var r Row
		if err := rs.Scan(&r.ID, &r.Date, &r.Name, &r.Feature); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
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
	res, err := s.db.Exec(`UPDATE features SET date = ?, name = ?, feature = ? WHERE id = ?`, r.Date, r.Name, r.Feature, id)
	if err != nil {
		return Row{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Row{}, ErrNotFound
	}
	r.ID = id
	return r, nil
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
