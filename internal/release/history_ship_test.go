package release

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// shipRig is a history file and log files for a HistoryShipper.
type shipRig struct {
	t       *testing.T
	history string
	dir     string
	files   *logs.Files
	h       *HistoryShipper
}

func newShipRig(t *testing.T) *shipRig {
	t.Helper()
	sr := &shipRig{t: t, history: filepath.Join(t.TempDir(), HistoryFile), dir: filepath.Join(t.TempDir(), "logs")}
	sr.files = logs.Open(logs.Options{Dir: sr.dir})
	t.Cleanup(func() { sr.files.Close() })
	sr.h = &HistoryShipper{History: sr.history, Cursor: filepath.Join(sr.dir, HistoryCursor), Files: sr.files}
	return sr
}

// add appends a line with id.
func (sr *shipRig) add(id string) {
	sr.t.Helper()
	line, _ := json.Marshal(HistoryEntry{ID: id, Action: "update", Result: ResultOK, From: "0.1.0", To: id})
	f, err := os.OpenFile(sr.history, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		sr.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		sr.t.Fatal(err)
	}
}

func (sr *shipRig) ship(want int) {
	sr.t.Helper()
	n, _ := sr.h.Ship()
	if n != want {
		sr.t.Fatalf("shipped %d lines, want %d", n, want)
	}
}

// auditLines counts the lines of audit.log.
func (sr *shipRig) auditLines() int {
	sr.t.Helper()
	if err := sr.files.Close(); err != nil {
		sr.t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(sr.dir, "audit.log"))
	if err != nil {
		sr.t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// A cursor that cannot be written does not make every tick write the same
// lines again (finding R3-11).
func TestShipperWithACursorThatCannotBeWritten(t *testing.T) {
	sr := newShipRig(t)
	if err := os.MkdirAll(sr.h.Cursor+".new", 0o755); err != nil {
		t.Fatal(err)
	}
	sr.add("20261120T100000.000000000Z-00000001")
	if _, err := sr.h.Ship(); err == nil {
		t.Fatal("the cursor was written")
	}
	sr.ship(0)
	sr.add("20261120T100001.000000000Z-00000002")
	sr.ship(1)
	if n := sr.auditLines(); n != 2 {
		t.Fatalf("%d audit lines, want 2", n)
	}
}

// A line written after the clock was set back sorts below the cursor and is
// shipped all the same: lines after the line of the cursor count, in the
// order of the file (finding R3-11).
func TestShipperAfterTheClockWentBack(t *testing.T) {
	sr := newShipRig(t)
	sr.add("20261120T100000.000000000Z-00000001")
	sr.ship(1)
	sr.add("20261120T090000.000000000Z-00000002")
	sr.ship(1)
	// a new shipper, as serve after a restart, starts from the cursor file
	sr.h = &HistoryShipper{History: sr.h.History, Cursor: sr.h.Cursor, Files: sr.files}
	sr.add("20261120T090001.000000000Z-00000003")
	sr.ship(1)
	if n := sr.auditLines(); n != 3 {
		t.Fatalf("%d audit lines, want 3", n)
	}
}

// A file whose size and time did not change since the last tick is not read
// again (finding R3-11).
func TestShipperSkipsAnUnchangedFile(t *testing.T) {
	sr := newShipRig(t)
	sr.add("20261120T100000.000000000Z-00000001")
	sr.ship(1)
	info, err := os.Stat(sr.history)
	if err != nil {
		t.Fatal(err)
	}
	// the same size and time with another line: only a read would see it
	line, _ := json.Marshal(HistoryEntry{ID: "20261120T100000.000000000Z-00000009", Action: "update", Result: ResultOK, From: "0.1.0", To: "20261120T100000.000000000Z-00000001"})
	if err := os.WriteFile(sr.history, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sr.history, time.Time{}, info.ModTime()); err != nil {
		t.Fatal(err)
	}
	sr.ship(0)
}
