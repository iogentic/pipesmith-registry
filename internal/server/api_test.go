package server

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/index"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/scan"
	"github.com/mirosso25/pipesmith-registry/internal/stores"
	"github.com/mirosso25/pipesmith-registry/internal/testkit"
	"github.com/mirosso25/pipesmith-registry/internal/version"
)

// sendRequest sends one request with a body, the target and the headers exactly as
// given, and reads the answer. Host is the address of the server unless
// headers name another; a nil body sends no Content-Length.
func sendRequest(t *testing.T, addr, method, target string, body []byte, headers ...string) *response {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", method, target)
	hasHost := false
	for i := 0; i+1 < len(headers); i += 2 {
		if strings.EqualFold(headers[i], "Host") {
			hasHost = true
		}
		fmt.Fprintf(&b, "%s: %s\r\n", headers[i], headers[i+1])
	}
	if !hasHost {
		fmt.Fprintf(&b, "Host: %s\r\n", addr)
	}
	if body != nil {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	}
	b.WriteString("Connection: close\r\n\r\n")
	b.Write(body)
	if _, err := conn.Write(b.Bytes()); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(method, "http://"+addr+"/", nil)
	answer, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	defer answer.Body.Close()
	data, err := io.ReadAll(answer.Body)
	if err != nil {
		t.Fatal(err)
	}
	return &response{status: answer.StatusCode, header: answer.Header, body: data}
}

func (a *app) port() string {
	return a.addr()[strings.LastIndex(a.addr(), ":")+1:]
}

// localhost is the Host of the server as a browser on this machine names it.
func (a *app) localhost() string { return "localhost:" + a.port() }

func jsonText(t *testing.T, v any) string {
	t.Helper()
	data, err := pyjson.ResponseBody(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func decodeJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&out); err != nil {
		t.Fatalf("%v: %q", err, data)
	}
	return out
}

func expectBody(t *testing.T, r *response, status int, body string) {
	t.Helper()
	if r.status != status || string(r.body) != body {
		t.Fatalf("got %d %q, want %d %q", r.status, r.body, status, body)
	}
}

// The vectors of tools/vectors/server_api.py.

type apiVectors struct {
	Constants struct {
		PostHeader      string  `json:"post_header"`
		BrowseLimit     int     `json:"browse_limit"`
		MaxBrowseLimit  int     `json:"max_browse_limit"`
		ScanStopSeconds float64 `json:"scan_stop_seconds"`
		HTML            string  `json:"html"`
	} `json:"constants"`
	Page struct {
		SHA256  string            `json:"sha256"`
		Bytes   int               `json:"bytes"`
		Headers map[string]string `json:"headers"`
	} `json:"page"`
	PostRefusal []struct {
		Port    int         `json:"port"`
		Headers [][2]string `json:"headers"`
		Refusal *string     `json:"refusal"`
	} `json:"post_refusal"`
	Scan []struct {
		Name         string            `json:"name"`
		Kind         string            `json:"kind"`
		SnapshotKeys []string          `json:"snapshot_keys"`
		Idle         map[string]any    `json:"idle"`
		Keys         []string          `json:"keys"`
		Types        map[string]string `json:"types"`
		Values       map[string]any    `json:"values"`
	} `json:"scan"`
}

