package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

const jarURL = "/repository/demo-releases/" + jarPath

func jarApp(t *testing.T, content string) (*app, string) {
	t.Helper()
	base := t.TempDir()
	store := blobStore(t, base, "s1", jarSpec(content))
	cfg := prepare(t, base, []config.StoreConfig{store}, appOptions{defs: []*pyjson.Object{def("demo-releases", "maven2", "hosted")}})
	return startApp(t, base, cfg, appOptions{}), store.Root
}

func TestGetAndHeadFile(t *testing.T) {
	content := "jar bytes of the demo library"
	a, _ := jarApp(t, content)
	get := a.get(jarURL)
	expect(t, get, 200)
	if string(get.body) != content {
		t.Fatalf("body %q", get.body)
	}
	h := get.header
	if h.Get("Content-Type") != "application/java-archive" || h.Get("Content-Length") != fmt.Sprint(len(content)) ||
		h.Get("Last-Modified") != "Tue, 14 Nov 2023 22:13:20 GMT" || h.Get("ETag") != `"`+testkit.SHA1Hex([]byte(content))+`"` ||
		h.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("headers %v", h)
	}
	head := a.head(jarURL)
	expect(t, head, 200)
	if len(head.body) != 0 {
		t.Fatalf("HEAD body %q", head.body)
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Last-Modified", "ETag", "Accept-Ranges"} {
		if head.header.Get(name) != h.Get(name) {
			t.Errorf("HEAD %s %q, GET %q", name, head.header.Get(name), h.Get(name))
		}
	}
}

func TestContentTypeAndEtagForms(t *testing.T) {
	base := t.TempDir()
	pom := spec("demo-releases", "com/example/x/1.0/x-1.0.pom", []byte("<project/>"), "")
	tree := treeStore(t, base, "tree", map[string][]byte{
		"demo-raw/data.bin": []byte("12345"), "demo-raw/sum.bin": []byte("12345"),
		"demo-raw/sum.bin.sha1": []byte(testkit.SHA1Hex([]byte("12345"))),
	})
	a := newApp(t, []config.StoreConfig{blobStore(t, base, "s1", pom), tree},
		appOptions{defs: []*pyjson.Object{def("demo-releases", "maven2", "hosted"), def("demo-raw", "raw", "hosted")}})
	r := a.get("/repository/demo-releases/com/example/x/1.0/x-1.0.pom")
	expect(t, r, 200)
	if r.header.Get("Content-Type") != "application/xml" {
		t.Fatalf("content type %q", r.header.Get("Content-Type"))
	}
	weak := a.get("/repository/demo-raw/data.bin")
	if !regexp.MustCompile(`^W/"5-\d+"$`).MatchString(weak.header.Get("ETag")) {
		t.Fatalf("etag %q", weak.header.Get("ETag"))
	}
	strong := a.get("/repository/demo-raw/sum.bin")
	if strong.header.Get("ETag") != `"`+testkit.SHA1Hex([]byte("12345"))+`"` {
		t.Fatalf("etag %q", strong.header.Get("ETag"))
	}
}

func TestNotFoundAnswers(t *testing.T) {
	a, _ := jarApp(t, "x")
	r := a.get("/repository/no-such-repo/anything.txt")
	expect(t, r, 404)
	if string(r.body) != "Repository no-such-repo not found\n" || r.header.Get("Content-Type") != "text/plain; charset=utf-8" ||
		r.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%s", r)
	}
	missing := a.get("/repository/demo-releases/com/example/nope/1.0/nope-1.0.jar")
	expect(t, missing, 404)
	if string(missing.body) != "Not found\n" {
		t.Fatalf("%s", missing)
	}
	head := a.head("/repository/demo-releases/com/example/nope/1.0/nope-1.0.jar")
	expect(t, head, 404)
	if head.header.Get("Content-Length") != "10" {
		t.Fatalf("HEAD 404 length %q", head.header.Get("Content-Length"))
	}
}

