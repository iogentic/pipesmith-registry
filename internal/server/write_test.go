package server

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/docker"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// oldAnswer is an answer of 0.1.1 (a70b733) to a write below /repository/,
// as refuseWrite gives it.
type oldAnswer struct {
	status            int
	contentType, body string
	allow             string
}

var (
	refusedFile   = oldAnswer{405, textType, "Method not allowed: this server is read only\n", "GET, HEAD"}
	refusedPush   = oldAnswer{405, jsonType, string(docker.ErrorBody("UNSUPPORTED", "this registry is read only: push and delete are refused")), "GET, HEAD"}
	refusedNpmDir = oldAnswer{404, jsonType, npmNotFoundBody, ""}
)

// writeCase is one write target: on the main port, or on the Docker port
// of docker when that is set.
type writeCase struct {
	docker, target string
	want           oldAnswer
}

var writeCases = []writeCase{
	{"", "/repository/demo-raw/docs/readme.txt", refusedFile},
	{"", "/repository/demo-raw/docs/new.txt", refusedFile},
	{"", "/repository/demo-raw", refusedFile},
	{"", "/repository/demo-raw/", refusedFile},
	{"", "/repository/demo-maven/" + jarPath, refusedFile},
	{"", "/repository/demo-maven/com/example/demo-lib/maven-metadata.xml", refusedFile},
	{"", "/repository/demo-group/" + jarPath, refusedFile},
	{"", "/repository/demo-npm/widget", refusedFile},
	{"", "/repository/demo-npm/@demo%2fwidget", refusedFile},
	{"", "/repository/demo-npm/widget/-/widget-1.2.3.tgz", refusedFile},
	{"", "/repository/demo-npm/-/user/org.couchdb.user:demo", refusedNpmDir},
	{"", "/repository/demo-npm/-/v1/login", refusedNpmDir},
	{"", "/repository/demo-docker/v2/app/manifests/1.0", refusedPush},
	{"", "/repository/demo-docker/v2/app/blobs/uploads/", refusedPush},
	{"", "/repository/demo-docker/docs/x.txt", refusedFile},
	{"", "/repository/missing/x.txt", refusedFile},
	{"demo-docker", "/v2/app/manifests/2.0", refusedPush},
	{"demo-docker", "/v2/app/blobs/uploads/", refusedPush},
	{"demo-docker", "/v2/app/blobs/uploads/abc?digest=sha256:0123", refusedPush},
	{"demo-docker", "/v2/", refusedPush},
}

var writeMethods = []string{"PUT", "POST", "DELETE", "PATCH"}

// tokenRoute is a write case that a route of tokens answers with security
// on, writes on or off: npm login is PUT .../-/user/org.couchdb.user:<name>
// (tokens.go, tokens_test.go).
func tokenRoute(c writeCase, method string) bool {
	return method == http.MethodPut && c.docker == "" && strings.Contains(c.target, "/-/user/org.couchdb.user:")
}

// startWriting starts the server of startSecured with writes on or off.
func startWriting(t *testing.T, security model.Security, writes bool) *app {
	t.Helper()
	blobs := []testkit.BlobSpec{
		spec("demo-maven", jarPath, []byte("jar bytes"), "application/java-archive"),
		spec("demo-npm", "widget/-/widget-1.2.3.tgz", []byte("tarball"), "application/x-tgz"),
		spec("demo-raw", "docs/readme.txt", []byte("readme"), "text/plain"),
	}
	blobs = append(blobs, blobsOf("demo-docker", dockerImage("app", "1.0", []byte("layer")))...)
	base := t.TempDir()
	stores := []config.StoreConfig{blobStore(t, base, "store-a", blobs...)}
	opts := appOptions{defs: []*pyjson.Object{def("demo-maven", "maven2", "hosted"), def("demo-npm", "npm", "hosted"),
		def("demo-raw", "raw", "hosted"), def("demo-group", "maven2", "group", "demo-maven"), dockerDef("demo-docker", 14690)},
		dockerRepos: []string{"demo-docker"}, security: security, writes: writes}
	return startApp(t, base, prepare(t, base, stores, opts), opts)
}

// writeTo sends one write of a case with a body of seven bytes.
func (a *app) writeTo(c writeCase, method string, headers ...string) *answer {
	a.t.Helper()
	addr := a.addr()
	if c.docker != "" {
		addr = a.docker[c.docker].Listener.Addr().String()
	}
	return do(a.t, addr, method, c.target, []byte("payload"), headers...)
}

// The write entry of routes is the dispatch (seam Write dispatch).
func TestWriteRouteIsTheDispatch(t *testing.T) {
	want := reflect.ValueOf((*Server).write).Pointer()
	found := 0
	for _, rt := range routes {
		if slices.Contains(rt.methods, "PUT") {
			found++
			if reflect.ValueOf(rt.handle).Pointer() != want || rt.rule.kind != ruleClient || rt.pattern.String() != `^/repository/(.*)\n?$` ||
				!slices.Equal(rt.methods, writeMethods) {
				t.Fatalf("the write route is %v %v %v", rt.pattern, rt.methods, rt.rule)
			}
		}
	}
	if found != 1 {
		t.Fatalf("%d write routes", found)
	}
}

// With writes off every write method answers as in 0.1.1, security off.
func TestWritesOffAnswerAsBefore(t *testing.T) {
	a := startWriting(t, nil, false)
	for _, c := range writeCases {
		for _, method := range writeMethods {
			r := a.writeTo(c, method)
			if r.status != c.want.status || r.header.Get("Content-Type") != c.want.contentType || string(r.body) != c.want.body ||
				r.header.Get("Allow") != c.want.allow {
				t.Errorf("%s %s%s: %d %q %v, want %+v", method, c.docker, c.target, r.status, r.body, r.header, c.want)
			}
		}
	}
}

// With writes on the four handlers of the formats still answer the refusal
// of the port: every answer of an administrator equals the one with writes
// off, byte for byte and header for header.
func TestWriteStubsKeepTheRefusal(t *testing.T) {
	off := startWriting(t, newFakeSecurity(), false)
	on := startWriting(t, newFakeSecurity(), true)
	if off.srv.writes.writer != nil || on.srv.writes.writer == nil {
		t.Fatal("the writes switch")
	}
	for _, c := range writeCases {
		if builtWrite(c) {
			continue
		}
		for _, method := range writeMethods {
			if servedByOCI(c) {
				continue
			}
			before := off.writeTo(c, method, "basic", asAdmin)
			after := on.writeTo(c, method, "basic", asAdmin)
			before.header.Del("Date")
			after.header.Del("Date")
			if before.status != after.status || string(before.body) != string(after.body) || !reflect.DeepEqual(before.header, after.header) {
				t.Errorf("%s %s%s: writes on %d %q %v, off %d %q %v", method, c.docker, c.target,
					after.status, after.body, after.header, before.status, before.body, before.header)
			}
			if (before.status != c.want.status || string(before.body) != c.want.body) && !tokenRoute(c, method) {
				t.Errorf("%s %s%s: %d %q, want %+v", method, c.docker, c.target, before.status, before.body, c.want)
			}
		}
	}
	if r := on.writeTo(writeCase{target: "/repository/demo-raw/docs/readme.txt"}, http.MethodGet, "basic", asAdmin); r.status != 200 ||
		string(r.body) != "readme" {
		t.Fatalf("GET with writes on: %d %q", r.status, r.body)
	}
}