func loadAPIVectors(t *testing.T) *apiVectors {
	t.Helper()
	var v apiVectors
	if err := json.Unmarshal(mustRead(t, "../../testdata/vectors/server_api.json"), &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

func TestAPIConstants(t *testing.T) {
	v := loadAPIVectors(t)
	if v.Constants.PostHeader != postHeader || v.Constants.BrowseLimit != browseLimit || v.Constants.MaxBrowseLimit != maxBrowseLimit ||
		v.Constants.ScanStopSeconds != scanStopTimeout.Seconds() || v.Constants.HTML != htmlType {
		t.Fatalf("constants %+v", v.Constants)
	}
}

// Page

func TestPageVectors(t *testing.T) {
	v := loadAPIVectors(t)
	sum := sha256.Sum256(pageHTML)
	if hex.EncodeToString(sum[:]) != v.Page.SHA256 || len(pageHTML) != v.Page.Bytes {
		t.Fatalf("the embedded page is not the page of tools/vectors/server_api.py: %d bytes", len(pageHTML))
	}
	got := map[string]string{}
	for _, kv := range pageHeaders {
		got[kv[0]] = kv[1]
	}
	delete(got, "Content-Type")
	if len(got) != len(v.Page.Headers) {
		t.Fatalf("headers %v, want %v", got, v.Page.Headers)
	}
	for name, value := range v.Page.Headers {
		if got[name] != value {
			t.Errorf("%s %q, want %q", name, got[name], value)
		}
	}
}

func TestPageRoute(t *testing.T) {
	a := newApp(t, nil, appOptions{noScan: true})
	for _, target := range []string{"/", "/?x=1", "/%0A"} {
		r := a.get(target)
		expect(t, r, 200)
		if !bytes.Equal(r.body, pageHTML) || r.header.Get("Content-Type") != "text/html; charset=utf-8" ||
			r.header.Get("Content-Length") != fmt.Sprint(len(pageHTML)) || r.header.Get("X-Content-Type-Options") != "nosniff" ||
			r.header.Get("Referrer-Policy") != "no-referrer" || r.header.Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s: %d %v", target, r.status, r.header)
		}
		csp := r.header.Get("Content-Security-Policy")
		blocks := regexp.MustCompile(`(?s)<(?:script|style)>(.*?)</(?:script|style)>`).FindAllSubmatch(r.body, -1)
		if len(blocks) != 3 || !strings.HasPrefix(csp, "default-src 'none'; script-src 'sha256-") {
			t.Fatalf("%d blocks, policy %q", len(blocks), csp)
		}
		for _, block := range blocks {
			sum := sha256.Sum256(block[1])
			if !strings.Contains(csp, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
				t.Fatalf("policy %q lacks the hash of a block", csp)
			}
		}
	}
	page := string(pageHTML)
	for _, want := range []string{"<title>Pipesmith Registry</title>", "<h1>Pipesmith Registry</h1>", "build by mirosso25",
		`localStorage.getItem("pipesmith-registry.theme")`, `var THEME_KEY = "pipesmith-registry.theme";`,
		`"X-Artifact-Repository": "1"`, "./pipesmith-registry store add", "./pipesmith-registry repos port"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	for _, old := range []string{"Artifact Repository", "artifact-repository", "artifact_repository"} {
		if strings.Contains(page, old) {
			t.Errorf("the page still holds %q", old)
		}
	}
	head := a.head("/")
	expect(t, head, 405)
	if head.header.Get("Allow") != "GET" || head.header.Get("Content-Length") != "31" || len(head.body) != 0 {
		t.Fatalf("%s", head)
	}
	expectBody(t, a.get("//"), 404, `{"detail":"Not Found"}`)
	expectBody(t, a.get("/index.html"), 404, `{"detail":"Not Found"}`)
	expect(t, a.get("/", "Host", "evil.example"), 403)
}

// Status and repositories

func TestStatus(t *testing.T) {
	base := t.TempDir()
	s1 := blobStore(t, base, "s1",
		spec("demo-releases", "com/example/a/1.0/a-1.0.jar", []byte("12345"), "application/java-archive"),
		func() testkit.BlobSpec {
			s := spec("demo-releases", "shared.txt", []byte("old"), "text/plain")
			s.CreatedMS = 1_600_000_000_000
			return s
		}())
	tree := treeStore(t, base, "tree", map[string][]byte{"demo-raw/docs/readme.txt": []byte("hello"), "demo-releases/shared.txt": []byte("newer")})
	a := newApp(t, []config.StoreConfig{s1, tree}, appOptions{})
	r := a.get("/api/status")
	expect(t, r, 200)
	storeJSON := func(sc config.StoreConfig, available bool, files, size int) string {
		return fmt.Sprintf(`{"name":%s,"kind":%s,"root":%s,"repos":null,"manifest":null,"available":%t,"files":%d,"bytes":%d}`,
			jsonText(t, sc.Name), jsonText(t, sc.Kind), jsonText(t, sc.Root), available, files, size)
	}
	idle := `{"running":false,"store":null,"progress":null,"started_ms":null,"last":{}}`
	want := `{"name":"Pipesmith Registry","version":` + jsonText(t, version.Version) + `,"data_dir":` + jsonText(t, a.cfg.DataDir) +
		`,"bind":"127.0.0.1","port":` + a.port() + `,"docker_ports":[],"stores":[` + storeJSON(s1, true, 2, 8) + "," +
		storeJSON(tree, true, 2, 10) + `],"totals":{"stores":2,"repositories":2,"files":3,"bytes":15},"scan":` + idle + `}`
	if string(r.body) != want || r.header.Get("Content-Type") != "application/json" {
		t.Fatalf("status\n%s\nwant\n%s", r.body, want)
	}
	// a store whose root is gone keeps its rows and counts as unavailable
	if err := os.Rename(tree.Root, filepath.Join(base, "moved")); err != nil {
		t.Fatal(err)
	}
	r = a.get("/api/status")
	if !strings.Contains(string(r.body), storeJSON(tree, false, 2, 10)) {
		t.Fatalf("%s", r.body)
	}
	expect(t, a.head("/api/status"), 405)
	expect(t, a.get("/api/status", "Host", "evil.example"), 403)
}

func TestStatusStoreFields(t *testing.T) {
	base := t.TempDir()
	tree := treeStore(t, base, "tree", map[string][]byte{"demo-raw/a.txt": []byte("a"), "other/b.txt": []byte("b")})
	tree.Repos = []string{"demo-raw"}
	empty := treeStore(t, base, "empty", map[string][]byte{"demo-raw/c.txt": []byte("c")})
	empty.Repos = []string{}
	a := newApp(t, []config.StoreConfig{tree, empty}, appOptions{})
	r := a.get("/api/status")
	stores := decodeJSON(t, r.body)["stores"].([]any)
	first, second := stores[0].(map[string]any), stores[1].(map[string]any)
	if fmt.Sprint(first["repos"]) != "[demo-raw]" || first["files"] != json.Number("1") ||
		fmt.Sprint(second["repos"]) != "[]" || second["repos"] == nil || second["files"] != json.Number("0") {
		t.Fatalf("%s", r.body)
	}
}

func TestStatusDockerPorts(t *testing.T) {
	base := t.TempDir()
	a := newApp(t, []config.StoreConfig{blobStore(t, base, "s1", blobsOf("demo-docker", dockerImage("demo/app", "1.0", []byte("layer")))...)},
		appOptions{defs: []*pyjson.Object{dockerDef("demo-docker", 50100), dockerDef("demo-docker-old", 50101)},
			dockerRepos: []string{"demo-docker", "demo-docker-old"}})
	ports := map[string]string{}
	for repo, ts := range a.docker {
		addr := ts.Listener.Addr().String()
		ports[repo] = addr[strings.LastIndex(addr, ":")+1:]
	}
	first, second := "demo-docker", "demo-docker-old"
	if len(ports[first]) > len(ports[second]) || len(ports[first]) == len(ports[second]) && ports[first] > ports[second] {
		first, second = second, first
	}
	want := fmt.Sprintf(`"docker_ports":[{"port":%s,"repo":"%s"},{"port":%s,"repo":"%s"}]`, ports[first], first, ports[second], second)
	r := a.get("/api/status")
	if !strings.Contains(string(r.body), want) {
		t.Fatalf("%s\nwant %s", r.body, want)
	}
	// a Docker port answers the registry only
	r = a.on("demo-docker", "GET", "/api/status")
	expect(t, r, 404)
	if !strings.Contains(string(r.body), "UNSUPPORTED") {
		t.Fatalf("%s", r)
	}
}

func TestRepositories(t *testing.T) {
	base := t.TempDir()
	old := spec("old-repo", "x.txt", []byte("xy"), "text/plain")
	old.CreatedMS = 1_650_000_000_000
	s1 := blobStore(t, base, "s1", spec("demo-releases", jarPath, []byte("jar"), "application/java-archive"), old,
		spec("demo-docker", "v2/blobs/layer", []byte("layer"), ""))
	a := newApp(t, []config.StoreConfig{s1}, appOptions{defs: []*pyjson.Object{
		def("demo-releases", "maven2", "hosted"), dockerDef("demo-docker", 14555),
		def("demo-group", "maven2", "group", "demo-releases", "missing")}})
	r := a.get("/api/repositories")
	expect(t, r, 200)
	want := `{"repositories":[` +
		`{"name":"demo-docker","format":"docker","type":"hosted","defined":true,"access":"docker","docker_port":14555,"members":[],` +
		`"files":1,"bytes":5,"newest_ms":1700000000000,"version_policy":null,"layout_policy":null,"write_policy":null,"remote_url":null},` +
		`{"name":"demo-group","format":"maven2","type":"group","defined":true,"access":"maven","docker_port":null,` +
		`"members":["demo-releases","missing"],"files":0,"bytes":0,"newest_ms":null,"version_policy":null,"layout_policy":null,` +
		`"write_policy":null,"remote_url":null},` +
		`{"name":"demo-releases","format":"maven2","type":"hosted","defined":true,"access":"maven","docker_port":null,"members":[],` +
		`"files":1,"bytes":3,"newest_ms":1700000000000,"version_policy":null,"layout_policy":null,"write_policy":null,"remote_url":null},` +
		`{"name":"old-repo","format":"unknown","type":"hosted","defined":false,"access":"files","docker_port":null,"members":[],` +
		`"files":1,"bytes":2,"newest_ms":1650000000000,"version_policy":null,"layout_policy":null,"write_policy":null,"remote_url":null}]}`
	if string(r.body) != want {
		t.Fatalf("repositories\n%s\nwant\n%s", r.body, want)
	}
	expect(t, a.head("/api/repositories"), 405)
}

func TestRepositoriesTakeTheDockerPortOfTheStartConfiguration(t *testing.T) {
	base := t.TempDir()
	cfg := prepare(t, base, []config.StoreConfig{blobStore(t, base, "s1", spec("demo-docker", "v2/x", []byte("x"), ""))},
		appOptions{defs: []*pyjson.Object{dockerDef("demo-docker", 14555)}})
	cfg.DockerPorts = map[string]int{"demo-docker": 14556}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	a := startApp(t, base, cfg, appOptions{})
	if !strings.Contains(string(a.get("/api/repositories").body), `"docker_port":14556`) {
		t.Fatal("the configuration does not win over the definition")
	}
	// config.json changes while the server runs: the stores follow, the Docker ports of the start stay
	changed := *cfg
	changed.DockerPorts = map[string]int{"demo-docker": 14557}
	changed.Stores = append(cfg.Stores, treeStore(t, base, "tree", map[string][]byte{"demo-raw/a.txt": []byte("a")}))
	if err := config.Save(&changed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(a.get("/api/status").body), `"name":"tree"`) {
		t.Fatal("the stores do not follow config.json")
	}
	if !strings.Contains(string(a.get("/api/repositories").body), `"docker_port":14556`) {
		t.Fatal("the Docker ports of the start configuration do not stay")
	}
}

// Browse

func browseApp(t *testing.T) *app {
	t.Helper()
	base := t.TempDir()
	blobs := []testkit.BlobSpec{
		spec("demo-raw", "docs/readme.txt", []byte("hello"), "text/plain"),
		spec("demo-raw", "top.txt", []byte("top"), "text/plain"),
		spec("demo-releases", "com/example/a.txt", []byte("first"), "text/plain"),
		spec("demo-releases", "com/example/both.txt", []byte("from releases"), "text/plain"),
		spec("demo-snapshots", "com/example/both.txt", []byte("from snapshots, longer"), "text/plain"),
		spec("demo-snapshots", "com/example/z.txt", []byte("last"), "text/plain"),
		spec("demo-releases", "mixed/x/inner.txt", []byte("dir x"), "text/plain"),
		spec("demo-snapshots", "mixed/x", []byte("file x"), "text/plain"),
		spec("demo-snapshots", "mixed/w/inner.txt", []byte("dir w"), "text/plain"),
		spec("demo-releases", "mixed/w/other.txt", []byte("dir w"), "text/plain"),
	}
	for n := 0; n < 30; n++ {
		s := spec("demo-raw", fmt.Sprintf("bulk/file-%05d.txt", n), bytes.Repeat([]byte("b"), n%7+1), "text/plain")
		s.CreatedMS = createdMS + int64(n)
		blobs = append(blobs, s)
	}
	return newApp(t, []config.StoreConfig{blobStore(t, base, "s1", blobs...)}, appOptions{defs: []*pyjson.Object{
		def("demo-raw", "raw", "hosted"), def("demo-releases", "maven2", "hosted"), def("demo-snapshots", "maven2", "hosted"),
		def("demo-group", "maven2", "group", "demo-releases", "demo-snapshots"), def("demo-empty-group", "maven2", "group", "missing"),
		def("demo-nested", "maven2", "group", "demo-group", "demo-raw"),
	}})
}

type child struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	IsDir     bool   `json:"is_dir"`
	Size      *int64 `json:"size"`
	CreatedMS *int64 `json:"created_ms"`
}

type browsePage struct {
	Repo     string      `json:"repo"`
	Path     string      `json:"path"`
	Limit    json.Number `json:"limit"`
	Offset   json.Number `json:"offset"`
	More     bool        `json:"more"`
	Children []child     `json:"children"`
}

func (a *app) browse(query string) *browsePage {
	a.t.Helper()
	r := a.get("/api/browse/" + query)
	expect(a.t, r, 200)
	var page browsePage
	d := json.NewDecoder(bytes.NewReader(r.body))
	d.UseNumber()
	if err := d.Decode(&page); err != nil {
		a.t.Fatal(err)
	}
	return &page
}

func names(page *browsePage) string {
	var out []string
	for _, c := range page.Children {
		suffix := ""
		if c.IsDir {
			suffix = "/"
		}
		out = append(out, c.Name+suffix)
	}
	return strings.Join(out, " ")
}

func TestBrowse(t *testing.T) {
	a := browseApp(t)
	r := a.get("/api/browse/demo-raw?path=docs")
	expectBody(t, r, 200, `{"repo":"demo-raw","path":"docs","limit":500,"offset":0,"more":false,"children":[`+
		`{"name":"readme.txt","path":"docs/readme.txt","is_dir":false,"size":5,"created_ms":1700000000000}]}`)
	if r.header.Get("Content-Type") != "application/json" {
		t.Fatalf("%s", r)
	}
	root := `{"repo":"demo-raw","path":"","limit":500,"offset":0,"more":false,"children":[` +
		`{"name":"bulk","path":"bulk","is_dir":true,"size":null,"created_ms":null},` +
		`{"name":"docs","path":"docs","is_dir":true,"size":null,"created_ms":null},` +
		`{"name":"top.txt","path":"top.txt","is_dir":false,"size":3,"created_ms":1700000000000}]}`
	for _, query := range []string{"demo-raw", "demo-raw?path=", "demo-raw?path=/", "demo-raw?path=//", "demo-raw?limit=", "demo-raw?offset="} {
		expectBody(t, a.get("/api/browse/"+query), 200, root)
	}
	expectBody(t, a.get("/api/browse/demo-raw?path=/docs/"), 200, string(a.get("/api/browse/demo-raw?path=docs").body))
	// paging
	all := a.browse("demo-raw?path=bulk")
	if len(all.Children) != 30 || all.More || all.Children[0].Path != "bulk/file-00000.txt" || *all.Children[29].CreatedMS != createdMS+29 {
		t.Fatalf("%+v", all)
	}
	for _, c := range []struct {
		query string
		first string
		count int
		more  bool
	}{
		{"limit=7&offset=20", "file-00020.txt", 7, true},
		{"limit=10&offset=20", "file-00020.txt", 10, false},
		{"limit=11&offset=20", "file-00020.txt", 10, false},
		{"limit=1", "file-00000.txt", 1, true},
		{"offset=29", "file-00029.txt", 1, false},
		{"offset=30", "", 0, false},
		{"limit=10000", "file-00000.txt", 30, false},
		{"limit=%2B2", "file-00000.txt", 2, true},
		{"limit=%202%20", "file-00000.txt", 2, true},
		{"limit=1_0", "file-00000.txt", 10, true},
		{"limit=%D9%A1", "file-00000.txt", 1, true},
		{"limit=1&limit=3", "file-00000.txt", 3, true},
		{"offset=1&offset=0", "file-00000.txt", 30, false},
	} {
		page := a.browse("demo-raw?path=bulk&" + c.query)
		if len(page.Children) != c.count || page.More != c.more || c.count > 0 && page.Children[0].Name != c.first {
			t.Fatalf("%s: %+v", c.query, page)
		}
	}
	huge := a.get("/api/browse/demo-raw?offset=99999999999999999999999")
	expectBody(t, huge, 200, `{"repo":"demo-raw","path":"","limit":500,"offset":99999999999999999999999,"more":false,"children":[]}`)
	// the Docker and npm layouts are browsed as plain trees
	if names(a.browse("demo-raw?path=bulk&limit=2")) != "file-00000.txt file-00001.txt" {
		t.Fatal("names")
	}
}

func TestBrowseErrors(t *testing.T) {
	a := browseApp(t)
	segment := `{"error":"the path holds an empty, '.' or '..' segment"}`
	for _, c := range []struct {
		target, body string
		status       int
	}{
		{"/api/browse/nope", `{"error":"repository nope not found"}`, 404},
		{"/api/browse/demo%20raw", `{"error":"repository demo raw not found"}`, 404},
		{"/api/browse/x%FF", `{"error":"repository x` + "�" + ` not found"}`, 404},
		{"/api/browse/%C3%A9", `{"error":"repository é not found"}`, 404},
		{"/api/browse/demo-raw%0A", `{"error":"repository demo-raw\n not found"}`, 404},
		{"/api/browse/demo-raw?path=nowhere", `{"error":"nowhere is not a directory of demo-raw"}`, 404},
		{"/api/browse/demo-raw?path=docs/readme.txt", `{"error":"docs/readme.txt is not a directory of demo-raw"}`, 404},
		{"/api/browse/demo-raw?path=docs%0A", `{"error":"docs\n is not a directory of demo-raw"}`, 404},
		{"/api/browse/demo-raw?path=%FF", `{"error":"` + "�" + ` is not a directory of demo-raw"}`, 404},
		{"/api/browse/demo-raw?path=docs&path=nowhere", `{"error":"nowhere is not a directory of demo-raw"}`, 404},
		{"/api/browse/demo-raw?path=docs//x", segment, 400},
		{"/api/browse/demo-raw?path=../x", segment, 400},
		{"/api/browse/demo-raw?path=docs/..", segment, 400},
		{"/api/browse/demo-raw?path=./docs", segment, 400},
		{"/api/browse/demo-raw?path=a%00b", `{"error":"the path holds a NUL byte"}`, 400},
		{"/api/browse/demo-raw?limit=0", `{"error":"limit must be from 1 to 10000"}`, 400},
		{"/api/browse/demo-raw?limit=10001", `{"error":"limit must be from 1 to 10000"}`, 400},
		{"/api/browse/demo-raw?limit=-5", `{"error":"limit must be from 1 to 10000"}`, 400},
		{"/api/browse/demo-raw?limit=abc", `{"error":"limit is not a number"}`, 400},
		{"/api/browse/demo-raw?limit=1.5", `{"error":"limit is not a number"}`, 400},
		{"/api/browse/demo-raw?offset=-1", `{"error":"offset must be at least 0"}`, 400},
		{"/api/browse/demo-raw?offset=x", `{"error":"offset is not a number"}`, 400},
		// the parameters are checked before the repository
		{"/api/browse/nope?limit=0", `{"error":"limit must be from 1 to 10000"}`, 400},
		{"/api/browse/nope?path=..", segment, 400},
		{"/api/browse/nope?path=a&limit=0&offset=-1", `{"error":"limit must be from 1 to 10000"}`, 400},
		{"/api/browse/demo-raw?limit=0&path=..", segment, 400},
		{"/api/browse/demo-raw?offset=-1&limit=0", `{"error":"limit must be from 1 to 10000"}`, 400},
		{"/api/browse/demo-empty-group", `{"repo":"demo-empty-group","path":"","limit":500,"offset":0,"more":false,"children":[]}`, 200},
		{"/api/browse/demo-empty-group?path=a", `{"error":"a is not a directory of demo-empty-group"}`, 404},
	} {
		r := a.get(c.target)
		expectBody(t, r, c.status, c.body)
		if r.header.Get("Content-Type") != "application/json" {
			t.Fatalf("%s: %s", c.target, r)
		}
	}
	long := a.get("/api/browse/demo-raw?offset=" + strings.Repeat("9", 4301))
	expectBody(t, long, 400, `{"error":"offset is not a number"}`)
}

func TestBrowseGroupUnion(t *testing.T) {
	a := browseApp(t)
	if got := names(a.browse("demo-group")); got != "com/ mixed/" {
		t.Fatalf("root %q", got)
	}
	page := a.browse("demo-group?path=com/example")
	if got := names(page); got != "a.txt both.txt z.txt" {
		t.Fatalf("listing %q", got)
	}
	if *page.Children[1].Size != int64(len("from releases")) {
		t.Fatalf("the first member does not win: %+v", page.Children[1])
	}
	page = a.browse("demo-group?path=com/example&limit=2&offset=1")
	if names(page) != "both.txt z.txt" || page.More {
		t.Fatalf("%+v", page)
	}
	if page = a.browse("demo-group?path=com/example&limit=1"); names(page) != "a.txt" || !page.More {
		t.Fatalf("%+v", page)
	}
	if page = a.browse("demo-group?path=com/example&limit=2"); names(page) != "a.txt both.txt" || !page.More {
		t.Fatalf("%+v", page)
	}
	if page = a.browse("demo-group?path=com/example&offset=3"); len(page.Children) != 0 || page.More {
		t.Fatalf("%+v", page)
	}
	// a name that is a directory in one member and a file in the other shows twice, the directory first
	page = a.browse("demo-group?path=mixed")
	if got := names(page); got != "w/ x/ x" {
		t.Fatalf("mixed %q", got)
	}
	if page.Children[2].Path != "mixed/x" || *page.Children[2].Size != int64(len("file x")) || page.Children[1].Size != nil {
		t.Fatalf("%+v", page.Children)
	}
	// a group of a group and a hosted repository pages over the leaf members
	if got := names(a.browse("demo-nested")); got != "bulk/ com/ docs/ mixed/ top.txt" {
		t.Fatalf("nested %q", got)
	}
	r := a.get("/api/browse/demo-group?path=com/example&limit=1&offset=2")
	expectBody(t, r, 200, `{"repo":"demo-group","path":"com/example","limit":1,"offset":2,"more":false,"children":[`+
		`{"name":"z.txt","path":"com/example/z.txt","is_dir":false,"size":4,"created_ms":1700000000000}]}`)
}

// A group pages through the children of each member in pages of
// listingPage; the union crosses the page bounds.
func TestBrowseGroupAcrossPages(t *testing.T) {
	a := browseApp(t)
	defer func(old int) { listingPage = old }(listingPage)
	want := names(a.browse("demo-nested?path=bulk"))
	for _, size := range []int{1, 2, 3, 7} {
		listingPage = size
		if got := names(a.browse("demo-nested?path=bulk")); got != want {
			t.Fatalf("page size %d: %q, want %q", size, got, want)
		}
		if got := names(a.browse("demo-group?path=mixed")); got != "w/ x/ x" {
			t.Fatalf("page size %d: %q", size, got)
		}
		page := a.browse("demo-nested?path=bulk&offset=25&limit=4")
		if names(page) != "file-00025.txt file-00026.txt file-00027.txt file-00028.txt" || !page.More {
			t.Fatalf("page size %d: %+v", size, page)
		}
	}
}

// Scan

func scanApp(t *testing.T, count int) (*app, config.StoreConfig) {
	t.Helper()
	base := t.TempDir()
	var blobs []testkit.BlobSpec
	for n := 0; n < count; n++ {
		blobs = append(blobs, spec("demo-releases", fmt.Sprintf("com/example/file-%06d.txt", n), []byte("a"), "text/plain"))
	}
	store := blobStore(t, base, "s1", blobs...)
	a := newApp(t, []config.StoreConfig{store, treeStore(t, base, "tree", map[string][]byte{"demo-raw/a.txt": []byte("a")})},
		appOptions{defs: []*pyjson.Object{def("demo-releases", "maven2", "hosted")}})
	return a, store
}

func (a *app) post(body []byte, headers ...string) *response {
	a.t.Helper()
	return sendRequest(a.t, a.addr(), "POST", "/api/scan", body, headers...)
}

func (a *app) scanState() map[string]any {
	a.t.Helper()
	return decodeJSON(a.t, a.get("/api/status").body)["scan"].(map[string]any)
}

func (a *app) waitIdle() map[string]any {
	a.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if state := a.scanState(); state["running"] == false {
			return state
		}
		time.Sleep(5 * time.Millisecond)
	}
	a.t.Fatal("the scan did not end")
	return nil
}