func TestConditionalRequests(t *testing.T) {
	a, _ := jarApp(t, "jar bytes")
	first := a.get(jarURL)
	etag, modified := first.header.Get("ETag"), first.header.Get("Last-Modified")
	for _, c := range []struct {
		headers []string
		status  int
	}{
		{[]string{"If-None-Match", etag}, 304},
		{[]string{"If-None-Match", "*"}, 304},
		{[]string{"If-None-Match", `"other", ` + etag}, 304},
		{[]string{"If-None-Match", "W/" + etag}, 304},
		{[]string{"If-None-Match", `"0000000000000000000000000000000000000000"`}, 200},
		{[]string{"If-Modified-Since", modified}, 304},
		{[]string{"If-Modified-Since", "Wed, 15 Nov 2023 22:13:20 GMT"}, 304},
		{[]string{"If-Modified-Since", "Mon, 13 Nov 2023 22:13:20 GMT"}, 200},
		{[]string{"If-None-Match", `"other"`, "If-Modified-Since", modified}, 200},
	} {
		r := a.get(jarURL, c.headers...)
		expect(t, r, c.status)
		if c.status == 304 {
			if len(r.body) != 0 || r.header.Get("ETag") != etag || r.header.Get("Last-Modified") != modified ||
				r.header.Get("Content-Type") != "" || r.header.Get("Content-Length") != "" || r.header.Get("Accept-Ranges") != "bytes" {
				t.Fatalf("%v: %s", c.headers, r)
			}
		} else if string(r.body) != "jar bytes" {
			t.Fatalf("%v: %s", c.headers, r)
		}
	}
	expect(t, a.head(jarURL, "If-None-Match", etag), 304)
}

func TestRanges(t *testing.T) {
	base := t.TempDir()
	a := newApp(t, []config.StoreConfig{blobStore(t, base, "s1", spec("demo-raw", "ten.bin", []byte("0123456789"), "application/octet-stream"))},
		appOptions{defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}})
	url := "/repository/demo-raw/ten.bin"
	for _, c := range []struct {
		value, body, contentRange string
		status                    int
	}{
		{"bytes=0-3", "0123", "bytes 0-3/10", 206},
		{"bytes=4-", "456789", "bytes 4-9/10", 206},
		{"bytes=-4", "6789", "bytes 6-9/10", 206},
		{"bytes=5-100", "56789", "bytes 5-9/10", 206},
		{"bytes=-100", "0123456789", "bytes 0-9/10", 206},
		{"bytes=20-30", "", "bytes */10", 416},
		{"bytes=-0", "", "bytes */10", 416},
		{"bytes=0-1,4-5", "0123456789", "", 200},
		{"bytes=5-2", "0123456789", "", 200},
		{"items=0-1", "0123456789", "", 200},
		{"bytes=x-y", "0123456789", "", 200},
		{"bytes", "0123456789", "", 200},
	} {
		r := a.get(url, "Range", c.value)
		expect(t, r, c.status)
		if string(r.body) != c.body || r.header.Get("Content-Range") != c.contentRange {
			t.Fatalf("%s: %s", c.value, r)
		}
		if c.status == 206 && r.header.Get("Content-Length") != fmt.Sprint(len(c.body)) {
			t.Fatalf("%s: length %q", c.value, r.header.Get("Content-Length"))
		}
		if c.status == 416 && (r.header.Get("Accept-Ranges") != "bytes" || r.header.Get("Content-Length") != "0" ||
			r.header.Get("ETag") != "" || r.header.Get("Content-Type") != "") {
			t.Fatalf("416 headers %v", r.header)
		}
	}
	// int() of more than 4300 digits raises ValueError, which _send does not catch
	for _, value := range []string{"bytes=0-" + strings.Repeat("9", 4301), "bytes=-" + strings.Repeat("9", 4301),
		"bytes=" + strings.Repeat("9", 4301) + "-"} {
		r := a.get(url, "Range", value)
		if r.status != 500 || string(r.body) != "Internal Server Error" {
			t.Fatalf("%.20s: %s", value, r)
		}
	}
	if r := a.get(url, "Range", "bytes=0-"+strings.Repeat("9", 4300)); r.status != 206 || string(r.body) != "0123456789" {
		t.Fatalf("4300 digits: %s", r)
	}
	head := a.head(url, "Range", "bytes=2-5")
	expect(t, head, 206)
	if len(head.body) != 0 || head.header.Get("Content-Range") != "bytes 2-5/10" || head.header.Get("Content-Length") != "4" {
		t.Fatalf("%s", head)
	}
	first := a.get(url)
	for _, c := range []struct {
		ifRange string
		status  int
	}{
		{first.header.Get("ETag"), 206},
		{first.header.Get("Last-Modified"), 206},
		{`"0000000000000000000000000000000000000000"`, 200},
		{"W/" + first.header.Get("ETag"), 200},
		{"Mon, 01 Jan 1990 00:00:00 GMT", 200},
	} {
		r := a.get(url, "Range", "bytes=0-3", "If-Range", c.ifRange)
		expect(t, r, c.status)
	}
}

