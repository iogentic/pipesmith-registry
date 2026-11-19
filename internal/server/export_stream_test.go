package server

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// rawAnswer is an answer read from the socket of an HTTP/1.1 request with
// Connection: close: the bytes as they came, the status, the body and the
// error of reading the body.
type rawAnswer struct {
	raw     []byte
	status  int
	body    []byte
	bodyErr error
}

func rawGet(t *testing.T, addr, target string) rawAnswer {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", target, addr)
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, bodyErr := io.ReadAll(resp.Body)
	return rawAnswer{raw: raw, status: resp.StatusCode, body: body, bodyErr: bodyErr}
}

// lastChunk ends a whole chunked body.
var lastChunk = []byte("\r\n0\r\n\r\n")

// failingStore fails the nth open of a bytes file.
type failingStore struct {
	stores.Store
	mu    sync.Mutex
	opens int
	fail  int
}

func (f *failingStore) Open(location string) (*os.File, error) {
	f.mu.Lock()
	f.opens++
	n := f.opens
	f.mu.Unlock()
	if n == f.fail {
		return nil, errors.New("input/output error of the test")
	}
	return f.Store.Open(location)
}

func threeJars() []testkit.BlobSpec {
	return []testkit.BlobSpec{
		spec("demo-raw", "docs/a.jar", []byte("AAAA first file AAAA"), "application/java-archive"),
		spec("demo-raw", "docs/b.jar", []byte("BBBB second file BBBB"), "application/java-archive"),
		spec("demo-raw", "docs/c.jar", []byte("CCCC third file CCCC"), "application/java-archive"),
	}
}

// An export that fails after the head breaks the transfer: the chunked body
// has no last chunk, so a client reports an error and keeps no archive that
// looks finished.
func TestExportCutAfterTheHeadFailsTheTransfer(t *testing.T) {
	var failing *failingStore
	a := startExport(t, exportOptions{blobs: threeJars(), defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")},
		wrap: func(s stores.Store) stores.Store { failing = &failingStore{Store: s, fail: 3}; return failing }})
	target := exportTarget("demo-raw", "path", "docs")
	cut := rawGet(t, a.addr, target)
	if cut.status != 200 {
		t.Fatalf("status %d, want the 200 of the head", cut.status)
	}
	if bytes.HasSuffix(cut.raw, lastChunk) {
		t.Fatalf("the cut answer ends with the last chunk")
	}
	if !errors.Is(cut.bodyErr, io.ErrUnexpectedEOF) {
		t.Fatalf("the body read of the cut answer returned %v, want an unexpected EOF", cut.bodyErr)
	}
	// the positive twin: the next export fails no open and ends whole
	whole := rawGet(t, a.addr, target)
	if whole.status != 200 || whole.bodyErr != nil || !bytes.HasSuffix(whole.raw, lastChunk) {
		t.Fatalf("the whole export: %d, body error %v, last chunk %v", whole.status, whole.bodyErr, bytes.HasSuffix(whole.raw, lastChunk))
	}
	if rows := readZip(t, whole.body); len(rows) != 3 {
		t.Fatalf("the whole export holds %d entries", len(rows))
	}
}

// A maven2 group folder whose maven-metadata.xml the merge cannot read:
// a GET of that path answers 404, so the archive leaves the entry out and
// holds every other file whole.
func TestExportGroupLeavesOutMetadataTheMergeRefuses(t *testing.T) {
	folder := "com/example/demo-lib"
	blobs := []testkit.BlobSpec{
		spec("demo-maven", folder+"/1.0.0/demo-lib-1.0.0.jar", []byte("jar"), "application/java-archive"),
		spec("demo-maven", folder+"/maven-metadata.xml", []byte("<metadata><unclosed>"), "application/xml"),
		spec("demo-maven", folder+"/zz-last.txt", []byte("after the metadata"), "text/plain"),
	}
	defs := []*pyjson.Object{def("demo-maven", "maven2", "hosted"), def("demo-group", "maven2", "group", "demo-maven")}
	a := startExport(t, exportOptions{blobs: blobs, defs: defs})
	status(t, a.get("/repository/demo-group/"+folder+"/maven-metadata.xml"), 404)
	got := rawGet(t, a.addr, exportTarget("demo-group", "path", folder))
	if got.status != 200 || got.bodyErr != nil {
		t.Fatalf("export of the group: %d, body error %v", got.status, got.bodyErr)
	}
	rows := readZip(t, got.body)
	want := []string{folder + "/zz-last.txt", folder + "/1.0.0/demo-lib-1.0.0.jar"}
	if fmt.Sprint(entryNames(rows)) != fmt.Sprint(want) {
		t.Fatalf("entries %q, want %q", entryNames(rows), want)
	}
	for _, r := range rows {
		file := a.get("/repository/demo-group/" + r.name)
		if file.status != 200 || !bytes.Equal(file.body, r.data) {
			t.Errorf("%s differs from the GET of the group (%d)", r.name, file.status)
		}
	}
	// the positive twin: the member itself stores the document and exports it
	if rows := readZip(t, a.get(exportTarget("demo-maven", "path", folder)).body); len(rows) != 3 {
		t.Fatalf("the export of the member holds %d entries", len(rows))
	}
}