func TestPostRefusalVectors(t *testing.T) {
	for _, c := range loadAPIVectors(t).PostRefusal {
		r, _ := http.NewRequest("POST", "http://x/api/scan", nil)
		r.Header = http.Header{}
		r.Host = ""
		hostSeen := false
		for _, kv := range c.Headers {
			// the vectors hold latin-1 text, a header carries its bytes
			raw := make([]byte, 0, len(kv[1]))
			for _, ch := range kv[1] {
				raw = append(raw, byte(ch))
			}
			if kv[0] == "host" {
				if !hostSeen {
					r.Host, hostSeen = string(raw), true
				}
				continue
			}
			r.Header.Add(kv[0], string(raw))
		}
		got, refused := postRefusal(r, c.Port, nil)
		if c.Refusal == nil {
			if refused {
				t.Errorf("%q: refused with %q", c.Headers, got)
			}
		} else if !refused || got != *c.Refusal {
			t.Errorf("%q: %q %t, want %q", c.Headers, got, refused, *c.Refusal)
		}
	}
}

func TestScanGuards(t *testing.T) {
	a, store := scanApp(t, 3)
	before, err := testkit.TreeDigest(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	port, other := a.port(), fmt.Sprint(mustAtoi(t, a.port())+1)
	good := []byte(`{"store": "s1"}`)
	nope := []byte(`{"store": "nope"}`)
	missing := `{"error":"the header X-Artifact-Repository: 1 is missing"}`
	notAttached := `{"error":"store nope is not attached"}`
	notJSON := `{"error":"the body is not JSON"}`
	needs := `{"error":"the body needs {\"store\": <name>}"}`
	h := []string{"X-Artifact-Repository", "1", "Host", a.localhost()}
	with := func(extra ...string) []string { return append(append([]string{}, h...), extra...) }
	for _, c := range []struct {
		name    string
		headers []string
		body    []byte
		status  int
		answer  string
	}{
		{"no header", []string{"Host", a.localhost()}, good, 403, missing},
		{"header 2", []string{"X-Artifact-Repository", "2", "Host", a.localhost()}, good, 403, missing},
		{"header 1 with spaces", []string{"X-Artifact-Repository", " 1 ", "Host", a.localhost()}, nope, 404, notAttached},
		{"header empty", []string{"X-Artifact-Repository", "", "Host", a.localhost()}, good, 403, missing},
		{"two headers", []string{"X-Artifact-Repository", "2", "X-Artifact-Repository", "1", "Host", a.localhost()}, good, 403, missing},
		{"two headers, 1 first", []string{"X-Artifact-Repository", "1", "X-Artifact-Repository", "2", "Host", a.localhost()}, nope, 404, notAttached},
		{"lower case name", []string{"x-artifact-repository", "1", "Host", a.localhost()}, nope, 404, notAttached},
		{"origin evil", with("Origin", "http://evil.example"), good, 403, `{"error":"the Origin 'http://evil.example' is not this server"}`},
		{"origin null", with("Origin", "null"), good, 403, `{"error":"the Origin 'null' is not this server"}`},
		{"origin other port", with("Origin", "http://localhost:"+other), good, 403, `{"error":"the Origin 'http://localhost:` + other + `' is not this server"}`},
		{"origin https", with("Origin", "https://localhost:"+port), good, 403, `{"error":"the Origin 'https://localhost:` + port + `' is not this server"}`},
		{"origin slash", with("Origin", "http://localhost:"+port+"/"), good, 403, `{"error":"the Origin 'http://localhost:` + port + `/' is not this server"}`},
		{"origin empty", with("Origin", ""), good, 403, `{"error":"the Origin '' is not this server"}`},
		{"origin latin-1", with("Origin", "http://\xc0\xe9\x85"), good, 403, `{"error":"the Origin 'http://Àé\\x85' is not this server"}`},
		{"origin upper", with("Origin", "HTTP://LOCALHOST:"+port), nope, 404, notAttached},
		{"origin ipv4", with("Origin", "http://127.0.0.1:"+port), nope, 404, notAttached},
		{"origin ipv6", with("Origin", "http://[::1]:"+port), nope, 404, notAttached},
		{"origin two, first evil", with("Origin", "evil", "Origin", "http://localhost:"+port), good, 403, `{"error":"the Origin 'evil' is not this server"}`},
		{"host evil", []string{"X-Artifact-Repository", "1", "Host", "evil.example:" + port}, good, 403, forbidden + "\n"},
		{"host bare", []string{"X-Artifact-Repository", "1", "Host", "localhost"}, good, 403, `{"error":"the Host 'localhost' is not this server"}`},
		{"host other port", []string{"X-Artifact-Repository", "1", "Host", "localhost:" + other}, good, 403, `{"error":"the Host 'localhost:` + other + `' is not this server"}`},
		{"host upper", []string{"X-Artifact-Repository", "1", "Host", "LOCALHOST:" + port}, nope, 404, notAttached},
		{"host ipv4", []string{"X-Artifact-Repository", "1", "Host", "127.0.0.1:" + port}, nope, 404, notAttached},
		{"host ipv6", []string{"X-Artifact-Repository", "1", "Host", "[::1]:" + port}, nope, 404, notAttached},
		{"host ipv6 other port", []string{"X-Artifact-Repository", "1", "Host", "[::1]:" + other}, good, 403, `{"error":"the Host '[::1]:` + other + `' is not this server"}`},
		{"body empty", h, []byte{}, 400, notJSON},
		{"no body", h, nil, 400, notJSON},
		{"body oops", h, []byte("{oops"), 400, notJSON},
		{"body trailing", h, []byte(`{"store": "nope"} x`), 400, notJSON},
		{"body bad utf-8", h, []byte("{\"store\": \"\xff\"}"), 400, notJSON},
		{"body long int", h, []byte(`{"store": "nope", "n": ` + strings.Repeat("9", 5000) + `}`), 400, notJSON},
		{"body list", h, []byte("[]"), 400, needs},
		{"body object empty", h, []byte("{}"), 400, needs},
		{"body number", h, []byte(`{"store": 5}`), 400, needs},
		{"body empty name", h, []byte(`{"store": ""}`), 400, needs},
		{"body string", h, []byte(`"s1"`), 400, needs},
		{"body null", h, []byte("null"), 400, needs},
		{"body nope", h, nope, 404, notAttached},
		{"body utf-16", h, []byte("\xff\xfe{\x00\"\x00s\x00t\x00o\x00r\x00e\x00\"\x00:\x00 \x00\"\x00n\x00o\x00p\x00e\x00\"\x00}\x00"), 404, notAttached},
		{"body with BOM", h, append([]byte("\xef\xbb\xbf"), nope...), 404, notAttached},
		{"body NaN", h, []byte(`{"store": "nope", "x": NaN}`), 404, notAttached},
		{"body last key wins", h, []byte(`{"store": "s1", "store": "nope"}`), 404, notAttached},
		{"body unicode name", h, []byte(`{"store": "née"}`), 404, `{"error":"store née is not attached"}`},
		{"body lone surrogate", h, []byte(`{"store": "\ud800"}`), 500, "Internal Server Error"},
		{"body too deep", h, append(bytes.Repeat([]byte("["), 100000), bytes.Repeat([]byte("]"), 100000)...), 500, "Internal Server Error"},
	} {
		r := a.post(c.body, c.headers...)
		if r.status != c.status || string(r.body) != c.answer {
			t.Errorf("%s: %d %q, want %d %q", c.name, r.status, r.body, c.status, c.answer)
		}
		if c.status != 500 && c.answer[0] == '{' && r.header.Get("Content-Type") != "application/json" {
			t.Errorf("%s: %v", c.name, r.header)
		}
	}
	// no refused request started a scan
	if state := a.scanState(); state["running"] != false || fmt.Sprint(state["last"]) != "map[]" {
		t.Fatalf("%v", state)
	}
	after, _ := testkit.TreeDigest(store.Root)
	if after != before {
		t.Fatal("the store changed")
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscan(s, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The guard takes the port the server listens on, also when the
// configuration asks for port 0.
func TestScanGuardTakesTheListeningPort(t *testing.T) {
	s := serveFiles(t)
	port := s.addr[strings.LastIndex(s.addr, ":")+1:]
	r := sendRequest(t, s.addr, "POST", "/api/scan", []byte(`{"store": "nope"}`), "X-Artifact-Repository", "1", "Host", "localhost:"+port)
	expectBody(t, r, 404, `{"error":"store nope is not attached"}`)
	r = sendRequest(t, s.addr, "POST", "/api/scan", []byte(`{"store": "nope"}`), "X-Artifact-Repository", "1", "Host", "localhost:0")
	expectBody(t, r, 403, `{"error":"the Host 'localhost:0' is not this server"}`)
	status := sendRequest(t, s.addr, "GET", "/api/status", nil, "Host", "localhost:"+port)
	if !strings.Contains(string(status.body), `"port":`+port+`,`) {
		t.Fatalf("%s", status.body)
	}
}

func TestScanRuns(t *testing.T) {
	a, store := scanApp(t, 1)
	expect(t, a.get("/repository/demo-releases/com/example/new.txt"), 404)
	expect(t, a.get("/repository/demo-new/x.txt"), 404)
	if _, err := testkit.MakeBlobStore(store.Root, []testkit.BlobSpec{
		spec("demo-releases", "com/example/file-000000.txt", []byte("a"), "text/plain"),
		spec("demo-releases", "com/example/new.txt", []byte("new blob"), "text/plain"),
		spec("demo-new", "x.txt", []byte("in a new repository"), "text/plain"),
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := testkit.TreeDigest(store.Root)
	started := time.Now().UnixMilli()
	r := a.post([]byte(`{"store": "s1"}`), "X-Artifact-Repository", "1", "Host", a.localhost(), "Origin", "http://"+a.localhost())
	expect(t, r, 202)
	if !regexp.MustCompile(`^\{"running":(true,"store":"s1","progress":\d+,"started_ms":\d{13}|false,"store":null,"progress":null,"started_ms":null),"last":\{`).Match(r.body) {
		t.Fatalf("%s", r.body)
	}
	state := a.waitIdle()
	last := state["last"].(map[string]any)["s1"].(map[string]any)
	if last["live"] != json.Number("3") || last["error"] != nil || last["store"] != "s1" || last["deleted"] != json.Number("0") {
		t.Fatalf("%v", last)
	}
	finished, err := last["finished_ms"].(json.Number).Int64()
	if err != nil || finished < started || finished > time.Now().UnixMilli() {
		t.Fatalf("finished_ms %v", last["finished_ms"])
	}
	if _, err := last["seconds"].(json.Number).Float64(); err != nil {
		t.Fatalf("seconds %v", last["seconds"])
	}
	if fmt.Sprint(last["hints"]) != "map[]" {
		t.Fatalf("hints %v", last["hints"])
	}
	if r := a.get("/repository/demo-releases/com/example/new.txt"); string(r.body) != "new blob" {
		t.Fatalf("%s", r)
	}
	if r := a.get("/repository/demo-new/x.txt"); string(r.body) != "in a new repository" {
		t.Fatalf("%s", r)
	}
	if !strings.Contains(string(a.get("/api/repositories").body), `"name":"demo-new"`) {
		t.Fatal("the new repository is not listed")
	}
	totals := decodeJSON(t, a.get("/api/status").body)["totals"].(map[string]any)
	if totals["files"] != json.Number("4") { // three in s1, one in tree
		t.Fatalf("%v", totals)
	}
	after, _ := testkit.TreeDigest(store.Root)
	if after != before {
		t.Fatal("the store changed")
	}
	// a second scan of the other store keeps the outcome of the first, in the order of the first scans
	expect(t, a.post([]byte(`{"store": "tree"}`), "X-Artifact-Repository", "1", "Host", a.localhost()), 202)
	a.waitIdle()
	expect(t, a.post([]byte(`{"store": "s1"}`), "X-Artifact-Repository", "1", "Host", a.localhost()), 202)
	a.waitIdle()
	status := string(a.get("/api/status").body)
	if !regexp.MustCompile(`"last":\{"s1":\{"live":3,.*\},"tree":\{"live":1,`).MatchString(status) {
		t.Fatalf("%s", status)
	}
}

func TestScanFailureIsReported(t *testing.T) {
	base := t.TempDir()
	tree := treeStore(t, base, "tree", map[string][]byte{"demo-raw/a.txt": []byte("a")})
	a := newApp(t, []config.StoreConfig{tree}, appOptions{})
	if err := os.Rename(tree.Root, filepath.Join(base, "moved")); err != nil {
		t.Fatal(err)
	}
	expect(t, a.post([]byte(`{"store": "tree"}`), "X-Artifact-Repository", "1", "Host", a.localhost()), 202)
	last := a.waitIdle()["last"].(map[string]any)["tree"].(map[string]any)
	if last["error"] != "store root "+tree.Root+" is not a directory" || last["live"] != nil || last["seconds"] != nil {
		t.Fatalf("%v", last)
	}
	stores := decodeJSON(t, a.get("/api/status").body)["stores"].([]any)
	if stores[0].(map[string]any)["files"] != json.Number("1") {
		t.Fatalf("%v", stores)
	}
}

// valueType is type(value).__name__ of the Python value a Go value stands for.
func valueType(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case int, int64:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case bool:
		return "bool"
	case map[string]string, *pyjson.Object:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

func TestScanOutcomeVectors(t *testing.T) {
	base := t.TempDir()
	cases := loadAPIVectors(t).Scan
	if len(cases) != 5 {
		t.Fatalf("%d cases", len(cases))
	}
	treeRoot, err := testkit.MakeTree(filepath.Join(base, "tree"), map[string][]byte{
		"demo-npm/demo-lib/-/demo-lib-1.0.0.tgz": []byte("tgz"), "demo-raw/a.txt": []byte("a"),
		"demo-releases/com/example/a/1.0/a-1.0.pom": []byte("<project/>")})
	if err != nil {
		t.Fatal(err)
	}
	gone := spec("demo-releases", "gone.txt", []byte("gone"), "")
	gone.Deleted = true
	blobStoreRoot, err := testkit.MakeBlobStore(filepath.Join(base, "blobs"), []testkit.BlobSpec{
		spec("demo-docker", "v2/-/blobs/sha256:"+strings.Repeat("0", 64), []byte("layer"), ""),
		spec("demo-releases", "com/example/a.txt", []byte("first"), ""), gone})
	if err != nil {
		t.Fatal(err)
	}
	type setup struct {
		store config.StoreConfig
		index string
		root  string
	}
	setups := map[string]setup{
		"directory":               {config.StoreConfig{Name: "tree", Kind: "directory", Root: treeRoot}, "tree", treeRoot},
		"directory without root":  {config.StoreConfig{Name: "tree", Kind: "directory", Root: filepath.Join(base, "gone")}, "tree", filepath.Join(base, "gone")},
		"blob store":              {config.StoreConfig{Name: "s1", Kind: "nexus-file", Root: blobStoreRoot}, "blob-store", blobStoreRoot},
		"blob store emptied":      {config.StoreConfig{Name: "s1", Kind: "nexus-file", Root: blobStoreRoot}, "blob-store", blobStoreRoot},
		"blob store without root": {config.StoreConfig{Name: "s1", Kind: "nexus-file", Root: filepath.Join(base, "missing")}, "missing", filepath.Join(base, "missing")},
	}
	indexes := map[string]*indexHandle{}
	for _, c := range cases {
		su, ok := setups[c.Name]
		if !ok || su.store.Kind != c.Kind {
			t.Fatalf("case %q", c.Name)
		}
		if c.Name == "blob store emptied" {
			content := filepath.Join(blobStoreRoot, "content")
			if err := os.RemoveAll(content); err != nil || os.Mkdir(content, 0o755) != nil {
				t.Fatal("emptying the store")
			}
		}
		ih := indexes[su.index]
		if ih == nil {
			ih = openIndex(t, filepath.Join(base, su.index+".sqlite"))
			indexes[su.index] = ih
		}
		store := mustStore(t, su.store)
		job := newScanJob(ih.ix, func() error { return nil })
		if !job.start(store) {
			t.Fatal("the job did not start")
		}
		deadline := time.Now().Add(30 * time.Second)
		for running(job.snapshot()) {
			if time.Now().After(deadline) {
				t.Fatal("the job did not end")
			}
			time.Sleep(time.Millisecond)
		}
		snapshot := job.snapshot()
		if strings.Join(snapshot.Keys(), " ") != strings.Join(c.SnapshotKeys, " ") {
			t.Fatalf("%s: snapshot keys %v", c.Name, snapshot.Keys())
		}
		for key, want := range c.Idle {
			got, _ := snapshot.Get(key)
			if fmt.Sprint(got) != fmt.Sprint(want) && !(got == nil && want == nil) {
				t.Errorf("%s: idle %s %v, want %v", c.Name, key, got, want)
			}
		}
		lastValue, _ := snapshot.Get("last")
		outcomeValue, _ := lastValue.(*pyjson.Object).Get(su.store.Name)
		outcome := outcomeValue.(*pyjson.Object)
		if strings.Join(outcome.Keys(), " ") != strings.Join(c.Keys, " ") {
			t.Fatalf("%s: keys %v, want %v", c.Name, outcome.Keys(), c.Keys)
		}
		for key, want := range c.Types {
			got, _ := outcome.Get(key)
			if valueType(got) != want {
				t.Errorf("%s: %s is %s, want %s", c.Name, key, valueType(got), want)
			}
		}
		for key, want := range c.Values {
			got, _ := outcome.Get(key)
			if text, ok := got.(string); ok && key == "error" {
				got = strings.ReplaceAll(text, su.root, "<root>")
			}
			gotJSON, err := pyjson.Dumps(got)
			if err != nil {
				t.Fatal(err)
			}
			var gotValue any
			json.Unmarshal(gotJSON, &gotValue)
			if fmt.Sprint(gotValue) != fmt.Sprint(want) {
				t.Errorf("%s: %s %v, want %v", c.Name, key, gotValue, want)
			}
		}
	}
}

func running(snapshot *pyjson.Object) bool {
	v, _ := snapshot.Get("running")
	return v == true
}

// pacedStore hands over one entry per token, so a test decides how far a
// scan has come. Once done is closed it hands over the rest at once.
type pacedStore struct {
	stores.Store
	tokens <-chan struct{}
	done   <-chan struct{}
}

func (p *pacedStore) Entries(counts *stores.ScanCounts, yield func(stores.Entry) error) error {
	return p.Store.Entries(counts, func(e stores.Entry) error {
		select {
		case <-p.tokens:
		case <-p.done:
		}
		return yield(e)
	})
}

// pace makes the scans of the job of a paced: each entry waits for a token.
func pace(t *testing.T, a *app) chan<- struct{} {
	t.Helper()
	tokens, done := make(chan struct{}), make(chan struct{})
	a.srv.job.setRun(func(s stores.Store, ix *index.Index, progress func(int)) (scan.Result, error) {
		return scan.Run(&pacedStore{Store: s, tokens: tokens, done: done}, ix, progress)
	})
	t.Cleanup(func() { close(done) })
	return tokens
}

type indexHandle struct{ ix *index.Index }

func openIndex(t *testing.T, path string) *indexHandle {
	t.Helper()
	ix, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return &indexHandle{ix}
}

func mustStore(t *testing.T, sc config.StoreConfig) stores.Store {
	t.Helper()
	s, err := stores.Make(sc, "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestScanJobAndRequestsAtTheSameTime(t *testing.T) {
	a, store := scanApp(t, 300)
	tokens := pace(t, a)
	r := a.post([]byte(`{"store": "s1"}`), "X-Artifact-Repository", "1", "Host", a.localhost())
	expect(t, r, 202)
	if !regexp.MustCompile(`^\{"running":true,"store":"s1","progress":0,"started_ms":\d{13},"last":\{\}\}$`).Match(r.body) {
		t.Fatalf("%s", r.body)
	}
	for n := 0; n < 250; n++ {
		tokens <- struct{}{}
	}
	state := a.scanState()
	if state["running"] != true || state["store"] != "s1" || state["progress"] != json.Number("200") {
		t.Fatalf("%v", state)
	}
	conflict := a.post([]byte(`{"store": "tree"}`), "X-Artifact-Repository", "1", "Host", a.localhost())
	expect(t, conflict, 409)
	if !regexp.MustCompile(`^\{"error":"a scan is running","scan":\{"running":true,"store":"s1","progress":200,"started_ms":\d{13},"last":\{\}\}\}$`).Match(conflict.body) {
		t.Fatalf("%s", conflict.body)
	}
	// requests are answered while the scan holds the index
	var wg sync.WaitGroup
	errs := make(chan string, 100)
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 10; n++ {
				for _, target := range []string{"/api/status", "/api/repositories", "/api/browse/demo-releases?path=com/example&limit=5",
					"/repository/demo-releases/com/example/file-000007.txt", "/api/asset/demo-releases?path=com/example/file-000001.txt"} {
					conn, err := net.DialTimeout("tcp", a.addr(), 5*time.Second)
					if err != nil {
						errs <- err.Error()
						return
					}
					fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", target, a.addr())
					answer, err := http.ReadResponse(bufio.NewReader(conn), nil)
					if err != nil || answer.StatusCode != 200 {
						errs <- fmt.Sprintf("%s: %v %v", target, err, answer)
					}
					conn.Close()
				}
			}
		}()
	}
	feeding := make(chan struct{})
	go func() {
		for {
			select {
			case tokens <- struct{}{}:
			case <-feeding:
				return
			}
		}
	}()
	seen := []int64{200}
	for {
		state := a.scanState()
		if state["running"] != true {
			break
		}
		progress, _ := state["progress"].(json.Number).Int64()
		seen = append(seen, progress)
	}
	close(feeding)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] {
			t.Fatalf("the progress fell: %v", seen)
		}
	}
	last := a.scanState()["last"].(map[string]any)["s1"].(map[string]any)
	if last["live"] != json.Number("300") || last["error"] != nil {
		t.Fatalf("%v", last)
	}
	_ = store
}

func TestScanIsCancelledWhenTheServerCloses(t *testing.T) {
	a, _ := scanApp(t, 300)
	tokens := pace(t, a)
	expect(t, a.post([]byte(`{"store": "s1"}`), "X-Artifact-Repository", "1", "Host", a.localhost()), 202)
	for n := 0; n < 10; n++ {
		tokens <- struct{}{}
	}
	closed := make(chan error, 1)
	go func() { closed <- a.srv.Close() }()
	deadline := time.Now().Add(10 * time.Second)
	for !a.srv.job.cancelled.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the cancel was not set")
		}
		time.Sleep(time.Millisecond)
	}
	go func() {
		for {
			select {
			case tokens <- struct{}{}:
			case <-time.After(5 * time.Second):
				return
			}
		}
	}()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Close did not return")
	}
	snapshot := a.srv.job.snapshot()
	lastValue, _ := snapshot.Get("last")
	outcomeValue, _ := lastValue.(*pyjson.Object).Get("s1")
	outcome := outcomeValue.(*pyjson.Object)
	if errText, _ := outcome.Get("error"); errText != "cancelled" ||
		strings.Join(outcome.Keys(), " ") != "live deleted copies no_bytes unparsable seconds store error finished_ms" {
		t.Fatalf("%v %v", outcome.Keys(), errText)
	}
	if _, err := a.srv.rp.index.Lookup("demo-releases", "com/example/file-000000.txt"); err == nil {
		t.Fatal("the index is still open after Close")
	}
	// the index keeps the rows of the scan before
	ih := openIndex(t, a.cfg.IndexPath())
	stats, err := ih.ix.StoreStats()
	if err != nil || len(stats) != 2 || stats[0].Store != "s1" || stats[0].Files != 300 {
		t.Fatalf("%v %v", stats, err)
	}
	// a stopped job starts nothing
	if a.srv.job.start(mustStore(t, a.cfg.Stores[0])) {
		t.Fatal("a scan started after the stop")
	}
}

func TestCloseLeavesTheIndexToAScanThatDoesNotStop(t *testing.T) {
	a, _ := scanApp(t, 1)
	release := make(chan struct{})
	a.srv.job.setRun(func(s stores.Store, ix *index.Index, progress func(int)) (scan.Result, error) {
		<-release
		return scan.Run(s, ix, progress)
	})
	old := scanStopTimeout
	scanStopTimeout = 50 * time.Millisecond
	t.Cleanup(func() { scanStopTimeout = old })
	expect(t, a.post([]byte(`{"store": "s1"}`), "X-Artifact-Repository", "1", "Host", a.localhost()), 202)
	started := time.Now()
	if err := a.srv.Close(); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(started); waited < 50*time.Millisecond || waited > 5*time.Second {
		t.Fatalf("Close waited %v", waited)
	}
	if _, err := a.srv.rp.index.Lookup("demo-releases", "com/example/file-000000.txt"); err != nil {
		t.Fatalf("the index was closed under a running scan: %v", err)
	}
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for running(a.srv.job.snapshot()) {
		if time.Now().After(deadline) {
			t.Fatal("the scan did not end")
		}
		time.Sleep(time.Millisecond)
	}
}

// Framework answers

func TestFrameworkAnswersOfTheAPIRoutes(t *testing.T) {
	a := browseApp(t)
	methodNotAllowed := `{"detail":"Method Not Allowed"}`
	for _, c := range []struct {
		target string
		allow  string
	}{
		{"/", "GET"}, {"/api/status", "GET"}, {"/api/repositories", "GET"}, {"/api/browse/demo-raw", "GET"}, {"/api/scan", "POST"},
		{"/service/rest/repository/browse/demo-raw", "GET, HEAD"}, {"/service/rest/repository/browse/demo-raw/docs", "GET, HEAD"},
	} {
		for _, method := range []string{"GET", "HEAD", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"} {
			if strings.Contains(c.allow, method) {
				continue
			}
			var body []byte
			if method == "POST" || method == "PUT" || method == "PATCH" {
				body = []byte{}
			}
			r := sendRequest(t, a.addr(), method, c.target, body)
			want := methodNotAllowed
			if method == "HEAD" {
				want = ""
			}
			if r.status != 405 || string(r.body) != want || r.header.Get("Allow") != c.allow ||
				r.header.Get("Content-Type") != "application/json" || r.header.Get("Content-Length") != "31" {
				t.Errorf("%s %s: %s", method, c.target, r)
			}
		}
	}
	for _, c := range []struct {
		method, target, location string
	}{
		{"GET", "/api/status/", "/api/status"},
		{"GET", "/api/repositories/", "/api/repositories"},
		{"GET", "/api/browse/demo-raw/", "/api/browse/demo-raw"},
		{"GET", "/api/browse/demo-raw/?path=docs", "/api/browse/demo-raw?path=docs"},
		{"GET", "/api/scan/", "/api/scan"},
		{"POST", "/api/scan/", "/api/scan"},
		{"GET", "/service/rest/repository/browse", "/service/rest/repository/browse/"},
		{"HEAD", "/service/rest/repository/browse?x=1", "/service/rest/repository/browse/?x=1"},
	} {
		r := sendRequest(t, a.addr(), c.method, c.target, nil)
		if r.status != 307 || r.header.Get("Location") != "http://"+a.addr()+c.location || r.header.Get("Content-Length") != "0" {
			t.Errorf("%s %s: %s", c.method, c.target, r)
		}
	}
	for _, target := range []string{"/api/browse", "/api/browse/", "/api/browse/a%2Fb", "/api/scanx", "/api/statusx", "//"} {
		expectBody(t, a.get(target), 404, `{"detail":"Not Found"}`)
	}
}

func TestBrowseRoutes(t *testing.T) {
	a := browseApp(t)
	base := t.TempDir()
	extra := newApp(t, []config.StoreConfig{blobStore(t, base, "s1",
		spec("demo-docker", "v2/-/blobs/sha256:"+strings.Repeat("0", 64), []byte("layer"), ""),
		spec("demo-npm", "demo-lib/-/demo-lib-1.0.0.tgz", []byte("tgz"), ""),
		spec("demo-pypi", "pkg/a.whl", []byte("whl"), ""))},
		appOptions{defs: []*pyjson.Object{dockerDef("demo-docker", 14555), def("demo-npm", "npm", "hosted"), def("demo-pypi", "pypi", "hosted")}})
	notFound := "Not found\n"
	dockerText := "Repository demo-docker has format docker; it answers the registry API under /repository/demo-docker/v2/ and on its Docker port\n"
	for _, c := range []struct {
		on             *app
		method, target string
		status         int
		body, location string
	}{
		{a, "GET", "/service/rest/repository/browse/", 404, notFound, ""},
		{a, "GET", "/service/rest/repository/browse//x", 404, notFound, ""},
		{a, "GET", "/service/rest/repository/browse/demo-raw", 302, "", "/service/rest/repository/browse/demo-raw/"},
		{a, "HEAD", "/service/rest/repository/browse/demo-raw", 302, "", "/service/rest/repository/browse/demo-raw/"},
		{a, "GET", "/service/rest/repository/browse/demo-raw/docs", 302, "", "/service/rest/repository/browse/demo-raw/docs/"},
		{a, "GET", "/service/rest/repository/browse/demo-raw/docs?x=1", 302, "", "/service/rest/repository/browse/demo-raw/docs/"},
		{a, "HEAD", "/service/rest/repository/browse/demo-raw/docs", 302, "", "/service/rest/repository/browse/demo-raw/docs/"},
		{a, "GET", "/service/rest/repository/browse/demo-group/com", 302, "", "/service/rest/repository/browse/demo-group/com/"},
		{a, "GET", "/service/rest/repository/browse/demo%20raw?x=1", 404, "Repository demo raw not found\n", ""},
		{a, "GET", "/service/rest/repository/browse/demo-raw/docs/readme.txt", 404, notFound, ""},
		{a, "HEAD", "/service/rest/repository/browse/demo-raw/docs/readme.txt", 404, "", ""},
		{a, "GET", "/service/rest/repository/browse/demo-raw/nowhere", 404, notFound, ""},
		{a, "GET", "/service/rest/repository/browse/demo-releases/com/example/a.txt.sha1", 404, notFound, ""},
		{a, "GET", "/service/rest/repository/browse/nope/", 404, "Repository nope not found\n", ""},
		{a, "GET", "/service/rest/repository/browse/nope", 404, "Repository nope not found\n", ""},
		{a, "GET", "/service/rest/repository/browse/demo-raw/a/../b", 400, "Bad request path: empty, '.' or '..' segment\n", ""},
		{a, "GET", "/service/rest/repository/browse/demo-raw//x", 400, "Bad request path: empty, '.' or '..' segment\n", ""},
		{a, "GET", "/service/rest/repository/browse/demo-raw/%FF", 400, "Bad request path: not UTF-8\n", ""},
		{a, "GET", "/service/rest/repository/browse/demo-raw/a%00", 400, "Bad request path: NUL byte\n", ""},
		{extra, "GET", "/service/rest/repository/browse/demo-docker/", 404, dockerText, ""},
		{extra, "GET", "/service/rest/repository/browse/demo-docker/v2/x", 404, dockerText, ""},
		{extra, "GET", "/service/rest/repository/browse/demo-npm/demo-lib", 302, "", "/service/rest/repository/browse/demo-npm/demo-lib/"},
		{extra, "GET", "/service/rest/repository/browse/demo-npm/-/ping", 404, notFound, ""},
		{extra, "GET", "/service/rest/repository/browse/demo-npm/demo-lib/-/demo-lib-1.0.0.tgz", 404, notFound, ""},
		{extra, "GET", "/service/rest/repository/browse/demo-pypi/pkg", 302, "", "/service/rest/repository/browse/demo-pypi/pkg/"},
	} {
		r := sendRequest(t, c.on.addr(), c.method, c.target, nil)
		if r.status != c.status || string(r.body) != c.body || r.header.Get("Location") != c.location {
			t.Errorf("%s %s: %s", c.method, c.target, r)
		}
		if c.status != 302 && r.header.Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Errorf("%s %s: %v", c.method, c.target, r.header)
		}
	}
	expect(t, a.get("/service/rest/repository/browse/demo-raw/", "Host", "evil.example"), 403)
}