func TestOverflowingDateIsAServerError(t *testing.T) {
	a, _ := jarApp(t, "x")
	r := a.get(jarURL, "If-Modified-Since", "Tue, 14 Nov 2023 22:13:20 +99999999999999")
	expect(t, r, 500)
	if string(r.body) != "Internal Server Error" || r.header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("%s", r)
	}
	expect(t, a.get(jarURL), 200)
}

func TestWritesAreRefused(t *testing.T) {
	a, root := jarApp(t, "x")
	before, err := testkit.TreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"PUT", "POST", "DELETE", "PATCH"} {
		for _, url := range []string{jarURL, "/repository/demo-releases/com/example/new-file.jar"} {
			r := raw(t, a.addr(), method, url, "Content-Length", "7")
			expect(t, r, 405)
			if r.header.Get("Allow") != "GET, HEAD" || string(r.body) != "Method not allowed: this server is read only\n" ||
				r.header.Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Fatalf("%s %s: %s", method, url, r)
			}
		}
	}
	after, _ := testkit.TreeDigest(root)
	if after != before {
		t.Fatal("the store changed")
	}
	expect(t, a.get(jarURL), 200)
	options := raw(t, a.addr(), "OPTIONS", jarURL)
	expect(t, options, 405)
	if options.header.Get("Allow") != "GET, HEAD" || string(options.body) != `{"detail":"Method Not Allowed"}` {
		t.Fatalf("%s", options)
	}
}

func TestUndefinedAndProxyRepositories(t *testing.T) {
	base := t.TempDir()
	orphan := spec("orphan-repo", "docs/a.txt", []byte("orphan content"), "text/plain")
	cached := spec("central-proxy", "com/example/cached/1.0/cached-1.0.pom", []byte("<project/>"), "application/xml")
	proxy := def("central-proxy", "maven2", "proxy").Set("remote_url", "https://repo.example.com/maven2/")
	a := newApp(t, []config.StoreConfig{blobStore(t, base, "s1", orphan, cached)}, appOptions{defs: []*pyjson.Object{proxy}})
	r := a.get("/repository/orphan-repo/docs/a.txt")
	expect(t, r, 200)
	if string(r.body) != "orphan content" {
		t.Fatalf("%s", r)
	}
	expect(t, a.get("/repository/central-proxy/com/example/cached/1.0/cached-1.0.pom"), 200)
	expect(t, a.get("/repository/central-proxy/com/example/other/1.0/other-1.0.pom"), 404)
}

func TestVanishedFileDropsTheRow(t *testing.T) {
	a, root := jarApp(t, "x")
	if err := os.Remove(filepath.Join(root, testkit.BytesLocation(jarSpec("x")))); err != nil {
		t.Fatal(err)
	}
	expect(t, a.head(jarURL), 404)
	expect(t, a.get(jarURL), 404)
	r := a.get("/api/asset/demo-releases?path=" + jarPath)
	expect(t, r, 404)
	if string(r.body) != `{"error":"`+jarPath+` not found in demo-releases"}` {
		t.Fatalf("%s", r)
	}
	expect(t, a.get(jarURL+".sha1"), 404)
}