// A client that stops reading loses its export once a write waits longer
// than the stall time, and its place is free again.
func TestExportStalledReadersLoseTheirPlaces(t *testing.T) {
	a := startExport(t, exportOptions{blobs: largeBlobs(t), defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}, stall: 500 * time.Millisecond})
	target := exportTarget("demo-raw", "path", "big")
	slow := []net.Conn{openStream(t, a.addr, target, 1024), openStream(t, a.addr, target, 1024)}
	defer func() {
		for _, c := range slow {
			c.Close()
		}
	}()
	waitWithin(t, "two streams", 5*time.Second, func() bool { return len(a.srv.exports.slots) == 2 })
	waitWithin(t, "the places of two clients that read nothing to free", 10*time.Second, func() bool { return len(a.srv.exports.slots) == 0 })
	status(t, a.get(exportTarget("demo-raw", "path", "small")), 200)
}

// The positive twin: a client that reads slowly but without a pause of the
// stall time gets the whole archive, although the export lasts many times
// the stall time.
func TestExportSlowReaderKeepsItsPlace(t *testing.T) {
	const stall = 400 * time.Millisecond
	a := startExport(t, exportOptions{blobs: largeBlobs(t), defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}, stall: stall})
	conn, err := net.DialTimeout("tcp", a.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", exportTarget("demo-raw", "path", "big"), a.addr)
	started := time.Now()
	var raw bytes.Buffer
	chunk := make([]byte, 1<<20)
	for {
		time.Sleep(stall / 4)
		n, err := io.ReadAtLeast(conn, chunk, len(chunk))
		raw.Write(chunk[:n])
		if err != nil {
			break
		}
	}
	took := time.Since(started)
	if took < 4*stall {
		t.Fatalf("the read took %s only, not long enough to show the deadline renews", took)
	}
	resp, err := http.ReadResponse(bufio.NewReader(&raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || err != nil {
		t.Fatalf("the slow read: %d, body error %v after %s", resp.StatusCode, err, took)
	}
	if rows := readZip(t, body); len(rows) != 200 {
		t.Fatalf("the slow read holds %d entries", len(rows))
	}
}

// The dry call takes no place and answers 200, and it says whether both
// places are taken, so the dialog can show the wait before the download
// starts: a download that meets 429 fails in the browser without a page
// that could show it.
func TestExportDryCallTellsBothPlacesTaken(t *testing.T) {
	a := startExport(t, exportOptions{blobs: largeBlobs(t), defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}})
	dry := exportTarget("demo-raw", "path", "small", "dry", "1")
	// the positive twin first: with free places busy is false
	if got := dryOf(t, a.get(dry)); got["busy"] != false || got["retry_after"] != nil {
		t.Fatalf("dry with free places: %v", got)
	}
	target := exportTarget("demo-raw", "path", "big")
	slow := []net.Conn{openStream(t, a.addr, target, 1024), openStream(t, a.addr, target, 1024)}
	waitWithin(t, "two streams", 5*time.Second, func() bool { return len(a.srv.exports.slots) == 2 })
	if got := dryOf(t, a.get(dry)); got["busy"] != true || got["retry_after"] != json.Number("30") {
		t.Fatalf("dry with both places taken: %v", got)
	}
	if len(a.srv.exports.slots) != 2 {
		t.Fatalf("the dry call changed the places: %d taken", len(a.srv.exports.slots))
	}
	for _, c := range slow {
		c.Close()
	}
	waitWithin(t, "free places", 5*time.Second, func() bool { return len(a.srv.exports.slots) == 0 })
	if got := dryOf(t, a.get(dry)); got["busy"] != false {
		t.Fatalf("dry after the streams ended: %v", got)
	}
}

// openStreamAs is openStream with Basic credentials "user:password".
func openStreamAs(t *testing.T, addr, target, credentials string, n int) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Basic %s\r\n\r\n", target, addr, base64.StdEncoding.EncodeToString([]byte(credentials)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", err, resp)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	return conn
}

