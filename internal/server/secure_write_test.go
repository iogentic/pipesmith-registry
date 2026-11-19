package server

import (
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/rest"
)

// The permission of a write (seam Permission of a write of the phase 4
// execution plan): with writes on a PUT, POST or PATCH of a new path asks
// add, of a path the repository holds edit, a DELETE asks delete, and GET
// and HEAD ask read. With writes off every method asks read, as in 0.1.1.
func TestWritePermissions(t *testing.T) {
	const (
		asAdder   = "demo-adder:adder-pw"
		asEditor  = "demo-editor:editor-pw"
		asDeleter = "demo-deleter:deleter-pw"
		asFetcher = "demo-fetcher:fetcher-pw"
	)
	sec := newFakeSecurity()
	sec.addBasic(asAdder, subject("demo-adder", "", rest.ViewPermission("raw", "demo-raw", "add"),
		rest.ViewPermission("docker", "demo-docker", "add")))
	sec.addBasic(asEditor, subject("demo-editor", "", rest.ViewPermission("raw", "demo-raw", "edit")))
	sec.addBasic(asDeleter, subject("demo-deleter", "", rest.ViewPermission("raw", "demo-raw", "delete")))
	sec.addBasic(asFetcher, subject("demo-fetcher", "", rest.ViewPermission("raw", "demo-raw", "read")))
	on := startWriting(t, sec, true)
	off := startWriting(t, sec, false)
	held, fresh := "/repository/demo-raw/docs/readme.txt", "/repository/demo-raw/docs/new.txt"
	jar := "/repository/demo-maven/" + jarPath
	// A write past the check reaches the handler of its format: raw and
	// maven2 answer 400 for the write policy the fake state does not hold,
	// and 405 for a method they do not take (leg 4b).
	for _, c := range []struct {
		who, method, target string
		on, off             int
	}{
		{asAdder, "PUT", fresh, 400, 403},
		{asAdder, "POST", fresh, 405, 403},
		{asAdder, "PATCH", fresh, 405, 403},
		{asAdder, "PUT", held, 403, 403},
		{asAdder, "DELETE", held, 403, 403},
		{asAdder, "GET", held, 403, 403},
		{asEditor, "PUT", held, 400, 403},
		{asEditor, "POST", held, 405, 403},
		{asEditor, "PUT", fresh, 403, 403},
		{asDeleter, "DELETE", held, 400, 403},
		{asDeleter, "DELETE", fresh, 400, 403},
		{asDeleter, "PUT", fresh, 403, 403},
		{asFetcher, "GET", held, 200, 200},
		{asFetcher, "HEAD", held, 200, 200},
		{asFetcher, "PUT", fresh, 403, 405},
		{asFetcher, "PUT", held, 403, 405},
		{asFetcher, "DELETE", held, 403, 405},
		{asReader, "PUT", jar, 403, 405},
		{asReader, "GET", jar, 200, 200},
		{asAdmin, "PUT", jar, 400, 405},
		{asAdmin, "DELETE", "/repository/demo-group/" + jarPath, 405, 405},
		{asAdmin, "PUT", "/repository/missing/x.txt", 405, 405},
	} {
		for _, a := range []struct {
			name string
			app  *app
			want int
		}{{"on", on, c.on}, {"off", off, c.off}} {
			r := do(t, a.app.addr(), c.method, c.target, []byte("payload"), "basic", c.who)
			if r.status != a.want {
				t.Errorf("writes %s: %s %s as %s: %d %q, want %d", a.name, c.method, c.target, c.who, r.status, r.body, a.want)
				continue
			}
			if a.want == 403 && string(r.body) != denied+"\n" {
				t.Errorf("writes %s: %s %s as %s: %q", a.name, c.method, c.target, c.who, r.body)
			}
		}
	}
	// the Docker port: a push asks add of the repository of the port
	for _, c := range []struct {
		who  string
		want int
		code string
	}{{asAdder, 405, "UNSUPPORTED"}, {asReader, 403, "DENIED"}} {
		r := on.on("demo-docker", "POST", "/v2/app/blobs/uploads/", "Authorization", basicHeader(c.who))
		if r.status != c.want || errorCode(t, r) != c.code {
			t.Errorf("push as %s: %s", c.who, r)
		}
	}
}

// The anonymous user of anonymous access holds read rights only: with writes
// on a write asks for credentials, with writes off it answers as in 0.1.1.
func TestAnonymousWrites(t *testing.T) {
	sec := newFakeSecurity()
	sec.anonymous = true
	sec.anonymousPermissions = anonymousRights
	on := startWriting(t, sec, true)
	off := startWriting(t, sec, false)
	r := do(t, on.addr(), "PUT", "/repository/demo-raw/docs/new.txt", []byte("payload"))
	if r.status != 401 || !strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("writes on: %d %v %q", r.status, r.header, r.body)
	}
	if r := do(t, on.addr(), "GET", "/repository/demo-raw/docs/readme.txt", nil); r.status != 200 {
		t.Fatalf("read with writes on: %d", r.status)
	}
	if r := do(t, off.addr(), "PUT", "/repository/demo-raw/docs/new.txt", []byte("payload")); r.status != 405 {
		t.Fatalf("writes off: %d %q", r.status, r.body)
	}
}