func TestChangedSizeGivesWeakEtag(t *testing.T) {
	content := "jar bytes of the demo library"
	a, root := jarApp(t, content)
	f, err := os.OpenFile(filepath.Join(root, testkit.BytesLocation(jarSpec(content))), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(" and appended bytes")
	f.Close()
	r := a.get(jarURL)
	expect(t, r, 200)
	if string(r.body) != content+" and appended bytes" || !strings.HasPrefix(r.header.Get("ETag"), "W/") ||
		r.header.Get("Content-Length") != fmt.Sprint(len(r.body)) {
		t.Fatalf("%s", r)
	}
	expect(t, a.get(jarURL+".sha1"), 404)
}

func TestEncodedPaths(t *testing.T) {
	base := t.TempDir()
	tree := treeStore(t, base, "tree", map[string][]byte{
		"demo-raw/a b+c.txt": []byte("plus and space"), "demo-raw/a b.txt": []byte("spaced"),
		"demo-raw/etc/hosts": []byte("inside"), "demo-raw/big.bin": bytes.Repeat([]byte("0123456789"), 1000),
	})
	if err := os.WriteFile(filepath.Join(base, "secret"), []byte("outside the store"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "secret"), filepath.Join(tree.Root, "demo-raw", "link.txt")); err != nil {
		t.Fatal(err)
	}
	a := newApp(t, []config.StoreConfig{tree}, appOptions{defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}})
	for _, url := range []string{"/repository/demo-raw/a%20b%2Bc.txt", "/repository/demo-raw/a%20b+c.txt"} {
		r := a.get(url)
		expect(t, r, 200)
		if string(r.body) != "plus and space" {
			t.Fatalf("%s", r)
		}
	}
	expect(t, a.get("/repository/demo-raw/a%20b.txt"), 200)
	expect(t, a.get("/repository/demo-raw/a%2520b.txt"), 404)
	for _, url := range []string{
		"/repository/demo-raw/%2e%2e/secret", "/repository/demo-raw/a/%00/b", "/repository/demo-raw/a%2f..%2fsecret.txt",
		"/repository/demo-raw/a//b", "/repository/demo-raw/%2e%2e/%2e%2e/secret", "/repository/demo-raw//etc/hosts",
		"/repository/demo-raw/%2Fetc/hosts", "/repository/demo-raw/etc/%2e/hosts", "/repository/demo-raw/../secret",
		"/repository/demo-raw/./big.bin", "/repository/../../../../etc/passwd", "/repository/%2e%2e/%2e%2e/%2e%2e/etc/passwd",
	} {
		r := a.get(url)
		expect(t, r, 400)
		if !strings.HasPrefix(string(r.body), "Bad request path: ") || bytes.Contains(r.body, []byte("outside")) {
			t.Fatalf("%s: %s", url, r)
		}
	}
	notUTF8 := a.get("/repository/demo-raw/%ff")
	expect(t, notUTF8, 400)
	if string(notUTF8.body) != "Bad request path: not UTF-8\n" {
		t.Fatalf("%s", notUTF8)
	}
	expect(t, a.get("/repository/..%5c..%5c..%5cetc%5cpasswd"), 404)
	link := a.get("/repository/demo-raw/link.txt")
	expect(t, link, 404)
	if bytes.Contains(link.body, []byte("outside")) {
		t.Fatal("the link was followed")
	}
	expect(t, a.get("/repository/demo-raw/etc/hosts"), 200)
	ranged := a.get("/repository/demo-raw/big.bin", "Range", "bytes=1000-1999")
	expect(t, ranged, 206)
	if string(ranged.body) != strings.Repeat("0123456789", 100) {
		t.Fatal("range of the big file")
	}
	// a request target outside visible ASCII is refused as uvicorn's parser refuses it
	odd := a.get("/repository/demo-raw/\xc3\xa4")
	expect(t, odd, 400)
	if string(odd.body) != "Invalid HTTP request received." {
		t.Fatalf("%s", odd)
	}
}