// One account streams one export at a time: a second export of the same
// subject answers 429 with a message that says so, and its dry call says
// busy with own_export. The twin: another account exports side by side, and
// the first account exports again once its stream ended.
func TestExportOneStreamPerSubject(t *testing.T) {
	sec := newFakeSecurity()
	sec.addBasic("alice:pw", subject("alice", "", "nexus:repository-view:raw:demo-raw:browse,read"))
	sec.addBasic("bob:pw", subject("bob", "", "nexus:repository-view:raw:demo-raw:browse,read"))
	a := startExport(t, exportOptions{security: sec, blobs: largeBlobs(t), defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}})
	small := exportTarget("demo-raw", "path", "small")
	dry := exportTarget("demo-raw", "path", "small", "dry", "1")
	held := openStreamAs(t, a.addr, exportTarget("demo-raw", "path", "big"), "alice:pw", 1024)
	waitWithin(t, "the stream of alice", 5*time.Second, func() bool { return len(a.srv.exports.slots) == 1 })
	second := a.get(small, "basic", "alice:pw")
	if second.status != 429 || !strings.Contains(string(second.body), "one export at a time per account") || second.header.Get("Retry-After") != "30" {
		t.Fatalf("a second export of alice: %d %s", second.status, second.body)
	}
	if got := dryOf(t, a.get(dry, "basic", "alice:pw")); got["busy"] != true || got["own_export"] != true || got["retry_after"] != json.Number("30") {
		t.Fatalf("dry of alice while her export streams: %v", got)
	}
	other := a.get(small, "basic", "bob:pw")
	status(t, other, 200)
	if rows := readZip(t, other.body); len(rows) != 1 {
		t.Fatalf("the export of bob holds %d entries", len(rows))
	}
	if got := dryOf(t, a.get(dry, "basic", "bob:pw")); got["busy"] != false || got["own_export"] != false {
		t.Fatalf("dry of bob: %v", got)
	}
	held.Close()
	waitWithin(t, "the place of alice to free", 5*time.Second, func() bool { return len(a.srv.exports.slots) == 0 })
	status(t, a.get(small, "basic", "alice:pw"), 200)
}

// An export that runs past its time limit breaks its transfer as any
// failure after the head, with one log line that names the limit. The
// twin: an export that ends within the limit is whole.
func TestExportTimeLimitBreaksTheTransfer(t *testing.T) {
	logs := captureLog(t)
	const stall = 400 * time.Millisecond
	a := startExport(t, exportOptions{blobs: largeBlobs(t), defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")},
		stall: stall, limit: time.Second})
	whole := rawGet(t, a.addr, exportTarget("demo-raw", "path", "big"))
	if whole.status != 200 || whole.bodyErr != nil || !bytes.HasSuffix(whole.raw, lastChunk) {
		t.Fatalf("an export within its time limit: %d, body error %v", whole.status, whole.bodyErr)
	}
	if rows := readZip(t, whole.body); len(rows) != 200 {
		t.Fatalf("the export within its limit holds %d entries", len(rows))
	}
	conn, err := net.DialTimeout("tcp", a.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", exportTarget("demo-raw", "path", "big"), a.addr)
	started := time.Now()
	var raw bytes.Buffer
	chunk := make([]byte, 1<<20)
	for {
		time.Sleep(stall / 4)
		n, err := io.ReadAtLeast(conn, chunk, len(chunk))
		raw.Write(chunk[:n])
		if err != nil {
			break
		}
	}
	took := time.Since(started)
	resp, err := http.ReadResponse(bufio.NewReader(&raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, bodyErr := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || bodyErr == nil || bytes.HasSuffix(raw.Bytes(), lastChunk) {
		t.Fatalf("an export past its time limit: %d, body error %v after %s", resp.StatusCode, bodyErr, took)
	}
	if took > 10*time.Second {
		t.Fatalf("the slow export ended after %s, its limit is 1 s", took)
	}
	var lines []string
	for _, line := range logs.all() {
		if strings.Contains(line, "time limit") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "ended early") {
		t.Fatalf("log lines on the time limit: %q", lines)
	}
}
