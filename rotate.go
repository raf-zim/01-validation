package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const dayLayout = "2006-01-02"

// DailyFile is an io.Writer that writes to <dir>/<prefix>-YYYY-MM-DD.log.
// It switches to a new file on the first write after midnight (in the
// clock's time zone). On startup and on every rotation it deletes
// <prefix>-YYYY-MM-DD.log files more than keepDays days old.
//
// With keepDays = 4, on 2026-11-09 the files for 11-05 through 11-09 are
// kept and 11-04 and earlier are deleted.
type DailyFile struct {
	mu       sync.Mutex
	dir      string
	prefix   string
	keepDays int
	now      func() time.Time

	day string
	f   *os.File
}

func OpenDailyFile(dir, prefix string, keepDays int, now func() time.Time) (*DailyFile, error) {
	if keepDays < 0 {
		return nil, fmt.Errorf("keepDays must be >= 0, got %d", keepDays)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	d := &DailyFile{dir: dir, prefix: prefix, keepDays: keepDays, now: now}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.rotateLocked(now()); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *DailyFile) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.now()
	if t.Format(dayLayout) != d.day || d.f == nil {
		if err := d.rotateLocked(t); err != nil {
			return 0, err
		}
	}
	return d.f.Write(p)
}

// Path is the file currently being written.
func (d *DailyFile) Path() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pathFor(d.day)
}

func (d *DailyFile) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}

func (d *DailyFile) pathFor(day string) string {
	return filepath.Join(d.dir, d.prefix+"-"+day+".log")
}

func (d *DailyFile) rotateLocked(t time.Time) error {
	if d.f != nil {
		d.f.Close()
		d.f = nil
	}
	day := t.Format(dayLayout)
	f, err := os.OpenFile(d.pathFor(day), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	d.f, d.day = f, day

	removed, purgeErr := d.purgeLocked(t)
	note := fmt.Sprintf("logrotate: %s opened %s (keeping %d days)", t.Format("2006/01/02 15:04:05"), filepath.Base(d.pathFor(day)), d.keepDays)
	if len(removed) > 0 {
		note += "; removed " + strings.Join(removed, ", ")
	}
	if purgeErr != nil {
		note += "; purge error: " + purgeErr.Error()
	}
	fmt.Fprintln(d.f, note)
	return nil
}

// purgeLocked deletes this writer's dated files older than keepDays,
// leaving anything else in the directory alone.
func (d *DailyFile) purgeLocked(t time.Time) ([]string, error) {
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	cutoff := midnight.AddDate(0, 0, -d.keepDays)

	matches, err := filepath.Glob(filepath.Join(d.dir, d.prefix+"-*.log"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	var removed []string
	var firstErr error
	for _, m := range matches {
		name := filepath.Base(m)
		datePart := strings.TrimSuffix(strings.TrimPrefix(name, d.prefix+"-"), ".log")
		day, err := time.ParseInLocation(dayLayout, datePart, t.Location())
		if err != nil || !day.Before(cutoff) {
			continue
		}
		if err := os.Remove(m); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed = append(removed, name)
	}
	return removed, firstErr
}