func TestUnusualBlobNameAndNewestStore(t *testing.T) {
	base := t.TempDir()
	odd := spec("demo-raw", "dir:x/a=b/c d/e\\f/ä.txt", []byte("escapes"), "application/octet-stream")
	older := spec("demo-raw", "same/path.txt", []byte("older content"), "text/plain")
	newer := spec("demo-raw", "same/path.txt", []byte("newer content"), "text/plain")
	newer.CreatedMS = createdMS + 60_000
	storeA := blobStore(t, base, "a", older, odd)
	storeB := blobStore(t, base, "b", newer)
	a := newApp(t, []config.StoreConfig{storeB, storeA}, appOptions{defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}})
	r := a.get("/repository/demo-raw/dir%3Ax/a%3Db/c%20d/e%5Cf/%C3%A4.txt")
	expect(t, r, 200)
	if string(r.body) != "escapes" {
		t.Fatalf("%s", r)
	}
	r = a.get("/repository/demo-raw/same/path.txt")
	if string(r.body) != "newer content" {
		t.Fatalf("%s", r)
	}
	if err := os.Remove(filepath.Join(storeB.Root, testkit.BytesLocation(newer))); err != nil {
		t.Fatal(err)
	}
	r = a.get("/repository/demo-raw/same/path.txt")
	expect(t, r, 200)
	if string(r.body) != "older content" {
		t.Fatalf("%s", r)
	}
	record := a.get("/api/asset/demo-raw?path=same/path.txt")
	expect(t, record, 200)
	var asset map[string]any
	if err := json.Unmarshal(record.body, &asset); err != nil || asset["store"] != "a" {
		t.Fatalf("%s %v", record, err)
	}
}

func TestStatusAndHealthRoutes(t *testing.T) {
	a := newApp(t, nil, appOptions{noScan: true})
	r := a.get("/api/health")
	expect(t, r, 200)
	if string(r.body) != `{"status":"ok"}` || r.header.Get("Content-Type") != "application/json" || r.header.Get("Content-Length") != "15" {
		t.Fatalf("%s", r)
	}
	status := a.get("/service/rest/v1/status")
	expect(t, status, 200)
	if len(status.body) != 0 || status.header.Get("Content-Length") != "0" || status.header.Get("Content-Type") != "" {
		t.Fatalf("%s", status)
	}
	writable := a.get("/service/rest/v1/status/writable")
	expect(t, writable, 503)
	if writable.header.Get("Content-Length") != "0" {
		t.Fatalf("%s", writable)
	}
	head := a.head("/api/health")
	expect(t, head, 405)
	if head.header.Get("Allow") != "GET" || head.header.Get("Content-Length") != "31" {
		t.Fatalf("%s", head)
	}
	post := raw(t, a.addr(), "POST", "/service/rest/v1/status")
	expect(t, post, 405)
	if string(post.body) != `{"detail":"Method Not Allowed"}` || post.header.Get("Allow") != "GET" {
		t.Fatalf("%s", post)
	}
	// the Python router takes a final newline as the end of the path
	expect(t, a.get("/api/health%0A"), 200)
	expect(t, a.get("/%61pi/health"), 200)
	slash := a.get("/api/health/")
	expect(t, slash, 307)
	if slash.header.Get("Location") != "http://"+a.addr()+"/api/health" || slash.header.Get("Content-Length") != "0" {
		t.Fatalf("%s", slash)
	}
	root := a.get("/repository?x=1")
	expect(t, root, 307)
	if root.header.Get("Location") != "http://"+a.addr()+"/repository/?x=1" {
		t.Fatalf("%s", root)
	}
	quoted := a.get("/api/asset/a%20b/?path=x%20y")
	expect(t, quoted, 307)
	if quoted.header.Get("Location") != "http://"+a.addr()+"/api/asset/a%20b?path=x%20y" {
		t.Fatalf("%s", quoted)
	}
}

func TestRedirects(t *testing.T) {
	a, _ := jarApp(t, "x")
	r := a.get("/repository/demo-releases")
	expect(t, r, 302)
	if r.header.Get("Location") != "/repository/demo-releases/" || r.header.Get("Content-Length") != "0" {
		t.Fatalf("%s", r)
	}
	expect(t, a.get("/repository/no-such-repo"), 404)
	dir := a.get("/repository/demo-releases/com/example")
	expect(t, dir, 302)
	if dir.header.Get("Location") != "/repository/demo-releases/com/example/" {
		t.Fatalf("%s", dir)
	}
	expect(t, a.get("/repository/"), 404)
}

