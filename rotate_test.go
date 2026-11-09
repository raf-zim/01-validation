package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable clock for rotation tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func at(s string, loc *time.Location) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc)
	if err != nil {
		panic(err)
	}
	return t
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func touch(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDailyFileWritesToDatedFile(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: at("2026-11-09 10:00:00", time.UTC)}
	d, err := OpenDailyFile(dir, "http", 4, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	fmt.Fprintln(d, "hello")
	if got := filepath.Base(d.Path()); got != "http-2026-11-09.log" {
		t.Fatalf("path = %s", got)
	}
	content := readFile(t, filepath.Join(dir, "http-2026-11-09.log"))
	if !strings.Contains(content, "logrotate: 2026/11/09 10:00:00 opened http-2026-11-09.log") || !strings.HasSuffix(content, "hello\n") {
		t.Errorf("unexpected content %q", content)
	}
}

func TestDailyFileRotatesAtMidnight(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: at("2026-11-09 23:59:59", time.UTC)}
	d, _ := OpenDailyFile(dir, "http", 4, clk.Now)
	defer d.Close()

	fmt.Fprintln(d, "before midnight")
	clk.Set(at("2026-11-10 00:00:00", time.UTC))
	fmt.Fprintln(d, "after midnight")
	clk.Set(at("2026-11-10 13:00:00", time.UTC))
	fmt.Fprintln(d, "same day")

	if got := listDir(t, dir); strings.Join(got, ",") != "http-2026-11-09.log,http-2026-11-10.log" {
		t.Fatalf("files = %v", got)
	}
	day1 := readFile(t, filepath.Join(dir, "http-2026-11-09.log"))
	day2 := readFile(t, filepath.Join(dir, "http-2026-11-10.log"))
	if !strings.Contains(day1, "before midnight") || strings.Contains(day1, "after midnight") {
		t.Errorf("day1 = %q", day1)
	}
	if !strings.Contains(day2, "after midnight") || !strings.Contains(day2, "same day") {
		t.Errorf("day2 = %q", day2)
	}
}

func TestDailyFileRotationUsesClockTimeZone(t *testing.T) {
	jlm, err := time.LoadLocation("Asia/Jerusalem")
	if err != nil {
		t.Fatalf("tzdata not available: %v", err)
	}
	dir := t.TempDir()
	// 23:30 UTC on Nov 9 is already 01:30 on Nov 10 in Jerusalem (UTC+2).
	clk := &fakeClock{t: at("2026-11-09 23:30:00", time.UTC).In(jlm)}
	d, _ := OpenDailyFile(dir, "http", 4, clk.Now)
	defer d.Close()
	if got := filepath.Base(d.Path()); got != "http-2026-11-10.log" {
		t.Errorf("path = %s, want the Jerusalem date", got)
	}
}

func TestDailyFilePurgesOnStartup(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"http-2026-11-01.log", "http-2026-11-04.log", // older than 4 days: removed
		"http-2026-11-05.log", "http-2026-11-08.log", // within 4 days: kept
		"http-notadate.log", "other-2026-11-01.log", "notes.txt", // not ours: kept
	} {
		touch(t, dir, n)
	}
	clk := &fakeClock{t: at("2026-11-09 08:00:00", time.UTC)}
	d, err := OpenDailyFile(dir, "http", 4, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	want := "http-2026-11-05.log,http-2026-11-08.log,http-2026-11-09.log,http-notadate.log,notes.txt,other-2026-11-01.log"
	if got := strings.Join(listDir(t, dir), ","); got != want {
		t.Errorf("files after purge:\n got %s\nwant %s", got, want)
	}
	note := readFile(t, d.Path())
	if !strings.Contains(note, "removed http-2026-11-01.log, http-2026-11-04.log") {
		t.Errorf("purge not recorded: %q", note)
	}
}

func TestDailyFilePurgesOnRotation(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: at("2026-11-01 12:00:00", time.UTC)}
	d, _ := OpenDailyFile(dir, "http", 4, clk.Now)
	defer d.Close()

	// Write one line a day for 10 days; only the last 5 dated files survive.
	for i := 0; i < 10; i++ {
		clk.Set(at("2026-11-01 12:00:00", time.UTC).AddDate(0, 0, i))
		fmt.Fprintf(d, "day %d\n", i)
	}
	want := "http-2026-11-06.log,http-2026-11-07.log,http-2026-11-08.log,http-2026-11-09.log,http-2026-11-10.log"
	if got := strings.Join(listDir(t, dir), ","); got != want {
		t.Errorf("files:\n got %s\nwant %s", got, want)
	}
}

func TestDailyFileAppendsAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: at("2026-11-09 09:00:00", time.UTC)}
	d1, _ := OpenDailyFile(dir, "http", 4, clk.Now)
	fmt.Fprintln(d1, "first run")
	d1.Close()
	d2, _ := OpenDailyFile(dir, "http", 4, clk.Now)
	fmt.Fprintln(d2, "second run")
	d2.Close()

	content := readFile(t, filepath.Join(dir, "http-2026-11-09.log"))
	if !strings.Contains(content, "first run") || !strings.Contains(content, "second run") {
		t.Errorf("restart truncated the log: %q", content)
	}
}

func TestDailyFileConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: at("2026-11-09 23:59:59", time.UTC)}
	d, _ := OpenDailyFile(dir, "http", 4, clk.Now)
	defer d.Close()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if g == 0 && i == 100 {
					clk.Set(at("2026-11-10 00:00:01", time.UTC))
				}
				fmt.Fprintf(d, "g%d line %d\n", g, i)
			}
		}(g)
	}
	wg.Wait()

	total := 0
	for _, n := range listDir(t, dir) {
		for _, l := range strings.Split(readFile(t, filepath.Join(dir, n)), "\n") {
			if strings.HasPrefix(l, "g") {
				total++
			}
		}
	}
	if total != 8*200 {
		t.Errorf("lines written = %d, want %d", total, 8*200)
	}
}

func TestDailyFileRejectsNegativeRetention(t *testing.T) {
	if _, err := OpenDailyFile(t.TempDir(), "http", -1, time.Now); err == nil {
		t.Error("expected error for negative keepDays")
	}
}
