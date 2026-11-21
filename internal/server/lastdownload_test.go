package server

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
)

// The writer of decision A19 on the read path, at a fake clock: a GET with
// 200, or with 206 from byte 0, sets the last download time; a HEAD and a
// range that starts above byte 0 do not; a second GET within 12 hours
// writes nothing; the batch is written at close.

// catalogRescan scans the write store again, so that catalog.sqlite holds
// the blobs the write path wrote.
func (w *writing) catalogRescan() {
	w.t.Helper()
	if _, err := w.srv.Rescan(context.Background(), "local-write"); err != nil {
		w.t.Fatal(err)
	}
}

// lastDownloaded reads assets.last_downloaded_ms of a path from the file,
// zero for none.
func (w *writing) lastDownloaded(repo, path string) time.Time {
	w.t.Helper()
	abs, err := filepath.Abs(catalog.Path(w.cfg.DataDir))
	if err != nil {
		w.t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath()+"?mode=ro")
	if err != nil {
		w.t.Fatal(err)
	}
	defer db.Close()
	var ms sql.NullInt64
	if err := db.QueryRow("SELECT last_downloaded_ms FROM assets WHERE repo = ? AND path = ?", repo, path).Scan(&ms); err != nil {
		w.t.Fatalf("%s/%s: %v", repo, path, err)
	}
	if !ms.Valid {
		return time.Time{}
	}
	return time.UnixMilli(ms.Int64).UTC()
}

func TestADownloadSetsTheLastDownloadTime(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	for _, p := range []string{"docs/get.txt", "docs/head.txt", "docs/tail.txt", "docs/front.txt"} {
		if _, err := w.ingest("demo-raw", p, "some bytes of "+p); err != nil {
			t.Fatal(err)
		}
	}
	w.catalogRescan()
	at := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	c := &clock{now: at}
	side := w.srv.side()
	side.setClock(c.Now)
	get := func(path string, headers ...string) {
		t.Helper()
		r := do(t, w.addr(), "GET", "/repository/demo-raw/"+path, nil, append([]string{"basic", asAdmin}, headers...)...)
		if r.status != 200 && r.status != 206 {
			t.Fatalf("GET %s: %d", path, r.status)
		}
	}
	get("docs/get.txt")
	if r := do(t, w.addr(), "HEAD", "/repository/demo-raw/docs/head.txt", nil, "basic", asAdmin); r.status != 200 {
		t.Fatalf("HEAD %d", r.status)
	}
	get("docs/tail.txt", "Range", "bytes=2-")
	get("docs/front.txt", "Range", "bytes=0-3")
	if !w.lastDownloaded("demo-raw", "docs/get.txt").IsZero() {
		t.Fatal("the time was written before its batch")
	}
	if n, err := side.downloads.Flush(); err != nil || n != 2 {
		t.Fatalf("flush %d %v", n, err)
	}
	for path, want := range map[string]time.Time{"docs/get.txt": at, "docs/front.txt": at, "docs/head.txt": {}, "docs/tail.txt": {}} {
		if got := w.lastDownloaded("demo-raw", path); !got.Equal(want) {
			t.Errorf("%s: %v, want %v", path, got, want)
		}
	}
	// a second GET within 12 hours writes nothing; at close the batch of a
	// GET after 12 hours is written
	c.add(11 * time.Hour)
	get("docs/get.txt")
	if n, err := side.downloads.Flush(); err != nil || n != 0 || !w.lastDownloaded("demo-raw", "docs/get.txt").Equal(at) {
		t.Fatalf("a second GET within 12 hours: %d %v", n, err)
	}
	c.add(2 * time.Hour)
	get("docs/get.txt")
	w.srv.closeSide()
	if got := w.lastDownloaded("demo-raw", "docs/get.txt"); !got.Equal(at.Add(13 * time.Hour)) {
		t.Fatalf("after close %v", got)
	}
}