func TestUnavailableStoreKeepsRows(t *testing.T) {
	a, root := jarApp(t, "x")
	moved := filepath.Join(filepath.Dir(root), "unmounted")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	r := a.get(jarURL)
	expect(t, r, 503)
	if string(r.body) != "Store s1 is not available\n" {
		t.Fatalf("%s", r)
	}
	expect(t, a.get("/api/asset/demo-releases?path="+jarPath), 200)
	if err := os.Rename(moved, root); err != nil {
		t.Fatal(err)
	}
	expect(t, a.get(jarURL), 200)
}

func TestEmptyMountPointKeepsRows(t *testing.T) {
	for _, kind := range []string{"nexus-file", "directory"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			var store config.StoreConfig
			var emptied, url, repo, path string
			if kind == "nexus-file" {
				store = blobStore(t, base, "s1", jarSpec("x"))
				emptied, url, repo, path = filepath.Join(store.Root, "content"), jarURL, "demo-releases", jarPath
			} else {
				store = treeStore(t, base, "tree", map[string][]byte{"demo-raw/docs/a.txt": []byte("a")})
				emptied, url, repo, path = store.Root, "/repository/demo-raw/docs/a.txt", "demo-raw", "docs/a.txt"
			}
			a := newApp(t, []config.StoreConfig{store}, appOptions{defs: []*pyjson.Object{def("demo-releases", "maven2", "hosted"),
				def("demo-raw", "raw", "hosted")}})
			away := filepath.Join(base, "away")
			os.Mkdir(away, 0o755)
			entries, _ := os.ReadDir(emptied)
			for _, e := range entries {
				os.Rename(filepath.Join(emptied, e.Name()), filepath.Join(away, e.Name()))
			}
			expect(t, a.get(url), 503)
			expect(t, a.get("/api/asset/"+repo+"?path="+path), 200)
			entries, _ = os.ReadDir(away)
			for _, e := range entries {
				os.Rename(filepath.Join(away, e.Name()), filepath.Join(emptied, e.Name()))
			}
			expect(t, a.get(url), 200)
		})
	}
}

func TestHostGuard(t *testing.T) {
	a, _ := jarApp(t, "x")
	for _, host := range []string{"evil.example", "evil.example:80", "localhost.evil", "127.0.0.2"} {
		for _, url := range []string{"/api/health", jarURL, "/nope"} {
			r := a.get(url, "Host", host)
			expect(t, r, 403)
			if string(r.body) != "Forbidden: the Host header does not name this server\n" ||
				r.header.Get("Content-Type") != "text/plain; charset=utf-8" || r.header.Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("%s %s: %s", host, url, r)
			}
		}
	}
	for _, host := range []string{"localhost", "LOCALHOST:1", "127.0.0.1:99999", "[::1]:80"} {
		expect(t, a.get("/api/health", "Host", host), 200)
	}
	expect(t, raw(t, a.addr(), "GET", "/api/health", "Host", "\x00none"), 400) // HTTP/1.1 needs a Host
}

