package server

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// builtWrite reports a write case of write_test.go whose repository has the
// handler of its format now (leg 4b: raw and maven2), so that it no longer
// answers the refusal of the port with writes on.
func builtWrite(c writeCase) bool {
	return c.docker == "" && (strings.HasPrefix(c.target, "/repository/demo-raw/") || strings.HasPrefix(c.target, "/repository/demo-maven/"))
}

// A raw PUT answers 201 and the read path serves the bytes; a DELETE
// answers 204 and then 404; other methods answer 405 with the methods of
// the repository.
func TestRawPutAndDelete(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	put := do(t, w.addr(), "PUT", "/repository/demo-raw/docs/hello.txt", []byte("hello raw"), "basic", asAdmin)
	if put.status != 201 || len(put.body) != 0 {
		t.Fatalf("PUT: %d %q", put.status, put.body)
	}
	got := do(t, w.addr(), "GET", "/repository/demo-raw/docs/hello.txt", nil, "basic", asAdmin)
	if got.status != 200 || string(got.body) != "hello raw" || got.header.Get("ETag") != `"`+sha1Hex([]byte("hello raw"))+`"` {
		t.Fatalf("GET: %d %v %q", got.status, got.header, got.body)
	}
	row, err := w.srv.rp.index.Lookup("demo-raw", "docs/hello.txt")
	if err != nil || row == nil || row.Store != "local-write" {
		t.Fatalf("%+v %v", row, err)
	}
	if props := w.properties(row.Location); props["@BlobStore.created-by"] != "demo-admin" || props["@BlobStore.created-by-ip"] != "127.0.0.1" {
		t.Fatalf("%q", props)
	}
	del := do(t, w.addr(), "DELETE", "/repository/demo-raw/docs/hello.txt", nil, "basic", asAdmin)
	if del.status != 204 || len(del.body) != 0 || del.header.Get("Content-Length") != "" {
		t.Fatalf("DELETE: %d %v %q", del.status, del.header, del.body)
	}
	for _, method := range []string{"GET", "DELETE"} {
		if r := do(t, w.addr(), method, "/repository/demo-raw/docs/hello.txt", nil, "basic", asAdmin); r.status != 404 {
			t.Fatalf("%s after the delete: %d %q", method, r.status, r.body)
		}
	}
	for _, method := range []string{"POST", "PATCH"} {
		r := do(t, w.addr(), method, "/repository/demo-raw/docs/hello.txt", []byte("x"), "basic", asAdmin)
		if r.status != 405 || r.header.Get("Allow") != "GET, HEAD, PUT, DELETE" {
			t.Fatalf("%s: %d %v", method, r.status, r.header)
		}
	}
	if files := w.workFiles(); len(files) != 0 {
		t.Fatalf("work files %v", files)
	}
}

// A write to the root or a folder of the repository, or to a path with a
// control character, is refused with 400 before a byte is read.
func TestRawWriteNeedsTheTargetOfAFile(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	for _, target := range []string{"/repository/demo-raw/", "/repository/demo-raw/docs/", "/repository/demo-raw/docs/a%01b.txt",
		"/repository/demo-raw/docs/a%7Fb.txt"} {
		for _, method := range []string{"PUT", "DELETE"} {
			if r := do(t, w.addr(), method, target, []byte("x"), "basic", asAdmin); r.status != 400 {
				t.Errorf("%s %s: %d %q", method, target, r.status, r.body)
			}
		}
	}
	if counts, _ := w.scan(); counts.Live != 0 {
		t.Fatalf("%+v", counts)
	}
}

// A refusal of ingest or erase is answered with its status and text.
func TestRawRefusalsAnswerTheirStatus(t *testing.T) {
	old := spec("demo-raw", "docs/readme.txt", []byte("readme"), "text/plain")
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW_ONCE"), "demo-npm": storage("ALLOW")}, old)
	r := do(t, w.addr(), "PUT", "/repository/demo-raw/docs/readme.txt", []byte("again"), "basic", asAdmin)
	if r.status != 400 || string(r.body) != "the repository does not allow updating assets\n" {
		t.Fatalf("redeploy: %d %q", r.status, r.body)
	}
	r = do(t, w.addr(), "DELETE", "/repository/demo-raw/docs/readme.txt", nil, "basic", asAdmin)
	if r.status != 409 || !strings.Contains(string(r.body), "store-a, which is read only") {
		t.Fatalf("delete in the old store: %d %q", r.status, r.body)
	}
}

// rawRequest sends the head of a PUT with a declared length and the first
// part of its body on a connection of its own.
func rawRequest(t *testing.T, addr, target string, declared int, part []byte) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte(asAdmin))
	fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: localhost\r\nAuthorization: Basic %s\r\nContent-Length: %d\r\n\r\n", target, auth, declared)
	if _, err := conn.Write(part); err != nil {
		t.Fatal(err)
	}
	return conn
}

// waitFor polls a condition for at most 10 s.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatalf("no %s within 10 s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Review Focus 2 of the execution plan for PUT: a client that leaves after
// half the declared length leaves no blob, no row and no work file.
func TestRawPutOfACutBody(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW_ONCE")})
	conn := rawRequest(t, w.addr(), "/repository/demo-raw/docs/cut.bin", 1<<20, make([]byte, 1<<19))
	conn.Close()
	waitFor(t, "access line of the PUT", func() bool { return strings.Contains(w.access.String(), "PUT /repository/demo-raw/docs/cut.bin") })
	if files := w.workFiles(); len(files) != 0 {
		t.Fatalf("work files %v", files)
	}
	if row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/cut.bin"); row != nil {
		t.Fatalf("row %+v", row)
	}
	if counts, _ := w.scan(); counts.Live != 0 {
		t.Fatalf("%+v", counts)
	}
	if r := do(t, w.addr(), "PUT", "/repository/demo-raw/docs/cut.bin", []byte("whole"), "basic", asAdmin); r.status != 201 {
		t.Fatalf("the PUT after: %d %q", r.status, r.body)
	}
}

// A body that stops arriving is given up after the chunk timeout with 408;
// nothing of it stays.
func TestRawPutOfAStalledBody(t *testing.T) {
	chunkTimeout.Store(int64(300 * time.Millisecond))
	t.Cleanup(func() { chunkTimeout.Store(0) })
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	conn := rawRequest(t, w.addr(), "/repository/demo-raw/docs/stalled.bin", 1<<20, []byte("first bytes"))
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestTimeout || !strings.Contains(string(body), "did not arrive in full") {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	if files := w.workFiles(); len(files) != 0 {
		t.Fatalf("work files %v", files)
	}
	if row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/stalled.bin"); row != nil {
		t.Fatalf("row %+v", row)
	}
}