// TestScanOfAManifestPastTheDigitLimit: int() of Python reads at most 4300
// digits, so a manifest line with a longer size fails the scan with the
// ValueError of int(), as the reference reported it on 2026-11-12.
func TestScanOfAManifestPastTheDigitLimit(t *testing.T) {
	base := t.TempDir()
	tree := treeStore(t, base, "t3", map[string][]byte{"demo-raw/a/ovf.txt": []byte("a")})
	manifest := filepath.Join(base, "t3-manifest", "part-00000.tsv")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o750); err != nil {
		t.Fatal(err)
	}
	line := "s\tb\tdemo-raw\ta/ovf.txt\t" + strings.Repeat("9", 4301) + "\t1\t0\t\t\n"
	if err := os.WriteFile(manifest, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(manifest)
	tree.Manifest = &dir
	a := newApp(t, []config.StoreConfig{tree}, appOptions{noScan: true})
	expect(t, a.post([]byte(`{"store": "t3"}`), "X-Artifact-Repository", "1", "Host", a.localhost()), 202)
	last := a.waitIdle()["last"].(map[string]any)["t3"].(map[string]any)
	want := "Exceeds the limit (4300 digits) for integer string conversion: value has 4301 digits; " +
		"use sys.set_int_max_str_digits() to increase the limit"
	if last["error"] != want {
		t.Fatalf("%v", last)
	}
}