func TestChecksumsFromTheIndex(t *testing.T) {
	base := t.TempDir()
	content := []byte("jar bytes without a stored checksum file")
	first := spec("demo-releases", jarPath, []byte("jar bytes in the first member"), "application/java-archive")
	second := spec("demo-snapshots", jarPath, []byte("jar bytes in the second member"), "application/java-archive")
	secondSum := spec("demo-snapshots", jarPath+".sha1", []byte(testkit.SHA1Hex(second.Content)), "text/plain")
	stored := spec("demo-other", jarPath, content, "application/java-archive")
	storedSum := spec("demo-other", jarPath+".sha1", []byte("0123456789abcdef0123456789abcdef01234567  demo-lib-1.0.0.jar"), "text/plain")
	old := spec("old-repo", jarPath, content, "application/java-archive")
	a := newApp(t, []config.StoreConfig{blobStore(t, base, "s1", first, second, secondSum, stored, storedSum, old),
		treeStore(t, base, "tree", map[string][]byte{"demo-plain/" + jarPath: []byte("plain")})},
		appOptions{defs: []*pyjson.Object{def("demo-releases", "maven2", "hosted"), def("demo-snapshots", "maven2", "hosted"),
			def("demo-other", "maven2", "hosted"), def("demo-plain", "maven2", "hosted"),
			def("demo-group", "maven2", "group", "demo-releases", "demo-snapshots")}})
	r := a.get("/repository/demo-releases/" + jarPath + ".sha1")
	expect(t, r, 200)
	sum := testkit.SHA1Hex(first.Content)
	if string(r.body) != sum || r.header.Get("Content-Type") != "text/plain" || r.header.Get("Content-Length") != "40" ||
		r.header.Get("ETag") != `"`+testkit.SHA1Hex([]byte(sum))+`"` || r.header.Get("Last-Modified") == "" {
		t.Fatalf("%s", r)
	}
	head := a.head("/repository/demo-releases/" + jarPath + ".sha1")
	if head.status != 200 || len(head.body) != 0 || head.header.Get("Content-Length") != "40" {
		t.Fatalf("%s", head)
	}
	expect(t, a.get("/repository/demo-releases/"+jarPath+".sha1", "If-None-Match", r.header.Get("ETag")), 304)
	if string(a.get("/repository/demo-other/"+jarPath+".sha1").body) != string(storedSum.Content) {
		t.Fatal("the stored checksum does not win")
	}
	expect(t, a.get("/repository/demo-plain/"+jarPath+".sha1"), 404)
	for _, suffix := range []string{"md5", "sha256", "sha512"} {
		expect(t, a.get("/repository/demo-releases/"+jarPath+"."+suffix), 404)
	}
	if string(a.get("/repository/old-repo/"+jarPath+".sha1").body) != testkit.SHA1Hex(content) {
		t.Fatal("undefined repository")
	}
	group := a.get("/repository/demo-group/" + jarPath + ".sha1")
	if string(group.body) != sum {
		t.Fatalf("group checksum %s", group)
	}
}

func TestRunningServerFollowsTheDataDirectory(t *testing.T) {
	base := t.TempDir()
	one := treeStore(t, base, "one", map[string][]byte{"demo-raw/a.txt": []byte("from one")})
	cfg := prepare(t, base, []config.StoreConfig{one}, appOptions{})
	a := startApp(t, base, cfg, appOptions{})
	expect(t, a.get("/repository/demo-raw/a.txt"), 200)
	expect(t, a.get("/repository/demo-new/c.txt"), 404)
	two := treeStore(t, base, "two", map[string][]byte{"demo-raw/b.txt": []byte("from two"), "demo-new/c.txt": []byte("new")})
	cfg.Stores = append(cfg.Stores, two)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	rescan(t, cfg)
	if r := a.get("/repository/demo-new/c.txt"); r.status != 200 || string(r.body) != "new" {
		t.Fatalf("%s", r)
	}
	if err := repos.WriteDefinitions(cfg.RepositoriesPath(), []*pyjson.Object{def("demo-raw", "docker", "hosted")}); err != nil {
		t.Fatal(err)
	}
	r := a.get("/repository/demo-raw/a.txt")
	expect(t, r, 404)
	if string(r.body) != "Repository demo-raw has format docker; it answers the registry API under /repository/demo-raw/v2/ and on its Docker port\n" {
		t.Fatalf("%s", r)
	}
	os.WriteFile(cfg.ConfigPath(), []byte("{not json"), 0o644)
	os.WriteFile(cfg.RepositoriesPath(), []byte("{not json"), 0o644)
	expect(t, a.get("/repository/demo-new/c.txt"), 200)
	expect(t, a.get("/repository/demo-raw/a.txt"), 404)
}

func TestAccessLine(t *testing.T) {
	a, _ := jarApp(t, "x")
	a.get("/repository/demo-releases/a%20b?x=%20")
	a.head(jarURL, "Range", "bytes=0-0")
	a.get(jarURL, "Range", "bytes=5-")
	lines := strings.Split(strings.TrimSpace(a.access.String()), "\n")
	pattern := regexp.MustCompile(`^INFO:     127\.0\.0\.1:\d+ - "(\w+) (\S+) HTTP/1\.1" (\d{3}) (.+)$`)
	want := [][]string{
		{"GET", "/repository/demo-releases/a%20b?x=%20", "404", "Not Found"},
		{"HEAD", jarURL, "206", "Partial Content"},
		{"GET", jarURL, "416", "Requested Range Not Satisfiable"},
	}
	if len(lines) != len(want) {
		t.Fatalf("access lines %q", lines)
	}
	for i, line := range lines {
		m := pattern.FindStringSubmatch(line)
		if m == nil || m[1] != want[i][0] || m[2] != want[i][1] || m[3] != want[i][2] || m[4] != want[i][3] {
			t.Errorf("access line %q, want %v", line, want[i])
		}
	}
}

func TestAssetRecord(t *testing.T) {
	base := t.TempDir()
	image := dockerImage("demo/app", "1.0", []byte("layer one"))
	blobs := append(blobsOf("demo-docker", image), jarSpec("jar"))
	a := newApp(t, []config.StoreConfig{blobStore(t, base, "s1", blobs...)}, appOptions{
		defs:        []*pyjson.Object{def("demo-releases", "maven2", "hosted"), dockerDef("demo-docker", 50100)},
		dockerRepos: []string{"demo-docker"},
	})
	r := a.get("/api/asset/demo-releases?path=" + jarPath)
	expect(t, r, 200)
	sha := testkit.SHA1Hex([]byte("jar"))
	want := `{"repo":"demo-releases","member":"demo-releases","path":"` + jarPath + `","name":"demo-lib-1.0.0.jar","size":3,` +
		`"sha1":"` + sha + `","content_type":"application/java-archive","created_ms":1700000000000,"store":"s1","access":"maven",` +
		`"download_url":"/repository/demo-releases/` + jarPath + `","maven":{"group_id":"com.example","artifact_id":"demo-lib",` +
		`"version":"1.0.0","classifier":null,"extension":"jar"},"docker":null,"npm":null}`
	if string(r.body) != want {
		t.Fatalf("asset\n%s\nwant\n%s", r.body, want)
	}
	manifest := image["v2/demo/app/manifests/1.0"].data
	r = a.get("/api/asset/demo-docker?path=v2/demo/app/manifests/1.0")
	expect(t, r, 200)
	var record map[string]any
	if err := json.Unmarshal(r.body, &record); err != nil {
		t.Fatal(err)
	}
	docker := record["docker"].(map[string]any)
	port := a.docker["demo-docker"].Listener.Addr().String()
	port = port[strings.LastIndex(port, ":")+1:]
	if docker["kind"] != "manifest" || docker["image"] != "demo/app" || docker["tag"] != "1.0" || docker["digest"] != digestOf(manifest) ||
		docker["port"] != float64(50100) || docker["pull"] != "docker pull 127.0.0.1:"+port+"/demo/app:1.0" ||
		record["download_url"] != "/repository/demo-docker/v2/demo/app/manifests/1.0" || record["maven"] != nil {
		t.Fatalf("%s", r.body)
	}
	blob := a.get("/api/asset/demo-docker?path=v2/-/blobs/" + digestOf([]byte("layer one")))
	if !strings.Contains(string(blob.body), `"docker":{"kind":"blob","image":null,"tag":null,"digest":"`+digestOf([]byte("layer one"))+`","port":50100,"pull":null}`) ||
		!strings.Contains(string(blob.body), `"download_url":null`) {
		t.Fatalf("%s", blob.body)
	}
	for _, c := range []struct {
		url, body string
		status    int
	}{
		{"/api/asset/no-such?path=a", `{"error":"repository no-such not found"}`, 404},
		{"/api/asset/demo-releases", `{"error":"the query parameter path is required"}`, 400},
		{"/api/asset/demo-releases?path=a/../b", `{"error":"the path holds an empty, '.' or '..' segment"}`, 400},
		{"/api/asset/demo-releases?path=a%00", `{"error":"the path holds a NUL byte"}`, 400},
		{"/api/asset/demo-releases?path=nope", `{"error":"nope not found in demo-releases"}`, 404},
	} {
		r := a.get(c.url)
		expect(t, r, c.status)
		if string(r.body) != c.body {
			t.Fatalf("%s: %s", c.url, r)
		}
	}
}
